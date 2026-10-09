<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import Icon from './Icon.vue'
import UiCheckbox from './components/ui/UiCheckbox.vue'
import UiCopyValue from './components/ui/UiCopyValue.vue'
import UiOverlay from './components/ui/UiOverlay.vue'
import UiSelect from './components/ui/UiSelect.vue'
import UiSegmented from './components/ui/UiSegmented.vue'
import UiSwitch from './components/ui/UiSwitch.vue'
import UiTooltip from './components/ui/UiTooltip.vue'
import UiActionMenu from './components/ui/UiActionMenu.vue'
import { usePageScope } from './page-scope.ts'
import { isRequestCancelled } from './request.ts'
import ServiceAvatar from './components/ServiceAvatar.vue'
import { isPublishActive, publishStatus } from './usePublishJobs.js'

const props = defineProps({ state: Object, busy: String, jobs: { type: Array, default: () => [] } })
const emitEvent = defineEmits(['state', 'notice', 'busy', 'publish-service', 'view-job', 'refresh-jobs'])
const scope = usePageScope(), api = scope.api, emit = scope.guard(emitEvent)
const loadedAccounts = ref(null), accountsLoading = ref(false), accountsError = ref('')
let accountReadGeneration = 0
const accounts = computed(() => loadedAccounts.value ?? accountsFromState(props.state))
const baseDomain = computed(() => props.state?.settings?.baseDomain || '')
const origins = computed(() => props.state?.edgeOrigins || [])
const enabledAccounts = computed(() => accounts.value.filter(account => account.enabled))
const configuredHosts = computed(() => (props.state?.services || []).filter(service => service.entryMode === 'edge').flatMap(service => service.domains || []))
const hosts = ref([]), hostsLoading = ref(false), hostsError = ref(''), providerErrors = ref([]), hostsHaveResult = ref(false)
const accountsOpen = ref(false), accountOpen = ref(false), editingAccount = ref(null), accountOverlay = ref(null), accountNameInput = ref(null)
const accountSaving = ref(false), accountError = ref(''), deleting = ref(null), deleteError = ref('')
const accountForm = reactive(emptyAccount()), accountSnapshot = ref(JSON.stringify(accountForm))
const accountDirty = computed(() => JSON.stringify(accountForm) !== accountSnapshot.value)
const accountManual = ref(false), accountDiscovery = ref(null), accountDiscoveryBusy = ref(false), accountDiscoveryError = ref('')
const accountDiscoveryToken = ref(''), accountZoneId = ref('')
let accountDiscoveryGeneration = 0, accountAutoName = ''
const accountAuto = computed(() => accountForm.provider === 'cloudflare' && !editingAccount.value && !accountManual.value)
const accountZones = computed(() => accountDiscovery.value?.zones || [])
const accountZoneOptions = computed(() => accountZones.value.map(zone => ({ value: zone.zoneId, label: `${zone.zoneName}${zone.eligible ? '' : '（不可用）'}`, disabled: !zone.eligible })))
const accountSelectedZone = computed(() => accountZones.value.find(zone => zone.zoneId === accountZoneId.value && zone.eligible))
const preview = ref(null), previewOpen = ref(false), previewHost = ref(''), previewProvider = ref('edgeone')
const previewBusy = ref(false), previewError = ref(''), confirmed = ref(false), confirmedFallbackReplacement = ref(false), applyMessage = ref('')
const previewReplacesFallback = computed(() => previewProvider.value === 'cloudflare' && Boolean(preview.value?.steps?.some(step => step.action === 'replace-fallback')))
const postchecks = ref({}), postcheckBusy = ref(''), restoreHost = ref(null), restoreConfirmed = ref(false), restoreError = ref('')
const httpValidationHost = ref(null), httpValidationBusy = ref(false), httpValidationConfirmed = ref(false), httpValidationError = ref('')
const httpValidationUncertain = ref({})
function canStartHTTPValidation(host) {
  return host.provider === 'cloudflare' && host.enabled !== false && activePublish(host)?.status === 'waiting' &&
    host.hostnameStatus === 'active' && host.originStatus === 'active' && String(host.certificateStatus || '').startsWith('pending') &&
    !host.usePreferred && !serviceFor(host).usePreferred && Boolean(host.standardTarget) && !host.recordSays
}
function openHTTPValidation(host) {
  if (props.busy || hostsLoading.value || hostsError.value || accountSaving.value || httpValidationBusy.value || !canStartHTTPValidation(host)) return
  httpValidationHost.value = { ...host, revision: props.state.revision }
  httpValidationConfirmed.value = false; httpValidationError.value = ''
}
function closeHTTPValidation() { if (!httpValidationBusy.value) httpValidationHost.value = null }
async function refreshHTTPValidation() { emit('refresh-jobs'); await loadHosts() }
async function startHTTPValidation() {
  const host = httpValidationHost.value
  if (!host || !httpValidationConfirmed.value || httpValidationUncertain.value[host.host] || props.busy || httpValidationBusy.value) return
  if (host.revision !== props.state.revision || !canStartHTTPValidation(host)) {
    httpValidationError.value = '配置或发布状态已变化，请关闭后重新读取云端状态并核对。'; return
  }
  httpValidationBusy.value = true; httpValidationError.value = ''; emit('busy', 'cloudflare-http-validation')
  let refreshNeeded = false
  try {
    const result = await api.startCloudflareHTTPValidation(host.host, { confirm: true, revision: host.revision })
    refreshNeeded = true; httpValidationHost.value = null
    emit('notice', result.message || '已发布标准入口 CNAME，等待 Cloudflare 自动 HTTP 验证；证书尚未就绪，暂不能使用 HTTPS。')
    const job = props.jobs.find(job => job.id === result.jobId)
    if (job) emit('view-job', job)
  } catch (err) {
    if (!isRequestCancelled(err)) {
      httpValidationError.value = err.message
      if (err.outcomeUnknown) { httpValidationUncertain.value[host.host] = true; refreshNeeded = true }
    }
  } finally { httpValidationBusy.value = false; httpValidationConfirmed.value = false; emit('busy', '') }
  if (refreshNeeded) await refreshHTTPValidation()
}
async function checkPublished(host) {
  if (postcheckBusy.value || props.busy) return
  postcheckBusy.value = host.host
  try { postchecks.value[host.host] = await api.checkCloudflareEntry(host.host) }
  catch (err) { if (!isRequestCancelled(err)) postchecks.value[host.host] = { verified: false, message: err.message } }
  finally { postcheckBusy.value = '' }
}
async function prepareStandardRestore() {
  if (!restoreConfirmed.value || !restoreHost.value || props.busy || accountSaving.value) return
  accountSaving.value = true; restoreError.value = ''; emit('busy', 'standard-target-draft')
  try {
    const current = await api.state()
    const service = current.services.find(service => service.id === restoreHost.value.serviceId)
    if (!service || service.edgeProvider !== 'cloudflare') throw new Error('服务入口已变化，请刷新后核对。')
    acceptState(await api.updateService(service.id, { ...service, usePreferred: false }))
    emit('notice', '已保存恢复标准入口的草稿；请到服务页预览并发布。当前 DNS 未改变，缓存不会立即失效。')
    restoreHost.value = null
  } catch (err) { if (!isRequestCancelled(err)) restoreError.value = err.message }
  finally { accountSaving.value = false; emit('busy', '') }
}
const providers = [{ value: 'edgeone', label: '腾讯云 EdgeOne' }, { value: 'cloudflare', label: 'Cloudflare' }]
const rowCopy = {
  preview: '预览接入', progress: '查看进度', more: '更多操作：', publish: '自动发布', check: '独立访问检查', checking: '检查中…',
  restore: '恢复标准入口', http: '尝试自动 HTTP 验证', hostname: '主机名', origin: '回源配置', certificate: 'CF 边缘证书',
  loading: '读取中…', unknown: '未核对', activeHostname: '已激活', activeCertificate: '已签发', activeOrigin: '已就绪',
  pending: '处理中', pendingValidation: '待验证', pendingIssuance: '签发中', pendingDeployment: '部署中',
  initializing: '初始化中', deleting: '删除中', deleted: '已删除', moved: '已迁移', blocked: '受限', error: '异常',
  validationTimeout: '验证超时', issuanceTimeout: '签发超时', deploymentTimeout: '部署超时', expired: '已过期',
  cfCertificateHelp: 'Cloudflare 边缘证书状态；不等同于 EdgeOne 的源站证书绑定，也不代表访问已通过。',
  cfOriginHelp: 'Fallback 云资源状态，不代表源站访问已通过。',
}
const modeOptions = [{ value: 'address', label: '直接用公网地址' }, { value: 'base', label: '用基站域名' }]
const stepLabels = { edgeone: {
  'create-origin-group': '创建源站组', 'update-origin': '更新回源地址', 'create-domain': '创建加速域名',
  'update-domain-origin': '改回源配置', 'enable-domain': '启用加速域名', 'upload-certificate': '上传证书',
  'bind-certificate': '绑定证书', 'enable-websocket': '开启 WebSocket',
}, cloudflare: { 'set-ssl-strict': '设置完整严格 SSL', 'create-fallback-dns': '创建回源记录', 'create-target-dns': '创建标准接入记录',
  'set-fallback': '设置回源', 'replace-fallback': '替换共享 Fallback 回源', 'create-hostname': '创建加速域名', 'ensure-validation-txt': '添加验证记录', 'use-dcv-delegation': '复用证书验证委派（只读）' } }
