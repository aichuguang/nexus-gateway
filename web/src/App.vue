<script setup>
import { formatDateTime } from './utils/datetime'
import { computed, nextTick, onMounted, provide, reactive, ref, watch } from 'vue'
import Icon from './Icon.vue'
import LoginPage from './components/LoginPage.vue'
import BrandLogo from './components/BrandLogo.vue'
import { updateDocumentBranding } from './branding'
import Services from './Services.vue'
import Resolve from './Resolve.vue'
import EdgeAcceleration from './EdgeAcceleration.vue'
import Certificates from './Certificates.vue'
import Diagnostics from './Diagnostics.vue'
import Traffic from './Traffic.vue'
import System from './System.vue'
import PublishTopology from './components/PublishTopology.vue'
import PublishProgress from './components/PublishProgress.vue'
import { usePublishJobs, publishStatus } from './usePublishJobs.js'
import OverviewMetrics from './components/OverviewMetrics.vue'
import ServiceAvatar from './components/ServiceAvatar.vue'
import { serviceKind } from './service-icons.js'
import { onSessionLost, invalidateSessionRequests } from './api.js'
import { usePageScope, isReadBusy } from './page-scope.ts'
import { isRequestCancelled } from './request.ts'
import { appVersion } from './build-info.ts'
import { serviceEntry, upstreamPortOf } from './service-entry.js'
import { certificateStatus, isCertificateUsable } from './certificate-status.js'
import UiSelect from './components/ui/UiSelect.vue'
import UiCheckbox from './components/ui/UiCheckbox.vue'
import UiCopyValue from './components/ui/UiCopyValue.vue'
import UiTooltip from './components/ui/UiTooltip.vue'
import UiToast from './components/ui/UiToast.vue'

const scope = usePageScope()
const api = scope.api
const networkCopy = {
  noPublicAddress: '还没有可用的公网地址；域名无法解析到本机，只能通过已配置的 VPN 内网地址访问。',
  management: 'VPN 内网管理',
  addService: '添加一个网关可达的 VPN 内网上游，开始发布。',
}
let stateLoadSequence = 0
let previewLoadSequence = 0

// Each page describes something the gateway owns: the services it relays,
// the names that point at it, the accelerator that may answer for those names
// instead, the certificates that identify it, and the evidence that any of it
// works.
const tabs = [
  { id: 'overview', name: '网关概览', icon: 'grid', caption: '当前配置、待办与本机入口' },
  { id: 'services', name: '服务管理', icon: 'server', caption: '一条上游地址，一种发布方式' },
  { id: 'resolve', name: '域名解析', icon: 'globe', caption: '公网地址、DNS 账号与域名解析' },
  { id: 'edgeone', name: '边缘加速', icon: 'bolt', caption: '加速域名、回源与证书下发' },
  { id: 'certificates', name: '证书管理', icon: 'shield', caption: '证书槽、材料与到期状态' },
  { id: 'diagnostics', name: '监测与诊断', icon: 'pulse', caption: '连通性监测、入口检查与本机信息' },
  { id: 'traffic', name: '流量统计', icon: 'route', caption: 'HTTP 代理的今日用量、服务分布与历史记录' },
  { id: 'system', name: '系统管理', icon: 'wrench', caption: '配置资料、通知渠道与登录安全' },
]

const profile = ref({ projectName: '澄光网关', clientName: '本地网关', logo: '' })
provide('gatewayProfile', profile)
let profileSequence = 0
function adoptProfile(value) {
  profileSequence++
  profile.value = { projectName: value.projectName, clientName: value.clientName, logo: value.logo || '' }
}
async function loadProfile() {
  const sequence = ++profileSequence
  try {
    if (typeof api.profile !== 'function') return
    const value = await api.profile()
    if (sequence === profileSequence) adoptProfile(value)
  } catch { /* Public branding is optional; authentication remains available. */ }
}
watch(profile, updateDocumentBranding, { immediate: true })
const auth = ref('checking')
const password = ref('')
const loginBusy = ref(false)
const loginError = ref('')
const activeTab = ref('overview')
const state = ref(null)
const loading = ref(false)
const error = ref('')
const toast = ref(null)
const busy = ref('')
const preflight = ref(null)
const previewOpen = ref(false)
const preview = ref(null)
const previewLoading = ref(false)
const applyError = ref('')
const confirmPublic = ref(false)
const servicesTab = ref(null)
const certificatesTab = ref(null)
const diagnosticsTab = ref(null)
const resolveTab = ref(null)
const edgeTab = ref(null)
const mobileNavOpen = ref(false)
const theme = ref('light')
const baseEditing = ref(false)
watch(error, message => {
  if (message) { flash({ type: 'error', message }); error.value = '' }
})
watch(auth, value => { if (value !== 'ready') toast.value?.clear() })
const returnToPublish = ref('')
const publishCopy = { accessSeparate: '不代表网站可访问或 HTTPS 正常；DNS 缓存可能尚未刷新，访问检查独立进行。' }
const publishTasks = reactive(usePublishJobs(computed(() => auth.value === 'ready'), async job => {
  const generation = scope.generation()
  await loadState()
  if (auth.value !== 'ready' || !scope.isCurrent(generation)) return
  const message = job.status === 'succeeded' && job.edgeProvider === 'cloudflare' && job.dns
    ? `${publishStatus(job)}；${publishCopy.accessSeparate}` : job.message || `${job.serviceName} · ${publishStatus(job)}`
  if (job.status === 'succeeded') flash(message)
  else error.value = message
}))
const writeBusy = computed(() => busy.value || (publishTasks.locked ? 'publish-task' : ''))
const navigationBusy = computed(() => Boolean(busy.value && !isReadBusy(busy.value)))

