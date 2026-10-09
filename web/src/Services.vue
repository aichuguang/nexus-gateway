<script setup>
import { formatDateTime } from './utils/datetime'
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { certificateStatus, isCertificateUsable as isUsable } from './certificate-status.js'
import Icon from './Icon.vue'
import RoutePath from './components/RoutePath.vue'
import ServiceAvatar from './components/ServiceAvatar.vue'
import ServiceIconPicker from './components/ServiceIconPicker.vue'
import { serviceIconOptions } from './service-icons.js'
import UiCopyValue from './components/ui/UiCopyValue.vue'
import UiTooltip from './components/ui/UiTooltip.vue'
import UiSelect from './components/ui/UiSelect.vue'
import UiSwitch from './components/ui/UiSwitch.vue'
import UiCheckbox from './components/ui/UiCheckbox.vue'
import UiSegmented from './components/ui/UiSegmented.vue'
import UiActionMenu from './components/ui/UiActionMenu.vue'
import UiOverlay from './components/ui/UiOverlay.vue'
import { usePageScope } from './page-scope.ts'
import { isRequestCancelled } from './request.ts'
import { isPublishActive, publishRequestId, publishStatus } from './usePublishJobs.js'
import { useMonitor } from './useMonitor.ts'
import MonitorCheckDetails from './components/MonitorCheckDetails.vue'
import { serviceForm, servicePayload, setServiceMode } from './service-form.ts'
import { serviceEntry, upstreamPortOf } from './service-entry.js'

const props = defineProps({ state: Object, busy: String, jobs: { type: Array, default: () => [] }, submissionError: String, publishUnavailable: Boolean })
const emitEvent = defineEmits(['state', 'notice', 'busy', 'publish', 'view-job', 'configure', 'preview-opened'])
const scope = usePageScope()
const api = scope.api
const emit = scope.guard(emitEvent)
const monitor = reactive(useMonitor(api))
const upstreamCopy = {
  empty: '填写你的 VPN 内网上游地址，再选择域名或独立端口入口。',
  label: '上游地址（VPN 内网）',
  example: 'http://10.20.0.20:8080',
  help: '填写网关可达的 IPv4 地址和端口，网段按你的 VPN 配置，不要求与网关同网段。',
}

const open = ref(false)
const formError = ref('')
const confirming = ref(null)
const deleteOptions = reactive({ deleteDNS: false, deleteEdgeOne: false, deleteCloudflare: false })
const deleteRevision = ref(undefined)
const deletePending = ref(false)
const deleteError = ref('')
const formSnapshot = ref('')
const formDirty = computed(() => JSON.stringify(form) !== formSnapshot.value)
const probeResults = reactive({})
const checkResults = reactive({})
const inspection = ref(null)
const inspectionLoading = ref(false)
const inspectionPending = reactive({ local: false, upstream: false })
const externalError = ref('')
const moreActions = [
  { id: 'inspect', label: '本机与上游探测', icon: 'pulse' },
  { id: 'external', label: '外部探测', icon: 'globe' },
  { id: 'delete', label: '删除服务', icon: 'trash', danger: true, separator: true },
]

function serviceAction(action, service) {
  if (props.busy || deletePending.value) return
  if (action === 'inspect') inspect(service)
  else if (action === 'external') external(service)
  else if (action === 'delete') {
    deleteError.value = ''
    Object.assign(deleteOptions, { deleteDNS: false, deleteEdgeOne: false, deleteCloudflare: false })
    deleteRevision.value = props.state?.revision
    confirming.value = service
  }
}
function cancelDelete() {
  if (props.busy || deletePending.value) return
  confirming.value = null
}

// The two entry styles are exclusive, and the form asks the question first
// because that is the decision: a name shares 443 with every other name, a port
// takes a number of its own and carries no certificate at all.
const form = reactive(serviceForm())
const iconService = computed(() => ({
  domains: form.mode === 'name' ? form.domains.map(domain => domain.trim().toLowerCase()).filter(Boolean) : [],
  entryMode: form.mode === 'name' && form.entryMode === 'edge' ? 'edge' : 'direct',
  edgeProvider: form.edgeProvider === 'cloudflare' ? 'cloudflare' : 'edgeone',
}))

const services = computed(() => props.state?.services || [])
const query = ref('')
const serviceFilter = ref('all')
const serviceFilters = computed(() => [
  { value: 'all', label: `全部服务 ${services.value.length}` },
  { value: 'domain', label: '域名入口' },
  { value: 'port', label: '端口入口' },
])
const filteredServices = computed(() => {
  const search = query.value.trim().toLowerCase()
  return services.value.filter(service => {
    const named = Boolean(service.domains?.length)
    if (serviceFilter.value === 'domain' && !named) return false
    if (serviceFilter.value === 'port' && named) return false
    return `${service.name} ${service.upstream} ${(service.domains || []).join(' ')} ${entry(service)}`.toLowerCase().includes(search)
  })
})
const entryStyles = [{ value: 'name', label: '用域名发布' }, { value: 'port', label: '用端口发布' }]
const entryModes = [{ value: 'direct', label: '本机直连' }, { value: 'edge', label: '边缘加速' }]
const edgeProviders = [{ value: 'edgeone', label: 'EdgeOne' }, { value: 'cloudflare', label: 'Cloudflare' }]
const edgeName = service => service?.edgeProvider === 'cloudflare' ? 'Cloudflare' : 'EdgeOne'
const cfEntryCopy = { preferred: '配置的优选：', standard: '配置的入口：标准 Cloudflare' }
const serviceStateCopy = { enabled: '配置已启用', disabled: '配置已停用' }
const cfPublishCopy = { verification: '本次将切换业务 CNAME，并从托管商重新读取核对；读回一致才算云端解析已更新。访问检查独立进行，不作为解析切换门槛。' }

