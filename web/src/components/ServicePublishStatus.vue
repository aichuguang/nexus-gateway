<script setup>
import { computed, ref, watch } from 'vue'
import Icon from '../Icon.vue'
import { usePageScope } from '../page-scope.ts'
import { isRequestCancelled } from '../request.ts'
import { formatDateTime } from '../utils/datetime'

const props = defineProps({ serviceId: { type: String, required: true } })
const scope = usePageScope(), api = scope.api
const copy = {
  title: '当前配置与解析', refresh: '刷新当前状态', refreshing: '正在刷新',
  help: '每次打开都会重新读取本机配置及托管商记录，不沿用历史发布结果；不会写入 DNS。',
  local: '本机配置', loading: '读取中…', unknown: '状态未知', applied: '仅本机配置已应用',
  unpublished: '尚未发布', changed: '有未发布修改', deleted: '服务已删除，无法核对当前状态。',
  stateError: '当前配置读取失败：', recordsError: '解析记录读取失败：', readError: '读取失败，请重试',
  localHelp: '本机应用不代表 Cloudflare 或 DNS 发布完成，也不代表本机入口、上游或公网可达性已经验证。',
  checkedAt: '读取时间', observedAt: '核对时间', service: '服务域名', base: '基站域名',
  matched: '云端记录一致', drift: '云端记录不一致', noDNS: '该服务未配置域名，无需核对域名解析。',
  expected: '当前托管值', cloud: '云端记录', noRecords: '没有同名记录',
  unpublishedDNS: '该域名尚未发布，不能确认解析已完成。',
  uncertainDNS: '该域名的当前托管状态待核对，请到解析页查看。',
  observationError: '云端读取失败：', missingDNS: '当前解析列表中没有该域名，无法确认解析状态。',
  invalidObservation: '未取得完整的云端核对结果，请重新刷新。',
  noReachability: '云端记录核对仅检查托管商 API，不代表权威解析缓存已更新或公网可达。',
  configuredTarget: '配置的 CF 目标', configuredPreferred: '配置的优选', configuredStandard: '配置的标准入口',
  observedCNAME: '云端实际记录', pendingSwitch: '入口待切换', targetMatched: '云端解析已更新',
  preferredPending: '优选尚未生效，线上仍使用标准入口。',
  preferredOther: '优选尚未生效，云端记录尚未指向配置的优选目标。',
  preferredMatched: '云端 CNAME 已指向所选优选入口。',
  standardPending: '标准入口尚未生效，云端记录仍未切回配置的标准入口。',
  standardMatched: '云端 CNAME 已指向所选标准入口。',
  targetUnknown: '未取得完整的 CF 目标配置，无法确认入口是否切换。',
  ambiguousCNAME: '云端记录存在冲突、非默认线路或状态不完整，无法确认当前入口。',
  preferredHelp: '优选开关仅表示配置意图；云端 CNAME 一致不代表 HTTPS、可达性或速度收益已验证。访问检查独立进行，不作为解析切换门槛。',
}
const state = ref(null), stateError = ref(''), stateLoading = ref(true), stateCheckedAt = ref('')
const recordsError = ref(''), rows = ref([]), loading = ref(false)
const service = computed(() => state.value?.services?.find(item => item.id === props.serviceId) || null)
const deleted = computed(() => !stateLoading.value && state.value && !service.value)
const local = computed(() => {
  if (stateLoading.value) return { status: 'loading', label: copy.loading }
  if (!state.value || deleted.value) return { status: 'unknown', label: copy.unknown }
  if (!Number.isFinite(state.value.appliedRevision) || state.value.appliedRevision < 0) return { status: 'unknown', label: copy.unknown }
  if (state.value.appliedRevision === 0) return { status: 'draft', label: copy.unpublished }
  if (!Array.isArray(state.value.unappliedServiceIds)) return { status: 'unknown', label: copy.unknown }
  if (state.value.unappliedServiceIds.includes(props.serviceId)) return { status: 'draft', label: copy.changed }
  return { status: 'applied', label: copy.applied }
})
const normalHost = value => String(value || '').trim().toLowerCase().replace(/\.$/, '')
const tone = status => ['matched', 'applied'].includes(status) ? 'private' : status === 'drift' ? 'amber' : 'neutral'
const statusLabel = status => ({ loading: copy.loading, draft: copy.unpublished, matched: copy.matched, drift: copy.drift })[status] || copy.unknown
const recordType = record => record?.Type || record?.type
const recordData = record => record?.Value || record?.value
const recordValue = record => [recordType(record), recordData(record)].filter(Boolean).join(' ') || '—'
const cfTarget = computed(() => {
  const current = service.value
  if (current?.entryMode !== 'edge' || current.edgeProvider !== 'cloudflare') return null
  const account = state.value?.cloudflareAccounts?.find(item => item.id === current.edgeAccountId)
  const standard = normalHost(account?.standardTarget)
  return { preferred: Boolean(current.usePreferred), standard, target: normalHost(current.usePreferred ? current.preferredDomain : standard) }
})
const rowLabel = row => row.cfTarget && row.status === 'drift' ? copy.pendingSwitch
  : row.cfTarget && row.status === 'matched' ? copy.targetMatched : statusLabel(row.status)

