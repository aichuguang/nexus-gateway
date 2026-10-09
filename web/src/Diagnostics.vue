<script setup>
import { formatDateTime } from './utils/datetime'
import { computed, ref } from 'vue'
import Icon from './Icon.vue'
import MonitorSettings from './components/MonitorSettings.vue'
import { usePageScope } from './page-scope.ts'
import { isRequestCancelled } from './request.ts'
import UiCopyValue from './components/ui/UiCopyValue.vue'
import UiOverlay from './components/ui/UiOverlay.vue'

const props = defineProps({ state: Object, busy: String })
const emitEvent = defineEmits(['goto', 'export', 'busy', 'notice'])
const scope = usePageScope()
const api = scope.api
const emit = scope.guard(emitEvent)

const report = ref(null)
const loading = ref(false)
const loadError = ref('')
const preflight = ref(null)
const preflightBusy = ref(false)
const preflightError = ref('')
const checkedAt = ref('')
const activeSection = ref('monitor')
const sections = [{ id: 'monitor', label: '连通检测', icon: 'pulse' }, { id: 'entry', label: '配置核验', icon: 'shield' }, { id: 'device', label: '设备信息', icon: 'server' }]
const serviceFilter = ref('all')
const selectedServiceId = ref('')
const selectedService = computed(() => preflight.value?.services?.find(service => service.serviceId === selectedServiceId.value) || null)
const dataPanel = ref('')
const addressReport = ref(null), addressLoading = ref(false), addressError = ref('')

const notes = computed(() => report.value?.notes || [])
const upstreams = computed(() => report.value?.upstreams || [])
const system = computed(() => report.value?.system || {})
const easytier = computed(() => report.value?.easytier || {})
const proxy = computed(() => report.value?.nginx || {})

function localCheck(service) {
  return service.localTLS && service.localTLS.status !== 'not-checked' ? service.localTLS : service.localEntry
}

const summaryChecks = computed(() => {
  const services = preflight.value?.services || []
  const named = services.filter(service => service.domains?.length)
  const configured = services.filter(service => service.configurationApplied).length
  const certificateReady = named.filter(service => service.certificateReady).length
  const localReady = services.filter(service => localCheck(service)?.status === 'ok').length
  const checked = Boolean(preflight.value && services.length)
  return [
    { icon: 'code', title: '配置核对', note: preflight.value?.dryRun ? '模拟记录不代表真实配置已生效' : checked ? `${configured} / ${services.length} 个服务与已应用配置一致` : '等待读取已应用快照', label: preflight.value?.dryRun ? '仅模拟' : checked ? (configured === services.length ? '已核对' : '待应用') : '未检查', tone: preflight.value?.dryRun || !checked ? 'neutral' : configured === services.length ? 'private' : 'blue' },
    { icon: 'shield', title: '证书材料', note: !checked ? '核对证书材料与域名覆盖' : named.length ? `${certificateReady} / ${named.length} 个域名服务证书可用` : '端口入口不需要证书', label: named.length ? (certificateReady === named.length ? '已核对' : '待处理') : (checked ? '不适用' : '未检查'), tone: named.length ? (certificateReady === named.length ? 'private' : 'amber') : 'neutral', skipped: checked && !named.length },
    { icon: 'pulse', title: '本机入口', note: preflight.value?.dryRun ? '模拟模式未执行真实监听与握手检查' : checked ? `${localReady} / ${services.length} 个入口本机检查通过` : '核对本机握手与代理链路', label: preflight.value?.dryRun ? '仅模拟' : checked ? (localReady === services.length ? '已核对' : '待核对') : '未检查', tone: preflight.value?.dryRun || !checked ? 'neutral' : localReady === services.length ? 'private' : services.some(service => localCheck(service)?.status === 'error') ? 'amber' : 'neutral' },
  ]
})

function line(value) {
  if (!value) return '—'
  return String(value).split('\n')[0]
}