async function startPublish(input) {
  const job = await publishTasks.create(input)
  if (job) {
    servicesTab.value?.closePublish()
  }
}

function configureForPublish(target) {
  returnToPublish.value = target.serviceId
  servicesTab.value?.closePublish()
  goTab(target.tab)
}

async function reopenPublish(serviceId) {
  if (writeBusy.value) return
  publishTasks.close()
  returnToPublish.value = ''
  await loadState()
  const service = state.value?.services.find(item => item.id === serviceId)
  if (!service) { error.value = '这个服务已不存在，发布记录仍然保留。'; return }
  goTab('services')
  await nextTick()
  servicesTab.value?.openPublish(service)
}

function inspectPublishTarget(tab) {
  publishTasks.close()
  goTab(tab)
}


const currentTab = computed(() => tabs.find(item => item.id === activeTab.value))
const services = computed(() => state.value?.services || [])
const certificates = computed(() => state.value?.certificates || [])
const settings = computed(() => state.value?.settings || {})
const publicAddress = computed(() => state.value?.appliedPublicAddress || '')
const usableCertificates = computed(() => certificates.value.filter(isUsable))
// A service publish lives on the service's own row, because that is where the
// change was made. Two kinds of change have no row to stand on: a deleted
// service, and the gateway address. Those get the banner, and only those --
// a banner that also appeared for a row the operator can already see would be
// a second, competing place to do the same thing.
const orphanChanges = computed(() => Boolean(state.value?.unappliedElsewhere))
const pendingServices = computed(() => state.value?.unappliedServiceIds || [])

// The same question the tags and the banner answer, so the three cannot disagree.
// `revision` alone is not it: saving a service and deleting it again moves the
// revision while leaving the draft identical to what is running, and a header
// announcing work that no row and no banner can show the operator is worse than
// saying nothing.
const anythingPending = computed(() => pendingServices.value.length > 0 || orphanChanges.value)
const enabledServices = computed(() => services.value.filter(service => service.enabled))
const namedServices = computed(() => enabledServices.value.filter(service => service.domains?.length))
const domains = computed(() => namedServices.value.flatMap(service => service.domains))
// The names an accelerator answers for, which are exactly the names whose DNS
// record must NOT follow this device's address. The two sets are disjoint, and
// keeping both visible is what makes a mistake in either one noticeable.
const edgeDomains = computed(() => namedServices.value.filter(service => service.entryMode === 'edge').flatMap(service => service.domains))
const edgeOneDomains = computed(() => namedServices.value.filter(service => service.entryMode === 'edge' && service.edgeProvider !== 'cloudflare').flatMap(service => service.domains))
const edgeAccounts = computed(() => (state.value?.edgeOneAccounts || []).filter(account => account.enabled))
// The accounts whose origin group fetches from the base station. An empty
// originMode reads as the address mode, the same way the server reads it, so this
// must compare against 'base' rather than test for "not address".
const baseOriginAccounts = computed(() => edgeAccounts.value.filter(account => account.originMode === 'base'))

// The base station is a one-time decision everything else leans on: every origin
// group in base mode fetches from this name, and so does anything the operator
// later points at it. It therefore lives on the overview rather than inside the
// DNS table, and its state here is "configured AND published" -- filling in the
// name is half of it, and the half that leaves the other half looking done.
const baseRecord = ref(null)
const baseRecordsError = ref('')
const baseRecordsLoading = ref(true)
let baseLoadSequence = 0
const baseForm = reactive({ domain: '', ttl: 600 })
const baseError = ref('')
// The base station is the one record whose TTL is the operator's to choose, and
// the shortest TTL a zone accepts is a property of that zone's plan which no API
// states. 600 is listed first because it is the only value every plan accepts;
// the other two are worth trying and a zone that refuses one says so.
const baseTtls = [
  { value: 600, label: '600 秒（推荐）' },
  { value: 300, label: '300 秒（需要套餐支持）' },
  { value: 60, label: '60 秒（需要套餐支持）' },
]
const baseDirty = computed(() =>
  baseForm.domain.trim().toLowerCase() !== (settings.value.baseDomain || '') ||
  Number(baseForm.ttl) !== Number(settings.value.baseTtl || 600))