const accountValid = computed(() => {
  if (!accountAuto.value && !accountForm.name.trim()) return false
  if (accountForm.provider === 'edgeone') return (accountForm.originMode !== 'base' || Boolean(baseDomain.value)) &&
    (Boolean(editingAccount.value) && !accountForm.secretId.trim() && !accountForm.secretKey.trim() || Boolean(accountForm.secretId.trim() && accountForm.secretKey.trim()))
  if (accountAuto.value) return !accountDiscoveryBusy.value && Boolean(accountSelectedZone.value && (!accountSelectedZone.value.requiresSSLStrict || accountForm.autoSSLStrict) && accountDiscoveryToken.value && accountDiscoveryToken.value === accountForm.apiToken.trim() && accountForm.zoneId && accountForm.fallbackDomain && accountForm.standardTarget)
  return Boolean(editingAccount.value) || Boolean(accountForm.apiToken.trim() && accountForm.zoneId.trim() && accountForm.fallbackDomain.trim() && accountForm.standardTarget.trim())
})
const previewTarget = computed(() => preview.value?.target || preview.value?.cname || '')
const previewNeedsDNS = computed(() => previewTarget.value && !hosts.value.some(host => host.provider === previewProvider.value && host.host === previewHost.value && sameCname(host.recordSays, previewTarget.value)))
const validationRecords = computed(() => [...new Map((preview.value?.validationRecords || []).filter(record => !(preview.value?.steps || []).some(step => step.action === 'use-dcv-delegation' && step.target === record.name)).map(record => [`${record.type}:${record.name}:${record.value}`, record])).values()])
const previewSteps = computed(() => (preview.value?.steps || []).filter(step => step.action !== 'ensure-validation-txt' || !validationRecords.value.length))