// Which rows still have changes that are not running. The browser only ever
// holds the draft, so it cannot work this out for itself — the server compares
// the draft against the snapshot that is actually being served and names the
// services that differ. A tag the browser guessed at would be wrong exactly
// when it mattered.
const unappliedIds = computed(() => props.state?.unappliedServiceIds || [])
const unappliedElsewhere = computed(() => Boolean(props.state?.unappliedElsewhere))
function unapplied(service) {
  return unappliedIds.value.includes(service.id)
}

const publishTarget = ref(null)
const publishItem = ref(null)
const publishRecords = ref([])
const publishRecordsError = ref('')
const publishRevision = ref(0)
const publishFingerprint = ref('')
const publishImpact = ref([])
const publishLoading = ref(false)
const publishError = ref('')
let previewSequence = 0
let requestId = ''
const changeLabels = { create: '新增', update: '修改', unchanged: '本机配置无变化' }
const publishEdge = computed(() => publishTarget.value?.enabled && publishTarget.value?.domains?.length && publishTarget.value?.entryMode === 'edge')
const publishAlsoOthers = computed(() => Boolean(publishTarget.value &&
  (unappliedIds.value.some(id => id !== publishTarget.value.id) || unappliedElsewhere.value)))
const publishNames = computed(() => {
  if (!publishTarget.value) return []
  const wanted = new Set((publishTarget.value.domains || []).map(domain => String(domain).toLowerCase()))
  const base = String(props.state?.settings?.baseDomain || '').toLowerCase()
  if (base) wanted.add(base)
  return publishRecords.value.filter(record => wanted.has(record.host))
})
const publishBlockers = computed(() => {
  const service = publishTarget.value
  if (!service?.enabled) return []
  const blockers = []
  if (service.domains?.length) {
    const cert = certificates.value.find(item => item.id === service.certificateId)
    if (!isUsable(cert) || !service.domains.every(host => covers(cert, host))) {
      blockers.push({ text: '需要一张有效且覆盖全部域名的证书。', tab: 'certificates', label: '去证书页处理' })
    }
  }
  if (publishEdge.value && !hasAccountFor(service)) blockers.push({ text: `还没有可用的 ${edgeName(service)} 接入账号，或服务选择的账号已停用。`, tab: 'edgeone', label: '去添加账号' })
  if (publishEdge.value && service.edgeProvider === 'cloudflare' && !props.state?.settings?.baseDomain) blockers.push({ text: 'Cloudflare SaaS 需要先在概览设置基站域名并核对 IPv6 解析。', tab: 'overview', label: '去设置基站' })
  return blockers
})

function latestJob(service) { return props.jobs.find(job => job.serviceId === service.id) }
function closePublish() { publishTarget.value = null; publishLoading.value = false; previewSequence++ }
function configurePublish(blocker) { emit('configure', { tab: blocker.tab, serviceId: publishTarget.value.id }) }

async function openPublish(service) {
  const active = props.jobs.find(job => job.serviceId === service.id && isPublishActive(job))
  if (active && (active.status === 'running' || !unapplied(service))) { emit('view-job', active); return }
  if (props.busy) return
  const sequence = ++previewSequence
  emit('preview-opened')
  publishTarget.value = service
  publishItem.value = null
  publishRecords.value = []
  publishRecordsError.value = ''
  publishImpact.value = []
  publishFingerprint.value = ''
  requestId = ''
  publishError.value = ''
  publishLoading.value = true
  const [preview, records] = await Promise.allSettled([api.preview(), api.dnsRecords()])
  if (sequence !== previewSequence) return
  if (preview.status === 'fulfilled') {
    publishRevision.value = preview.value.revision
    publishFingerprint.value = preview.value.publishFingerprint || ''
    publishImpact.value = preview.value.impact || []
    publishItem.value = (preview.value.services || []).find(item => item.id === service.id) || null
    if (!publishItem.value) publishError.value = '预览中没有这个服务，请刷新页面。'
    else if (!publishFingerprint.value) publishError.value = '预览缺少发布标识，请刷新管理界面后重试。'
  } else publishError.value = preview.reason.message
  if (records.status === 'fulfilled') publishRecords.value = records.value.records || []
  else publishRecordsError.value = records.reason.message
  publishLoading.value = false
}

function publish() {
  if (!publishItem.value || props.publishUnavailable || props.busy || publishLoading.value || publishBlockers.value.length || !publishFingerprint.value) return
  try { requestId ||= publishRequestId() }
  catch { publishError.value = '浏览器无法生成发布标识，请更换支持加密随机数的浏览器。'; return }
  emit('publish', { serviceId: publishTarget.value.id, revision: publishRevision.value, publishFingerprint: publishFingerprint.value, requestId })
}
const certificates = computed(() => props.state?.certificates || [])
const edgeAccounts = computed(() => props.state?.edgeOneAccounts || [])
const hasEdgeAccount = computed(() => edgeAccounts.value.some(account => account.enabled))
const cloudflareAccounts = computed(() => props.state?.cloudflareAccounts || [])
const cloudflareOptions = computed(() => [{ value: '', label: '选择 Cloudflare 接入账号' }, ...cloudflareAccounts.value.map(account => ({ value: account.id, label: `${account.name}${account.enabled ? '' : '（已停用）'}` }))])
const selectedCFAccount = computed(() => cloudflareAccounts.value.find(account => account.id === form.edgeAccountId))
function hasAccountFor(service) { return service?.edgeProvider === 'cloudflare' ? cloudflareAccounts.value.some(account => account.id === service.edgeAccountId && account.enabled) : hasEdgeAccount.value }
const editing = computed(() => Boolean(form.id))
const upstreamPort = computed(() => upstreamPortOf(form.upstream))
const portPlaceholder = computed(() => upstreamPort.value ? `留空则沿用上游端口 ${upstreamPort.value}` : '上游地址里没有端口时必填')
const canSave = computed(() => Boolean(form.name.trim() && form.upstream.trim() && (form.mode === 'port' || form.domains.some(domain => domain.trim()))))