const baseState = computed(() => {
  if (!settings.value.baseDomain) return { publicationState: 'unconfigured', label: '未设置', tone: 'neutral', note: '' }
  if (baseRecordsLoading.value) return { publicationState: 'loading', label: '读取中', tone: 'neutral', note: '' }
  if (baseRecordsError.value) return { publicationState: 'unknown', label: '状态未知', tone: 'neutral', note: baseRecordsError.value }
  const record = baseRecord.value
  if (!record || record.host !== settings.value.baseDomain) return { publicationState: 'unknown', label: '状态未知', tone: 'neutral', note: '未读取到当前基站的发布状态，请重新读取；这不代表云端解析不存在。' }
  // Publication is local evidence, independent of translated labels and cloud observation.
  const publicationState = record.managed?.pending ? 'unknown' : record.publicationState ||
    (['已发布', '上次发布'].includes(record.status) ? 'published' : record.status === '未发布' ? 'draft' : 'unknown')
  if (publicationState === 'published') {
    return { publicationState, label: '已发布', tone: 'private', note: '已保存上次发布记录；云端实时状态可在解析页单独核对。' }
  }
  if (publicationState === 'draft') return { publicationState, label: '尚无发布记录', tone: 'amber', note: '本机尚无发布记录；如已在云端配置，请先预览核对，无需重复创建。' }
  return { publicationState: 'unknown', label: '待核对', tone: 'neutral', note: record.managed?.pending ? '上次写入结果尚未确认，请到解析页只读核对，不要重复发布。' : '暂时无法确认本地发布状态，请到解析页核对。' }
})

watch(settings, value => {
  baseForm.domain = value.baseDomain || ''
  baseForm.ttl = value.baseTtl || 600
}, { immediate: true, deep: true })

// The entry an operator types into a browser. A named service is reached by its
// name on the shared 443; a port service is reached on the gateway's own address
// at its number, which is the upstream's own port unless it was moved.
function entryOf(service) {
  return serviceEntry(service, publicAddress.value || settings.value.gatewayIp)
}

function isUsable(cert) {
  return isCertificateUsable(cert)
}

function covers(cert, host) {
  const name = String(host || '').toLowerCase()
  return (cert.domains || []).some(pattern => {
    const rule = String(pattern).toLowerCase()
    return rule === name || (rule.startsWith('*.') && name.endsWith(rule.slice(1)) && name.split('.').length === rule.split('.').length)
  })
}

function certificateFor(service) {
  const cert = certificates.value.find(item => item.id === service.certificateId)
  if (!cert) return { ok: false, reason: '还没有选择证书' }
  const status = certificateStatus(cert)
  if (!status.usable) return { ok: false, reason: `证书${status.label}`, unknown: status.tone === 'neutral' }
  for (const domain of service.domains || []) {
    if (!covers(cert, domain)) return { ok: false, reason: `证书不覆盖 ${domain}` }
  }
  return { ok: true, cert }
}

// The overview's reason to exist: the gap between "configured" and "reachable".
const todos = computed(() => {
  if (!state.value) return []
  const list = []
  if (anythingPending.value) {
    const single = pendingServices.value.length === 1 && !orphanChanges.value
    list.push({
      tone: 'info',
      text: `配置草稿 v${state.value.revision} 尚未应用，线上仍是 ${state.value.appliedRevision ? `v${state.value.appliedRevision}` : '尚未应用过任何配置'}。`,
      label: single ? '去发布' : '应用全部改动',
      tab: single ? 'services' : null,
      apply: !single,
    })
  }
  // A base-mode origin group fetches from a name this gateway is the one that
  // publishes. Both halves of that arrangement are checked together, because
  // neither half works alone: a name with no record behind it is no better than
  // no name at all, and both leave EdgeOne answering for a site it cannot fetch
  // from -- a failure with no symptom on any page that only reads local state.
  if (baseOriginAccounts.value.length) {
    const accounts = baseOriginAccounts.value.map(account => `「${account.name}」`).join('、')
    if (!settings.value.baseDomain) {
      list.push({ text: `EdgeOne 账号 ${accounts} 正在以基站域名回源，但基站域名还没有设置；这些站点会回源到一个不存在的名字。` })
    } else if (baseState.value.publicationState === 'unknown') {
      list.push({ tone: 'neutral', text: baseRecordsError.value
        ? `暂时无法核对基站域名 ${settings.value.baseDomain} 的解析状态；读取失败不代表解析不存在。`
        : `基站域名 ${settings.value.baseDomain} 的发布状态待核对；状态未知不代表解析不存在，请先核对，不要重复发布。`, label: '去核对', tab: 'resolve' })
    } else if (baseState.value.publicationState === 'draft') {
      list.push({ text: `基站域名 ${settings.value.baseDomain} 尚无本机发布记录，EdgeOne 账号 ${accounts} 使用它回源；请先预览核对现有解析。`, label: '去预览', tab: 'resolve' })
    }
  }
  for (const service of namedServices.value) {
    const verdict = certificateFor(service)
    if (!verdict.ok) list.push({ tone: verdict.unknown ? 'neutral' : undefined, text: `服务「${service.name}」的域名 ${service.domains.join('、')}：${verdict.reason}，${verdict.unknown ? '发布前请核对材料' : '浏览器访问会失败'}。`, label: '去处理证书', tab: 'certificates' })
  }
  if (namedServices.value.length && !publicAddress.value) {
    list.push({ text: networkCopy.noPublicAddress, label: '查看诊断', tab: 'diagnostics' })
  }
  // A name marked for EdgeOne with no account behind it is the one EdgeOne
  // problem visible without asking the cloud, and it is the quietest: the name
  // is simply never published, and every page that could show it shows nothing.
  if (edgeOneDomains.value.length && !edgeAccounts.value.length) {
    list.push({ text: `域名 ${edgeOneDomains.value.join('、')} 标记为经 EdgeOne 发布，但还没有可用的 EdgeOne 账号；这些域名不会被写入任何解析。`, label: '去配置', tab: 'edgeone' })
  }
  const cfMissing = namedServices.value.filter(service => service.entryMode === 'edge' && service.edgeProvider === 'cloudflare' && !(state.value?.cloudflareAccounts || []).some(account => account.enabled && account.id === service.edgeAccountId))
  if (cfMissing.length) {
    list.push({ text: `Cloudflare 服务 ${cfMissing.map(service => service.name).join('、')} 尚未选择可用的边缘接入账号。`, label: '去配置', tab: 'edgeone' })
  }
  for (const service of enabledServices.value) {
    if (!service.domains?.length && !(service.port || upstreamPortOf(service.upstream))) {
      list.push({ text: `服务「${service.name}」既没有域名，也没有可用的发布端口。`, label: '去修改', tab: 'services' })
    }
  }
  return list.slice(0, 6)
})