function emptyAccount() { return { provider: 'edgeone', name: '', secretId: '', secretKey: '', apiToken: '', originMode: 'address', zoneId: '', fallbackDomain: '', standardTarget: '', enabled: true, autoSSLStrict: false } }
function providerName(provider) { return ({ cloudflare: 'Cloudflare', edgeone: 'EdgeOne' })[provider] || '未知服务商' }
function cloudflareDefaultName(zone) { return `CF · ${zone.zoneName.slice(0, 94)}` }
function serviceFor(host) {
  return (props.state?.services || []).find(service => service.id === host.serviceId ||
    (!host.serviceId && (service.edgeProvider || 'edgeone') === host.provider && service.domains?.includes(host.host))) ||
    { name: host.serviceName || '已无服务使用', entryMode: 'edge', edgeProvider: host.provider, domains: [host.host] }
}
function activePublish(host) { return props.jobs.find(job => job.serviceId === serviceFor(host).id && isPublishActive(job)) }
function hostActions(host) {
  const blocked = Boolean(props.busy || previewBusy.value || accountSaving.value)
  const actions = serviceFor(host).id ? [{ id: 'publish', label: rowCopy.publish, icon: 'cloud', disabled: blocked || Boolean(activePublish(host)) }] : []
  if (host.provider !== 'cloudflare') return actions
  actions.push({ id: 'check', label: postcheckBusy.value === host.host ? rowCopy.checking : rowCopy.check, icon: 'pulse', disabled: blocked || Boolean(postcheckBusy.value) })
  if (canStartHTTPValidation(host)) actions.push({ id: 'http', label: rowCopy.http, icon: 'globe', disabled: blocked || hostsLoading.value || Boolean(hostsError.value) || httpValidationBusy.value })
  if (host.usePreferred) actions.push({ id: 'restore', label: rowCopy.restore, icon: 'refresh', disabled: blocked, separator: true })
  return actions
}
function hostAction(action, host) {
  const item = hostActions(host).find(item => item.id === action)
  if (!item || item.disabled) return
  if (action === 'publish') publishService(host)
  else if (action === 'check') checkPublished(host)
  else if (action === 'http') openHTTPValidation(host)
  else if (action === 'restore') {
    restoreHost.value = { ...host, serviceId: serviceFor(host).id }
    restoreConfirmed.value = false; restoreError.value = ''
  }
}
function publishService(host) {
  const job = activePublish(host)
  if (job) { emit('view-job', job); return }
  if (props.busy) return
  const service = serviceFor(host)
  if (service.id) { previewOpen.value = false; emit('publish-service', service.id) }
}
function modeText(mode) { return mode === 'base' ? '用基站域名' : '直接用公网地址' }
function accountsFromState(snapshotState) {
  return [...(snapshotState?.edgeOneAccounts || []).map(account => ({ ...account, provider: 'edgeone', key: `edgeone:${account.id}` })),
    ...(snapshotState?.cloudflareAccounts || []).map(account => ({ ...account, provider: 'cloudflare', key: `cloudflare:${account.id}` }))]
    .sort((left, right) => left.name.localeCompare(right.name) || left.key.localeCompare(right.key))
}
function unknownHosts() {
  return (props.state?.services || []).filter(service => service.entryMode === 'edge').flatMap(service => (service.domains || []).map(host => ({
    key: `${service.edgeProvider || 'edgeone'}:${host}`, provider: service.edgeProvider || 'edgeone', host,
    serviceId: service.id, serviceName: service.name, enabled: service.enabled, status: 'unknown', ready: false,
    usePreferred: service.usePreferred, target: '', detail: {},
  })))
}
function acceptState(updatedState) { accountReadGeneration++; loadedAccounts.value = accountsFromState(updatedState); emit('state', updatedState) }
function sameCname(left, right) {
  const normalize = value => String(value || '').trim().replace(/\.$/, '').toLowerCase()
  return Boolean(normalize(left) && normalize(left) === normalize(right))
}
function certificateBinding(host) {
  const detail = host.detail || {}
  if (hostsLoading.value || hostsError.value || !detail.domainState) return { label: '未核对', tone: 'neutral' }
  if (!detail.certBound) return { label: '未绑定', tone: 'amber' }
  if (detail.certLocal && detail.certLocal !== detail.certBound) return { label: '版本不一致', tone: 'amber' }
  return { label: '已绑定', tone: 'private' }
}
function cloudflareResourceStatus(host, kind) {
  if (hostsLoading.value) return { label: rowCopy.loading, tone: 'neutral' }
  if (hostsError.value || host.status === 'unknown') return { label: rowCopy.unknown, tone: 'neutral' }
  const status = host[`${kind}Status`]
  if (status === 'active' || (kind === 'origin' && status === 'ready')) {
    return { label: ({ hostname: rowCopy.activeHostname, certificate: rowCopy.activeCertificate, origin: rowCopy.activeOrigin })[kind], tone: 'private' }
  }
  const labels = {
    pending: rowCopy.pending, pending_validation: rowCopy.pendingValidation, pending_issuance: rowCopy.pendingIssuance,
    pending_deployment: rowCopy.pendingDeployment, initializing: rowCopy.initializing, pending_deletion: rowCopy.deleting,
    deleted: rowCopy.deleted, moved: rowCopy.moved, blocked: rowCopy.blocked, pending_blocked: rowCopy.blocked, error: rowCopy.error,
    validation_timed_out: rowCopy.validationTimeout, issuance_timed_out: rowCopy.issuanceTimeout,
    deployment_timed_out: rowCopy.deploymentTimeout, expired: rowCopy.expired,
  }
  const label = labels[status]
  if (!label) return { label: rowCopy.unknown, tone: 'neutral' }
  return { label, tone: ['pending', 'pending_validation', 'pending_issuance', 'pending_deployment', 'initializing', 'pending_deletion'].includes(status) ? 'blue' : 'amber' }
}
function domainStatus(host) {
  const job = activePublish(host)
  if (job) return { label: publishStatus(job), tone: 'blue' }
  if (hostsLoading.value) return { label: '读取中…', tone: 'neutral' }
  if (hostsError.value) return { label: hostsHaveResult.value ? '上次状态 · 待重读' : '状态未取得', tone: 'neutral' }
  if (host.status === 'disabled') return { label: '服务或账号已停用', tone: 'neutral' }
  if (host.status === 'error') return { label: '接入异常', tone: 'amber' }
  if (host.status === 'unknown') return { label: '状态未取得', tone: 'neutral' }
  if (host.provider === 'cloudflare') return { label: host.ready ? '发布记录已对齐' : ({ pending: '接入 / 解析待完成', error: '接入异常', unknown: '状态未取得' }[host.status] || '待核对'), tone: host.ready ? 'private' : host.status === 'error' ? 'amber' : 'neutral' }
  const detail = host.detail || {}
  if (!detail.domainState) return { label: '状态未取得', tone: 'neutral' }
  if (['init', 'process'].includes(detail.domainState)) return { label: '接入处理中', tone: 'blue' }
  if (detail.domainState === 'offline') return { label: '加速域名未启用', tone: 'amber' }
  if (detail.domainState !== 'online' || !host.target) return { label: '状态待核对', tone: 'neutral' }
  if (!host.recordSays) return { label: '解析待发布', tone: 'blue' }
  if (!sameCname(host.recordSays, host.target)) return { label: '发布记录不匹配', tone: 'amber' }
  if (!detail.certBound) return { label: '证书待绑定', tone: 'amber' }
  if (detail.certLocal && detail.certLocal !== detail.certBound) return { label: '证书版本待核对', tone: 'amber' }
  return host.ready ? { label: '发布记录已对齐', tone: 'private' } : { label: '接入状态待核对', tone: 'neutral' }
}
function accountName(id) { return accounts.value.find(account => account.provider === 'edgeone' && account.id === id)?.name || '已删除的账号' }
function invalidateAccountDiscovery(clearFields = true) {
  accountDiscoveryGeneration++; accountDiscovery.value = null; accountDiscoveryBusy.value = false; accountDiscoveryError.value = ''; accountDiscoveryToken.value = ''; accountZoneId.value = ''
  if (clearFields) {
    accountForm.zoneId = ''; accountForm.fallbackDomain = ''; accountForm.standardTarget = ''
    if (accountAutoName && accountForm.name === accountAutoName) accountForm.name = ''
    accountAutoName = ''
  }
}
function clearAccount() { invalidateAccountDiscovery(); accountManual.value = false; Object.assign(accountForm, emptyAccount()); editingAccount.value = null; accountError.value = ''; accountSnapshot.value = JSON.stringify(accountForm) }
// A provider change must not keep hidden credentials from the other form.
watch(() => accountForm.provider, () => {
  invalidateAccountDiscovery(); accountManual.value = false
  accountForm.secretId = ''; accountForm.secretKey = ''; accountForm.apiToken = ''
  accountForm.zoneId = ''; accountForm.fallbackDomain = ''; accountForm.standardTarget = ''; accountForm.originMode = 'address'
}, { flush: 'sync' })
watch(() => accountForm.apiToken, () => { if (!editingAccount.value) invalidateAccountDiscovery(!accountManual.value) }, { flush: 'sync' })
watch(accountManual, manual => invalidateAccountDiscovery(!manual), { flush: 'sync' })
watch(accountZoneId, zoneId => {
  if (!accountAuto.value) return
  const zone = accountZones.value.find(candidate => candidate.zoneId === zoneId && candidate.eligible)
  accountForm.zoneId = zone?.zoneId || ''; accountForm.fallbackDomain = zone?.fallbackDomain || ''; accountForm.standardTarget = zone?.standardTarget || ''
  if (zone && (!accountForm.name.trim() || accountForm.name === accountAutoName)) {
    accountAutoName = cloudflareDefaultName(zone); accountForm.name = accountAutoName
  }
}, { flush: 'sync' })
async function discoverCloudflareAccount() {
  if (!accountAuto.value || !accountOpen.value || accountSaving.value || props.busy || accountDiscoveryBusy.value || !accountForm.apiToken.trim()) return
  invalidateAccountDiscovery(); accountError.value = ''
  const generation = accountDiscoveryGeneration, apiToken = accountForm.apiToken.trim()
  accountDiscoveryBusy.value = true
  try {
    const result = await api.discoverCloudflareAccount({ apiToken })
    if (generation !== accountDiscoveryGeneration || !accountOpen.value || !accountAuto.value || apiToken !== accountForm.apiToken.trim()) return
    accountDiscovery.value = result; accountDiscoveryToken.value = apiToken
    const eligible = accountZones.value.filter(zone => zone.eligible)
    accountZoneId.value = eligible.find(zone => zone.zoneId === result.recommendedZoneId)?.zoneId || eligible[0]?.zoneId || ''
  } catch (err) {
    if (generation === accountDiscoveryGeneration && !isRequestCancelled(err)) accountDiscoveryError.value = err.message
  } finally { if (generation === accountDiscoveryGeneration) accountDiscoveryBusy.value = false }
}
function manageAccounts() { if (props.busy || accountSaving.value) return; clearAccount(); accountOpen.value = false; accountsOpen.value = true }
function newAccount() {
  if (props.busy || accountSaving.value) return
  clearAccount(); accountOpen.value = true; accountsOpen.value = true; nextTick(() => accountNameInput.value?.focus())
}
function editAccount(account) {
  if (props.busy || accountSaving.value) return
  clearAccount(); editingAccount.value = account
  Object.assign(accountForm, { provider: account.provider, name: account.name, originMode: account.originMode || 'address',
    zoneId: account.zoneId || '', fallbackDomain: account.fallbackDomain || '', standardTarget: account.standardTarget || '', enabled: Boolean(account.enabled), autoSSLStrict: account.autoSSLStrict === true })
  accountSnapshot.value = JSON.stringify(accountForm); accountOpen.value = true; accountsOpen.value = true
  nextTick(() => accountNameInput.value?.focus())
}
function closeAccounts() { accountsOpen.value = false; accountOpen.value = false; clearAccount() }
function askDelete(account) { if (props.busy || accountSaving.value) return; deleting.value = account; deleteError.value = '' }
async function loadAccounts() {
  if (accountsLoading.value) return
  const generation = accountReadGeneration
  accountsLoading.value = true; accountsError.value = ''
  try { const result = await api.edgeWorkspaceAccounts(); if (generation === accountReadGeneration) loadedAccounts.value = result.accounts || [] }
  catch (err) { if (!isRequestCancelled(err) && generation === accountReadGeneration) accountsError.value = err.message }
  finally { accountsLoading.value = false }
}
async function loadHosts() {
  if (hostsLoading.value) return
  hostsLoading.value = true; hostsError.value = ''
  try { const result = await api.edgeWorkspaceHosts(); hosts.value = result.hosts || []; providerErrors.value = result.errors || []; hostsHaveResult.value = true }
  catch (err) { if (!isRequestCancelled(err)) { hostsError.value = err.message; if (!hosts.value.length) hosts.value = unknownHosts() } }
  finally { hostsLoading.value = false }
}
async function refresh() { if (!props.busy && !accountSaving.value && !previewBusy.value) await Promise.all([loadAccounts(), loadHosts()]) }
async function saveAccount() {
  if (accountSaving.value || props.busy || !accountValid.value || !accountDirty.value) return
  accountSaving.value = true; accountError.value = ''; emit('busy', 'edge-account')
  let saved = false
  try {
    const provider = editingAccount.value?.provider || accountForm.provider
    let body = { name: accountForm.name.trim() || (accountAuto.value ? cloudflareDefaultName(accountSelectedZone.value) : '') }
    if (provider === 'cloudflare') body = { ...body, enabled: accountForm.enabled, autoSSLStrict: accountForm.autoSSLStrict,
      ...(accountForm.apiToken.trim() ? { apiToken: accountForm.apiToken.trim() } : {}),
      ...(accountAuto.value ? { autoConfigure: true, ...(accountSelectedZone.value?.fallbackMode ? { fallbackMode: accountSelectedZone.value.fallbackMode, existingFallback: accountSelectedZone.value.existingFallback || '' } : {}) } : {}),
      ...(!editingAccount.value ? { zoneId: accountForm.zoneId.trim(), fallbackDomain: accountForm.fallbackDomain.trim(), standardTarget: accountForm.standardTarget.trim() } : {}) }
    else body = { ...body, originMode: accountForm.originMode, ...(editingAccount.value ? { enabled: accountForm.enabled } : {}), ...(!editingAccount.value || accountForm.secretId.trim() || accountForm.secretKey.trim() ? { secretId: accountForm.secretId.trim(), secretKey: accountForm.secretKey.trim() } : {}) }
    const next = editingAccount.value ? await api.updateEdgeWorkspaceAccount(provider, editingAccount.value.id, body) : await api.createEdgeWorkspaceAccount({ provider, ...body })
    acceptState(next)
    accountOpen.value = false; clearAccount(); emit('notice', next.operationRecoveryMessage || `${providerName(provider)} 账号已保存；凭据不回显，尚未发布域名。`)
    saved = true
  } catch (err) { if (!isRequestCancelled(err)) accountError.value = err.message }
  finally { accountSaving.value = false; emit('busy', '') }
  if (saved) await loadHosts()
}
async function removeAccount() {
  if (!deleting.value || accountSaving.value || props.busy) return
  accountSaving.value = true; deleteError.value = ''; emit('busy', 'edge-account-delete')
  let removed = false
  try {
    acceptState(await api.deleteEdgeWorkspaceAccount(deleting.value.provider, deleting.value.id))
    deleting.value = null; removed = true; emit('notice', '本地边缘账号及凭据已删除，云端资源未删除。')
  } catch (err) { if (!isRequestCancelled(err)) { deleteError.value = err.message; emit('notice', { type: 'error', message: err.message }) } }
  finally { accountSaving.value = false; emit('busy', '') }
  if (removed) await loadHosts()
}
async function openPreview(host) {
  if (props.busy || previewBusy.value || accountSaving.value) return
  previewHost.value = host.host; previewProvider.value = host.provider; previewOpen.value = true; previewBusy.value = true
  previewError.value = ''; preview.value = null; confirmed.value = false; confirmedFallbackReplacement.value = false; applyMessage.value = ''
  try { preview.value = await api.previewEdgeWorkspace(host.provider, host.host) }
  catch (err) { if (!isRequestCancelled(err)) previewError.value = err.message }
  finally { previewBusy.value = false }
}
function closePreview() { if (previewBusy.value || props.busy) return; previewOpen.value = false; preview.value = null; confirmed.value = false; confirmedFallbackReplacement.value = false }
async function applyPreview() {
  if (!(preview.value?.steps?.length || preview.value?.requiresReconciliation) || !confirmed.value || (previewReplacesFallback.value && !confirmedFallbackReplacement.value) || previewBusy.value || props.busy) return
  previewBusy.value = true; previewError.value = ''; emit('busy', 'edge-apply')
  let applied = false
  try {
    const result = await api.applyEdgeWorkspace({ provider: previewProvider.value, host: preview.value.host, token: preview.value.token, revision: preview.value.revision, confirm: true,
      ...(previewReplacesFallback.value ? { confirmFallbackReplacement: true } : {}) })
    applyMessage.value = result.message || `${providerName(previewProvider.value)} 已接受提交；公网连通仍需独立验证。`
    preview.value = null; confirmed.value = false; applied = true; emit('notice', applyMessage.value)
  } catch (err) { if (!isRequestCancelled(err)) previewError.value = err.message }
  finally { previewBusy.value = false; confirmedFallbackReplacement.value = false; emit('busy', '') }
  if (applied) await loadHosts()
}
onMounted(refresh)
watch(() => props.jobs.filter(isPublishActive).map(job => job.id).join(','), (current, previous) => { if (previous && current !== previous) loadHosts() })
onBeforeUnmount(clearAccount)
defineExpose({ refresh })
</script>