function compareCFTarget(row) {
  // The endpoint's matched flag compares against the last managed ledger, not
  // the service's latest configuration. Never use it as proof of a switch.
  const desired = row.cfTarget, records = row.observation.records || []
  if (!desired.target) { row.status = 'unknown'; row.message = copy.targetUnknown; return }
  const actual = records[0]
  const complete = records.length === 1 && recordType(actual) === 'CNAME' && typeof recordData(actual) === 'string'
    && normalHost(recordData(actual)) && actual.Status === 'ENABLE' && actual.Line === '默认' && actual.LineId === '0'
    && !actual.proxied && !actual.Weight
  if (records.length && !complete) { row.status = 'unknown'; row.message = copy.ambiguousCNAME; return }
  const target = normalHost(recordData(actual))
  row.status = target === desired.target ? 'matched' : 'drift'
  row.message = row.status === 'matched' ? (desired.preferred ? copy.preferredMatched : copy.standardMatched)
    : desired.preferred ? (target && target === desired.standard ? copy.preferredPending : copy.preferredOther) : copy.standardPending
}

async function observe(row, generation) {
  try {
    const result = await api.dnsObservation(row.host)
    if (!scope.isCurrent(generation)) return
    const valid = normalHost(result.host) === row.host && result.source === 'provider-api' && formatDateTime(result.checkedAt) !== '—'
      && recordType(result.expected) && recordData(result.expected) && (Array.isArray(result.records) || (result.status === 'drift' && result.records === null))
      && (result.status !== 'matched' || (result.records.length > 0 && result.records.every(record => recordType(record) && recordData(record))))
    const publication = row.record?.managed?.pending ? 'unknown' : row.record?.publicationState
    row.observation = result
    row.status = valid && !recordsError.value && publication === 'published' && ['matched', 'drift'].includes(result.status) ? result.status : publication === 'draft' && !recordsError.value ? 'draft' : 'unknown'
    row.message = recordsError.value ? `${copy.recordsError}${recordsError.value}`
      : !row.record ? copy.missingDNS
      : publication === 'draft' ? copy.unpublishedDNS
      : publication !== 'published' ? copy.uncertainDNS
      : !valid ? (result.status === 'unknown' && result.message) || copy.invalidObservation
      : result.message || ''
    if (row.cfTarget && ['matched', 'drift'].includes(row.status)) compareCFTarget(row)
  } catch (error) {
    if (!scope.isCurrent(generation) || isRequestCancelled(error)) return
    row.observation = null
    row.status = 'unknown'
    row.message = `${copy.observationError}${error.message || copy.readError}`
  }
}

async function refresh() {
  scope.reset()
  const generation = scope.generation()
  loading.value = true
  state.value = null; stateError.value = ''; stateLoading.value = true; stateCheckedAt.value = ''
  recordsError.value = ''; rows.value = []
  let records = []
  // Reads are independent: a slow provider must not conceal the current local configuration.
  await Promise.all([
    (async () => {
      try {
        const result = await api.state()
        if (!scope.isCurrent(generation)) return
        state.value = result
        stateCheckedAt.value = new Date().toISOString()
      } catch (error) {
        if (scope.isCurrent(generation) && !isRequestCancelled(error)) stateError.value = error.message || copy.readError
      } finally { if (scope.isCurrent(generation)) stateLoading.value = false }
    })(),
    (async () => {
      try {
        const result = await api.dnsRecords()
        if (!scope.isCurrent(generation)) return
        records = Array.isArray(result.records) ? result.records : []
        if (result.warnings?.length) recordsError.value = result.warnings.join('；')
      } catch (error) {
        if (scope.isCurrent(generation) && !isRequestCancelled(error)) recordsError.value = error.message || copy.readError
      }
    })(),
  ])
  if (!scope.isCurrent(generation)) return
  if (!service.value) { loading.value = false; return }
  const domains = [...new Set((service.value.domains || []).map(normalHost).filter(Boolean))]
  const base = domains.length ? normalHost(state.value.settings?.baseDomain) : ''
  const hosts = [...new Set([base, ...domains].filter(Boolean))]
  rows.value = hosts.map(host => ({ host, base: host === base, cfTarget: host === base ? null : cfTarget.value, status: 'loading', observation: null, message: '', record: records.find(record => normalHost(record.host) === host) || null }))
  await Promise.all(rows.value.map(row => observe(row, generation)))
  if (scope.isCurrent(generation)) loading.value = false
}

