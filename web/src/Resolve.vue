<script setup>
import { formatDateTime } from './utils/datetime'
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import Icon from './Icon.vue'
import ServiceAvatar from './components/ServiceAvatar.vue'
import UiCheckbox from './components/ui/UiCheckbox.vue'
import UiCopyValue from './components/ui/UiCopyValue.vue'
import UiSwitch from './components/ui/UiSwitch.vue'
import UiTooltip from './components/ui/UiTooltip.vue'
import UiOverlay from './components/ui/UiOverlay.vue'
import UiSelect from './components/ui/UiSelect.vue'
import { usePageScope } from './page-scope.ts'
import { isRequestCancelled } from './request.ts'
import { isPublishActive, publishStatus } from './usePublishJobs.js'

const props = defineProps({ state: Object, busy: String, jobs: { type: Array, default: () => [] } })
const emitEvent = defineEmits(['state', 'notice', 'busy', 'view-job'])
const scope = usePageScope()
const api = scope.api
const emit = scope.guard(emitEvent)

const summary = ref(null)
const summaryError = ref('')
const summaryLoading = ref(false)
const status = ref(null)
const statusError = ref('')
const statusLoading = ref(false)
const accountsError = ref('')
const records = ref([])
const recordsError = ref('')
const recordsWarning = ref('')
const recordsLoading = ref(false)
const observations = ref({}), observationBusy = ref('')
const detailHost = ref('')
const detailRecord = computed(() => records.value.find(record => record.host === detailHost.value) || null)
function serviceFor(record) {
  if (record.base) return null
  return props.state?.services?.find(service => record.serviceId
    ? service.id === record.serviceId
    : service.domains?.includes(record.host)) || null
}
function activePublish(record) {
  const service = props.state?.services?.find(item => item.domains?.includes(record.host))
  return props.jobs.find(job => job.serviceId === service?.id && isPublishActive(job))
}
function publication(record) {
  const job = activePublish(record)
  if (job) return { published: false, label: publishStatus(job), tone: 'blue' }
  const publicationState = record.managed?.pending ? 'unknown' : record.publicationState ||
    (['已发布', '上次发布'].includes(record.status) ? 'published' : ['未发布', '待发布'].includes(record.status) ? 'draft' : 'unknown')
  const published = publicationState === 'published'
  return { published, label: published ? '已解析' : publicationState === 'unknown' ? '待核对' : '未解析', tone: published ? 'private' : publicationState === 'unknown' ? 'amber' : 'neutral' }
}
async function observe(host) {
  if (observationBusy.value) return
  detailHost.value = host
  observationBusy.value = host
  try { observations.value[host] = await api.dnsObservation(host) }
  catch (err) { if (!isRequestCancelled(err)) observations.value[host] = { status: 'unknown', message: err.message } }
  finally { observationBusy.value = '' }
}
const accountForm = reactive({ name: '', provider: 'dnspod', secretId: '', secretKey: '', apiToken: '', enabled: true })
const editingAccount = ref(null)
const accountSnapshot = ref('')
const providers = [{ value: 'dnspod', label: '腾讯云 DNSPod' }, { value: 'cloudflare', label: 'Cloudflare DNS' }, { value: 'alidns', label: '阿里云 DNS' }]
const dnsProviderHelp = 'DNS 解析服务商与域名注册商、边缘加速分别配置；接入 Cloudflare 加速也可继续使用原 DNS，无需迁移域名或更改 NS。'
const dnsAccountValidationHelp = '添加前只读校验凭据及域名列表读取权限；不会写入 DNS 测试权限，记录读写权限仍在预览与发布时核对。'
const providerName = provider => providers.find(item => item.value === (provider || 'dnspod'))?.label || provider
const accountChoice = ref(null), choiceHost = ref(''), selectedChoice = ref('')
const choiceLoading = ref(false), choiceSaving = ref(false), choiceError = ref(''), choiceNeedsRefresh = ref(false)
const choiceOptions = computed(() => [{ value: '', label: '请选择当前实际使用的解析账号' }, ...(accountChoice.value?.choices || []).map(item => ({ value: item.accountId, label: `${item.accountName} · ${providerName(item.provider)}` }))])
const accountOpen = ref(false)
const accountsOpen = ref(false)
const settingsOpen = ref(false)
const accountOverlay = ref(null)
const accountNameInput = ref(null)
const accountSaving = ref(false)
const accountFormError = ref('')
const deleting = ref(null)
const preview = ref(null)
const previewHost = ref('')
const previewOpen = ref(false)
const previewBusy = ref(false)
const previewError = ref('')
const confirmed = ref(false)
const applyMessage = ref('')
const applyBusy = ref(false), previewNeedsRefresh = ref(false)
const overwriteRequired = computed(() => preview.value?.action === 'replace' || preview.value?.requiresOverwrite === true)
const replacementReady = computed(() => preview.value?.action !== 'replace' || (preview.value.conflicts?.length > 0 && preview.value.conflicts.every(record => record.type && record.value && record.recordId)))

const accounts = computed(() => props.state?.dnsAccounts || [])
const autoResolve = computed(() => Boolean(props.state?.settings?.autoResolve))
const baseDomain = computed(() => props.state?.settings?.baseDomain || '')
const accountDirty = computed(() => JSON.stringify(accountForm) !== accountSnapshot.value)
const credentialsReady = computed(() => editingAccount.value ? (accountForm.provider === 'cloudflare' || (!accountForm.secretId && !accountForm.secretKey) || Boolean(accountForm.secretId.trim() && accountForm.secretKey.trim())) : accountForm.provider === 'cloudflare' ? Boolean(accountForm.apiToken.trim()) : Boolean(accountForm.secretId.trim() && accountForm.secretKey.trim()))