<template>
  <div class="cloud-toolbar">
    <div class="cloud-context"><strong>{{ configuredHosts.length }} 个加速域名</strong><span class="small muted">{{ enabledAccounts.length }} 个启用账号</span><span v-if="baseDomain" class="small muted">基站 {{ baseDomain }}</span></div>
    <div class="actions"><button class="button secondary small" :disabled="Boolean(busy) || accountSaving" @click="manageAccounts">管理账号 <span class="badge-count">{{ accounts.length }}</span></button></div>
  </div>
  <div v-if="accountsError" class="alert error" role="alert"><Icon name="warning" /><span>边缘账号读取失败：{{ accountsError }}</span><button class="text-button" @click="manageAccounts">查看账号</button></div>
  <div v-if="!enabledAccounts.length && !accountsError && !accountsLoading" class="alert info"><Icon name="info" /><span>还没有启用的边缘账号，添加账号时选择 EdgeOne 或 Cloudflare。</span><button class="text-button" :disabled="Boolean(busy)" @click="accounts.length ? manageAccounts() : newAccount()">{{ accounts.length ? '管理账号' : '添加账号' }}</button></div>
  <section class="card cloud-workspace">
    <div class="card-head"><h2>加速域名 <span class="badge-count">{{ hosts.length || configuredHosts.length }}</span></h2><button class="button secondary small" :disabled="hostsLoading || Boolean(busy) || previewBusy" @click="loadHosts"><Icon name="refresh" :size="15" />{{ hostsLoading ? '读取中…' : '读取云端状态' }}</button></div>
    <div v-if="hostsError || providerErrors.length" class="card-body">
      <div v-if="hostsError" class="alert error" role="alert">{{ hostsError }}{{ hosts.length ? (hostsHaveResult ? '；当前保留上次读取结果，请重新读取。' : '；当前仅显示本地域名配置，云端状态尚未取得。') : '' }}</div>
      <div v-for="(error, index) in providerErrors" :key="error.provider + index" class="alert warning" role="alert"><span>{{ providerName(error.provider) }}：{{ error.message }}。其他账号的结果不受影响。</span></div>
    </div>
    <div v-if="hostsLoading && !hosts.length" class="compact-empty" role="status"><span class="spinner"></span><p>正在读取边缘加速状态…</p></div>
    <div v-else-if="!hosts.length && !hostsError && !providerErrors.length" class="compact-empty"><Icon name="cloud" :size="30" /><h3>还没有加速域名</h3><p>在服务页选择「边缘加速」并保存服务，域名会统一显示在这里。</p></div>
    <div v-else-if="hosts.length" class="table-wrap" :aria-busy="hostsLoading">
      <table class="data-table edge-host-table"><thead><tr><th>服务</th><th>解析记录</th><th>站点 / 回源</th><th>证书</th><th>状态</th><th>操作</th><th>服务商</th></tr></thead><tbody>
        <tr v-for="host in hosts" :key="host.key || host.provider + ':' + host.host">
          <td><div class="name-cell"><ServiceAvatar :service="serviceFor(host)" /><strong>{{ serviceFor(host).name }}</strong></div></td>
          <td><UiCopyValue :value="host.host" label="复制业务域名" /><div class="dns-record-target"><span class="small muted">CNAME →</span><UiCopyValue v-if="host.target" :value="host.target" label="复制解析目标" /><span v-else class="secondary">尚未分配</span></div><small v-if="host.provider === 'cloudflare' && host.usePreferred" class="secondary">第三方优选 · 标准目标 <span class="mono">{{ host.standardTarget || '尚未配置' }}</span></small><small v-if="!hostsLoading && host.recordSays && !sameCname(host.recordSays, host.target)" class="secondary">本地记录 <UiCopyValue :value="host.recordSays" label="复制本地发布记录值" /></small><small v-else-if="!hostsLoading && !host.recordSays" class="secondary">暂无本地发布记录</small></td>
          <td><template v-if="host.provider === 'edgeone'"><UiCopyValue v-if="host.detail?.site" :value="host.detail.site" label="复制站点" /><small class="secondary">{{ modeText(host.originMode) }}</small></template><small v-else class="secondary">IPv6 HTTPS 回源</small><UiCopyValue v-if="host.origin" :value="host.origin" label="复制回源地址" /><UiTooltip v-if="host.provider === 'cloudflare'" :text="rowCopy.cfOriginHelp"><small class="secondary" tabindex="0">{{ rowCopy.origin }} · {{ cloudflareResourceStatus(host, 'origin').label }}</small></UiTooltip></td>
          <td><span v-if="host.provider === 'edgeone'" :class="['pill', certificateBinding(host).tone]">{{ certificateBinding(host).label }}</span><template v-else><UiTooltip :text="rowCopy.cfCertificateHelp"><span :class="['pill', cloudflareResourceStatus(host, 'certificate').tone]" tabindex="0">{{ cloudflareResourceStatus(host, 'certificate').label }}</span></UiTooltip><small class="secondary">{{ rowCopy.hostname }} · {{ cloudflareResourceStatus(host, 'hostname').label }}</small></template></td>
          <td><span :class="['pill', domainStatus(host).tone]">{{ domainStatus(host).label }}</span><small v-if="activePublish(host)?.status === 'waiting'" class="secondary edge-status-note">系统正在等待生效，将自动继续，无需操作。</small><small v-else-if="host.message && !hostsLoading && domainStatus(host).tone !== 'private'" class="secondary edge-status-note">{{ host.message }}</small><small v-if="postchecks[host.host]" class="secondary">切后核验：{{ postchecks[host.host].verified ? '本机 IPv4 路径通过' : '未通过或未知' }} · {{ postchecks[host.host].message }}</small></td>
          <td class="edge-actions">
            <div class="edge-row-actions">
              <button v-if="activePublish(host)" class="text-button" @click="publishService(host)">{{ rowCopy.progress }}</button>
              <button v-else class="text-button" :disabled="Boolean(busy) || previewBusy || accountSaving" @click="openPreview(host)">{{ rowCopy.preview }} <Icon name="chevron" :size="14" /></button>
              <UiActionMenu v-if="hostActions(host).length" :items="hostActions(host)" :label="rowCopy.more + serviceFor(host).name" :disabled="Boolean(busy) || previewBusy || accountSaving" @select="hostAction($event, host)" />
            </div>
          </td>
          <td><span class="pill neutral">{{ providerName(host.provider) }}</span></td>
        </tr>
      </tbody></table>
    </div>
  </section>

  <UiOverlay ref="accountOverlay" :open="accountsOpen" :title="accountOpen ? (editingAccount ? '编辑边缘账号' : '添加边缘账号') : '边缘账号'" description="账号凭据仅保存在设备本地；管理账号不会自动发布域名，DNS 账号另行管理。" variant="drawer" size="wide" :busy="accountSaving || Boolean(busy)" :dirty="accountOpen && accountDirty" @close="closeAccounts">
    <div v-if="accountsError && !accountOpen" class="alert error" role="alert">{{ accountsError }}</div>
    <div v-if="accountError" class="alert error" role="alert">{{ accountError }}</div>
    <template v-if="!accountOpen">
      <div class="cloud-account-actions"><button class="button primary small" :disabled="Boolean(busy)" @click="newAccount"><Icon name="plus" :size="15" />添加账号</button><button class="button secondary small" :disabled="accountsLoading || Boolean(busy)" @click="loadAccounts">{{ accountsLoading ? '读取中…' : '重新读取账号' }}</button></div>
      <div v-if="!accounts.length" class="compact-empty"><Icon name="cloud" :size="28" /><h3>还没有边缘账号</h3><p>添加 EdgeOne 或 Cloudflare 账号后，即可配置加速服务。</p></div>
      <div v-else class="table-wrap"><table class="data-table"><thead><tr><th>账号</th><th>凭据</th><th>接入配置</th><th>状态</th><th>操作</th></tr></thead><tbody>
        <tr v-for="account in accounts" :key="account.key || account.provider + ':' + account.id"><td><strong>{{ account.name }}</strong><span class="secondary">{{ providerName(account.provider) }}</span></td><td><span class="mono">{{ account.tokenHint || account.secretIdHint || '已保存' }}</span><small class="secondary">第 {{ account.credentialVersion }} 版凭据</small></td><td><template v-if="account.provider === 'cloudflare'"><small class="secondary mono">{{ account.zoneName || account.zoneId }}</small><UiCopyValue :value="account.standardTarget" label="复制标准入口" /><small class="secondary mono">Fallback {{ account.fallbackDomain }}</small></template><template v-else>{{ modeText(account.originMode) }}</template></td><td><span :class="['pill', account.enabled ? 'private' : 'neutral']">{{ account.enabled ? '已启用' : '已停用' }}</span></td><td><button class="text-button" :disabled="Boolean(busy)" :aria-label="`编辑账号：${account.name}`" @click="editAccount(account)">编辑</button> <button class="text-button danger" :disabled="Boolean(busy)" :aria-label="`删除账号：${account.name}`" @click="askDelete(account)">删除</button></td></tr>
      </tbody></table></div>
      <details v-if="origins.length" class="raw-details"><summary>源站组 · {{ origins.length }} 个</summary><div class="table-wrap"><table class="data-table"><thead><tr><th>站点 / 源站组</th><th>账号</th><th>本机记录的回源值</th></tr></thead><tbody><tr v-for="origin in origins" :key="origin.accountId + ':' + origin.zoneId + ':' + origin.groupId"><td><UiCopyValue :value="origin.zoneName || origin.zoneId" label="复制站点" /><small class="secondary mono">{{ origin.groupId }}</small></td><td>{{ accountName(origin.accountId) }}</td><td><UiCopyValue v-if="origin.value" :value="origin.value" label="复制回源值" /><span v-else>—</span></td></tr></tbody></table></div></details>
    </template>
    <form v-else id="edge-account-form" class="form-body" @submit.prevent="saveAccount">
      <div class="form-field"><label class="form-label" for="edge-provider">边缘服务商</label><UiSelect v-if="!editingAccount" id="edge-provider" v-model="accountForm.provider" :options="providers" :disabled="accountSaving || Boolean(busy)" aria-label="边缘服务商" /><strong v-else>{{ providerName(editingAccount.provider) }}</strong><span v-if="editingAccount" class="field-caption">已保存账号的服务商不可更改；请新增账号接入其他服务商。</span></div>
      <div class="form-field"><label class="form-label" for="edge-account-name">备注名称{{ accountAuto ? '（可省略）' : '' }}</label><input ref="accountNameInput" id="edge-account-name" v-model="accountForm.name" :disabled="accountSaving || Boolean(busy)" maxlength="100" :required="!accountAuto" :placeholder="accountAuto ? '读取账号后按接入域名自动命名' : ''" autocomplete="off" /></div>
      <template v-if="accountForm.provider === 'edgeone'">
        <div class="form-field"><label class="form-label" for="edge-account-secret-id">SecretId{{ editingAccount ? '（轮换或修复时填写）' : '' }}</label><input id="edge-account-secret-id" v-model="accountForm.secretId" class="mono" :disabled="accountSaving || Boolean(busy)" :required="!editingAccount" autocomplete="off" spellcheck="false" /></div><div class="form-field"><label class="form-label" for="edge-account-secret-key">SecretKey</label><input id="edge-account-secret-key" v-model="accountForm.secretKey" type="password" class="mono" :disabled="accountSaving || Boolean(busy)" :required="!editingAccount" autocomplete="new-password" /><span class="field-caption">编辑时两项都留空保留旧凭据；轮换或修复需完整填写，验证原站点权限后保留账号及资源归属。</span></div>
        <div class="form-field"><span class="form-label">回源方式</span><UiSegmented v-model="accountForm.originMode" :options="modeOptions" :disabled="accountSaving || Boolean(busy)" label="回源方式" /><span v-if="accountForm.originMode === 'address'" class="field-caption">地址变化时，本机必须在线才能更新源站组。</span><span v-else-if="!baseDomain" class="field-caption error-text">请先到概览页设置基站域名。</span><span v-else class="field-caption">由 {{ providerName(accountForm.provider) }} 解析 {{ baseDomain }} 回源。</span></div>
      </template>
      <template v-else>
        <div class="form-field"><label class="form-label" for="edge-cf-token">API Token{{ editingAccount ? '（留空保留，填写则轮换）' : '' }}</label><input id="edge-cf-token" v-model="accountForm.apiToken" :disabled="accountSaving || Boolean(busy)" :required="!editingAccount" type="password" autocomplete="new-password" /><span class="field-caption">需区域读取、区域设置（Zone Settings）读取、DNS 编辑及 SSL / 自定义主机名管理权限；自动设置完整（严格）模式还需 Zone Settings 编辑权限。「DNS 设置（DNS Settings）」不能代替「区域设置」。不是 Global API Key，也不用提供登录密码；建议只授权需要的域名。</span></div>
        <template v-if="accountAuto">
          <div class="cf-discovery-actions"><button type="button" class="button secondary" :disabled="accountSaving || Boolean(busy) || accountDiscoveryBusy || !accountForm.apiToken.trim()" @click="discoverCloudflareAccount"><Icon name="refresh" :size="15" />{{ accountDiscoveryBusy ? '正在读取并检查…' : accountDiscovery ? '重新读取并配置' : '读取并自动配置' }}</button><span class="field-caption">只读取云端；自动选择可用接入域名，并生成专用回源与标准入口。</span></div>
          <div v-if="accountDiscoveryError" class="alert error" role="alert">{{ accountDiscoveryError }}</div>
          <div v-if="accountDiscoveryBusy" class="field-caption" role="status">正在逐个核对区域与接入条件；一个区域不可用，不影响其他区域。</div>
          <template v-if="accountDiscovery">
            <div v-if="accountZones.some(zone => zone.eligible)" class="form-field"><label class="form-label" for="edge-cf-discovered-zone">接入域名（已自动选择，可更换）</label><UiSelect id="edge-cf-discovered-zone" v-model="accountZoneId" :options="accountZoneOptions" :disabled="accountSaving || Boolean(busy)" aria-label="Cloudflare 接入域名" /></div>
            <div v-if="accountSelectedZone" class="cf-discovery-plan" role="status">
              <strong>自动配置方案</strong>
              <dl class="definition">
                <div><dt>接入域名</dt><dd class="mono">{{ accountSelectedZone.zoneName }}</dd></div>
                <template v-if="accountSelectedZone.existingFallback">
                  <div><dt>现有共享 Fallback</dt><dd class="mono">{{ accountSelectedZone.existingFallback }}</dd></div>
                  <div><dt>现有实际回源值</dt><dd class="mono">{{ accountSelectedZone.existingFallbackTarget || '未取得，发布前需核对' }}</dd></div>
                  <div v-if="accountSelectedZone.fallbackStatus"><dt>云端 Fallback 状态</dt><dd>{{ accountSelectedZone.fallbackStatus }}</dd></div>
                </template>
                <div><dt>{{ accountSelectedZone.fallbackMode === 'reuse' ? '复用回源' : accountSelectedZone.fallbackMode === 'replace' ? '新专用 Fallback' : '专用回源' }}</dt><dd class="mono">{{ accountSelectedZone.fallbackDomain }}</dd></div>
                <div v-if="accountSelectedZone.desiredBaseDomain || baseDomain"><dt>本机基站</dt><dd class="mono">{{ accountSelectedZone.desiredBaseDomain || baseDomain }}</dd></div>
                <div><dt>标准入口</dt><dd class="mono">{{ accountSelectedZone.standardTarget }}</dd></div>
              </dl>
              <div v-if="accountSelectedZone.fallbackMode === 'replace'" class="alert warning">现有共享 Fallback 不符合本机回源要求，已生成替换方案，不代表 API 不可用。替换会影响 {{ accountSelectedZone.zoneName }} 区域内所有使用默认回源的自定义主机名。保存账号不会修改云端；请先到边缘接入「预览接入」，核对旧值 → 新值并单独确认后才能替换，服务自动发布不会代为授权。</div>
              <div v-else-if="accountSelectedZone.fallbackMode === 'reuse'" class="alert info">现有 Fallback 已指向本机基站，将直接复用，不修改原回源。</div>
              <span v-if="accountSelectedZone.message" class="field-caption">{{ accountSelectedZone.message }}</span>
              <span class="field-caption">保存账号只保存本地方案；之后从服务页发布，系统自动配置接入、验证与业务 DNS。</span>
            </div>
            <div v-else class="alert warning" role="alert">{{ accountZones.length ? '当前没有符合条件的接入域名，请按下方原因处理后重新读取。' : '此凭据下没有可用区域。CF 账号必须先接入一个自有域名；已有域名时，请检查 Token 是否授权了该区域。' }}</div>
            <div v-for="zone in accountZones.filter(zone => !zone.eligible)" :key="zone.zoneId" class="alert warning"><span>{{ zone.zoneName }} 不可用：{{ zone.message || '接入条件未满足，请在 Cloudflare 控制台核对。' }}</span></div>
            <div v-for="(warning, index) in accountDiscovery.warnings || []" :key="index" class="alert warning">{{ warning }}</div>
          </template>
          <span class="field-caption">CF 账号需先有已接入的自有域名。本程序不会购买域名、迁移 NS、开通付费服务；全区 SSL 模式仅在下方明确授权后由发布任务修改。</span>
        </template>
        <button v-if="!editingAccount" type="button" class="text-button cf-manual-toggle" :aria-expanded="accountManual" aria-controls="edge-cf-manual-fields" :disabled="accountSaving || Boolean(busy)" @click="accountManual = !accountManual">{{ accountManual ? '收起高级配置，返回自动配置' : '高级手动配置' }} <Icon name="chevron" :size="14" /></button>
        <div v-if="editingAccount || accountManual" id="edge-cf-manual-fields" class="cf-manual-fields">
          <div class="form-field"><label class="form-label" for="edge-cf-zone">SaaS 接入区 Zone ID</label><input id="edge-cf-zone" v-model="accountForm.zoneId" class="mono" :disabled="accountSaving || Boolean(busy) || Boolean(editingAccount)" required /><span class="field-caption">接入区需已启用 Cloudflare for SaaS，使用 Full (strict)；可在下方授权系统发布时自动设置。</span></div>
          <div class="form-field"><label class="form-label" for="edge-cf-fallback">Fallback 域名</label><input id="edge-cf-fallback" v-model="accountForm.fallbackDomain" placeholder="fallback.edge-example.net" class="mono" :disabled="accountSaving || Boolean(busy) || Boolean(editingAccount)" required /><span class="field-caption">接入区内的专用子域，指向概览页配置的基站域名。请勿填写第三方优选域名。</span></div>
          <div class="form-field"><label class="form-label" for="edge-cf-standard">标准接入目标</label><input id="edge-cf-standard" v-model="accountForm.standardTarget" placeholder="customers.edge-example.net" class="mono" :disabled="accountSaving || Boolean(busy) || Boolean(editingAccount)" required /><span class="field-caption">关闭优选并重新发布可恢复此目标，仍通过 Cloudflare，不回退 IPv6 直连。</span></div>
        </div>
        <div class="cf-ssl-consent">
          <p v-if="accountSelectedZone?.requiresSSLStrict" class="field-caption">当前 SSL 模式为 {{ accountSelectedZone.sslMode }}，需要完整（严格）模式。</p>
          <UiCheckbox v-model="accountForm.autoSSLStrict" :disabled="accountSaving || Boolean(busy)">授权发布时自动设置接入区为 Full (strict)（完整、严格）</UiCheckbox>
          <p class="field-caption">{{ accountSelectedZone?.sslStrictImpact || '此授权影响整个接入区及其中已有代理站点，不仅是当前服务；源站证书不合规的站点可能无法访问。' }} 保存账号不会修改云端，发布时才会执行并核对。</p>
        </div>
      </template>
      <div v-if="editingAccount || accountForm.provider === 'cloudflare'" class="switch-field"><span>启用账号</span><UiSwitch v-model="accountForm.enabled" :disabled="accountSaving || Boolean(busy)" aria-label="启用边缘账号" /></div>
    </form>
    <template #footer><button class="button secondary" :disabled="accountSaving || Boolean(busy)" @click="accountOverlay?.requestClose()">{{ accountOpen ? '取消' : '关闭' }}</button><button v-if="accountOpen" type="submit" form="edge-account-form" class="button primary" :disabled="accountSaving || Boolean(busy) || !accountDirty || !accountValid">{{ accountSaving ? '保存中…' : '保存账号' }}</button></template>
  </UiOverlay>

  <UiOverlay :open="Boolean(httpValidationHost)" title="尝试 Cloudflare 自动 HTTP 验证" :busy="httpValidationBusy || Boolean(busy)" @close="closeHTTPValidation">
    <p>提前创建以下业务 CNAME，让请求到达 Cloudflare，尝试自动 HTTP 验证。只创建不存在的记录，不覆盖已有 DNS。</p>
    <dl class="definition"><div><dt>业务域名</dt><dd class="mono">{{ httpValidationHost?.host }}</dd></div><div><dt>CNAME → 标准入口</dt><dd class="mono">{{ httpValidationHost?.standardTarget }}</dd></div></dl>
    <div class="alert warning">证书尚未就绪，HTTPS 暂不可用；创建 DNS 不代表证书签发或发布成功。现有发布任务将继续等待并核对证书。</div>
    <p class="field-caption">保留 Full (strict)、回源、现有自定义主机名及全部验证 DNS；不改 SSL 验证方式，也不切换第三方优选入口。</p>
    <div v-if="httpValidationError" class="alert error" role="alert">{{ httpValidationError }}</div>
    <div v-if="httpValidationUncertain[httpValidationHost?.host]" class="alert warning" role="status">操作结果待确认，已停止重复提交。请先读取云端状态并查看发布进度；请求超时不代表写入已撤销。<button class="text-button" :disabled="hostsLoading || httpValidationBusy || Boolean(busy)" @click="refreshHTTPValidation">读取云端状态与发布进度</button></div>
    <UiCheckbox v-model="httpValidationConfirmed" :disabled="httpValidationBusy || Boolean(busy) || httpValidationUncertain[httpValidationHost?.host]">我已核对业务域名和标准入口，确认提前创建 CNAME，并接受证书就绪前 HTTPS 暂不可用。</UiCheckbox>
    <template #footer><button class="button secondary" :disabled="httpValidationBusy || Boolean(busy)" @click="closeHTTPValidation">{{ httpValidationUncertain[httpValidationHost?.host] ? '关闭' : '取消' }}</button><button class="button primary" :disabled="!httpValidationConfirmed || httpValidationBusy || Boolean(busy) || httpValidationUncertain[httpValidationHost?.host]" @click="startHTTPValidation">{{ httpValidationBusy ? '提交中…' : '确认创建 CNAME 并验证' }}</button></template>
  </UiOverlay>
  <UiOverlay :open="Boolean(restoreHost)" title="恢复标准入口" :busy="accountSaving || Boolean(busy)" @close="restoreHost = null"><p>将 {{ restoreHost?.host }} 所属服务切回标准入口的选择保存为草稿。下一步必须到服务页核对影响范围并发布，当前 DNS 不会立即改变；恢复受 DNS 缓存影响。</p><div v-if="restoreError" class="alert error">{{ restoreError }}</div><UiCheckbox v-model="restoreConfirmed">确认保存标准入口草稿，并在服务页继续预览发布</UiCheckbox><template #footer><button class="button secondary" :disabled="accountSaving" @click="restoreHost = null">取消</button><button class="button primary" :disabled="!restoreConfirmed || accountSaving || Boolean(busy)" @click="prepareStandardRestore">保存恢复草稿</button></template></UiOverlay>
  <UiOverlay :open="Boolean(deleting)" title="删除边缘账号" :busy="accountSaving || Boolean(busy)" @close="deleting = null">
    <p>确认删除 {{ providerName(deleting?.provider) }} 账号「{{ deleting?.name }}」及本地凭据？不会删除云端资源，有服务或托管资源使用时将拒绝删除。</p><div v-if="deleteError" class="alert error" role="alert">{{ deleteError }}</div>
    <template #footer><button class="button secondary" :disabled="accountSaving || Boolean(busy)" @click="deleting = null">取消</button><button class="button danger" :disabled="accountSaving || Boolean(busy)" @click="removeAccount">{{ accountSaving ? '删除中…' : '删除账号' }}</button></template>
  </UiOverlay>
  <UiOverlay :open="previewOpen" :title="`${providerName(previewProvider)} 接入预览`" :description="previewHost" size="wide" :busy="previewBusy || Boolean(busy)" @close="closePreview">
    <div v-if="previewError" class="alert error" role="alert">{{ previewError }}</div><div v-if="applyMessage" class="alert info" role="status">{{ applyMessage }}</div><div v-if="previewBusy" class="compact-empty" role="status"><span class="spinner"></span><p>正在核对 {{ providerName(previewProvider) }}…</p></div>
    <template v-else-if="preview">
      <div v-if="previewProvider === 'cloudflare'" class="alert info"><span>{{ preview.status === 'pending' ? 'Cloudflare 尚未就绪。' : '' }}此处是高级单次接入，不会后台续跑。普通接入请使用服务自动发布，系统会负责验证、等待与解析切换。</span><button v-if="serviceFor({ host: previewHost, provider: previewProvider }).id" class="text-button" :disabled="Boolean(busy)" @click="publishService({ host: previewHost, provider: previewProvider })">服务自动发布</button></div>
      <div v-else class="preview-revisions"><span class="pill neutral">站点 {{ preview.zoneName || preview.zoneId }}</span><span class="pill neutral">{{ modeText(preview.originMode) }}</span><UiCopyValue v-if="preview.origin" :value="preview.origin" label="复制预览回源值" /><span v-if="preview.domainState" class="pill neutral">{{ preview.domainState }}</span></div>
      <details :open="previewProvider !== 'cloudflare' || previewReplacesFallback" class="raw-details">
      <summary>接入技术详情</summary>
      <dl v-if="previewProvider === 'cloudflare'" class="definition"><div><dt>标准入口</dt><dd class="mono">{{ preview.standardTarget }}</dd></div><div><dt>发布目标</dt><dd class="mono">{{ preview.target }}</dd></div><div><dt>云端状态</dt><dd>{{ preview.status }}<template v-if="preview.message"> · {{ preview.message }}</template></dd></div></dl>
      <div v-if="previewSteps.length" class="table-wrap"><table class="data-table"><thead><tr><th>步骤</th><th>对象</th><th>说明</th></tr></thead><tbody><tr v-for="(step, index) in previewSteps" :key="index"><td>{{ stepLabels[previewProvider]?.[step.action] || step.action }}</td><td><UiCopyValue :value="step.target" label="复制操作对象" /></td><td>{{ step.detail }}<small v-if="step.from || step.to" class="secondary mono">{{ step.from || '（空）' }} → {{ step.to }}</small></td></tr></tbody></table></div>
      <div v-for="(warning, index) in preview.warnings" :key="index" class="alert warning">{{ warning }}</div>
      <div v-if="validationRecords.length" class="table-wrap"><h3>域名验证记录</h3><p class="field-caption">由系统写入 DNS，无需手工复制。</p><table class="data-table"><thead><tr><th>类型</th><th>名称</th><th>值</th></tr></thead><tbody><tr v-for="record in validationRecords" :key="record.name + record.value"><td>{{ record.type }}</td><td><UiCopyValue :value="record.name" label="复制验证名称" /></td><td><UiCopyValue :value="record.value" label="复制验证值" /></td></tr></tbody></table></div>
      <div v-if="previewNeedsDNS" class="field-caption">业务 CNAME 目标：<UiCopyValue :value="previewTarget" label="复制待发布 CNAME" />；统一发布将在接入就绪后自动切换。</div>
      </details>
      <div v-if="previewReplacesFallback" class="alert warning cf-fallback-confirmation">
        <strong>共享回源替换会影响整个接入区</strong>
        <p>以上旧值 → 新值会影响此 Cloudflare 区域内所有使用默认回源的自定义主机名，不仅是当前业务域名。请先确认其他业务可以一起切换；不会改写旧 Fallback 的 DNS 记录。</p>
        <UiCheckbox v-model="confirmedFallbackReplacement" :disabled="Boolean(busy) || previewBusy">我已核对旧值与新值，确认替换共享 Fallback，并接受该区域所有使用默认回源的自定义主机名受到影响。</UiCheckbox>
      </div>
      <UiCheckbox v-if="preview.steps?.length || preview.requiresReconciliation" v-model="confirmed" :disabled="Boolean(busy) || previewBusy">{{ preview.steps?.length ? `确认以上 ${providerName(previewProvider)} 资源、证书及域名验证变更。` : '确认重新核对远端就绪状态并登记短期发布资格；不会自动切换业务 DNS。' }}</UiCheckbox>
    </template>
    <template #footer><button class="button secondary" :disabled="previewBusy || Boolean(busy)" @click="closePreview">关闭</button><button v-if="preview?.steps?.length || preview?.requiresReconciliation" class="button primary" :disabled="!confirmed || (previewReplacesFallback && !confirmedFallbackReplacement) || previewBusy || Boolean(busy)" @click="applyPreview">{{ previewBusy ? '提交中…' : preview?.steps?.length ? '提交变更' : '核对并继续' }}</button></template>
  </UiOverlay>
