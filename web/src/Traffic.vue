<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import Icon from './Icon.vue'
import ServiceAvatar from './components/ServiceAvatar.vue'
import UiTooltip from './components/ui/UiTooltip.vue'
import { usePageScope } from './page-scope'
import { errorDetails, isRequestCancelled } from './request'
import { upstreamPortOf } from './service-entry'
import { formatTrafficBytes as bytes, formatTrafficPercent as percent, trafficShare, isRemovedTrafficService, serviceTrafficRows, trendDays } from './traffic-view'
import { formatDateTime } from './utils/datetime'
import type { TrafficPeriod } from './traffic-view'
import type { Service, TrafficCounter, TrafficResponse } from './api-types'

const props = withDefaults(defineProps<{ services?: Service[] }>(), { services: () => [] })
const scope = usePageScope()
const report = ref<TrafficResponse | null>(null)
const snapshot = ref<TrafficResponse | null>(null)
const loading = ref(false)
const loadError = ref('')
const servicePeriod = ref<TrafficPeriod>('today')
const now = ref(Date.now())
const selectedDate = ref('')
const trafficCopy = {
  removedHistory: '已移除服务（历史合计）',
  noCurrentUsage: '暂无当前服务用量',
  retentionScope: '服务用量仅展示未移除的服务；已移除服务不再逐项展示，其历史用量仍计入总流量、趋势和分布中的历史合计。',
}
let disposed = false
let sequence = 0
let timer: ReturnType<typeof setTimeout> | undefined

const available = computed(() => Boolean(snapshot.value && (
  ['collecting', 'waiting'].includes(snapshot.value.status) || snapshot.value.totals.requests > 0 || snapshot.value.totals.totalBytes > 0)))
const warnings = computed(() => report.value?.warnings || [])
const periodLabel = computed(() => servicePeriod.value === 'today' ? '今日' : '累计')
const selected = computed(() => available.value ? snapshot.value?.[servicePeriod.value] : undefined)
const allRows = computed(() => serviceTrafficRows(available.value ? snapshot.value?.services : [], servicePeriod.value, selected.value?.totalBytes))
const rows = computed(() => allRows.value.filter(service => !isRemovedTrafficService(service)))
const removedRows = computed(() => allRows.value.filter(isRemovedTrafficService))
const metadata = computed(() => new Map(props.services.map(service => [service.id, service])))
const colors = ['var(--blue)', 'var(--purple)', 'var(--cyan)', 'var(--green)', 'var(--amber)', 'var(--rose)']
// A service keeps its color while the global period changes the ranking.
const colorByID = computed(() => new Map([...(snapshot.value?.services || [])].sort((a, b) => a.serviceId.localeCompare(b.serviceId))
  .map((service, index) => [service.serviceId, colors[index % colors.length]])))