function clearAccount() { Object.assign(accountForm, { name: '', provider: 'dnspod', secretId: '', secretKey: '', apiToken: '', enabled: true }); editingAccount.value = null; accountSnapshot.value = JSON.stringify(accountForm); accountFormError.value = '' }
function editAccount(account) {
  if (props.busy) return
  clearAccount(); editingAccount.value = account
  Object.assign(accountForm, { name: account.name, provider: account.provider || 'dnspod', enabled: account.enabled })
  accountSnapshot.value = JSON.stringify(accountForm); accountOpen.value = true; accountsOpen.value = true
  nextTick(() => accountNameInput.value?.focus())
}
async function checkAccountChoice(host) {
  if (props.busy || choiceLoading.value || choiceSaving.value) return false
  choiceLoading.value = true; choiceHost.value = host; choiceError.value = ''
  try {
    const result = await api.dnsRecordAccounts(host)
    if (result.status !== 'choice-required') {
      accountChoice.value = null
      emit('notice', { type: result.status === 'unavailable' ? 'error' : 'info', message: result.message })
      await loadRecords()
      return false
    }
    accountChoice.value = result; selectedChoice.value = ''; choiceNeedsRefresh.value = false
    previewOpen.value = false
    return true
  } catch (err) {
    if (!isRequestCancelled(err)) {
      choiceError.value = err.message; choiceNeedsRefresh.value = true
      if (!accountChoice.value) emit('notice', { type: 'error', message: err.message })
    }
    return false
  } finally { choiceLoading.value = false }
}
function closeAccountChoice() { accountChoice.value = null; selectedChoice.value = ''; choiceError.value = ''; choiceNeedsRefresh.value = false }
async function saveAccountChoice() {
  const choice = accountChoice.value?.choices.find(item => item.accountId === selectedChoice.value)
  if (!choice || props.busy || choiceLoading.value || choiceSaving.value || choiceNeedsRefresh.value) return
  const host = accountChoice.value.host
  let saved = false
  choiceSaving.value = true; choiceError.value = ''; emit('busy', 'dns-account-choice')
  try {
    await api.selectDNSRecordAccount(host, { revision: accountChoice.value.revision, accountId: choice.accountId, zone: choice.zone })
    saved = true
    closeAccountChoice()
    emit('notice', '解析账号已选择；尚未修改 DNS，请重新预览后发布。')
    emit('state', await api.state())
    await loadRecords()
  } catch (err) {
    if (!isRequestCancelled(err)) {
      if (saved) emit('notice', { type: 'error', message: '解析账号已保存，但页面刷新失败，请刷新后再预览。' })
      else { choiceError.value = err.message; choiceNeedsRefresh.value = true }
    }
  } finally { choiceSaving.value = false; emit('busy', '') }
}
function manageAccounts() { if (!props.busy) { accountOpen.value = false; accountsOpen.value = true; clearAccount() } }
function newAccount() {
  if (props.busy) return
  clearAccount(); accountOpen.value = true; accountsOpen.value = true
  nextTick(() => accountNameInput.value?.focus())
}
function closeAccounts() { accountsOpen.value = false; accountOpen.value = false; clearAccount() }

const actionLabel = { create: '新建记录', update: '更新记录', delete: '删除记录', replace: '强制覆盖现有解析', release: '解除本机托管', adopt: '登记托管', noop: '无需写入' }

const addressMatch = computed(() => {
  if (!summary.value?.appliedAddress) return { label: '尚无已应用地址', tone: 'neutral' }
  if (summary.value.matched === true) return { label: '与本机网卡一致', tone: 'private' }
  if (summary.value.matched === false) return { label: '本机网卡上未找到', tone: 'amber' }
  return { label: '网卡匹配状态未知', tone: 'neutral' }
})

const convergenceSummary = computed(() => {
  if (statusLoading.value) return { label: '读取中…', tone: 'neutral' }
  if (statusError.value || !status.value) return { label: '状态未知', tone: 'neutral' }
  if (status.value.error) return { label: '上次核对未完成', tone: 'amber' }
  if (!status.value.checkedAt) return { label: '尚未运行', tone: 'neutral' }
  if (status.value.changed === true) return { label: '地址曾发生变化', tone: 'private' }
  if (status.value.changed === false) return { label: '地址未变化', tone: 'private' }
  return { label: '结果待核对', tone: 'neutral' }
})

async function loadSummary() {
  summaryLoading.value = true
  summaryError.value = ''
  try { summary.value = await api.publicAddress() }
  catch (err) { if (isRequestCancelled(err)) return; summaryError.value = err.message; summary.value = null }
  finally { summaryLoading.value = false }
}

async function loadStatus() {
  if (statusLoading.value) return
  statusLoading.value = true
  statusError.value = ''
  try { status.value = await api.dnsStatus() }
  catch (err) { if (isRequestCancelled(err)) return; statusError.value = err.message; status.value = null }
  finally { statusLoading.value = false }
}

async function loadAccounts() {
  accountsError.value = ''
  try { await api.dnsAccounts() } catch (err) { if (isRequestCancelled(err)) return; accountsError.value = err.message }
}