async function loadBaseRecord() {
  const sequence = ++baseLoadSequence
  const generation = scope.generation()
  const domain = settings.value.baseDomain
  const current = () => sequence === baseLoadSequence && scope.isCurrent(generation) && domain === settings.value.baseDomain
  baseRecordsLoading.value = true
  baseRecordsError.value = ''
  try {
    const result = await api.dnsRecords()
    if (!current()) return
    baseRecord.value = (result.records || []).find(record => record.base && record.host === domain) || null
    baseRecordsError.value = (result.warnings || []).join('；')
  } catch (err) { if (isRequestCancelled(err) || !current()) return;
    baseRecordsError.value = err.message
    baseRecord.value = null
  } finally { if (current()) baseRecordsLoading.value = false }
}

async function saveBaseStation() {
  if (writeBusy.value) return
  const generation = scope.generation()
  busy.value = 'base-station'
  baseError.value = ''
  try {
    const domain = baseForm.domain.trim().toLowerCase()
    adopt(await api.saveSettings({ baseDomain: domain, baseTtl: Number(baseForm.ttl) }))
    flash(domain ? `基站域名已设为 ${domain}；它会在下次发布解析时写入` : '基站域名已清除')
    await loadBaseRecord()
    baseEditing.value = false
  } catch (err) { if (isRequestCancelled(err)) return; baseError.value = err.message }
  finally { if (scope.isCurrent(generation)) busy.value = '' }
}

function flash(message) {
  toast.value?.show(message)
}

function adopt(next) {
  if (next?.services) state.value = next
}

function adoptSystemState(next) {
  // A restored draft invalidates earlier state reads and apply previews. The
  // returned state is authoritative; refresh only the dependent local summary.
  stateLoadSequence++
  loading.value = false
  adopt(next)
  previewOpen.value = false
  preview.value = null
  preflight.value = null
  void loadBaseRecord()
}

function openEditor(service = null) {
  if (writeBusy.value) return
  mobileNavOpen.value = false
  activeTab.value = 'services'
  nextTick(() => servicesTab.value?.edit(service))
}

function goTab(id) {
  // Reads belong to the old page and are canceled when it unmounts. Writes
  // keep their page mounted until completion; background jobs do not block it.
  if (navigationBusy.value) return
  if (id === activeTab.value) return
  if (isReadBusy(busy.value)) busy.value = ''
  mobileNavOpen.value = false
  activeTab.value = id
}

function setTheme(value) {
  theme.value = value === 'dark' ? 'dark' : 'light'
  document.documentElement?.setAttribute('data-theme', theme.value)
  try { window.localStorage.setItem('gateway-ui-theme', theme.value) } catch { /* theme still works without storage */ }
}

function toggleTheme() { setTheme(theme.value === 'light' ? 'dark' : 'light') }

function serviceState(service) {
  if (pendingServices.value.includes(service.id)) return { label: '有未发布修改', tone: 'blue' }
  if (!service.enabled) return { label: '已停用', tone: 'neutral' }
  return { label: '本机已应用', tone: 'private' }
}

async function loadState() {
  const sequence = ++stateLoadSequence
  loading.value = true
  error.value = ''
  try {
    const next = await api.state()
    if (sequence !== stateLoadSequence) return
    state.value = next
    await loadBaseRecord()
    if (sequence === stateLoadSequence) baseEditing.value = false
  } catch (err) { if (isRequestCancelled(err)) return; error.value = err.message }
  finally { if (sequence === stateLoadSequence) loading.value = false }
}

async function checkSession() {
  try {
    const session = await api.session()
    auth.value = session.authenticated ? 'ready' : 'login'
    if (session.authenticated) await loadState()
  } catch (err) { if (!isRequestCancelled(err)) auth.value = 'login' }
}