const serviceColor = (id: string) => colorByID.value.get(id) || colors[0]
const volume = (value: number | undefined) => typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : 0
const direction = computed(() => ({ up: trafficShare(selected.value?.receivedBytes, selected.value?.totalBytes), down: trafficShare(selected.value?.sentBytes, selected.value?.totalBytes) }))
const distributionMismatch = computed(() => available.value && allRows.value.reduce((sum, row) => sum + volume(row.counter.totalBytes), 0) > volume(selected.value?.totalBytes))
const distribution = computed(() => {
  if (!available.value || distributionMismatch.value) return []
  const list = rows.value.slice(0, 5).map(row => ({ id: row.serviceId, name: row.name || row.serviceId, bytes: volume(row.counter.totalBytes), share: row.share, color: serviceColor(row.serviceId) }))
  if (rows.value.length > 5) {
    const rest = rows.value.slice(5).reduce((sum, row) => sum + volume(row.counter.totalBytes), 0)
    list.push({ id: '__other', name: `其他 ${rows.value.length - 5} 个服务`, bytes: rest, share: trafficShare(rest, selected.value?.totalBytes), color: 'var(--soft)' })
  }
  const removed = removedRows.value.reduce((sum, row) => sum + volume(row.counter.totalBytes), 0)
  if (removed > 0) list.push({ id: '__removed', name: trafficCopy.removedHistory, bytes: removed, share: trafficShare(removed, selected.value?.totalBytes), color: 'var(--muted)' })
  const missing = volume(selected.value?.totalBytes) - list.reduce((sum, item) => sum + item.bytes, 0)
  if (missing > 0) list.push({ id: '__unassigned', name: '未归类记录', bytes: missing, share: trafficShare(missing, selected.value?.totalBytes), color: 'var(--soft)' })
  return list
})
const donutSegments = computed(() => {
  let offset = 0
  return distribution.value.filter(item => item.share > 0).map(item => { const part = { ...item, offset }; offset += item.share; return part })
})
const days = computed(() => trendDays(available.value ? snapshot.value?.days : [], now.value))
const recordedDays = computed(() => days.value.filter(day => day.counter !== null))
const selectedDay = computed(() => days.value.find(day => day.date === selectedDate.value) || recordedDays.value.at(-1) || days.value.at(-1))
const peakBytes = computed(() => Math.max(1, ...days.value.map(day => volume(day.value ?? undefined))))
const todayDate = computed(() => days.value.at(-1)?.date || '')
const historyMetrics: Array<{ key: keyof TrafficCounter; label: string }> = [
  { key: 'receivedBytes', label: '上行' }, { key: 'sentBytes', label: '下行' }, { key: 'totalBytes', label: '合计' }, { key: 'requests', label: '请求数' },
]
function count(value: number | undefined) { return value === undefined || !Number.isFinite(value) || value < 0 ? '—' : value.toLocaleString('zh-CN', { maximumFractionDigits: 0 }) }
function counter(value: number | undefined, key: keyof TrafficCounter) { return key === 'requests' ? count(value) : bytes(value) }
function serviceBadge(service: TrafficResponse['services'][number]) {
  if (service.serviceStatus === 'disabled') return '已停用'
  if (service.serviceStatus === 'draft') return '未发布'
  if (service.serviceStatus === 'unknown') return '状态待确认'
  if (service.serviceStatus === 'active') return ''
  return service.enabled ? '' : '未启用'
}
function serviceMeta(service: TrafficResponse['services'][number]) { return metadata.value.get(service.serviceId) || { name: service.name } }
function activity(service: TrafficResponse['services'][number]) {
  const meta = metadata.value.get(service.serviceId)
  const port = meta ? upstreamPortOf(meta.upstream) : 0
  const recent = formatDateTime(service.lastSeenAt, '')
  return [port ? `上游 :${port}` : '', recent || ''].filter(Boolean).join(' · ')
}
function dayLabel(day: typeof days.value[number]) { return `${day.date}${day.date === todayDate.value ? '（今日）' : ''} · ${day.counter ? `${bytes(day.counter.totalBytes)}，${count(day.counter.requests)} 次请求` : '暂无记录'}` }
function dayHeight(day: typeof days.value[number]) { return day.value === null || day.value === 0 ? '3px' : `${Math.max(3, Math.min(100, day.value / peakBytes.value * 100))}%` }
function stopTimer() { clearTimeout(timer); timer = undefined }
function schedule() { stopTimer(); if (!disposed && document.visibilityState !== 'hidden') timer = setTimeout(refresh, 30000) }
async function refresh() {
  if (disposed || loading.value || document.visibilityState === 'hidden') return
  const current = ++sequence
  stopTimer(); loading.value = true; loadError.value = ''
  try {
    const result = await scope.api.traffic({ timeoutMs: 10000 })
    if (disposed || current !== sequence) return
    report.value = result
    if (result.status !== 'error') snapshot.value = result
    else if (!snapshot.value && result.totals.requests > 0) snapshot.value = result
    now.value = Date.now()
  } catch (error) {
    if (!disposed && current === sequence && !isRequestCancelled(error)) loadError.value = errorDetails(error).message
  } finally {
    if (!disposed && current === sequence) { loading.value = false; schedule() }
  }
}
function visibility() {
  if (document.visibilityState === 'hidden') { stopTimer(); sequence++; scope.reset(); loading.value = false }
  else void refresh()
}
onMounted(() => { document.addEventListener('visibilitychange', visibility); void refresh() })
onBeforeUnmount(() => { disposed = true; sequence++; stopTimer(); document.removeEventListener('visibilitychange', visibility) })
</script>