function memorySummary(value) {
  const row = String(value || '').split('\n').find(row => /^(Mem:|内存[：:]?)\s/.test(row.trim()))
  const fields = row?.trim().split(/\s+/)
  return fields?.length >= 3 ? `已用 ${fields[2]} / ${fields[1]}` : line(value)
}

function diskSummary(value) {
  const rows = String(value || '').trim().split('\n').filter(Boolean)
  const row = rows.slice(1).find(row => /\d+%/.test(row))
  const fields = row?.trim().split(/\s+/)
  return fields?.length >= 6 ? `已用 ${fields[2]} / ${fields[1]}（${fields[4]}）` : line(value)
}

async function load() {
  if (loading.value) return
  loading.value = true
  loadError.value = ''
  try { report.value = await api.diagnostics() }
  catch (err) { if (isRequestCancelled(err)) return; loadError.value = err.message }
  finally { loading.value = false }
}

async function runPreflight() {
  activeSection.value = 'entry'
  if (preflightBusy.value) return
  preflightBusy.value = true
  preflightError.value = ''
  try {
    preflight.value = await api.publicPreflight()
    // Preflight checks configuration and DNS only. The entry button must also
    // run the existing read-only per-service check rather than leave all rows
    // permanently at "not-checked" after reporting that checking finished.
    if (!preflight.value.dryRun) {
      await Promise.all((preflight.value.services || []).filter(service => service.configurationApplied).map(async service => {
        try { Object.assign(service, await api.checkService(service.serviceId)) }
        catch (err) { if (isRequestCancelled(err)) return;
          const key = service.domains?.length ? 'localTLS' : 'localEntry'
          service[key] = { status: 'error', error: err.message }
        }
      }))
    }
    checkedAt.value = formatDateTime(new Date())
  } catch (err) { if (isRequestCancelled(err)) return; preflightError.value = err.message; preflight.value = null }
  finally { preflightBusy.value = false }
}

function serviceSummary(service) {
  if (preflightBusy.value) return { label: '正在检查', tone: 'neutral', icon: 'clock' }
  const simulated = preflight.value?.dryRun
  const issues = [Boolean(service.domains?.length && !service.certificateReady), !simulated && localCheck(service)?.status === 'error']
  const count = issues.filter(Boolean).length
  if (count) return { label: `${count} 项需要处理`, tone: 'amber', icon: 'warning' }
  if (simulated) return { label: '仅模拟，未执行真实检查', tone: 'neutral', icon: 'info' }
  if (!service.configurationApplied) return { label: '配置待应用', tone: 'blue', icon: 'code' }
  if (localCheck(service)?.status === 'ok') return { label: '本机检查通过', tone: 'private', icon: 'check' }
  return { label: '本机入口未检查', tone: 'neutral', icon: 'clock' }
}

function checkLabel(check) {
  if (!check) return '未检查'
  if (check.status === 'ok') return '通过'
  if (check.status === 'error') return '失败'
  return '未检查'
}

async function selectSection(section) {
  if (props.busy) return
  if (activeSection.value !== section) { selectedServiceId.value = ''; dataPanel.value = '' }
  activeSection.value = section
  if (section === 'entry' && !preflight.value && !preflightError.value) await runPreflight()
  if (section === 'device' && !report.value && !loadError.value) await load()
}
function needsAttention(service) {
  return !service.configurationApplied || Boolean(service.domains?.length && (!service.certificateReady || !service.dns?.matches)) || (!preflight.value?.dryRun && localCheck(service)?.status !== 'ok')
}
const attentionCount = computed(() => (preflight.value?.services || []).filter(needsAttention).length)
const filteredServices = computed(() => (preflight.value?.services || []).filter(service => serviceFilter.value !== 'attention' || needsAttention(service)))
function stageLabel(check) { return check.external ? check.label : preflightBusy.value ? '检查中' : preflightError.value ? '读取失败' : check.label }
function stageTone(check) { return check.external || preflightBusy.value ? 'neutral' : preflightError.value ? 'amber' : check.tone }
async function openAddresses() {
  dataPanel.value = 'addresses'
  if (addressLoading.value) return
  addressLoading.value = true; addressError.value = ''
  try {
    // Address inspection must not run the separate per-service entry probes.
    addressReport.value = await api.publicPreflight()
  } catch (err) { if (!isRequestCancelled(err)) addressError.value = err.message }
  finally { addressLoading.value = false }
}
defineExpose({ runPreflight, preflightBusy, activeSection })
</script>