function covers(cert, host) {
  const name = String(host || '').trim().toLowerCase()
  if (!name) return false
  return (cert.domains || []).some(pattern => {
    const rule = String(pattern).toLowerCase()
    return rule === name || (rule.startsWith('*.') && name.endsWith(rule.slice(1)) && name.split('.').length === rule.split('.').length)
  })
}

function certificateHint(cert) {
  const status = certificateStatus(cert)
  if (!status.usable) return cert.error || status.label
  return `${(cert.domains || []).join('、')} · 有效至 ${formatDateTime(cert.notAfter)}`
}

// Editing never leaves a name bound to material that does not speak for it: the
// select only offers certificates that are usable and cover every domain typed.
const matchingCertificates = computed(() => {
  const hosts = form.domains.map(domain => domain.trim().toLowerCase()).filter(Boolean)
  if (!hosts.length) return certificates.value.filter(isUsable)
  return certificates.value.filter(cert => isUsable(cert) && hosts.every(host => covers(cert, host)))
})
const certificateOptions = computed(() => [{ value: '', label: '选择覆盖全部域名的证书' }, ...matchingCertificates.value.map(cert => ({ value: cert.id, label: cert.name }))])

const formCertificateWarning = computed(() => {
  if (form.mode !== 'name') return ''
  const cert = certificates.value.find(item => item.id === form.certificateId)
  if (!cert) return form.domains.some(domain => domain.trim()) ? '还没有选择证书；按域名发布的服务必须有一张覆盖全部域名的证书。' : ''
  if (!isUsable(cert)) return '所选证书当前不可用，应用会被拒绝。'
  const missing = form.domains.map(domain => domain.trim().toLowerCase()).filter(Boolean).filter(host => !covers(cert, host))
  return missing.length ? `所选证书不覆盖 ${missing.join('、')}。` : ''
})

function edit(service = null) {
  if (props.busy) return
  formError.value = ''
  const next = serviceForm(service)
  if (!serviceIconOptions.some(option => option.value === next.icon)) next.icon = ''
  Object.assign(form, next)
  formSnapshot.value = JSON.stringify(form)
  open.value = true
  nextTick(() => { formSnapshot.value = JSON.stringify(form) })
}

function close() {
  if (props.busy) return
  open.value = false
  formError.value = ''
  confirming.value = null
}

// Switching style clears the fields the other style owns, so a saved service can
// never carry a port it does not publish or a certificate nothing reads.
watch(() => form.mode, mode => setServiceMode(form, mode))

function addDomain() { form.domains.push('') }
function removeDomain(index) { form.domains.splice(index, 1); if (!form.domains.length) form.domains.push('') }

function payload() { return servicePayload(form) }

const saving = ref(false)
async function save(andPublish = false) {
  if (props.busy || saving.value) return
  saving.value = true
  emit('busy', 'service')
  formError.value = ''
  let savedService = null
  try {
    const input = payload(), before = new Set(services.value.map(service => service.id))
    const next = form.id ? await api.updateService(form.id, input) : await api.createService(input)
    const candidates = (next.services || []).filter(service => form.id ? service.id === form.id : !before.has(service.id) && service.name === input.name && service.upstream === input.upstream)
    savedService = candidates.length === 1 ? candidates[0] : null
    emit('state', next)
    emit('notice', next.operationRecoveryMessage || (andPublish ? '草稿已保存，请确认发布范围后开始自动发布。' : '已保存，发布后生效'))
    open.value = false
  } catch (err) { if (isRequestCancelled(err)) return; formError.value = err.message }
  finally { saving.value = false; emit('busy', '') }
  if (andPublish && savedService) { await nextTick(); await openPublish(savedService) }
  else if (andPublish && !open.value) emit('notice', '草稿已保存，但无法唯一确认新服务；请从服务列表打开发布预览。')
}

async function remove(service) {
  if (!service || props.busy || deletePending.value) return
  deletePending.value = true
  deleteError.value = ''
  const selection = { deleteDNS: deleteOptions.deleteDNS, deleteEdgeOne: deleteOptions.deleteEdgeOne,
    ...(service.edgeProvider === 'cloudflare' ? { deleteCloudflare: deleteOptions.deleteCloudflare } : {}), revision: deleteRevision.value }
  emit('busy', 'delete-service')
  try {
    const next = await api.deleteService(service.id, selection)
    emit('state', next)
    emit('notice', `服务「${service.name}」已从草稿中删除；${selection.deleteDNS || selection.deleteEdgeOne || selection.deleteCloudflare ? '已处理勾选的云端资源，未勾选项保留。' : '云端资源保留。'}本机入口需发布后撤下。`)
    confirming.value = null
  } catch (err) {
    if (isRequestCancelled(err) && !scope.isActive()) return
    const detail = isRequestCancelled(err) ? '已停止等待，删除结果待确认。' : err.message
    if (err.operationStage === 'preflight' && err.cloudChanges === false && !err.outcomeUnknown) {
      deleteError.value = detail
      return // Keep the confirmed choices; no cloud deletion has started.
    }
    deleteError.value = `服务「${service.name}」${err.outcomeUnknown || isRequestCancelled(err) ? '删除结果待确认' : '删除失败'}：${detail} 若有已完成的云端删除，不会自动回滚；结果不确定时请先核对线上资源和服务状态，不要重复提交。`
    emit('notice', { type: 'error', message: deleteError.value })
    confirming.value = null
  } finally { deletePending.value = false; emit('busy', '') }
}