async function loadRecords() {
  recordsLoading.value = true
  recordsError.value = ''
  recordsWarning.value = ''
  try {
    const result = await api.dnsRecords()
    records.value = result.records || []
    if (result.warnings?.length) recordsWarning.value = result.warnings.join('；')
  } catch (err) { if (isRequestCancelled(err)) return; recordsError.value = err.message; records.value = [] }
  finally { recordsLoading.value = false }
}

async function refresh() {
  if (props.busy) return
  emit('busy', 'dns-refresh')
  await Promise.all([loadSummary(), loadStatus(), loadAccounts(), loadRecords()])
  emit('busy', '')
}

async function toggleAutoResolve(next) {
  if (props.busy) return
  emit('busy', 'settings')
  try {
    emit('state', await api.saveSettings({ autoResolve: next }))
    emit('notice', next ? '已开启自动解析：公网地址变化后会在本机核验通过时自动重新发布' : '已关闭自动解析：地址变化需要手动重新发布')
    await loadStatus()
  } catch (err) { if (isRequestCancelled(err)) return; emit('notice', { type: 'error', message: err.message }) }
  finally { emit('busy', '') }
}

async function createAccount() {
  if (props.busy || accountSaving.value) return
  accountSaving.value = true
  emit('busy', 'dns-account')
  accountFormError.value = ''
  try {
    const credential = accountForm.provider === 'cloudflare' ? (accountForm.apiToken.trim() ? { apiToken: accountForm.apiToken.trim() } : {}) : (accountForm.secretId.trim() || accountForm.secretKey.trim() ? { secretId: accountForm.secretId.trim(), secretKey: accountForm.secretKey.trim() } : {})
    const body = { name: accountForm.name.trim(), ...credential, ...(editingAccount.value ? { enabled: accountForm.enabled } : { provider: accountForm.provider }) }
    const next = editingAccount.value ? await api.updateDNSAccount(editingAccount.value.id, body) : await api.createDNSAccount(body)
    emit('state', next)
    clearAccount()
    accountOpen.value = false
    emit('notice', next.operationRecoveryMessage || 'DNS 账号已保存；密钥只写入设备本地文件，不会回显')
  } catch (err) { if (isRequestCancelled(err)) return; accountFormError.value = err.message }
  finally { accountSaving.value = false; emit('busy', '') }
}

async function removeAccount(account) {
  if (props.busy) return
  emit('busy', 'dns-account-delete')
  try {
    emit('state', await api.deleteDNSAccount(account.id))
    deleting.value = null
    emit('notice', 'DNS 账号已删除；云端记录未被改动')
    await loadRecords()
  } catch (err) { if (isRequestCancelled(err)) return; emit('notice', { type: 'error', message: err.message }); deleting.value = null }
  finally { emit('busy', '') }
}

async function openPreview(record) {
  const job = activePublish(record)
  if (job) { emit('view-job', job); return }
  if (props.busy || previewBusy.value || applyBusy.value) return
  if (record.accountChoiceRequired) { await checkAccountChoice(record.host); return }
  previewOpen.value = true
  previewBusy.value = true
  previewError.value = ''
  applyMessage.value = ''
  confirmed.value = false
  previewNeedsRefresh.value = false
  preview.value = null
  previewHost.value = record.host
  try { preview.value = await api.previewDNS(record.host) }
  catch (err) {
    if (isRequestCancelled(err)) return
    previewError.value = err.message
    if (err.status !== 401 && typeof api.dnsRecordAccounts === 'function') await checkAccountChoice(record.host)
  }
  finally { previewBusy.value = false }
}

async function applyPreview(forceOverwrite = false) {
  if (!preview.value || preview.value.action === 'noop' || props.busy || applyBusy.value || previewNeedsRefresh.value || !replacementReady.value) return
  if (overwriteRequired.value ? forceOverwrite !== true : !confirmed.value) return
  applyBusy.value = true
  emit('busy', 'dns-apply')
  previewError.value = ''
  try {
    const result = await api.applyDNS({ host: preview.value.host, token: preview.value.token, revision: preview.value.revision, confirm: true, ...(overwriteRequired.value ? { confirmOverwrite: true } : {}) })
    applyMessage.value = result.message || 'DNS 托管商已接受提交'
    preview.value = null
    emit('notice', result.message || 'DNS 托管商已接受提交')
    await loadRecords()
  } catch (err) { if (isRequestCancelled(err)) return; previewError.value = err.message; confirmed.value = false; previewNeedsRefresh.value = true }
  finally { applyBusy.value = false; emit('busy', '') }
}

onMounted(refresh)
watch(() => props.jobs.filter(isPublishActive).map(job => job.id).join(','), (current, previous) => { if (previous && current !== previous) loadRecords() })
onBeforeUnmount(clearAccount)
defineExpose({ refresh })
</script>