<template>
  <nav class="diagnostic-tabs" aria-label="诊断视图">
    <button v-for="section in sections" :key="section.id" type="button" :class="{ active: activeSection === section.id }" :aria-current="activeSection === section.id ? 'page' : undefined" :disabled="Boolean(busy)" @click="selectSection(section.id)"><Icon :name="section.icon" :size="16" />{{ section.label }}</button>
  </nav>

  <MonitorSettings v-if="activeSection === 'monitor'" :busy="busy" @busy="emit('busy', $event)" @notice="emit('notice', $event)" />


  <section v-else-if="activeSection === 'entry'" class="card">
    <div class="card-head"><div><h2>配置核验</h2><p class="field-caption">核对配置、证书与本机入口；整体访问状态请查看“连通检测”。</p></div><span class="small muted">{{ preflightBusy ? '正在核对…' : checkedAt ? `最近检查 ${checkedAt}` : '尚未检查' }}</span></div>
    <div class="entry-stages" aria-label="入口核验阶段" :aria-busy="preflightBusy"><div v-for="check in summaryChecks" :key="check.title" :class="['entry-stage', { 'stage-external': check.external }]"><span><Icon :name="check.icon" :size="15" />{{ check.title }}</span><span :class="['stage-status', stageTone(check)]">{{ stageLabel(check) }}</span></div></div>
    <div v-if="preflightError" class="diagnostic-alert alert error" role="alert"><Icon name="warning" /><span>{{ preflightError }}</span><button class="text-button" :disabled="preflightBusy" @click="runPreflight">重新核验</button></div>
    <div v-if="preflightBusy && !preflight" class="compact-empty"><span class="spinner"></span><h3>正在核对</h3></div>
    <template v-else-if="preflight">
      <div class="diagnostic-filter"><div class="actions"><button class="filter-button" :class="{ active: serviceFilter === 'all' }" @click="serviceFilter = 'all'">全部 {{ preflight.services?.length || 0 }}</button><button class="filter-button" :class="{ active: serviceFilter === 'attention' }" @click="serviceFilter = 'attention'">需处理 {{ preflightBusy ? '—' : attentionCount }}</button></div><span v-if="preflight.dryRun" class="simulation-note">dry-run：没有真实监听</span><span v-else class="small muted">已应用地址 {{ preflight.appliedAddress || '未探测到' }}</span></div>
      <div v-if="!preflight.services?.length" class="compact-empty"><Icon name="server" :size="28" /><h3>还没有服务可以核对</h3><button class="text-button" @click="emit('goto', 'services')">去添加服务</button></div>
      <div v-else class="table-wrap"><table class="data-table diagnostic-service-table"><thead><tr><th>服务 / 入口</th><th>核验结论</th><th>解析</th><th class="align-right">操作</th></tr></thead><tbody>
        <tr v-for="service in filteredServices" :key="service.serviceId"><td><strong>{{ service.name }}</strong><span class="secondary mono">{{ service.entry || '入口未生成' }}</span></td><td><span :class="['check-summary', serviceSummary(service).tone]"><Icon :name="serviceSummary(service).icon" :size="15" />{{ serviceSummary(service).label }}</span></td><td><span class="small" :class="{ 'error-text': !preflightBusy && service.domains?.length && !service.dns?.matches }">{{ preflightBusy ? '核对中…' : !service.domains?.length ? '无需解析' : service.dns?.matches ? '已匹配' : '待核对' }}</span></td><td class="align-right"><button class="button secondary small" :aria-label="`查看核验详情：${service.name}`" @click="selectedServiceId = service.serviceId">查看详情</button></td></tr>
        <tr v-if="!filteredServices.length"><td colspan="4" class="table-empty">当前筛选下没有需要显示的服务。</td></tr>
      </tbody></table></div>
      <ul v-if="!preflightBusy && preflight.warnings?.length" class="todo-list diagnostic-warnings"><li v-for="(warning, index) in preflight.warnings" :key="index"><Icon name="info" :size="16" /><span>{{ warning }}</span></li></ul>
    </template>
    <div v-else-if="!preflightBusy && !preflightError" class="compact-empty"><p>尚未执行入口核验。</p><button class="button secondary small" @click="runPreflight">检查本机入口</button></div>
  </section>

  <section v-else class="card">
    <div class="card-head"><div><h2>设备信息</h2><p class="field-caption">按需读取本机系统、代理与隧道信息。</p></div><div class="actions"><button class="button secondary small" @click="emit('export')"><Icon name="download" :size="15" />导出配置</button><button class="button secondary small" :disabled="loading" @click="load">{{ loading ? '读取中…' : '重新读取' }}</button></div></div>
    <div class="card-body">
      <div v-if="loadError" class="alert error" role="alert"><Icon name="warning" /><span>{{ loadError }}。旧结果不作为本轮依据。</span></div>
      <div v-else-if="loading" class="compact-empty" role="status"><span class="spinner"></span><p>正在读取本机信息…</p></div>
      <template v-else-if="report">
        <div class="device-summary"><section><h3>系统概要</h3><dl class="definition"><div><dt>主机名</dt><dd class="mono">{{ system.hostname || '—' }}</dd></div><div><dt>架构</dt><dd class="mono">{{ system.arch || '—' }}</dd></div><div><dt>运行时长</dt><dd>{{ line(system.uptime) }}</dd></div><div><dt>内存</dt><dd>{{ memorySummary(system.memory) }}</dd></div><div><dt>磁盘</dt><dd>{{ diskSummary(system.disk) }}</dd></div></dl></section><section><h3>代理管理</h3><dl class="definition"><div><dt>可执行文件</dt><dd><span :class="['pill', proxy.available ? 'private' : 'amber']">{{ proxy.available ? '已找到' : '未找到' }}</span></dd></div><div><dt>托管状态</dt><dd>{{ proxy.enabled ? '已启用托管' : '未托管' }}</dd></div><div><dt>最近应用</dt><dd>{{ formatDateTime(proxy.appliedAt, '尚未应用') }}</dd></div><div><dt>已应用地址</dt><dd><UiCopyValue :value="proxy.appliedAddress" /></dd></div></dl></section></div>
        <div class="panel-note" :class="easytier.warning ? 'warning' : ''"><Icon :name="easytier.warning ? 'warning' : 'info'" :size="17" /><span>{{ easytier.warning || (easytier.peers && !easytier.peers.startsWith('不可用') ? '已读取邻居信息，当前输出未观察到中继路径。' : '尚无可用的邻居输出，无法判断 EasyTier 是否经过中继。') }}</span></div>
        <div class="device-detail-actions"><button class="button secondary small" @click="openAddresses"><Icon name="globe" :size="15" />本机地址候选</button><button class="button secondary small" @click="dataPanel = 'raw'"><Icon name="code" :size="15" />查看采集明细</button></div>
      </template>
    </div>
  </section>

  <UiOverlay :open="Boolean(selectedService) && activeSection === 'entry'" :title="`核验详情 · ${selectedService?.name || ''}`" description="本页核对配置与本机入口；整体访问状态请查看“连通检测”。" variant="drawer" @close="selectedServiceId = ''">
    <template v-if="selectedService">
      <div v-if="preflightBusy" class="compact-empty" role="status"><span class="spinner"></span><p>正在重新检查，旧结果暂不显示。</p></div>
      <template v-else-if="!preflightError">
        <UiCopyValue v-if="selectedService.entry" :value="selectedService.entry" label="服务入口" address />
        <dl class="check-details"><div><dt>配置</dt><dd :class="preflight.dryRun ? 'muted' : selectedService.configurationApplied ? 'status-good' : 'status-pending'">{{ preflight.dryRun ? '仅模拟' : selectedService.configurationApplied ? '配置已应用' : '配置待应用' }}</dd></div><div><dt>证书</dt><dd :class="selectedService.domains?.length && !selectedService.certificateReady ? 'status-attention' : 'muted'">{{ selectedService.domains?.length ? (selectedService.certificateReady ? '证书可用' : '证书不可用') : '不需要证书' }}</dd></div><div><dt>本机入口</dt><dd :class="!preflight.dryRun && localCheck(selectedService)?.status === 'error' ? 'status-attention' : 'muted'">{{ preflight.dryRun ? '仅模拟' : checkLabel(localCheck(selectedService)) }}</dd></div></dl>
        <div class="probe-result"><Icon :name="selectedService.dns?.matches ? 'check' : 'info'" :size="16" /><span v-if="!selectedService.domains?.length">按端口发布，不需要解析记录。</span><span v-else-if="selectedService.entryMode === 'edge' && selectedService.dns?.matches">解析已匹配边缘 CNAME<span v-if="selectedService.dns.cname" class="mono">：{{ selectedService.dns.cname }}</span></span><span v-else-if="selectedService.dns?.matches">{{ selectedService.address ? `解析已匹配本机地址 ${selectedService.address}。` : '解析已匹配当前配置。' }}</span><span v-else-if="selectedService.dns?.error">{{ selectedService.dns.error }}<span v-if="selectedService.dns.records?.length" class="mono">（当前解析：{{ selectedService.dns.records.join('、') }}）</span></span><span v-else-if="selectedService.entryMode === 'edge'" class="error-text">尚未确认边缘 CNAME 解析，请在边缘加速页面核对接入状态。</span><span v-else class="error-text">解析尚未指向本机。</span></div>
        <div v-if="selectedService.certificateError" class="alert warning"><Icon name="warning" :size="16" /><span>{{ selectedService.certificateError }}</span></div><div v-if="!preflight.dryRun && localCheck(selectedService)?.error" class="alert warning"><Icon name="warning" :size="16" /><span>{{ localCheck(selectedService).error }}</span></div>
      </template>
    </template>
    <template #footer="{ close }"><button class="button secondary" @click="close">返回列表</button></template>
  </UiOverlay>

  <UiOverlay :open="Boolean(dataPanel) && activeSection === 'device'" :title="dataPanel === 'addresses' ? '本机地址候选' : '设备采集明细'" description="仅展示采集到的结果；缺少输出不等于状态正常。" variant="drawer" size="wide" @close="dataPanel = ''">
    <template v-if="dataPanel === 'addresses'"><div v-if="addressLoading" class="compact-empty" role="status"><span class="spinner"></span><p>正在读取地址候选…</p></div><div v-else-if="addressError" class="alert error" role="alert"><span>{{ addressError }}</span><button class="text-button" @click="openAddresses">重试</button></div><template v-else><div class="table-wrap"><table class="data-table"><thead><tr><th>地址</th><th>网卡</th><th>判定</th></tr></thead><tbody><tr v-for="candidate in addressReport?.candidates || []" :key="candidate.address"><td><UiCopyValue :value="candidate.address" /></td><td>{{ candidate.interface }}</td><td><span :class="['pill', candidate.eligible ? 'private' : 'neutral']">{{ candidate.eligible ? '可用' : candidate.reason }}</span></td></tr><tr v-if="!addressReport?.candidates?.length"><td colspan="3" class="table-empty">没有读取到任何地址</td></tr></tbody></table></div><p class="page-help mono">默认路由：{{ addressReport?.routes || '未读取到' }}</p></template></template>
    <template v-else-if="dataPanel === 'raw'"><p v-if="loading || loadError" class="field-caption">本轮采集尚未成功，旧原始输出不再展示。</p><template v-else><h3 class="section-title">上游 HTTP 探测</h3><div class="table-wrap"><table class="data-table"><thead><tr><th>目标</th><th>地址</th><th>结果</th></tr></thead><tbody><tr v-for="probe in upstreams" :key="probe.url"><td>{{ probe.name }}</td><td class="mono">{{ probe.url || '未配置' }}</td><td><span v-if="!probe.url" class="pill neutral">未配置</span><span v-else-if="probe.ok" class="pill private">HTTP {{ probe.status }} · {{ probe.latencyMs }} ms</span><span v-else class="pill amber">{{ probe.error || '没有成功响应' }}</span></td></tr><tr v-if="!upstreams.length"><td colspan="3" class="table-empty">暂无上游探测结果</td></tr></tbody></table></div><div class="diagnostic-raw"><div><span class="terminal-label">ip -br addr</span><pre class="mono">{{ system.interfaces || '不可用' }}</pre></div><div><span class="terminal-label">easytier-cli node</span><pre class="mono">{{ easytier.node || '不可用' }}</pre></div><div><span class="terminal-label">easytier-cli peer</span><pre class="mono">{{ easytier.peers || '不可用' }}</pre></div></div><ul class="todo-list"><li v-for="(note, index) in notes" :key="index"><Icon name="info" :size="16" /><span>{{ note }}</span></li></ul></template></template>
    <template #footer="{ close }"><button class="button secondary" @click="close">返回设备信息</button></template>
  </UiOverlay>