async function inspect(service) {
  if (props.busy || inspectionLoading.value) return
  inspection.value = { service, kind: 'local-upstream' }
  inspectionLoading.value = true
  inspectionPending.local = true
  inspectionPending.upstream = true
  delete checkResults[service.id]
  delete probeResults[service.id]
  emit('busy', 'check-' + service.id)
  // Keep the two outcomes independent so a slow or failed probe cannot hide
  // the other result. Every open/refresh starts fresh read-only checks.
  async function checkLocal() {
    try { checkResults[service.id] = await api.checkService(service.id) }
    catch (err) { if (!isRequestCancelled(err)) checkResults[service.id] = { error: err.message } }
    finally { inspectionPending.local = false }
  }
  async function checkUpstream() {
    try { probeResults[service.id] = await api.probeService(service.id) }
    catch (err) { if (!isRequestCancelled(err)) probeResults[service.id] = { ok: false, error: err.message } }
    finally { inspectionPending.upstream = false }
  }
  try { await Promise.all([checkLocal(), checkUpstream()]) }
  finally { inspectionLoading.value = false; emit('busy', '') }
}

async function external(service) {
  if (props.busy || inspectionLoading.value) return
  inspection.value = { service, kind: 'external' }
  externalError.value = ''
  if (monitor.status?.running) { externalError.value = '已有连通检测正在进行，请等待完成后重试。'; return }
  inspectionLoading.value = true
  try { monitor.status = await api.checkServiceExternal(service.id); monitor.changed() }
  catch (err) { if (!isRequestCancelled(err)) externalError.value = err.message }
  finally { inspectionLoading.value = false }
}

function entry(service) {
  return serviceEntry(service, props.state?.appliedPublicAddress || props.state?.settings?.gatewayIp)
}

function localCheck(result) {
  return result.localTLS && result.localTLS.status !== 'not-checked' ? result.localTLS : result.localEntry
}

defineExpose({ edit, openPublish, closePublish })
</script>