</template>

<style scoped>
.cloud-toolbar{display:flex;justify-content:space-between;align-items:center;gap:16px;margin-bottom:12px;flex-wrap:wrap}
.cloud-context{display:flex;align-items:center;gap:12px;flex-wrap:wrap;min-width:0}.cloud-context strong{font-size:14px;overflow-wrap:anywhere}
.cloud-account-actions{display:flex;gap:10px;justify-content:flex-end;margin-bottom:18px;flex-wrap:wrap}
.cloud-workspace{margin-top:0}.data-table td{overflow-wrap:anywhere;max-width:320px}
.edge-host-table .name-cell{min-width:140px}.edge-host-table .name-cell strong{white-space:normal}.dns-record-target{display:flex;align-items:center;gap:6px;flex-wrap:wrap;margin-top:6px}.edge-actions .text-button{display:flex;white-space:nowrap}
.edge-host-table th:last-child{text-align:left}
.edge-row-actions{display:flex;align-items:center;gap:8px;white-space:nowrap}.edge-row-actions .text-button{min-height:32px}
.edge-status-note{min-width:10em;max-width:26em;white-space:normal;overflow-wrap:anywhere}.definition{margin-bottom:18px}
.cf-discovery-actions{display:flex;align-items:center;gap:12px;flex-wrap:wrap}.cf-discovery-plan{border:1px solid var(--line);border-radius:10px;padding:16px;min-width:0}.cf-discovery-plan .definition{margin:12px 0}.cf-discovery-plan dd{overflow-wrap:anywhere}.cf-manual-toggle{align-self:flex-start}.cf-manual-fields{display:grid;gap:18px}
.cf-fallback-confirmation{display:grid;gap:12px}.cf-fallback-confirmation p{margin:0}
@media(max-width:640px){.cloud-toolbar{align-items:flex-start}.cloud-context{gap:8px}}
</style>