async function login() {
  const generation = scope.generation()
  loginBusy.value = true
  loginError.value = ''
  try {
    await api.login(password.value)
    password.value = ''
    auth.value = 'ready'
    await loadState()
  } catch (err) { if (isRequestCancelled(err)) return; loginError.value = err.message }
  finally { if (scope.isCurrent(generation)) loginBusy.value = false }
}

async function logout() {
  if (navigationBusy.value) return
  invalidateSessionRequests()
  scope.reset()
  try { await api.logout() }
  catch (err) {
    if (isRequestCancelled(err)) return
    loginError.value = '退出请求未确认；请核对连接，刷新页面可能仍保留原会话。'
  }
  auth.value = 'login'
  busy.value = ''
  state.value = null
  preflight.value = null
  previewOpen.value = false
  password.value = ''
  error.value = ''
  toast.value?.clear()
}

/* ------------------------------------------------------------ preview/apply */

async function showPreview() {
  if (!state.value || writeBusy.value) return
  const sequence = ++previewLoadSequence
  previewOpen.value = true
  previewLoading.value = true
  applyError.value = ''
  confirmPublic.value = false
  try { preview.value = await api.preview() }
  catch (err) { if (isRequestCancelled(err)) return; applyError.value = err.message; preview.value = null }
  finally { if (sequence === previewLoadSequence) previewLoading.value = false }
}

async function applyDraft() {
  if (!preview.value || writeBusy.value) return
  const generation = scope.generation()
  busy.value = 'apply'
  applyError.value = ''
  try {
    const result = await api.apply(preview.value.revision)
    state.value = result.state
    previewOpen.value = false
    preflight.value = null
    flash(result.publicAddress ? `已应用，本机公网入口 ${result.publicAddress}` : '已应用；当前没有可用的公网地址')
  } catch (err) { if (isRequestCancelled(err)) return; applyError.value = err.message }
  finally { if (scope.isCurrent(generation)) busy.value = '' }
}