<template>
  <div class="toolbar">
    <UiSegmented v-model="serviceFilter" :options="serviceFilters" aria-label="筛选服务入口" />
    <div class="actions">
      <label class="search"><Icon name="search" :size="17" /><span class="sr-only">搜索服务</span><input v-model="query" type="search" placeholder="搜索服务、域名或上游" aria-label="搜索服务" /></label>
    </div>
  </div>
  <div v-if="formError && !open" class="alert error" role="alert"><Icon name="warning" /><span>{{ formError }}</span></div>
  <div v-if="deleteError && !confirming" class="alert error" role="alert"><Icon name="warning" /><span>{{ deleteError }}</span><button class="text-button" @click="deleteError = ''">关闭提示</button></div>
  <section class="card services-table">
    <div v-if="!services.length" class="compact-empty">
      <div class="empty-icon"><Icon name="server" :size="32" /></div><h3>把第一个上游连接到网关</h3>
      <p>{{ upstreamCopy.empty }}</p>
      <button class="button primary" :disabled="Boolean(busy)" @click="edit()"><Icon name="plus" :size="16" />添加第一个服务</button>
    </div>
    <div v-else class="table-wrap">
      <table class="data-table">
        <thead><tr><th>服务 / 上游</th><th>发布入口</th><th>SSL</th><th>状态</th><th class="align-right">操作</th></tr></thead>
        <tbody>
          <template v-for="service in filteredServices" :key="service.id">
            <tr>
              <td><div class="name-cell"><ServiceAvatar :service="service" class="service-icon" /><div><strong class="service-name">{{ service.name }}</strong><div class="secondary"><UiCopyValue :value="service.upstream" :display-value="service.upstream.replace(/^http:\/\//, '')" label="上游地址" /></div></div></div></td>
              <td><UiCopyValue :value="entry(service)" label="服务入口" address /><RoutePath class="service-route" compact :edge="Boolean(service.domains?.length && service.entryMode === 'edge')" :edge-provider="service.edgeProvider" /><small v-if="service.entryMode === 'edge' && service.edgeProvider === 'cloudflare'" class="secondary">{{ service.usePreferred ? `${cfEntryCopy.preferred}${service.preferredDomain}` : cfEntryCopy.standard }}</small><div v-if="service.domains?.length > 1" class="domain-tags"><UiCopyValue v-for="domain in service.domains.slice(1)" :key="domain" :value="domain" label="域名" /></div></td>
              <td class="service-ssl">{{ certificates.find(cert => cert.id === service.certificateId)?.name || '' }}</td>
              <td>
                <div class="pills"><span :class="['pill', service.enabled ? 'private' : 'neutral']"><span class="status-dot"></span>{{ service.enabled ? serviceStateCopy.enabled : serviceStateCopy.disabled }}</span><span v-if="unapplied(service)" class="pill blue">有未发布修改</span></div>
              </td>
              <td><div class="actions service-table-actions">
                <UiTooltip v-if="latestJob(service)" text="查看发布结果"><button class="icon-button" :aria-label="`查看发布结果：${service.name}`" @click="emit('view-job', latestJob(service))"><Icon name="eye" :size="17" /></button></UiTooltip>
                <button v-if="isPublishActive(latestJob(service)) && (latestJob(service)?.status === 'running' || !unapplied(service))" class="button small secondary" @click="emit('view-job', latestJob(service))">{{ latestJob(service)?.status === 'waiting' ? '等待生效' : '查看进度' }}</button>
                <button v-else :class="['button small', unapplied(service) ? 'primary' : 'secondary']" :disabled="Boolean(busy)" @click="openPublish(service)">发布</button>
                <UiTooltip text="编辑服务"><button class="icon-button" :disabled="Boolean(busy)" :aria-label="`编辑：${service.name}`" @click="edit(service)"><Icon name="edit" :size="17" /></button></UiTooltip>
                <UiActionMenu :items="moreActions" :label="`更多操作：${service.name}`" :disabled="Boolean(busy)" @select="serviceAction($event, service)" />
                <span v-if="inspectionLoading && inspection?.kind === 'local-upstream' && inspection.service.id === service.id" class="service-action-pending" role="status"><span class="spinner tiny"></span>探测中…</span>
              </div></td>
            </tr>
          </template>
          <tr v-if="!filteredServices.length"><td colspan="5" class="table-empty">没有匹配的服务，请调整筛选条件或关键词。</td></tr>
        </tbody>
      </table>
    </div>
    <div class="table-meta"><span>显示 {{ filteredServices.length }} / {{ services.length }} 个服务</span></div>
  </section>

  <details v-if="jobs.length" class="raw-details publish-history">
    <summary>最近发布 · {{ jobs.length }}</summary>
    <div v-for="job in jobs" :key="job.id" class="publish-history-row"><div><strong>{{ job.serviceName }}</strong><span class="small muted">{{ formatDateTime(job.createdAt) }}</span></div><span :class="['pill', job.status === 'succeeded' ? 'private' : job.status === 'failed' ? 'amber' : 'neutral']">{{ publishStatus(job) }}</span><button class="text-button" @click="emit('view-job', job)">{{ isPublishActive(job) ? '查看进度' : '查看结果' }}</button></div>
  </details>

  <UiOverlay :open="Boolean(publishTarget)" :title="`发布「${publishTarget?.name || ''}」`" description="确认本机配置、域名解析与外部资源影响范围。" size="wide" @close="closePublish">
      <template v-if="publishTarget">
        <div v-if="publishError || submissionError" class="alert error" role="alert"><Icon name="warning" /><span>{{ publishError || submissionError }}</span></div>
        <div v-for="blocker in publishBlockers" :key="blocker.tab" class="alert warning" role="alert"><Icon name="warning" /><span>{{ blocker.text }}</span><button class="text-button" :disabled="Boolean(busy)" @click="configurePublish(blocker)">{{ blocker.label }}</button></div>
        <div v-if="publishLoading" class="compact-empty"><span class="spinner"></span><h3>正在读取发布预览</h3></div>
        <template v-else-if="publishItem">
          <dl class="publish-facts">
            <div><dt>访问入口</dt><dd><UiCopyValue :value="publishItem.entry" label="访问入口" /></dd></div>
            <div><dt>目标上游</dt><dd><UiCopyValue :value="publishItem.upstream" label="目标上游" /></dd></div>
            <div><dt>本机变化</dt><dd>{{ changeLabels[publishItem.change] || publishItem.change }}</dd></div>
          </dl>
          <div v-if="!publishItem.enabled" class="alert warning"><Icon name="warning" /><span>发布后将停用这个服务的入口。</span></div>
          <div v-if="publishAlsoOthers" class="alert warning"><Icon name="info" /><span>本机将应用整份草稿，不只是当前服务。影响范围：{{ publishImpact.join('、') || '其他服务或网关设置也有未发布修改' }}。</span></div>
          <p v-else-if="publishImpact.length" class="field-caption">本次影响：{{ publishImpact.join('、') }}</p>
          <div class="publish-section"><h3>本次步骤</h3><div class="pills"><span class="pill neutral">1 · 本机配置</span><span v-if="publishEdge" class="pill neutral">2 · {{ edgeName(publishTarget) }}</span><span v-if="publishNames.length || publishItem.domains?.length || state?.settings?.baseDomain" class="pill neutral">{{ publishEdge ? '3' : '2' }} · 域名解析</span></div></div>
          <div v-if="publishEdge && publishTarget.edgeProvider === 'cloudflare'" class="alert info"><Icon name="info" /><span>{{ publishTarget.usePreferred ? `将引用第三方优选域名 ${publishTarget.preferredDomain}；兼容性与访问速度待外部验收。` : '将使用账号的标准 Cloudflare 入口；若之前开启优选，本次发布会恢复标准入口。' }}</span></div>
          <p v-if="publishEdge && publishTarget.edgeProvider === 'cloudflare'" class="field-caption">{{ cfPublishCopy.verification }}</p>
          <div v-if="publishRecordsError" class="alert warning"><Icon name="info" /><span>解析信息暂时读不到：{{ publishRecordsError }}。执行时会重新核对，不代表解析已通过检查。</span></div>
          <details class="raw-details"><summary>配置与解析详情</summary><dl class="publish-facts"><div><dt>配置版本</dt><dd>{{ publishRevision }}</dd></div><div v-if="publishItem.domains?.length"><dt>域名</dt><dd class="mono">{{ publishItem.domains.join('、') }}</dd></div><div v-if="publishItem.domains?.length"><dt>证书</dt><dd>{{ publishItem.certificate || '未选择' }}</dd></div></dl><ul class="publish-hosts"><li v-for="record in publishNames" :key="record.host"><span class="mono">{{ record.host }}</span><span class="pill neutral">{{ record.status }}</span><span class="muted mono">{{ record.managed?.value || '发布时按实际入口生成' }}</span></li></ul></details>
          <p v-if="publishUnavailable" class="field-caption">发布进度服务暂不可用；编辑、保存和其他配置操作不受影响。</p><p v-else class="field-caption">发布开始后可以关闭窗口，稍后查看结果。</p>
        </template>
      </template>
      <template #footer="{ close: dismiss }"><button class="button secondary" @click="dismiss">取消</button><button class="button primary" :disabled="publishUnavailable || !publishItem || !publishFingerprint || publishLoading || Boolean(busy) || publishBlockers.length > 0 || Boolean(publishError)" @click="publish">发布</button></template>
  </UiOverlay>

  <UiOverlay :open="Boolean(confirming)" title="删除服务" :busy="Boolean(busy) || deletePending" @close="cancelDelete">
      <template v-if="confirming">
        <div v-if="deleteError" class="alert error" role="alert"><Icon name="warning" :size="18" /><span>{{ deleteError }}</span></div>
        <p class="delete-message">确认删除服务「{{ confirming.name }}」？</p>
        <p v-if="confirming.domains?.length" class="delete-domains mono">{{ confirming.domains.join('、') }}</p>
        <fieldset class="delete-cloud-options" :disabled="Boolean(busy) || deletePending">
          <legend>同时删除哪些云端资源？</legend>
          <UiCheckbox id="delete-service-dns" v-model="deleteOptions.deleteDNS" class="delete-cloud-option" :disabled="Boolean(busy) || deletePending"><span class="delete-cloud-copy"><strong>确认删除 DNS 解析</strong><small>通过 API 删除该服务托管的 DNS 解析，包含边缘 CNAME；不会删除加速域名。</small></span></UiCheckbox>
          <UiCheckbox v-if="confirming.edgeProvider !== 'cloudflare'" id="delete-service-edgeone" v-model="deleteOptions.deleteEdgeOne" class="delete-cloud-option" :disabled="Boolean(busy) || deletePending"><span class="delete-cloud-copy"><strong>确认删除边缘加速服务和解析（EdgeOne）</strong><small>通过 API 删除该服务的 EdgeOne 加速域名及关联 CNAME；其他普通 DNS 解析保留。</small></span></UiCheckbox>
          <UiCheckbox v-else id="delete-service-cloudflare" v-model="deleteOptions.deleteCloudflare" class="delete-cloud-option" :disabled="Boolean(busy) || deletePending"><span class="delete-cloud-copy"><strong>同时清理 Cloudflare 加速接入</strong><small>自动删除本程序创建且属于该服务的 CF 自定义主机名、关联业务 CNAME，以及本程序追加的验证 TXT；其他 TXT 保留。</small></span></UiCheckbox>
        </fieldset>
        <p v-if="confirming.edgeProvider === 'cloudflare'" class="field-caption">勾选后由系统清理该服务的专属接入，无需另去 Cloudflare 操作。共享 Fallback、标准入口、接入区域及 SSL 设置属于边缘账号，保留供其他服务使用；归属不明或已被修改的资源不会误删。</p>
        <p class="field-caption">仅清理该服务独占且由本网关托管的资源；共享域名、基站、源站组、证书及其他记录保留。</p>
        <p class="hint-panel"><Icon name="warning" :size="17" /><span>只删除勾选项，不勾选不删除。云端删除立即生效，已完成的删除不会自动回滚；本机入口仍需发布后撤下。</span></p>
        <p v-if="deletePending" class="field-caption" role="status">正在删除，请勿重复提交或关闭页面。停止等待并不代表操作已取消。</p>
      </template>
      <template #footer="{ close: dismiss }">
        <button class="button secondary" :disabled="Boolean(busy) || deletePending" @click="dismiss">取消</button>
        <button class="button danger" :disabled="Boolean(busy) || deletePending" @click="remove(confirming)">{{ deletePending ? '删除中…' : '删除服务' }}</button>
      </template>
  </UiOverlay>

  <UiOverlay :open="open" :title="editing ? '编辑服务' : '新增服务'" description="先保存草稿，再预览发布；现有线上入口不会因编辑立即改变。" variant="drawer" size="wide" :busy="Boolean(busy)" :dirty="formDirty" @close="close">
      <div class="service-form-body">
        <div v-if="formError" class="alert error" role="alert"><Icon name="warning" /><span>{{ formError }}</span></div>

        <div class="form-row">
          <div class="form-field"><label class="form-label" for="service-name">服务名称</label><div class="service-name-input"><ServiceIconPicker v-model="form.icon" :service="iconService" :disabled="Boolean(busy)" /><input id="service-name" v-model="form.name" maxlength="60" placeholder="飞牛 NAS" autocomplete="off" /></div></div>
          <div class="form-field"><label class="form-label" for="service-upstream">{{ upstreamCopy.label }}</label><input id="service-upstream" v-model="form.upstream" class="mono" :placeholder="upstreamCopy.example" autocomplete="off" spellcheck="false" aria-describedby="service-upstream-help" /><span id="service-upstream-help" class="field-caption">{{ upstreamCopy.help }}</span></div>
        </div>

        <div class="form-field">
          <span class="form-label">发布方式</span>
          <UiSegmented v-model="form.mode" :options="entryStyles" :disabled="Boolean(busy)" aria-label="服务发布方式" />
        </div>

        <template v-if="form.mode === 'name'">
          <div class="form-field">
            <span class="form-label">域名（可以多个）</span>
            <div v-for="(domain, index) in form.domains" :key="index" class="form-row domain-row">
              <input v-model="form.domains[index]" class="mono" placeholder="fnos.example.com" autocomplete="off" spellcheck="false" aria-label="域名" />
              <UiTooltip text="移除这个域名"><button type="button" class="icon-button danger-hover" :disabled="Boolean(busy)" aria-label="移除这个域名" @click="removeDomain(index)"><Icon name="trash" :size="16" /></button></UiTooltip>
            </div>
            <button type="button" class="text-button" :disabled="Boolean(busy)" @click="addDomain"><Icon name="plus" :size="14" />再加一个域名</button>
            <span class="field-caption">同一个域名不能出现在两个服务里。</span>
          </div>
          <div class="form-field">
            <label class="form-label" for="service-certificate">证书</label>
            <UiSelect id="service-certificate" v-model="form.certificateId" :options="certificateOptions" :disabled="Boolean(busy)" aria-label="服务证书" />
            <span v-if="matchingCertificates.length" class="field-caption">{{ certificateHint(certificates.find(cert => cert.id === form.certificateId) || matchingCertificates[0]) }}</span>
            <span v-else class="field-caption">没有可用于上述域名的证书；请先到证书页创建槽位并导入材料。</span>
            <span v-if="formCertificateWarning" class="field-caption error-text">{{ formCertificateWarning }}</span>
          </div>
          <div class="form-field">
            <span class="form-label">入口方式</span>
            <UiSegmented v-model="form.entryMode" :options="entryModes" :disabled="Boolean(busy)" aria-label="域名入口方式" />
            <span class="field-caption">DNS 托管账号与边缘账号相互独立，请在解析页设置 DNS 归属。</span>
          </div>
          <template v-if="form.entryMode === 'edge'">
            <div class="form-field"><span class="form-label">边缘提供商</span><UiSegmented v-model="form.edgeProvider" :options="edgeProviders" :disabled="Boolean(busy)" label="边缘提供商" /><span v-if="form.edgeProvider === 'edgeone' && !hasEdgeAccount" class="field-caption error-text">未配置可用的 EdgeOne 账号，请先到边缘加速页面添加。</span></div>
            <template v-if="form.edgeProvider === 'cloudflare'">
              <div class="form-field"><label class="form-label" for="service-cf-account">Cloudflare 接入账号</label><UiSelect id="service-cf-account" v-model="form.edgeAccountId" :options="cloudflareOptions" :disabled="Boolean(busy)" label="Cloudflare 接入账号" /><span v-if="!hasAccountFor(form)" class="field-caption error-text">请选择启用的 Cloudflare 账号；账号在边缘加速页管理。</span><span v-else class="field-caption">标准入口：{{ selectedCFAccount.standardTarget }}</span></div>
              <div class="switch-field"><div><strong>使用第三方优选域名</strong><span class="field-caption">只改变访客入口，不改变源站 AAAA。关闭并重新发布可恢复标准入口。</span></div><UiSwitch v-model="form.usePreferred" :disabled="Boolean(busy)" aria-label="使用第三方优选域名" /></div>
              <div v-if="form.usePreferred" class="form-field"><label class="form-label" for="service-preferred-domain">第三方优选域名</label><input id="service-preferred-domain" v-model="form.preferredDomain" class="mono" placeholder="preferred.example.net" :disabled="Boolean(busy)" autocomplete="off" spellcheck="false" /><span class="field-caption">仅填写域名，不含协议、路径或端口。不自建优选池、不测速。第三方可能调整或停用线路，兼容性和收益请从真实访问网络验收。</span></div>
            </template>
          </template>
        </template>

        <div v-else class="form-row">
          <div class="form-field">
            <label class="form-label" for="service-port">发布端口</label>
            <input id="service-port" v-model="form.port" class="mono" inputmode="numeric" placeholder="8080" autocomplete="off" />
            <span class="field-caption">{{ portPlaceholder }}；443 与管理端口不可占用。</span>
          </div>
          <div class="form-field">
            <span class="form-label">预计入口</span>
            <code class="entry-preview">{{ entry({ upstream: form.upstream, port: Number(form.port) || 0, domains: [] }) }}</code>
          </div>
        </div>

        <div class="switch-field"><div><strong>启用服务</strong><span class="field-caption">停用后需发布草稿，线上入口才会撤下。</span></div><UiSwitch v-model="form.enabled" :disabled="Boolean(busy)" aria-label="启用服务" /></div>
      </div>
      <template #footer="{ close: dismiss }">
        <button class="button secondary" :disabled="Boolean(busy)" @click="dismiss">取消</button>
        <button class="button secondary" :disabled="!canSave || Boolean(busy) || saving" @click="save(false)">保存为草稿</button>
        <button class="button primary" :disabled="!canSave || Boolean(busy) || saving || publishUnavailable" @click="save(true)">{{ saving ? '保存中…' : '保存并发布' }}</button>
      </template>
  </UiOverlay>

  <UiOverlay :open="Boolean(inspection)" :title="`${inspection?.kind === 'external' ? '外部探测' : '本机与上游探测'} · ${inspection?.service.name || ''}`" @close="inspection = null">
    <template v-if="inspection">
      <template v-if="inspection.kind === 'external'">
        <div v-if="inspectionLoading || (monitor.status?.running && !externalError)" class="compact-empty" role="status"><span class="spinner"></span><p>检查中…</p></div>
        <div v-else-if="externalError || monitor.error" class="alert warning" role="alert">{{ externalError || monitor.error }}</div>
        <MonitorCheckDetails v-else label="外部访问" :check="monitor.status?.services?.find(item => item.serviceId === inspection.service.id)?.checks.external" />
      </template>
      <div v-else class="inspection-sections">
        <section class="inspection-section" aria-label="本机入口探测">
          <h3><Icon name="shield" :size="18" />本机入口</h3>
          <div v-if="inspectionPending.local" class="inspection-summary" role="status"><span class="spinner tiny"></span><span>检查中…</span></div>
          <div v-else-if="checkResults[inspection.service.id]" class="inspection-summary" role="status"><Icon :name="localCheck(checkResults[inspection.service.id])?.status === 'ok' ? 'check' : 'warning'" :size="20" /><span v-if="checkResults[inspection.service.id].error" class="error-text">本机检查失败：{{ checkResults[inspection.service.id].error }}</span><span v-else>本机入口：{{ localCheck(checkResults[inspection.service.id])?.status === 'ok' ? '本机握手与代理链路已通过' : (localCheck(checkResults[inspection.service.id])?.error || '未检查') }}</span></div>
          <p v-else class="field-caption">未获取本机入口结果，请重新探测。</p>
        </section>
        <section class="inspection-section" aria-label="上游探测">
          <h3><Icon name="pulse" :size="18" />上游</h3>
          <div v-if="inspectionPending.upstream" class="inspection-summary" role="status"><span class="spinner tiny"></span><span>探测中…</span></div>
          <div v-else-if="probeResults[inspection.service.id]" class="inspection-summary" role="status"><Icon :name="probeResults[inspection.service.id].ok ? 'check' : 'warning'" :size="20" /><span v-if="probeResults[inspection.service.id].ok">上游返回 HTTP {{ probeResults[inspection.service.id].status }}，用时 {{ probeResults[inspection.service.id].latencyMs }} ms。</span><span v-else class="error-text">上游探测失败：{{ probeResults[inspection.service.id].error || '没有成功响应' }}</span></div>
          <p v-else class="field-caption">未获取上游结果，请重新探测。</p>
        </section>
        <p class="field-caption">本次探测不验证外部可达性，也不替代持续监测的连续故障判断。</p>
      </div>
    </template>
    <template #footer="{ close: dismiss }"><button class="button secondary" @click="dismiss">返回列表</button><button v-if="inspection?.kind === 'local-upstream'" class="button primary" :disabled="Boolean(busy) || inspectionLoading" @click="inspect(inspection.service)"><Icon name="refresh" :size="16" />{{ inspectionLoading ? '探测中…' : '重新探测' }}</button></template>
  </UiOverlay>