<template>
  <div class="traffic-page" :aria-busy="loading">
    <div v-if="loadError" class="alert error" role="alert"><Icon name="warning" :size="18" /><span>{{ loadError }}</span></div>
    <div v-if="warnings.length" class="alert warning traffic-warnings" role="status"><Icon name="info" :size="18" /><ul><li v-for="(warning, index) in warnings" :key="index">{{ warning }}</li></ul></div>
    <section class="panel traffic-summary-band" aria-label="流量概览">
      <div class="traffic-summary-top"><span class="traffic-section-label">{{ periodLabel }}用量</span><div class="traffic-actions"><div class="traffic-period" role="group" aria-label="统计时段"><button type="button" :aria-pressed="servicePeriod === 'today'" @click="servicePeriod = 'today'">今日</button><button type="button" :aria-pressed="servicePeriod === 'totals'" @click="servicePeriod = 'totals'">累计</button></div><button class="button secondary traffic-refresh" type="button" :disabled="loading" @click="refresh"><Icon name="refresh" :size="15" />{{ loading ? '读取中…' : '刷新' }}</button></div></div>
      <div class="traffic-summary-grid">
        <div class="traffic-summary-total"><span class="traffic-metric-title">{{ periodLabel }}总流量</span><strong class="traffic-metric-value" data-metric="totalBytes">{{ bytes(selected?.totalBytes) }}</strong></div>
        <div class="traffic-summary-count"><span class="traffic-metric-title">请求数</span><div><strong class="traffic-metric-value" data-metric="requests">{{ count(selected?.requests) }}</strong></div></div>
        <div class="traffic-directions"><div class="traffic-direction"><span class="traffic-metric-title"><Icon name="upload" :size="13" />上行</span><div><strong class="traffic-direction-value" data-metric="receivedBytes">{{ bytes(selected?.receivedBytes) }}</strong><span v-if="selected" class="traffic-ratio">{{ percent(direction.up) }}</span></div></div><div class="traffic-direction"><span class="traffic-metric-title"><Icon name="download" :size="13" />下行</span><div><strong class="traffic-direction-value" data-metric="sentBytes">{{ bytes(selected?.sentBytes) }}</strong><span v-if="selected" class="traffic-ratio">{{ percent(direction.down) }}</span></div></div><div class="traffic-direction-bar" aria-hidden="true"><span class="traffic-direction-up" :style="{ width: `${direction.up}%` }"></span><span class="traffic-direction-down" :style="{ width: `${direction.down}%` }"></span></div></div>
      </div>
    </section>

    <div class="traffic-middle">
      <section class="panel traffic-ranking" aria-label="服务流量排行"><div class="panel-header traffic-panel-heading"><h2>服务用量</h2><span>{{ periodLabel }} · 按流量排序</span></div>
        <ol v-if="rows.length" class="traffic-ranking-list" tabindex="0" aria-label="服务用量排行列表">
          <li v-for="(service, index) in rows" :key="service.serviceId" class="traffic-ranking-item" :data-service-id="service.serviceId" :style="{ '--service-color': serviceColor(service.serviceId) }">
            <div class="traffic-service-identity"><span class="traffic-rank">{{ index + 1 }}</span><ServiceAvatar :service="serviceMeta(service)" /><div class="traffic-service-copy"><UiTooltip :text="service.name || service.serviceId"><strong class="traffic-service-name" tabindex="0">{{ service.name || service.serviceId }}</strong></UiTooltip><span v-if="activity(service)" class="traffic-service-activity">{{ activity(service) }}</span><span v-if="serviceBadge(service)" class="traffic-service-state">{{ serviceBadge(service) }}</span></div></div>
            <div class="traffic-service-meter"><div class="traffic-share-track" role="meter" :aria-label="`${service.name || service.serviceId}流量占比`" :aria-valuenow="service.share" :aria-valuemin="0" :aria-valuemax="100" :aria-valuetext="percent(service.share)"><span :style="{ width: `${service.share}%` }"></span></div><div class="traffic-service-directions"><span><Icon name="upload" :size="11" />{{ bytes(service.counter.receivedBytes) }}</span><span><Icon name="download" :size="11" />{{ bytes(service.counter.sentBytes) }}</span></div></div>
            <div class="traffic-service-total"><strong>{{ bytes(service.counter.totalBytes) }}</strong><span>{{ count(service.counter.requests) }} 次请求 <b>· {{ percent(service.share) }}</b></span></div>
          </li>
        </ol><div v-else class="compact-empty traffic-empty"><h3>{{ loading && !report ? '读取中…' : removedRows.length ? trafficCopy.noCurrentUsage : '暂无记录' }}</h3></div>
      </section>
      <section class="panel traffic-distribution" aria-label="服务流量分布"><div class="panel-header traffic-panel-heading"><h2>流量分布</h2><span>{{ periodLabel }}</span></div>
        <div class="traffic-donut" role="img" :aria-label="distributionMismatch ? '分布数据待核对' : `${periodLabel}服务流量分布，总流量 ${bytes(selected?.totalBytes)}`"><svg viewBox="0 0 128 128" aria-hidden="true"><circle class="traffic-donut-track" cx="64" cy="64" r="49" /><g transform="rotate(-90 64 64)"><circle v-for="segment in donutSegments" :key="segment.id" class="traffic-donut-segment" cx="64" cy="64" r="49" pathLength="100" :stroke="segment.color" :stroke-dasharray="`${segment.share} ${100 - segment.share}`" :stroke-dashoffset="-segment.offset" /></g></svg><div class="traffic-donut-center"><template v-if="available && !distributionMismatch"><span>总流量</span><strong>{{ bytes(selected?.totalBytes) }}</strong></template><strong v-else>{{ distributionMismatch ? '待核对' : '暂无记录' }}</strong></div></div>
        <ul v-if="distribution.length" class="traffic-legend"><li v-for="item in distribution" :key="item.id"><span class="traffic-legend-name"><i :style="{ background: item.bytes > 0 ? item.color : 'var(--line)' }"></i><UiTooltip :text="item.name"><span tabindex="0">{{ item.name }}</span></UiTooltip></span><span class="traffic-legend-value"><b>{{ percent(item.share) }}</b><span>{{ bytes(item.bytes) }}</span></span></li></ul><p v-else-if="distributionMismatch" class="traffic-distribution-note">分布数据待核对</p>
      </section>
    </div>

    <section class="panel traffic-trend" aria-label="近 30 天流量趋势"><div class="panel-header traffic-panel-heading"><h2>近 30 天趋势</h2><span>每日合计 · 北京时间</span></div>
      <template v-if="available && recordedDays.length"><div class="traffic-trend-body"><div class="traffic-trend-bars" role="group" aria-label="每日流量，选择日期查看用量"><UiTooltip v-for="day in days" :key="day.date" :text="dayLabel(day)"><button type="button" class="traffic-trend-day" :class="{ 'is-selected': selectedDay?.date === day.date, 'is-missing': day.counter === null, 'is-zero': day.value === 0 }" :aria-label="dayLabel(day)" :aria-pressed="selectedDay?.date === day.date" @click="selectedDate = day.date" @focus="selectedDate = day.date" @mouseenter="selectedDate = day.date"><span class="traffic-day-bar" :style="{ height: dayHeight(day) }"></span></button></UiTooltip></div><div class="traffic-trend-axis" aria-hidden="true"><span>{{ days[0]?.date.slice(5) }}</span><span>{{ days[14]?.date.slice(5) }}</span><span>今日</span></div><div class="traffic-day-detail" aria-label="所选日期用量"><span>{{ selectedDay?.date }}<b v-if="selectedDay?.date === todayDate">今日</b></span><template v-if="selectedDay?.counter"><span class="traffic-day-directions">↑ {{ bytes(selectedDay.counter.receivedBytes) }} <i></i> ↓ {{ bytes(selectedDay.counter.sentBytes) }}</span><strong>{{ bytes(selectedDay.counter.totalBytes) }}</strong><span>{{ count(selectedDay.counter.requests) }} 次请求</span></template><span v-else>暂无记录</span></div></div>
        <details class="traffic-history"><summary><span>历史明细</span><Icon name="chevron-down" :size="15" /></summary><div class="traffic-table-wrap" tabindex="0" aria-label="历史用量明细，可横向滚动"><table class="traffic-table"><thead><tr><th scope="col">日期</th><th v-for="metric in historyMetrics" :key="metric.key" scope="col">{{ metric.label }}</th></tr></thead><tbody><tr v-for="day in [...recordedDays].reverse()" :key="day.date"><th scope="row">{{ day.date }}</th><td v-for="metric in historyMetrics" :key="metric.key">{{ counter(day.counter?.[metric.key], metric.key) }}</td></tr></tbody></table></div></details>
      </template><div v-else class="compact-empty traffic-empty"><h3>{{ loading && !report ? '读取中…' : '暂无记录' }}</h3></div>
    </section>
    <details class="traffic-scope" aria-label="统计口径"><summary>统计口径<Icon name="chevron-down" :size="14" /></summary><ul><li>只统计本项目已发布服务的 HTTP 代理请求：上行包含请求行、请求头和请求体，下行包含响应头；不重复累加上游一段。</li><li>请求结束才记账，长下载与长连接在结束前暂不计入；WebSocket 升级后的上传帧不能完整计量。</li><li>边缘加速服务（EdgeOne / Cloudflare）只统计回源流量，不包含云端缓存命中。80 跳转、未知域名和管理探测不在服务用量内。</li><li>不含 SSH、EasyTier 协议开销，也不是整机网卡统计或实时网速。累计包含已采集及可恢复的日志记录；未保留日志无法追溯。</li><li>{{ trafficCopy.retentionScope }}</li></ul></details>
  </div>