<template>
  <div class="cloud-toolbar">
    <div class="cloud-context"><span class="small muted">正在发布的公网地址</span><strong class="mono">{{ summary?.appliedAddress || state?.appliedPublicAddress || '尚未应用' }}</strong><span :class="['pill', convergenceSummary.tone]">{{ autoResolve ? '自动解析' : '手动解析' }} · {{ convergenceSummary.label }}</span></div>
    <div class="actions"><button class="button secondary small" @click="settingsOpen = true">地址与自动解析</button><button class="button secondary small" :disabled="Boolean(busy)" @click="manageAccounts">管理解析账号 <span class="badge-count">{{ accounts.length }}</span></button></div>
  </div>
  <div v-if="accountsError" class="alert error" role="alert"><Icon name="warning" /><span>DNS 账号读取失败：{{ accountsError }}</span><button class="text-button" @click="manageAccounts">查看账号</button></div>
  <div v-if="summaryError || statusError || status?.error" class="alert warning" role="alert"><Icon name="warning" /><span>{{ summaryError || statusError || status?.error }}</span><button class="text-button" @click="settingsOpen = true">查看详情</button></div>
  <div v-if="!accounts.length && !accountsError" class="alert info"><Icon name="info" /><span>尚未配置 DNS 账号。添加后即可预览并发布域名解析。</span><button class="text-button" :disabled="Boolean(busy)" @click="newAccount">添加账号</button></div>

  <section class="card cloud-workspace">
    <div class="card-head">
      <div><h2>域名解析<span class="badge-count">{{ records.length }}</span></h2></div>
      <button class="button secondary small" :disabled="Boolean(busy) || recordsLoading" @click="loadRecords"><Icon name="refresh" :size="15" />{{ recordsLoading ? '刷新中…' : '刷新记录' }}</button>
    </div>
    <div v-if="recordsError || recordsWarning" class="card-body">
      <div v-if="recordsError" class="alert error" role="alert"><Icon name="warning" /><span>{{ recordsError }}</span></div>
      <div v-else-if="recordsWarning" class="alert warning" role="alert"><Icon name="warning" /><span>{{ recordsWarning }}</span></div>
    </div>
      <div v-if="recordsLoading && !records.length" class="compact-empty" role="status"><span class="spinner"></span><p>正在读取本地发布记录…</p></div>
      <div v-else-if="!records.length && !recordsError" class="compact-empty">
        <Icon name="globe" :size="30" /><h3>还没有需要解析的域名</h3>
        <p>在服务页给一个服务绑定域名，它就会出现在这里。</p>
      </div>

      <div v-else-if="records.length" class="table-wrap" :aria-busy="recordsLoading">
        <table class="data-table dns-record-table">
          <thead><tr><th>服务名</th><th>域名</th><th>入口方式</th><th>记录值</th><th>发布状态</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="record in records" :key="record.host">
              <td><button class="dns-service-name" :aria-label="`查看域名详情：${record.host}`" @click="detailHost = record.host"><ServiceAvatar v-if="serviceFor(record)" :service="serviceFor(record)" /><span v-else class="service-icon blue" aria-hidden="true"><Icon name="globe" :size="17" /></span><strong>{{ record.base ? '基站域名' : record.serviceName || '已无服务使用' }}</strong></button></td>
              <td><UiCopyValue :value="record.host" label="复制域名" /></td>
              <td><span class="type-label">{{ record.base ? '基站' : record.edge ? (record.edgeProvider === 'cloudflare' ? 'Cloudflare' : 'EdgeOne') : '本机直连' }}</span><small v-if="record.provider || record.accountId" class="secondary">{{ providerName(record.provider) }} · {{ accounts.find(account => account.id === record.accountId)?.name || record.zone }}</small></td>
              <!-- An edge-fronted name must never be shown this device's own
                   address: it does not resolve to it, and an operator who reads a
                   stale-looking value here will try to "fix" it into an outage. -->
              <td>
                <UiCopyValue v-if="record.managed" :value="record.managed.value" label="复制解析值" />
                <template v-else-if="record.edge">{{ record.value || (record.edgeProvider === 'cloudflare' ? '按 Cloudflare 入口配置生成' : '由 EdgeOne 分配') }}</template>
                <UiCopyValue v-else-if="props.state?.appliedPublicAddress" :value="props.state.appliedPublicAddress" label="复制期望地址" />
                <span v-else>—</span>
              </td>
              <td class="dns-publication-cell">
                <span :class="['pill', publication(record).tone]">{{ publication(record).label }}</span>
              </td>
              <td><button v-if="activePublish(record)" class="text-button" @click="emit('view-job', activePublish(record))">查看发布进度</button><button v-else class="text-button" :disabled="Boolean(busy) || choiceLoading" @click="openPreview(record)">{{ record.accountChoiceRequired ? '选择解析账号' : '预览改动' }} <Icon name="chevron" :size="14" /></button><button v-if="record.managed" class="text-button" :disabled="Boolean(observationBusy)" @click="observe(record.host)">{{ observationBusy === record.host ? '核对中…' : '核对云端' }}</button></td>
            </tr>
          </tbody>
        </table>
      </div>
  </section>



  <UiOverlay :open="Boolean(detailRecord)" title="域名详情" :description="detailHost" variant="drawer" @close="detailHost = ''">
    <template v-if="detailRecord">
      <dl class="definition">
        <div><dt>服务名</dt><dd>{{ detailRecord.base ? '基站域名' : detailRecord.serviceName || '已无服务使用' }}</dd></div>
        <div><dt>服务状态</dt><dd>{{ detailRecord.base ? '跟随本机地址' : detailRecord.serviceId ? (detailRecord.enabled ? '已启用' : '已停用') : '已无服务使用' }}</dd></div>
        <div><dt>发布状态</dt><dd>{{ publication(detailRecord).label }}</dd></div>
        <template v-if="detailRecord.managed">
          <div><dt>记录类型</dt><dd>{{ detailRecord.managed.type }}</dd></div>
          <div><dt>解析区域</dt><dd>{{ detailRecord.managed.zone }}</dd></div>
          <div><dt>TTL</dt><dd>{{ detailRecord.managed.ttl }}</dd></div>
          <div><dt>记录值</dt><dd><UiCopyValue :value="detailRecord.managed.value" /></dd></div>
          <div><dt>云端核对</dt><dd>{{ observationBusy === detailHost ? '核对中…' : observations[detailHost] ? ({ matched: '与上次记录一致', drift: '存在漂移', unknown: '状态未知' })[observations[detailHost].status] || '状态未知' : '尚未核对' }}</dd></div>
          <div v-if="observations[detailHost]?.checkedAt"><dt>核对时间</dt><dd>{{ formatDateTime(observations[detailHost].checkedAt) }}</dd></div>
        </template>
      </dl>
      <p class="field-caption">“已解析”表示已记录成功发布，不代表已验证当前权威解析或外部可达性。</p>
      <p v-if="observations[detailHost]?.message" class="alert warning">{{ observations[detailHost].message }}</p>
      <p v-if="detailRecord.message" class="field-caption">{{ detailRecord.message }}</p>
    </template>
    <template #footer="{ close }"><button class="button secondary" @click="close">关闭</button><button v-if="detailRecord?.managed" class="button primary" :disabled="Boolean(observationBusy)" @click="observe(detailHost)">{{ observationBusy === detailHost ? '核对中…' : '核对云端' }}</button></template>
  </UiOverlay>

  <UiOverlay :open="settingsOpen" title="地址与自动解析" variant="drawer" :busy="Boolean(busy)" @close="settingsOpen = false">
  <div class="cloud-settings-stack">
    <section class="card">
      <div class="card-head">
        <div class="card-title"><Icon name="globe" :size="18" /><h2>公网地址</h2></div>
      </div>
      <div class="card-body">
        <div v-if="summaryLoading" class="compact-empty" role="status"><span class="spinner"></span><p>正在探测本机公网地址…</p></div>
        <div v-else-if="summaryError" class="alert error" role="alert"><Icon name="warning" /><span>{{ summaryError }}</span></div>
        <template v-else-if="summary">
          <span class="small muted">正在发布的地址</span>
          <div class="big-address"><UiCopyValue v-if="summary.appliedAddress" :value="summary.appliedAddress" label="复制公网地址" /><span v-else>尚未应用</span></div>
          <div class="actions">
            <span :class="['pill', addressMatch.tone]">{{ addressMatch.label }}</span>
            <span class="small muted">{{ summary.followers ?? 0 }} 个域名跟随</span>
          </div>
          <div v-if="summary.domains?.length" class="actions address-followers"><UiCopyValue v-for="domain in summary.domains" :key="domain" :value="domain" label="复制跟随域名" /></div>
          <p v-else class="field-caption base-caption">暂无跟随域名</p>
          <details class="raw-details">
            <summary>地址候选 · {{ summary.candidates?.length || 0 }} 个 / 可用 {{ summary.eligible?.length || 0 }} 个</summary>
            <div class="table-wrap">
              <table class="data-table">
                <thead><tr><th>地址</th><th>网卡</th><th>类型</th><th>判定</th></tr></thead>
                <tbody>
                  <tr v-for="candidate in summary.candidates" :key="candidate.address">
                    <td><UiCopyValue :value="candidate.address" label="复制候选地址" /></td>
                    <td>{{ candidate.interface }}</td>
                    <td>{{ candidate.family === 'inet6' ? 'IPv6' : 'IPv4' }}</td>
                    <td><span :class="['pill', candidate.eligible ? 'green' : 'neutral']">{{ candidate.eligible ? '可用' : candidate.reason }}</span></td>
                  </tr>
                  <tr v-if="!summary.candidates?.length"><td colspan="4" class="table-empty">没有读取到任何地址</td></tr>
                </tbody>
              </table>
            </div>
          </details>
        </template>
        <div v-else class="compact-empty" role="status"><span v-if="busy === 'dns-refresh'" class="spinner"></span><p>{{ busy === 'dns-refresh' ? '正在探测本机公网地址…' : '尚未读取地址，请重新探测。' }}</p></div>
      </div>
    </section>

    <section class="card">
      <div class="card-head">
        <div class="card-title"><Icon name="refresh" :size="18" /><h2>自动解析</h2></div>
        <UiSwitch :model-value="autoResolve" :disabled="Boolean(busy)" aria-label="自动解析" @update:model-value="toggleAutoResolve" />
      </div>
      <div class="card-body" data-convergence-status :aria-busy="statusLoading">
        <dl class="definition">
          <div><dt>地址变化时</dt><dd>{{ autoResolve ? '本机核验后自动发布' : '手动重新发布' }}</dd></div>
          <div><dt>上次核对</dt><dd class="mono">{{ statusLoading ? '读取中…' : status?.checkedAt ? formatDateTime(status.checkedAt) : status && !status.error ? '尚未运行' : '—' }}</dd></div>
          <div><dt>核对结果</dt><dd :class="['check-summary', convergenceSummary.tone]">{{ convergenceSummary.label }}</dd></div>
          <div><dt>已重新发布</dt><dd>{{ !statusLoading && status?.checkedAt && Number.isInteger(status.published) ? `${status.published} 条` : '—' }}</dd></div>
        </dl>
        <div v-if="statusError" class="alert error" role="alert"><Icon name="warning" /><span>自动解析状态读取失败：{{ statusError }}</span></div>
        <div v-else-if="!statusLoading && status?.error" class="alert warning" role="alert"><Icon name="warning" /><span>{{ status.error }}</span></div>
      </div>
    </section>
  </div>


  </UiOverlay>

  <UiOverlay ref="accountOverlay" :open="accountsOpen" :title="accountOpen ? (editingAccount ? '编辑 DNS 解析账号' : '添加 DNS 解析账号') : 'DNS 解析账号'" description="账号凭据仅保存在设备本地；管理解析账号不会自动发布域名或更改 NS。" variant="drawer" size="wide" :busy="Boolean(busy) || accountSaving" :dirty="accountOpen && accountDirty" @close="closeAccounts">
    <p v-if="!accountOpen" class="field-caption">{{ dnsProviderHelp }}</p>
    <div v-if="accountsError" class="alert error" role="alert">{{ accountsError }}</div>
    <template v-if="!accountOpen"><div class="cloud-account-actions"><button class="button primary small" :disabled="Boolean(busy)" @click="newAccount"><Icon name="plus" :size="15" />添加账号</button><button class="button secondary small" :disabled="Boolean(busy)" @click="loadAccounts">重新读取账号</button></div>    <div v-if="!accounts.length" class="compact-empty"><Icon name="lock" :size="28" /><h3>还没有 DNS 账号</h3><p>支持腾讯云 DNSPod、Cloudflare DNS、阿里云 DNS；添加账号后可预览发布。</p></div>
    <div v-else class="table-wrap">
      <table class="data-table">
        <thead><tr><th>账号</th><th>凭据</th><th>状态</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="account in accounts" :key="account.id">
            <td><div class="name-cell"><span class="service-icon blue"><Icon name="lock" :size="17" /></span><div><strong>{{ account.name }}</strong><span class="secondary">{{ providerName(account.provider) }}</span></div></div></td>
            <td><span class="mono">{{ account.secretIdHint }}</span><span class="secondary">第 {{ account.credentialVersion }} 版凭据</span></td>
            <td><span :class="['pill', account.enabled ? 'green' : 'neutral']">{{ account.enabled ? '启用中' : '已停用' }}</span></td>
            <td><button class="text-button" :disabled="Boolean(busy)" @click="editAccount(account)">编辑</button> <UiTooltip text="删除账号"><button class="icon-button danger-hover" :disabled="Boolean(busy)" :aria-label="`删除账号：${account.name}`" @click="deleting = account"><Icon name="trash" :size="16" /></button></UiTooltip></td>
          </tr>
        </tbody>
      </table>
    </div></template>
      <form v-if="accountOpen" id="resolve-account-form" class="form-body" @submit.prevent="createAccount">
        <div v-if="accountFormError" class="alert error" role="alert"><Icon name="warning" /><span>{{ accountFormError }}</span></div>
        <div class="form-field"><label class="form-label" for="dns-provider">DNS 解析服务商</label><UiSelect id="dns-provider" v-model="accountForm.provider" :options="providers" :disabled="Boolean(busy) || accountSaving || Boolean(editingAccount)" label="DNS 解析服务商" /><span class="field-caption">{{ dnsProviderHelp }}</span></div>
        <div class="form-row">
          <div class="form-field"><label class="form-label" for="account-name">备注名称</label><input :disabled="Boolean(busy) || accountSaving" ref="accountNameInput" id="account-name" v-model="accountForm.name" maxlength="100" placeholder="主账号" autocomplete="off" /></div>
          <div v-if="accountForm.provider !== 'cloudflare'" class="form-field"><label class="form-label" for="account-secret-id">{{ accountForm.provider === 'alidns' ? 'AccessKey ID' : 'SecretId' }}</label><input :disabled="Boolean(busy) || accountSaving" id="account-secret-id" v-model="accountForm.secretId" class="mono" autocomplete="off" spellcheck="false" /></div>
        </div>
        <div v-if="accountForm.provider !== 'cloudflare'" class="form-field"><label class="form-label" for="account-secret-key">{{ accountForm.provider === 'alidns' ? 'AccessKey Secret' : 'SecretKey' }}</label><input :disabled="Boolean(busy) || accountSaving" id="account-secret-key" v-model="accountForm.secretKey" type="password" class="mono" autocomplete="new-password" /><span class="field-caption">{{ accountForm.provider === 'alidns' ? '使用仅限 DNS 权限的 RAM 访问密钥。' : '这里填腾讯云 API 密钥，不是旧版 DNSPod Token。' }}{{ editingAccount ? '两项留空保留原凭据；轮换时请同时填写。' : '' }}</span></div>
        <div v-else class="form-field"><label class="form-label" for="dns-api-token">API Token</label><input id="dns-api-token" v-model="accountForm.apiToken" type="password" :disabled="Boolean(busy) || accountSaving" autocomplete="new-password" /><span class="field-caption">限定目标区的 Zone Read 与 DNS Edit 权限；不使用 Global API Key。{{ editingAccount ? '留空保留原 Token。' : '' }}</span></div>
        <p v-if="!editingAccount" class="field-caption">{{ dnsAccountValidationHelp }}</p>
        <div v-if="editingAccount" class="switch-field"><span>启用账号</span><UiSwitch v-model="accountForm.enabled" :disabled="Boolean(busy) || accountSaving" aria-label="启用 DNS 账号" /></div>
      </form>

    <template #footer><button class="button secondary" :disabled="Boolean(busy) || accountSaving" @click="accountOverlay?.requestClose()">{{ accountOpen ? '取消' : '关闭' }}</button><button v-if="accountOpen" type="submit" form="resolve-account-form" class="button primary" :disabled="Boolean(busy) || accountSaving || !accountForm.name.trim() || !credentialsReady || !accountDirty">{{ accountSaving ? (editingAccount ? '保存中…' : '正在校验…') : (editingAccount ? '保存账号' : '校验并添加') }}</button></template>
  </UiOverlay>

  <UiOverlay :open="Boolean(accountChoice)" title="选择解析账号" :description="choiceHost" :busy="choiceSaving || choiceLoading || Boolean(busy)" @close="closeAccountChoice">
    <p>系统无法唯一确定这个域名的解析账号。请选择当前实际使用的 DNS 托管账号；这不会迁移域名或修改解析记录。</p>
    <p v-if="accountChoice?.message" class="field-caption">{{ accountChoice.message }}</p>
    <div v-if="choiceError" class="alert error" role="alert">{{ choiceError }}</div>
    <div class="form-field"><label class="form-label" for="dns-account-choice">解析账号</label><UiSelect id="dns-account-choice" v-model="selectedChoice" :options="choiceOptions" :disabled="choiceSaving || choiceLoading || choiceNeedsRefresh" label="解析账号" /></div>
    <p class="field-caption">请按当前权威 DNS 选择，而不是按域名购买平台选择。其他域名和已有配置不会被覆盖。</p>
    <template #footer="{ close }"><button class="button secondary" :disabled="choiceSaving || choiceLoading" @click="close">取消</button><button v-if="choiceNeedsRefresh" class="button secondary" :disabled="choiceSaving || choiceLoading" @click="checkAccountChoice(choiceHost)">重新检查</button><button class="button primary" :disabled="!selectedChoice || choiceLoading || choiceSaving || choiceNeedsRefresh" @click="saveAccountChoice">{{ choiceSaving ? '保存中…' : '确认账号' }}</button></template>
  </UiOverlay>

  <UiOverlay :open="previewOpen" :title="preview?.action === 'replace' ? 'DNS 冲突：预览并强制覆盖' : 'DNS 改动预览'" :description="previewHost" :busy="Boolean(busy) || previewBusy || applyBusy" @close="previewOpen = false">
        <div v-if="previewError" class="alert error" role="alert"><Icon name="warning" /><span>{{ previewError }}</span></div>
        <div v-if="applyMessage" class="alert" role="status"><Icon name="check" :size="17" /><span>{{ applyMessage }}</span></div>
        <div v-if="previewBusy" class="compact-empty"><span class="spinner"></span><h3>正在读取云端记录</h3></div>
        <template v-else-if="preview">
          <section v-if="preview.recovery" class="dns-conflict-summary">
            <div class="alert warning"><Icon name="warning" /><span>上次操作未完成，本次已重新读取云端记录。无需恢复历史记录，可按本次预览手动确认继续；该域名的异常不会阻塞其他域名或基站域名的自动解析。</span></div>
            <template v-if="preview.action !== 'replace'">
              <h3>当前云端记录</h3><p v-if="!preview.recovery.current?.length" class="field-caption">当前没有同名记录，已无解析冲突<span v-if="preview.action === 'create'">；本次仅创建目标记录，不删除任何记录</span>。</p>
              <p v-for="record in preview.recovery.current" :key="record.recordId" class="dns-conflict-record">{{ record.type }} · {{ record.value }} · TTL {{ record.ttl }}</p>
              <p v-if="preview.action === 'adopt'" class="field-caption">当前云端已是目标解析；本次仅登记本机托管，不写入或删除云端记录。</p>
            </template>
            <details class="raw-details"><summary>上次操作的历史快照（不是当前冲突）</summary>
              <p v-if="preview.recovery.phase" class="field-caption">上次操作阶段：{{ preview.recovery.phase }}</p>
              <div v-for="record in preview.recovery.before" :key="record.recordId" class="dns-conflict-record"><strong>{{ record.name }} · {{ record.type }}</strong><UiCopyValue :value="record.value" /><span class="secondary">TTL {{ record.ttl }} · 线路 {{ record.line }} · 状态 {{ record.status }}</span><span class="secondary">备注：{{ record.remark || '无' }}</span></div>
            </details>
          </section>
          <section v-if="preview.action === 'replace'" class="dns-conflict-summary">
            <div class="alert warning"><Icon name="warning" /><span>当前云端解析与目标冲突。点击“一键强制覆盖”将仅删除本域名下列同名记录并写入目标记录，原服务可能中断，DNS 传播期间新旧结果可能并存；不会修改其他域名的记录。</span></div>
            <h3>当前云端冲突记录</h3>
            <p v-for="record in preview.conflicts" :key="record.recordId" class="dns-conflict-record"><strong>{{ preview.host }}</strong> 已通过 <strong>{{ record.type }}</strong> 解析到 <UiCopyValue :value="record.value" label="复制原解析值" /><span class="secondary">TTL {{ record.ttl }} · {{ record.line || '默认线路' }}<span v-if="record.proxied"> · 代理已开启</span><span v-if="record.status === 'DISABLE'"> · 已停用</span><span v-if="record.weight > 0"> · 权重 {{ record.weight }}</span></span></p>
            <p class="field-caption">覆盖后的目标记录使用默认线路、启用状态和仅 DNS 解析（DNS only，不启用代理），权重为 0；不会保留原记录的特殊线路、代理、停用状态或权重。</p>
            <p v-if="!replacementReady" class="alert error">无法完整读取现有解析，禁止覆盖；请重新预览或到 DNS 托管商核对。</p>
          </section>
          <div class="preview-revisions">
            <span :class="['pill', preview.action === 'noop' ? 'neutral' : 'blue']">{{ actionLabel[preview.action] || preview.action }}</span>
            <span class="pill neutral">{{ preview.type }} · {{ preview.zone }} · TTL {{ preview.ttl }}</span>
          </div>
          <div class="table-wrap">
            <table class="data-table">
              <tbody>
                <tr><th>域名</th><td><UiCopyValue :value="preview.host" label="复制预览域名" /></td></tr>
                <tr><th>{{ preview.action === 'noop' ? '记录值' : '将要写入' }}</th><td><span v-if="preview.action === 'delete'">（删除这条记录）</span><UiCopyValue v-else :value="preview.value" label="复制待写入值" /></td></tr>
                <tr v-if="preview.previous && preview.action !== 'noop'"><th>当前云端值</th><td><span class="pill neutral">{{ preview.previous.type }}</span> <UiCopyValue :value="preview.previous.value" label="复制当前解析值" /></td></tr>
              </tbody>
            </table>
          </div>
          <ul v-if="preview.action !== 'noop' && preview.warnings?.length" class="todo-list">
            <li v-for="(warning, index) in preview.warnings" :key="index"><Icon name="info" :size="16" /><span>{{ warning }}</span></li>
          </ul>
          <p v-if="overwriteRequired" class="field-caption">预览和重新预览均不会写入 DNS。点击“一键强制覆盖”即确认按上述当前云端记录执行；如已在托管商删除或修改记录，请先重新预览。</p>
          <UiCheckbox v-else-if="preview.action !== 'noop'" v-model="confirmed" :disabled="Boolean(busy) || applyBusy || previewNeedsRefresh" class="public-apply-consent">{{ preview.action === 'release' ? '确认解除本机托管，云端记录不变。' : preview.action === 'adopt' ? '确认重新登记本机托管记录。' : '确认以上 DNS 变更。' }}</UiCheckbox>
        </template>
    <template #footer>
        <button class="button secondary" :disabled="Boolean(busy) || previewBusy || applyBusy" @click="previewOpen = false">关闭</button>
        <button v-if="preview || previewError" class="button secondary" :disabled="Boolean(busy) || previewBusy || applyBusy" @click="openPreview({ host: previewHost })">重新预览</button>
        <button v-if="preview && preview.action !== 'noop'" :class="['button', overwriteRequired ? 'danger' : 'primary']" :disabled="(!overwriteRequired && !confirmed) || Boolean(busy) || applyBusy || previewNeedsRefresh || !replacementReady" @click="applyPreview(overwriteRequired)">{{ applyBusy ? '提交中…' : overwriteRequired ? '一键强制覆盖' : preview.action === 'release' ? '解除托管' : preview.action === 'adopt' ? '登记托管' : '提交变更' }}</button></template>
  </UiOverlay>

  <UiOverlay :open="Boolean(deleting)" title="删除 DNS 账号" :busy="Boolean(busy)" @close="deleting = null">
    <p class="delete-message">确认删除账号「{{ deleting?.name }}」？本地凭据文件会被删除，云端已有的解析记录不会被改动。若该账号下仍有本程序发布的记录，删除会被拒绝。</p>
    <template #footer>
        <button class="button secondary" :disabled="Boolean(busy)" @click="deleting = null">取消</button>
        <button class="button danger" :disabled="Boolean(busy)" @click="removeAccount(deleting)">{{ busy === 'dns-account-delete' ? '删除中…' : '删除账号' }}</button></template>
  </UiOverlay>