</template>

<style scoped>
.diagnostic-tabs{display:flex;gap:6px;margin-bottom:20px;border-bottom:1px solid var(--line);padding-bottom:12px;overflow-x:auto}.diagnostic-tabs button{display:flex;align-items:center;gap:7px;flex-shrink:0;padding:9px 14px;border:1px solid transparent;border-radius:9px;color:var(--muted);background:none;font:inherit;font-size:13px;cursor:pointer}.diagnostic-tabs button.active{background:var(--blue-bg);border-color:color-mix(in srgb,var(--blue) 18%,transparent);color:var(--blue);font-weight:600}.diagnostic-tabs button:disabled{opacity:.55;cursor:not-allowed}
.entry-stages{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));padding:16px 20px;gap:16px;border-bottom:1px solid var(--line)}.entry-stage{display:flex;align-items:center;justify-content:space-between;gap:8px;font-size:12px}.entry-stage>span:first-child{display:flex;align-items:center;gap:6px;white-space:nowrap}.stage-external{border-left:1px dashed var(--line);padding-left:16px}.stage-status{white-space:nowrap;color:var(--muted)}.stage-status.private{color:var(--green)}.stage-status.amber{color:var(--amber)}.stage-status.blue{color:var(--blue)}
.diagnostic-filter{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:14px 20px;flex-wrap:wrap}.filter-button{background:none;border:0;border-radius:6px;padding:6px 10px;font:inherit;font-size:12px;color:var(--muted);cursor:pointer}.filter-button.active{color:var(--ink);background:var(--surface-2);font-weight:600}.diagnostic-service-table{min-width:600px}.diagnostic-alert{margin:16px 20px}.diagnostic-warnings{padding:12px 20px}.check-summary{display:inline-flex;align-items:center;gap:6px;font-size:12px}.check-summary.private{color:var(--green)}.check-summary.amber{color:var(--amber)}.check-summary.blue{color:var(--blue)}
.device-summary{display:grid;grid-template-columns:1fr 1fr;gap:28px;margin-bottom:20px}.device-summary h3{font-size:14px;margin:0 0 12px}.device-detail-actions{display:flex;flex-wrap:wrap;gap:10px;margin-top:20px}.diagnostic-raw{display:grid;gap:18px;margin-top:24px}.diagnostic-raw pre{max-width:100%;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;padding:14px;border-radius:10px;background:var(--surface-2)}
@media(max-width:850px){.entry-stages{grid-template-columns:1fr 1fr}.stage-external{border-left:0;padding-left:0}}
@media(max-width:600px){.device-summary{grid-template-columns:1fr;gap:18px}.diagnostic-tabs{gap:3px}.diagnostic-tabs button{padding:8px 10px}.entry-stages{padding:14px;gap:14px}.diagnostic-filter{padding:14px}}
</style>