watch(() => props.serviceId, refresh, { immediate: true })
defineExpose({ refresh })
</script>

<template>
  <section class="service-publish-status" :aria-label="copy.title" :aria-busy="loading">
    <div class="status-heading"><h3>{{ copy.title }}</h3><button class="button secondary small" :disabled="loading" @click="refresh"><Icon name="refresh" :size="15" />{{ loading ? copy.refreshing : copy.refresh }}</button></div>
    <p class="page-help">{{ copy.help }}</p>
    <div v-if="deleted" class="alert warning" role="status">{{ copy.deleted }}</div>
    <div v-if="stateError" class="alert warning" role="alert">{{ copy.stateError }}{{ stateError }}</div>
    <div class="current-local" :data-status="local.status">
      <div class="status-heading"><strong>{{ copy.local }}</strong><span :class="['pill', tone(local.status)]">{{ local.label }}</span></div>
      <p class="field-caption">{{ copy.localHelp }}</p>
      <p v-if="stateCheckedAt" class="field-caption">{{ copy.checkedAt }}：{{ formatDateTime(stateCheckedAt) }}</p>
    </div>
    <div v-if="recordsError" class="alert warning" role="alert">{{ copy.recordsError }}{{ recordsError }}</div>
    <ul v-if="rows.length" class="current-dns-list">
      <li v-for="row in rows" :key="row.host" :data-host="row.host" :data-status="row.status">
        <div class="status-heading"><strong>{{ row.base ? copy.base : copy.service }}</strong><code>{{ row.host }}</code><span :class="['pill', tone(row.status)]">{{ rowLabel(row) }}</span></div>
        <p v-if="row.message" class="field-caption" role="status">{{ row.message }}</p>
        <dl v-if="row.cfTarget || (row.observation && row.status !== 'draft')" class="current-dns-values">
          <div v-if="row.cfTarget"><dt>{{ copy.configuredTarget }}</dt><dd><span>{{ row.cfTarget.preferred ? copy.configuredPreferred : copy.configuredStandard }}</span><code>{{ row.cfTarget.target ? `CNAME ${row.cfTarget.target}` : '—' }}</code></dd></div>
          <div v-else><dt>{{ copy.expected }}</dt><dd><code>{{ recordValue(row.observation.expected) }}</code></dd></div>
          <div><dt>{{ row.cfTarget ? copy.observedCNAME : copy.cloud }}</dt><dd><template v-if="row.observation?.records?.length"><code v-for="(record, index) in row.observation.records" :key="index">{{ recordValue(record) }}</code></template><span v-else>{{ ['unknown', 'loading', 'draft'].includes(row.status) ? '—' : copy.noRecords }}</span></dd></div>
        </dl>
        <p v-if="row.cfTarget" class="field-caption">{{ copy.preferredHelp }}</p>
        <p v-if="row.observation?.checkedAt" class="field-caption">{{ copy.observedAt }}：{{ formatDateTime(row.observation.checkedAt) }}</p>
      </li>
    </ul>
    <p v-else-if="service && !loading" class="field-caption">{{ copy.noDNS }}</p>
    <p v-if="rows.length" class="field-caption">{{ copy.noReachability }}</p>
  </section>
</template>

<style scoped>
.status-heading { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.status-heading h3 { margin: 0; font-size: 15px; }
.status-heading > button, .status-heading > .pill { margin-left: auto; }
.current-local, .current-dns-list > li { padding: 14px; border: 1px solid var(--line); border-radius: 12px; }
.current-dns-list { list-style: none; display: grid; gap: 10px; margin: 12px 0; padding: 0; }
.current-dns-values { margin: 12px 0 0; display: grid; gap: 8px; font-size: 13px; }
.current-dns-values > div { display: grid; grid-template-columns: 96px minmax(0, 1fr); gap: 8px; }
.current-dns-values dt { color: var(--muted); }
.current-dns-values dd { margin: 0; display: grid; gap: 4px; }
code, .field-caption { overflow-wrap: anywhere; }
.service-publish-status > .alert { margin: 12px 0; }
@media (max-width: 540px) { .current-local, .current-dns-list > li { padding: 12px; } }
</style>