</template>

<style scoped>
.traffic-page{display:grid;gap:20px;min-width:0}.traffic-page .panel{margin:0;min-width:0}
.traffic-warnings{align-items:flex-start;margin:0}.traffic-warnings ul{margin:0;padding-left:18px;line-height:1.7}
.traffic-summary-band{padding:18px 24px 24px}.traffic-summary-top,.traffic-actions{display:flex;align-items:center;justify-content:space-between;gap:12px}.traffic-summary-top{margin-bottom:18px}.traffic-actions{justify-content:flex-end}.traffic-section-label{font-size:12px;font-weight:550;color:var(--muted)}
.traffic-period{display:flex;gap:2px;padding:3px;border:1px solid var(--line);border-radius:8px;background:var(--surface-2)}.traffic-period button{padding:4px 14px;border:0;border-radius:5px;background:transparent;color:var(--muted);font:inherit;font-size:12px;cursor:pointer}.traffic-period button[aria-pressed="true"]{background:var(--surface);color:var(--blue);box-shadow:0 1px 3px color-mix(in srgb,var(--ink) 8%,transparent)}
.traffic-summary-grid{display:grid;grid-template-columns:minmax(145px,1fr) minmax(100px,.7fr) minmax(260px,1.7fr);align-items:center}.traffic-summary-total,.traffic-summary-count{min-width:0;padding-right:22px}.traffic-summary-count,.traffic-directions{border-left:1px solid var(--line);padding-left:26px}.traffic-metric-title{display:flex;align-items:center;gap:5px;color:var(--muted);font-size:12px;margin-bottom:7px}.traffic-metric-value{font-family:var(--mono);font-size:29px;font-weight:600;line-height:1.3;letter-spacing:-1px;font-variant-numeric:tabular-nums;overflow-wrap:anywhere}.traffic-summary-total .traffic-metric-value{color:var(--blue)}.traffic-summary-count .traffic-metric-value{font-size:25px}
.traffic-directions{display:grid;grid-template-columns:1fr 1fr;gap:12px 24px;min-width:0}.traffic-direction{min-width:0}.traffic-direction>div{display:flex;align-items:baseline;gap:9px;flex-wrap:wrap}.traffic-direction-value{font-family:var(--mono);font-size:20px;font-weight:550;line-height:1.3;font-variant-numeric:tabular-nums;overflow-wrap:anywhere}.traffic-ratio{font-size:11px;color:var(--muted);font-variant-numeric:tabular-nums}.traffic-direction-bar{display:flex;grid-column:1/-1;height:4px;overflow:hidden;border-radius:3px;background:var(--line)}.traffic-direction-up{background:var(--blue)}.traffic-direction-down{background:var(--cyan)}
.traffic-middle{display:grid;grid-template-columns:minmax(0,1.85fr) minmax(280px,1fr);gap:20px;align-items:stretch;min-width:0}.traffic-panel-heading{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap}.traffic-panel-heading h2{margin:0}.traffic-panel-heading>span{font-size:11px;color:var(--muted)}
.traffic-ranking-list{display:grid;gap:10px;margin:0;padding:16px;list-style:none;max-height:410px;overflow:auto}.traffic-ranking-item{display:grid;grid-template-columns:minmax(155px,1.05fr) minmax(100px,1.35fr) minmax(80px,auto);align-items:center;gap:16px;padding:14px 15px;background:var(--surface-2);border-radius:9px;min-width:0}.traffic-service-identity{display:flex;align-items:center;gap:10px;min-width:0}.traffic-rank{width:12px;flex-shrink:0;color:var(--soft);font:11px var(--mono)}
.traffic-service-identity :deep(.service-avatar){width:34px;height:34px;border-radius:9px}.traffic-service-identity :deep(.service-avatar svg){width:19px;height:19px}.traffic-service-copy{min-width:0;display:flex;flex-direction:column;gap:3px}.traffic-service-copy :deep(.ui-tooltip){max-width:100%}.traffic-service-name{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:13px;font-weight:550}.traffic-service-activity{font-size:11px;line-height:1.5;color:var(--muted);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.traffic-service-state{font-size:11px;color:var(--muted)}
.traffic-service-meter{min-width:0}.traffic-share-track{height:5px;border-radius:4px;background:var(--line);overflow:hidden}.traffic-share-track>span{display:block;height:100%;border-radius:4px;background:var(--service-color)}.traffic-service-directions{display:flex;align-items:center;justify-content:space-between;gap:6px;margin-top:7px;color:var(--muted);font-size:11px;font-variant-numeric:tabular-nums}.traffic-service-directions>span{display:inline-flex;align-items:center;gap:3px;white-space:nowrap}.traffic-service-total{text-align:right;min-width:0}.traffic-service-total strong{display:block;font:550 13px var(--mono);white-space:nowrap}.traffic-service-total>span{display:block;margin-top:6px;font-size:11px;color:var(--muted);white-space:nowrap}.traffic-service-total b{font-weight:400}
.traffic-distribution{padding-bottom:18px}.traffic-donut{position:relative;width:180px;height:180px;margin:12px auto 9px}.traffic-donut svg{display:block;width:100%;height:100%;overflow:visible}.traffic-donut circle{fill:none;stroke-width:13}.traffic-donut-track{stroke:var(--line)}.traffic-donut-segment{stroke-linecap:butt}.traffic-donut-center{position:absolute;inset:0;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:4px;pointer-events:none}.traffic-donut-center>span{font-size:11px;color:var(--muted)}.traffic-donut-center>strong{max-width:115px;font:550 17px var(--mono);text-align:center;overflow-wrap:anywhere;line-height:1.4}
.traffic-legend{display:grid;gap:10px;list-style:none;margin:0;padding:0 22px}.traffic-legend li{display:flex;justify-content:space-between;align-items:center;gap:14px;min-width:0;font-size:11px}.traffic-legend-name{display:flex;align-items:center;gap:8px;color:var(--muted);min-width:0}.traffic-legend-name>i{width:7px;height:7px;border-radius:50%;flex-shrink:0}.traffic-legend-name :deep(.ui-tooltip){min-width:0;max-width:100%}.traffic-legend-name :deep(.ui-tooltip)>span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.traffic-legend-value{display:flex;align-items:center;gap:12px;white-space:nowrap;font-variant-numeric:tabular-nums}.traffic-legend-value>b{font-weight:550}.traffic-legend-value>span{min-width:55px;text-align:right;color:var(--muted)}.traffic-distribution-note{margin:10px 20px;text-align:center;color:var(--muted);font-size:12px}
.traffic-empty{min-height:200px}.traffic-empty h3{font-size:13px;font-weight:400;color:var(--muted)}.traffic-trend-body{padding:6px 24px 0}.traffic-trend-bars{display:grid;grid-template-columns:repeat(30,minmax(0,1fr));gap:5px;height:130px;align-items:end;background:linear-gradient(to top,var(--line) 1px,transparent 1px);min-width:0}.traffic-trend-bars :deep(.ui-tooltip){display:block;width:100%;height:100%;min-width:0}.traffic-trend-day{width:100%;height:100%;display:flex;align-items:flex-end;padding:0 1px;border:0;background:transparent;cursor:pointer}.traffic-day-bar{display:block;width:100%;border-radius:3px 3px 0 0;background:color-mix(in srgb,var(--blue) 55%,var(--surface));transition:background .15s}.traffic-trend-day.is-selected .traffic-day-bar,.traffic-trend-day:hover .traffic-day-bar{background:var(--blue)}.traffic-trend-day.is-zero .traffic-day-bar{background:var(--line)}.traffic-trend-day.is-missing .traffic-day-bar{background:repeating-linear-gradient(90deg,var(--line) 0 2px,transparent 2px 4px)}
.traffic-trend-axis{display:flex;justify-content:space-between;margin-top:9px;color:var(--soft);font:10px var(--mono)}.traffic-day-detail{display:flex;align-items:center;gap:16px;min-height:49px;font-size:11px;color:var(--muted);font-variant-numeric:tabular-nums;flex-wrap:wrap;padding:12px 0}.traffic-day-detail>span:first-child{margin-right:auto;color:var(--ink)}.traffic-day-detail b{margin-left:7px;font-weight:400;color:var(--blue)}.traffic-day-detail strong{color:var(--ink);font:550 12px var(--mono)}.traffic-day-directions{display:flex;align-items:center;gap:9px}.traffic-day-directions i{display:block;height:10px;border-left:1px solid var(--line)}
.traffic-history{border-top:1px solid var(--line)}.traffic-history>summary{display:flex;align-items:center;justify-content:space-between;padding:12px 24px;font-size:12px;color:var(--muted);cursor:pointer;list-style:none}.traffic-history>summary::-webkit-details-marker,.traffic-scope>summary::-webkit-details-marker{display:none}.traffic-history[open]>summary>svg,.traffic-scope[open]>summary>svg{transform:rotate(180deg)}.traffic-table-wrap{overflow:auto;border-top:1px solid var(--line)}.traffic-table{width:100%;min-width:540px;border-collapse:collapse;font-size:12px;text-align:right;font-variant-numeric:tabular-nums}.traffic-table th,.traffic-table td{padding:12px 24px;border-bottom:1px solid var(--line);white-space:nowrap}.traffic-table th:first-child{text-align:left}.traffic-table th{font-weight:500}.traffic-table thead{color:var(--muted);background:var(--surface-2)}.traffic-table tbody tr:last-child>*{border-bottom:0}
.traffic-scope{color:var(--muted);font-size:11px}.traffic-scope>summary{display:flex;align-items:center;gap:6px;cursor:pointer;list-style:none;width:max-content}.traffic-scope ul{margin:10px 0 0;padding-left:18px;line-height:1.9}.traffic-period button:focus-visible,.traffic-ranking-list:focus-visible,.traffic-trend-day:focus-visible,.traffic-history>summary:focus-visible,.traffic-scope>summary:focus-visible,.traffic-table-wrap:focus-visible{outline:2px solid var(--blue);outline-offset:3px;border-radius:4px}
@media(min-width:1500px){.traffic-ranking-item{grid-template-columns:minmax(175px,.85fr) minmax(140px,1.5fr) minmax(105px,auto);gap:22px}.traffic-donut{width:190px;height:190px}.traffic-service-activity,.traffic-service-directions,.traffic-service-total>span{font-size:11px}}
@media(max-width:1120px){.traffic-middle{grid-template-columns:minmax(0,1.5fr) minmax(250px,1fr);gap:16px}.traffic-ranking-item{grid-template-columns:minmax(0,1fr) auto;gap:12px}.traffic-service-meter{grid-row:2;grid-column:1/-1}.traffic-service-total{grid-column:2;grid-row:1}.traffic-ranking-list{gap:10px}.traffic-rank{display:none}.traffic-summary-grid{grid-template-columns:minmax(125px,1fr) minmax(95px,.7fr) minmax(230px,1.5fr)}.traffic-summary-count,.traffic-directions{padding-left:20px}.traffic-directions{column-gap:16px}.traffic-legend{padding:0 16px}}
@media(max-width:800px){.traffic-middle{grid-template-columns:1fr}.traffic-distribution{display:grid;grid-template-columns:180px minmax(0,1fr);padding-bottom:14px}.traffic-distribution>.panel-header{grid-column:1/-1}.traffic-donut{width:160px;height:160px;margin:12px 10px 0}.traffic-legend{align-content:center;padding:12px 20px 0 0}.traffic-ranking-list{max-height:420px}.traffic-empty{min-height:140px}.traffic-summary-band{padding:16px 20px 20px}.traffic-summary-grid{grid-template-columns:1fr 1fr;gap:20px 0}.traffic-directions{grid-column:1/-1;border-left:0;border-top:1px solid var(--line);padding:16px 0 0}.traffic-summary-count{padding-right:0}.traffic-summary-total .traffic-metric-value{font-size:27px}}
@media(max-width:480px){.traffic-page{gap:14px}.traffic-summary-band{padding:14px 16px 18px}.traffic-summary-top{gap:8px;margin-bottom:17px}.traffic-summary-top>.traffic-section-label{display:none}.traffic-actions{width:100%;justify-content:space-between;gap:8px}.traffic-period button{padding:4px 13px}.traffic-summary-total,.traffic-summary-count{padding-right:8px}.traffic-summary-count{padding-left:17px}.traffic-direction-value{font-size:18px}.traffic-metric-value{font-size:25px}.traffic-directions{column-gap:12px}.traffic-ratio{font-size:10px}.traffic-ranking-list{padding:12px}.traffic-ranking-item{padding:12px;gap:11px}.traffic-service-total strong{font-size:12px}.traffic-service-total>span{font-size:10px}.traffic-distribution{grid-template-columns:1fr}.traffic-donut{margin:8px auto 0;width:172px;height:172px}.traffic-legend{padding:7px 20px 3px}.traffic-trend-body{padding:4px 16px 0}.traffic-trend-bars{gap:3px;height:100px}.traffic-trend-day{padding:0}.traffic-day-detail{gap:7px 12px}.traffic-day-detail>span:first-child{flex-basis:100%}.traffic-history>summary{padding:12px 16px}.traffic-table th,.traffic-table td{padding:11px 16px}}
@media(min-width:1100px){.traffic-summary-band{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:24px;align-items:center;padding:22px 24px}.traffic-summary-top{display:contents}.traffic-summary-top>.traffic-section-label{display:none}.traffic-actions{grid-column:2;grid-row:1;align-self:start;margin-top:2px}.traffic-summary-grid{grid-column:1;grid-row:1;grid-template-columns:minmax(120px,1fr) minmax(85px,.65fr) minmax(210px,1.6fr)}.traffic-summary-count,.traffic-directions{padding-left:20px}.traffic-summary-total,.traffic-summary-count{padding-right:18px}.traffic-directions{column-gap:16px}}
@media(max-width:360px){:global(body:has(.traffic-page)){min-width:0}.traffic-summary-total .traffic-metric-value{font-size:22px}.traffic-summary-count{padding-right:0}.traffic-summary-count .traffic-metric-value{font-size:20px;letter-spacing:-.75px;white-space:nowrap}}
@media(prefers-reduced-motion:reduce){.traffic-day-bar{transition:none}}
</style>