async function exportConfig() {
  try {
    const config = await api.exportConfig()
    const blob = new Blob([JSON.stringify(config, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = 'gateway-config.json'
    link.click()
    URL.revokeObjectURL(url)
    flash('已导出当前草稿配置；导出内容不含任何云端凭据')
  } catch (err) { if (isRequestCancelled(err)) return; error.value = err.message }
}

async function loadPreflight() {
  try { preflight.value = await api.publicPreflight() }
  catch { preflight.value = null }
}

function loseSession(message = '会话已过期，请重新登录。') {
  invalidateSessionRequests()
  scope.reset()
  busy.value = ''
  loginBusy.value = false
  loading.value = false
  previewLoading.value = false
  mobileNavOpen.value = false
  auth.value = 'login'
  state.value = null
  previewOpen.value = false
  loginError.value = message
}
onSessionLost(() => loseSession())

onMounted(() => {
  try { setTheme(window.localStorage.getItem('gateway-ui-theme') || 'light') } catch { /* use the default theme */ }
  loadProfile()
  checkSession()
})
</script>

<template>
  <div v-if="auth === 'checking'" class="boot-screen"><span class="spinner"></span><span>正在连接网关…</span></div>

  <LoginPage v-else-if="auth === 'login'" v-model:password="password" :project-name="profile.projectName" :client-name="profile.clientName" :logo="profile.logo" :version="appVersion" :theme="theme" :busy="loginBusy" :error="loginError" @login="login" @toggle-theme="toggleTheme" />

  <div v-else :class="['app-shell', { 'menu-open': mobileNavOpen }]">
    <button v-if="mobileNavOpen" class="mobile-scrim" aria-label="关闭导航" @click="mobileNavOpen = false"></button>
    <aside class="sidebar">
      <button class="brand" :disabled="navigationBusy" @click="goTab('overview')"><BrandLogo class="brand-symbol" :src="profile.logo" /><span>{{ profile.projectName }}</span></button>
      <nav aria-label="主导航">
        <button v-for="tab in tabs" :key="tab.id" :class="['nav-item', { active: activeTab === tab.id }]" :disabled="navigationBusy" :aria-current="activeTab === tab.id ? 'page' : undefined" @click="goTab(tab.id)"><Icon :name="tab.icon" :size="19" /><span>{{ tab.name }}</span></button>
      </nav>
      <div class="sidebar-note"><div class="sidebar-note-icon"><strong>{{ profile.clientName }}</strong><span class="online"><i class="status-dot"></i>管理已连接</span></div><code>{{ settings.gatewayIp || '等待配置' }}</code><div class="sidebar-device-caption"><Icon name="lock" :size="12" />{{ networkCopy.management }}</div></div>
      <div class="sidebar-footer"><span class="mini-avatar">CG</span><div>网关管理员<small>本机管理会话</small></div><UiTooltip text="退出登录"><button class="icon-button" aria-label="退出登录" :disabled="navigationBusy" @click="logout"><Icon name="logout" :size="17" /></button></UiTooltip></div>
    </aside>
    <div class="main-shell">
      <header class="topbar"><div class="breadcrumb"><button class="icon-button mobile-menu" aria-label="展开导航" :aria-expanded="mobileNavOpen" @click="mobileNavOpen = !mobileNavOpen"><Icon name="menu" :size="18" /></button><button class="breadcrumb-home" aria-label="返回网关概览" @click="goTab('overview')">网关概览</button><Icon v-if="activeTab !== 'overview'" name="chevron" :size="12" /><strong v-if="activeTab !== 'overview'">{{ currentTab.name }}</strong></div><div class="topbar-right"><button class="icon-button" :aria-label="theme === 'light' ? '切换到深色主题' : '切换到浅色主题'" @click="toggleTheme"><Icon :name="theme === 'light' ? 'moon' : 'sun'" :size="18" /></button></div></header>
      <main class="content">
        <div class="page-heading"><div><h1>{{ currentTab.name }}</h1><p>{{ currentTab.caption }}</p></div>
          <button v-if="activeTab === 'services' && state" class="button primary" :disabled="Boolean(writeBusy)" @click="servicesTab?.edit()"><Icon name="plus" :size="16" />新增服务</button>
          <button v-else-if="activeTab === 'certificates' && state" class="button primary" :disabled="Boolean(writeBusy)" @click="certificatesTab?.openCreate()"><Icon name="upload" :size="16" />上传证书</button>
          <button v-else-if="activeTab === 'diagnostics' && state && diagnosticsTab?.activeSection === 'entry'" class="button primary" :disabled="diagnosticsTab?.preflightBusy" @click="diagnosticsTab?.runPreflight()"><Icon name="pulse" :size="16" />{{ diagnosticsTab?.preflightBusy ? '检查中…' : '检查本机入口' }}</button>
          <button v-else-if="activeTab === 'resolve' && state" class="button primary" :disabled="Boolean(writeBusy)" @click="resolveTab?.refresh()"><Icon name="refresh" :size="16" />{{ busy === 'dns-refresh' ? '探测中…' : '重新探测' }}</button>
          <button v-else-if="activeTab === 'edgeone' && state" class="button primary" :disabled="Boolean(writeBusy)" @click="edgeTab?.refresh()"><Icon name="refresh" :size="16" />{{ busy === 'edge-refresh' ? '读取中…' : '读取云端状态' }}</button>
        </div>
        <div v-if="publishTasks.active || publishTasks.starting" class="alert info publish-banner" role="status" data-publish-banner :data-phase="publishTasks.active?.status || 'starting'">
          <div class="publish-banner-indicator" aria-hidden="true"><Icon v-if="publishTasks.active?.status === 'waiting'" name="clock" :size="18" /><span v-else class="spinner small"></span></div>
          <div class="publish-banner-copy"><strong>{{ publishTasks.active ? `${publishTasks.active.serviceName} · ${publishStatus(publishTasks.active)}` : '正在提交发布…' }}</strong><p>{{ publishTasks.active?.status === 'waiting' ? '系统会自动检查并继续，无需重复操作。' : '正在后台处理，可以继续浏览其他页面。' }}</p></div>
          <button v-if="publishTasks.active" class="text-button publish-banner-action" @click="publishTasks.view(publishTasks.active)">查看进度<Icon name="chevron" :size="14" /></button>
        </div>
        <div v-if="publishTasks.error || publishTasks.uncertain || publishTasks.submitError" :class="['alert', publishTasks.uncertain || publishTasks.submitError ? 'warning' : 'info']" role="alert"><Icon name="info" /><span>{{ publishTasks.error || publishTasks.submitError }}</span><button class="text-button" @click="publishTasks.refresh()">重新查询发布记录</button><button v-if="publishTasks.uncertain" class="text-button" :disabled="publishTasks.starting" @click="publishTasks.recover()">使用原请求恢复提交</button></div>
        <div v-if="returnToPublish" class="alert info"><Icon name="info" /><span>处理配置后，可以继续刚才的发布。</span><button class="text-button" :disabled="Boolean(writeBusy)" @click="reopenPublish(returnToPublish)">返回发布预览</button></div>
        <div v-if="!state" class="empty-state panel"><span v-if="loading" class="spinner"></span><Icon v-else name="warning" :size="32" /><h3>{{ loading ? '正在读取网关配置' : '暂时无法读取网关配置' }}</h3><p>{{ loading ? '请稍候…' : '请检查管理程序是否运行，再重新读取。' }}</p><button v-if="!loading" class="button secondary" @click="loadState"><Icon name="refresh" :size="15" />重新读取</button></div>
        <template v-else>
          <section v-if="orphanChanges" class="revision-banner pending"><div class="revision-icon"><Icon name="code" :size="19" /></div><div class="revision-text"><strong>有已删除服务或网关设置等待应用</strong><span>草稿 v{{ state.revision }} · 已应用 {{ state.appliedRevision ? `v${state.appliedRevision}` : '无' }}</span></div><button class="button primary small" :disabled="Boolean(writeBusy)" @click="showPreview">预览全部改动</button></section>
          <Services v-if="activeTab === 'services'" ref="servicesTab" :state="state" :busy="writeBusy" :jobs="publishTasks.jobs" :submission-error="publishTasks.submitError" :publish-unavailable="!publishTasks.available" @state="adopt" @notice="flash" @busy="busy = $event" @publish="startPublish" @preview-opened="publishTasks.submitError = ''" @view-job="publishTasks.view" @configure="configureForPublish" />
          <Resolve ref="resolveTab" v-if="activeTab === 'resolve'" :state="state" :busy="writeBusy" :jobs="publishTasks.jobs" @view-job="publishTasks.view" @state="adopt" @notice="flash" @busy="busy = $event" />
          <EdgeAcceleration ref="edgeTab" v-if="activeTab === 'edgeone'" :state="state" :busy="writeBusy" :jobs="publishTasks.jobs" @publish-service="reopenPublish" @view-job="publishTasks.view" @refresh-jobs="publishTasks.refresh" @state="adopt" @notice="flash" @busy="busy = $event" />
          <Certificates ref="certificatesTab" v-if="activeTab === 'certificates'" :state="state" :busy="writeBusy" @state="adopt" @notice="flash" @busy="busy = $event" />
          <Diagnostics ref="diagnosticsTab" v-if="activeTab === 'diagnostics'" :state="state" :busy="writeBusy" @busy="busy = $event" @notice="flash" @goto="goTab" @export="exportConfig" />
          <Traffic v-if="activeTab === 'traffic'" :services="services" />
          <System v-if="activeTab === 'system'" :state="state" :busy="writeBusy" @busy="busy = $event" @state="adoptSystemState" @profile="adoptProfile" @notice="flash" @session-lost="loseSession" />
          <template v-if="activeTab === 'overview'">
            <OverviewMetrics :service-count="services.length" :enabled-count="enabledServices.length" :pending-count="pendingServices.length" :domain-count="domains.length" :edge-domain-count="edgeDomains.length" :certificate-count="usableCertificates.length" :certificate-total="certificates.length" />
            <PublishTopology :services="services" :gateway-ip="settings.gatewayIp" :unapplied-service-ids="pendingServices" :applied-revision="state.appliedRevision" :applied-at="state.appliedAt" :unapplied-elsewhere="orphanChanges" />
            <section v-if="todos.length" class="panel todo-panel"><div class="panel-header"><h2>待处理事项<span class="badge-count">{{ todos.length }}</span></h2></div><ul class="todo-list"><li v-for="(todo, index) in todos" :key="index" :class="todo.tone ? `todo-${todo.tone}` : null"><Icon name="info" :size="17" /><span>{{ todo.text }}</span><button v-if="todo.label" class="text-button" :disabled="Boolean(busy)" @click="todo.apply ? showPreview() : goTab(todo.tab)">{{ todo.label }}<Icon name="arrow" :size="14" /></button></li></ul></section>
            <div class="overview-layout">
              <div class="overview-primary">
                <section class="panel"><div class="panel-header"><h2>服务入口<span class="badge-count">{{ services.length }}</span></h2><button class="text-button" :disabled="navigationBusy" @click="goTab('services')">查看全部<Icon name="arrow" :size="14" /></button></div><div v-if="!services.length" class="compact-empty"><span class="empty-icon"><Icon name="server" :size="28" /></span><h3>还没有服务</h3><p>{{ networkCopy.addService }}</p><button class="button primary small" :disabled="Boolean(writeBusy)" @click="openEditor()"><Icon name="plus" :size="15" />添加服务</button></div><div v-else class="service-quick-list"><div v-for="service in services" :key="service.id" class="quick-service"><ServiceAvatar :service="service" /><div class="quick-service-info"><strong>{{ service.name }}<span class="type-label">{{ serviceKind(service).label }}</span></strong><UiCopyValue :value="entryOf(service)" :label="service.name" address /></div><span :class="['pill', serviceState(service).tone]">{{ serviceState(service).label }}</span><button class="icon-button" :aria-label="`编辑${service.name}`" :disabled="Boolean(writeBusy)" @click="openEditor(service)"><Icon name="chevron" :size="16" /></button></div></div></section>
              </div>
              <aside class="overview-aside">
                <section class="panel base-panel"><div class="panel-header"><h2>基站域名</h2><button class="icon-button" :aria-label="baseEditing ? '收起基站设置' : '编辑基站设置'" :disabled="Boolean(writeBusy)" @click="baseEditing = !baseEditing"><Icon :name="baseEditing ? 'close' : 'edit'" :size="17" /></button></div><div class="panel-body"><div class="base-mark"><Icon name="globe" :size="21" /></div><span class="field-caption">公网地址的固定别名</span><UiCopyValue v-if="settings.baseDomain" class="base-domain" :value="settings.baseDomain" /><strong v-else class="base-domain mono">{{ settings.baseDomain || '尚未设置' }}</strong><form v-if="baseEditing || !settings.baseDomain" class="base-station form-body" @submit.prevent="saveBaseStation"><div class="form-field"><label for="base-domain" class="form-label">基站域名</label><input id="base-domain" v-model="baseForm.domain" class="mono" placeholder="gateway.example.com" autocomplete="off" spellcheck="false" :disabled="Boolean(writeBusy)" /></div><div class="form-field"><label for="base-ttl" class="form-label">TTL</label><UiSelect id="base-ttl" v-model="baseForm.ttl" :options="baseTtls" :disabled="Boolean(writeBusy)" aria-label="基站 TTL" /></div><p class="field-caption">600 秒适用于所有套餐。正在回源的基站更名前，请先迁移源站引用。</p><div v-if="baseError" class="alert error" role="alert"><Icon name="warning" :size="16" /><span>{{ baseError }}</span></div><button class="button primary" :disabled="Boolean(writeBusy) || !baseDirty">{{ busy === 'base-station' ? '保存中…' : '保存基站设置' }}</button></form><dl v-else class="definition"><div><dt>解析状态</dt><dd><span :class="['pill', baseState.tone]">{{ baseState.label }}</span></dd></div><div><dt>记录类型</dt><dd class="mono">{{ baseRecord?.managed?.type || '—' }}</dd></div><div><dt>TTL</dt><dd class="mono">{{ settings.baseTtl || 600 }} s</dd></div></dl><div class="address-block"><span>已应用的公网地址</span><UiCopyValue v-if="publicAddress" :value="publicAddress" label="公网 IP 地址" truncate persistent-copy /><code v-else>{{ publicAddress || '未探测到' }}</code><span>{{ settings.autoResolve ? '自动跟随变化' : '手动重新发布' }}</span></div><p v-if="baseState.note" class="base-status-note" :class="{ 'error-text': baseRecordsError }">{{ baseState.note }}</p><div class="form-body-actions"><button class="text-button" :disabled="Boolean(writeBusy)" @click="loadBaseRecord"><Icon name="refresh" :size="14" />重新读取</button><button v-if="settings.baseDomain" class="text-button" :disabled="Boolean(writeBusy)" @click="goTab('resolve')">去解析页<Icon name="arrow" :size="14" /></button></div></div></section>
                <section class="panel"><div class="panel-header"><h2>证书状态</h2><Icon name="shield" :size="18" /></div><div class="panel-body"><div v-if="!certificates.length" class="small muted">还没有证书槽</div><div v-for="cert in certificates.slice(0, 3)" :key="cert.id" class="overview-certificate"><strong>{{ cert.name }}</strong><span :class="['pill', isUsable(cert) ? 'private' : 'amber']">{{ isUsable(cert) ? '可用' : '需处理' }}</span><code>{{ cert.domains?.join('、') || '尚未导入材料' }}</code><small>到期日 {{ formatDateTime(cert.notAfter) }}</small></div><button class="text-button" :disabled="navigationBusy" @click="goTab('certificates')">管理证书<Icon name="arrow" :size="14" /></button></div></section>
              </aside>
            </div>
          </template>
        </template>
      </main>
    </div>
  </div>

  <div v-if="previewOpen" class="modal-overlay" @click.self="busy ? null : (previewOpen = false)" @keydown.esc="busy ? null : (previewOpen = false)">
    <div class="modal wide" role="dialog" aria-modal="true" aria-label="预览并应用" tabindex="-1">
      <div class="modal-header"><div><span class="eyebrow">REVIEW BEFORE APPLY</span><h2>将要生效的配置</h2><p>核对完整草稿，确认后更新本机入口。</p></div><button class="icon-button" aria-label="关闭" :disabled="Boolean(busy)" @click="previewOpen = false"><Icon name="close" :size="18" /></button></div>
      <div class="modal-body"><div v-if="applyError" class="alert error" role="alert"><Icon name="warning" :size="18" /><span>{{ applyError }}</span></div><div v-if="previewLoading" class="compact-empty"><span class="spinner"></span><h3>正在生成配置预览</h3></div><template v-else-if="preview"><div class="preview-revisions"><span class="pill neutral">草稿 v{{ preview.revision }}</span><span :class="['pill', preview.publicAddress ? 'private' : 'amber']">公网地址 {{ preview.publicAddress || '未探测到' }}</span><span v-if="preview.dryRun" class="simulation-note">dry-run</span></div><ul v-if="preview.warnings?.length" class="todo-list"><li v-for="(warning, index) in preview.warnings" :key="index"><Icon name="info" :size="16" /><span>{{ warning }}</span></li></ul><details class="raw-details config-details"><summary>查看生成的 Nginx 配置</summary><div class="config-preview"><pre class="mono">{{ preview.config }}</pre></div></details><div class="public-apply-consent"><UiCheckbox v-model="confirmPublic" :disabled="Boolean(busy)">确认应用以上完整配置。公网可达性仍需独立验证。</UiCheckbox></div></template></div>
      <div class="modal-footer"><button class="button secondary" :disabled="Boolean(busy)" @click="previewOpen = false">取消</button><button class="button primary" :disabled="!preview || !confirmPublic || Boolean(writeBusy)" @click="applyDraft"><span v-if="busy === 'apply'" class="spinner small"></span>{{ busy === 'apply' ? '应用中…' : '应用配置' }}</button></div>
    </div>
  </div>
  <PublishProgress v-if="auth === 'ready' && publishTasks.selected" :key="publishTasks.selected.id" :job="publishTasks.selected" :error="publishTasks.error" @close="publishTasks.close" @refresh="publishTasks.refresh" @goto="inspectPublishTarget" @preview="reopenPublish" />
  <UiToast ref="toast" />
</template>