</template>

<style scoped>
.services-table .data-table{min-width:1120px}
.services-table th:first-child{width:24%}
.services-table th:nth-child(3){width:160px}
.services-table th:nth-child(4){width:160px}
.services-table th:last-child{width:224px}
.service-table-actions{gap:6px;min-width:max-content}
.service-table-actions :deep(.ui-tooltip),.service-table-actions :deep(.icon-button),.service-table-actions>.button{flex-shrink:0}
.service-ssl{overflow-wrap:anywhere}
.service-name-input{display:flex;align-items:center;gap:8px;min-width:0}
.service-name-input>input{flex:1;min-width:0;width:0}
.service-form-body{display:grid;gap:20px}
.delete-domains{overflow-wrap:anywhere;color:var(--muted);font-size:12px}
.delete-cloud-options{display:grid;gap:10px;padding:0;margin:18px 0;border:0;min-width:0}
.delete-cloud-options legend{padding:0;margin-bottom:10px;font-size:13px;font-weight:600}
.delete-cloud-option{display:flex;padding:12px;border:1px solid var(--line);border-radius:10px}
.delete-cloud-copy{display:grid;gap:5px;min-width:0;line-height:1.5}
.delete-cloud-option strong{font-size:13px}.delete-cloud-option small{font-size:12px;color:var(--muted)}
.delete-cloud-options:disabled .delete-cloud-option{cursor:wait;opacity:.7}
.inspection-sections{display:grid;gap:20px}.inspection-section h3{display:flex;align-items:center;gap:8px;margin:0 0 10px;font-size:14px}.inspection-summary{display:flex;align-items:flex-start;gap:12px;padding:16px;border:1px solid var(--line);border-radius:12px;line-height:1.6}.inspection-summary>svg,.inspection-summary>.spinner{flex-shrink:0;margin-top:3px}.inspection-summary>span{overflow-wrap:anywhere}

.publish-history { margin-top: 20px; }
.publish-history-row { display: flex; align-items: center; gap: 12px; padding: 12px 0; border-bottom: 1px solid var(--line); flex-wrap: wrap; }
.publish-history-row > div { flex: 1; min-width: 160px; display: grid; gap: 4px; }
.publish-history-row strong { overflow-wrap: anywhere; }
</style>