</template>

<style scoped>
.dns-conflict-record{overflow-wrap:anywhere;line-height:1.8;padding:12px 0;margin:0;border-bottom:1px solid var(--line)}
.dns-conflict-record :deep(.copy-value){max-width:100%;white-space:normal;overflow-wrap:anywhere}
.dns-record-table{min-width:980px}
.dns-record-table th:first-child{width:170px}
.dns-service-name{display:flex;align-items:center;gap:12px;max-width:240px;border:0;background:none;padding:0;color:var(--ink);font:inherit;text-align:left;cursor:pointer}
.dns-service-name strong{overflow-wrap:anywhere}
.dns-service-name:hover strong{color:var(--blue)}
.dns-service-name:focus-visible{outline:2px solid var(--blue);outline-offset:4px;border-radius:4px}
.dns-service-name .service-icon{flex-shrink:0}
.dns-publication-cell{white-space:nowrap}
.cloud-toolbar{display:flex;justify-content:space-between;align-items:center;gap:16px;margin-bottom:18px;flex-wrap:wrap}
.cloud-context{display:flex;align-items:center;gap:12px;flex-wrap:wrap;min-width:0}.cloud-context strong{font-size:14px;overflow-wrap:anywhere}
.cloud-account-actions{display:flex;gap:10px;justify-content:flex-end;margin-bottom:18px;flex-wrap:wrap}
.cloud-settings-stack{display:grid;gap:18px}.cloud-workspace{margin-top:0}
.edge-status-note{min-width:12em;max-width:28em;white-space:normal;overflow-wrap:anywhere}
@media(max-width:640px){.cloud-toolbar{align-items:flex-start}.cloud-toolbar>.actions{width:100%}}
</style>
