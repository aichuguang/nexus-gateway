<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, shallowRef, watch } from 'vue'
import type { GatewayState, JSONValue } from '../api-types'
import type { HistoryEntry, HistoryPreview } from '../system-types'
import { usePageScope } from '../page-scope'
import { errorDetails, isRequestCancelled } from '../request'
import { saveDownload } from '../download-file'
import Icon from '../Icon.vue'
import { formatDateTime } from '../utils/datetime'
import UiCheckbox from './ui/UiCheckbox.vue'
import UiOverlay from './ui/UiOverlay.vue'

const props = defineProps<{ state: GatewayState; busy?: string }>()
const emitEvent = defineEmits<{ busy: [value: string]; state: [value: GatewayState]; notice: [value: string] }>()
const scope = usePageScope(), api = scope.api, emit = scope.guard(emitEvent)
const entries = ref<HistoryEntry[]>([]), loading = ref(false), error = ref(''), working = ref('')
const preview = shallowRef<HistoryPreview | null>(null), previewLoading = ref(false), previewError = ref('')
const confirmed = ref(false), restorePassword = ref(''), backupError = ref('')
const backupOpen = ref(false), previewOpen = ref(false), restoreOpen = ref(false)
const selectedEntry = ref<HistoryEntry | null>(null)
let previewGeneration = 0, previewRead: AbortController | undefined
const backup = reactive({ password: '', passphrase: '', confirmPassphrase: '' })
const locked = computed(() => Boolean(props.busy || working.value))
const backupValid = computed(() => Boolean(backup.password && Array.from(backup.passphrase).length >= 12 && Array.from(backup.passphrase).length <= 128 && new TextEncoder().encode(backup.passphrase).length <= 512 && backup.passphrase === backup.confirmPassphrase))
function clearSecrets() { backup.password = ''; backup.passphrase = ''; backup.confirmPassphrase = ''; restorePassword.value = '' }
const backupDirty = computed(() => Boolean(backup.password || backup.passphrase || backup.confirmPassphrase))
function openBackup() { if (!locked.value) { clearSecrets(); backupError.value = ''; backupOpen.value = true } }
function closeBackup() { if (!locked.value) { backupOpen.value = false; backupError.value = ''; clearSecrets() } }
function closeRestore() { if (!locked.value) { restoreOpen.value = false; restorePassword.value = ''; confirmed.value = false } }
function openRestore() { if (!locked.value && preview.value?.canRestore) { restoreOpen.value = true; restorePassword.value = ''; confirmed.value = false } }
function closePreview() {
  if (locked.value) return
  previewGeneration++; previewRead?.abort(); previewRead = undefined
  previewOpen.value = false; previewLoading.value = false; preview.value = null; previewError.value = ''; selectedEntry.value = null
  closeRestore()
}
function show(value: JSONValue) { return typeof value === 'string' ? value : JSON.stringify(value, null, 2) }
function invalidatePreview(message: string) {
  if (preview.value) preview.value = { ...preview.value, canRestore: false, token: '' }
  confirmed.value = false; restorePassword.value = ''; restoreOpen.value = false; previewError.value = message
}
watch(() => props.state.revision, (revision) => {
  if (preview.value && preview.value.revision !== revision) invalidatePreview('配置已变化，请重新选择历史记录并预览。')
})
async function refreshState() {
  try { emit('state', await api.state()) }
  catch (err) { if (!isRequestCancelled(err)) error.value = `当前配置读取失败：${errorDetails(err).message}` }
}
async function load() {
  loading.value = true; error.value = ''
  try { const result = await api.history(); entries.value = result.entries || []; error.value = result.error || '' }
  catch (err) { if (!isRequestCancelled(err)) error.value = errorDetails(err).message }
  finally { loading.value = false }
}
async function openPreview(entry: HistoryEntry) {
  if (locked.value || previewLoading.value) return
  const generation = ++previewGeneration
  previewRead?.abort(); previewRead = new AbortController()
  previewOpen.value = true; selectedEntry.value = entry; restoreOpen.value = false
  preview.value = null; previewError.value = ''; previewLoading.value = true; confirmed.value = false; restorePassword.value = ''
  try {
    const result = await api.previewHistory(entry.id, props.state.revision, { signal: previewRead.signal })
    if (generation !== previewGeneration || !previewOpen.value) return
    preview.value = result
    if (preview.value.revision !== props.state.revision) invalidatePreview('配置已变化，请重新选择历史记录并预览。')
  }
  catch (err) {
    if (generation === previewGeneration && previewOpen.value && !isRequestCancelled(err)) {
      previewError.value = errorDetails(err).message
      if (errorDetails(err).status === 409) await refreshState()
    }
  }
  finally { if (generation === previewGeneration) { previewLoading.value = false; previewRead = undefined } }
}
async function restore() {
  const selected = preview.value
  if (locked.value || !selected?.canRestore || !confirmed.value || !restorePassword.value) return
  working.value = 'history-restore'; emit('busy', working.value); previewError.value = ''
  try {
    const pending = api.restoreHistory(selected.id, { revision: selected.revision, token: selected.token, confirm: true, password: restorePassword.value })
    restorePassword.value = ''
    const result = await pending
    preview.value = null; confirmed.value = false; previewOpen.value = false; restoreOpen.value = false; selectedEntry.value = null
    emit('state', result.state); emit('notice', result.message || '历史配置已恢复为草稿，自动解析已暂停')
    await load()
  } catch (err) {
    if (!isRequestCancelled(err)) {
      const details = errorDetails(err)
      previewError.value = details.message
      if (details.status === 409 || details.outcomeUnknown) {
        invalidatePreview(details.message)
        await load(); await refreshState()
      }
    }
  }
  finally { working.value = ''; emit('busy', '') }
}
async function download() {
  if (locked.value || !backupValid.value) return
  working.value = 'backup-download'; emit('busy', working.value); backupError.value = ''
  try {
    const pending = api.downloadBackup({ ...backup })
    clearSecrets()
    saveDownload(await pending)
    backupOpen.value = false
    emit('notice', '加密备份已生成，请妥善保管文件和备份口令')
  } catch (err) { if (!isRequestCancelled(err)) backupError.value = errorDetails(err).message }
  finally { working.value = ''; emit('busy', '') }
}
onMounted(load)
onBeforeUnmount(() => { clearSecrets(); previewRead?.abort() })
</script>

<template>
  <section class="backup-entry card">
    <div class="backup-entry-copy"><span class="backup-entry-icon"><Icon name="database" :size="23" /></span><div><h2>加密备份</h2><p>配置、凭据和证书私钥使用独立口令加密。下载不改变运行状态。</p></div></div>
    <button type="button" class="button primary" :disabled="locked" @click="openBackup"><Icon name="download" :size="16" />创建加密备份</button>
  </section>
  <section class="card section-gap"><div class="card-head"><div class="card-title"><Icon name="clock" /><h2>配置历史<span class="badge-count">{{ entries.length }}</span></h2></div><button type="button" class="button secondary small" :disabled="locked || loading" @click="load"><Icon name="refresh" :size="15" />刷新</button></div>
    <div class="history-intro"><p>选择版本查看差异，再决定是否还原。还原只生成新草稿，并暂停自动解析。</p><span class="pill">当前草稿 v{{ state.revision }}</span></div>
    <div v-if="error" class="card-body"><div class="alert error" role="alert">{{ error }}</div></div>
    <div v-if="loading && !entries.length" class="compact-empty"><span class="spinner"></span>正在读取历史…</div><div v-else-if="!entries.length && !error" class="compact-empty"><Icon name="clock" :size="28" /><h3>还没有配置历史</h3><p>保存服务配置后，可在这里查看历史版本。</p></div>
    <div v-else class="table-wrap"><table class="data-table"><thead><tr><th>配置版本</th><th>保存时间</th><th>摘要</th><th class="align-right">操作</th></tr></thead><tbody><tr v-for="entry in entries" :key="entry.id"><td class="mono">v{{ entry.revision }}<span class="secondary">{{ entry.serviceCount }} 个服务</span></td><td>{{ formatDateTime(entry.createdAt) }}</td><td>{{ entry.summary }}</td><td><button type="button" class="text-button" :disabled="locked || previewLoading" :aria-label="'查看版本 ' + entry.revision + ' 的还原差异'" @click="openPreview(entry)">查看差异</button></td></tr></tbody></table></div>
  </section>

  <UiOverlay :open="backupOpen" title="创建加密备份" description="备份含敏感材料，请将文件与备份口令分开保管。" :busy="locked" :dirty="backupDirty" @close="closeBackup">
    <form id="backup-download-form" class="form-body" @submit.prevent="download">
      <div v-if="backupError" class="alert error" role="alert">{{ backupError }}</div>
      <div class="form-field"><label class="form-label" for="backup-admin-password">当前管理密码</label><input id="backup-admin-password" v-model="backup.password" type="password" autocomplete="current-password" required :disabled="locked" /></div>
      <div class="form-field"><label class="form-label" for="backup-passphrase">备份加密口令</label><input id="backup-passphrase" v-model="backup.passphrase" type="password" autocomplete="new-password" required :disabled="locked" /><span class="field-caption">12–128 个字符，不超过 512 字节。</span></div>
      <div class="form-field"><label class="form-label" for="backup-confirm-passphrase">再次输入备份口令</label><input id="backup-confirm-passphrase" v-model="backup.confirmPassphrase" type="password" autocomplete="new-password" required :disabled="locked" /><span v-if="backup.confirmPassphrase && backup.confirmPassphrase !== backup.passphrase" class="field-caption error-text">两次备份口令不一致。</span></div>
      <div class="alert warning">此版本暂不提供完整备份恢复入口。备份不含管理密码、会话或服务器部署配置。</div>
    </form>
    <template #footer="{ close }"><button type="button" class="button secondary" :disabled="locked" @click="close">取消</button><button type="submit" form="backup-download-form" class="button primary" :disabled="locked || !backupValid">{{ working === 'backup-download' ? '正在生成…' : '加密并下载' }}</button></template>
  </UiOverlay>

  <UiOverlay :open="previewOpen" :title="selectedEntry ? '还原差异 · v' + selectedEntry.revision : '还原差异'" description="先核对差异，确认后才会保存为新的草稿，不自动发布。" variant="drawer" size="wide" :busy="locked" @close="closePreview">
    <div class="form-body">
      <div v-if="previewError" class="alert error" role="alert">{{ previewError }}</div><div v-if="previewLoading" class="compact-empty"><span class="spinner"></span>正在生成差异…</div>
      <template v-if="preview">
        <div class="history-comparison"><span>当前草稿 <strong>v{{ preview.revision }}</strong></span><Icon name="arrow" :size="18" /><span>历史配置 <strong>v{{ selectedEntry?.revision }}</strong></span></div>
        <div v-for="warning in preview.warnings" :key="warning" class="alert warning">{{ warning }}</div>
        <div v-if="!preview.changes.length" class="compact-empty">没有可还原的差异。</div>
        <div v-else class="table-wrap"><table class="data-table history-diff"><thead><tr><th>配置项</th><th>当前草稿</th><th>还原后</th></tr></thead><tbody><tr v-for="change in preview.changes" :key="change.path"><td class="mono">{{ change.path }}</td><td><pre>{{ show(change.before) }}</pre></td><td><pre>{{ show(change.after) }}</pre></td></tr></tbody></table></div>
        <div v-if="!preview.canRestore" class="alert warning">该预览当前不能还原。请关闭后重新预览，或先处理上方提示。</div>
      </template>
    </div>
    <template #footer="{ close }"><button type="button" class="button secondary" :disabled="locked" @click="close">关闭预览</button><button type="button" class="button primary" :disabled="locked || previewLoading || !preview?.canRestore" @click="openRestore">还原为草稿…</button></template>
  </UiOverlay>

  <UiOverlay :open="restoreOpen" title="确认还原为草稿" description="此操作不改变已发布入口，不撤销云端操作。自动解析将暂停。" :busy="locked" :dirty="Boolean(restorePassword || confirmed)" @close="closeRestore">
    <form id="history-restore-form" class="form-body" @submit.prevent="restore">
      <div v-if="previewError" class="alert error" role="alert">{{ previewError }}</div>
      <p class="field-caption">仅还原刚才预览的版本。预览失效或结果不明时，不会自动重试。</p>
      <UiCheckbox v-model="confirmed" :disabled="locked">确认只还原为草稿，并暂停自动解析。</UiCheckbox>
      <div class="form-field"><label class="form-label" for="history-password">当前管理密码</label><input id="history-password" v-model="restorePassword" type="password" autocomplete="current-password" required :disabled="locked" /></div>
    </form>
    <template #footer="{ close }"><button type="button" class="button secondary" :disabled="locked" @click="close">取消</button><button type="submit" form="history-restore-form" class="button primary" :disabled="locked || !preview?.canRestore || !confirmed || !restorePassword">{{ working === 'history-restore' ? '正在还原…' : '确认还原草稿' }}</button></template>
  </UiOverlay>
</template>

<style scoped>
.backup-entry{display:flex;align-items:center;justify-content:space-between;gap:20px;padding:21px}.backup-entry-copy{display:flex;align-items:center;gap:14px;min-width:0}.backup-entry-copy h2{font-size:15px;margin:0 0 6px}.backup-entry-copy p,.history-intro p{font-size:12px;color:var(--muted);line-height:1.7;margin:0}.backup-entry-icon{display:grid;place-items:center;width:44px;height:44px;border-radius:12px;color:var(--blue);background:var(--blue-bg);flex-shrink:0}.history-intro{display:flex;align-items:center;justify-content:space-between;gap:16px;padding:15px 20px;border-bottom:1px solid var(--line)}.history-comparison{display:flex;align-items:center;gap:18px;color:var(--muted);font-size:13px}.history-comparison strong{color:var(--ink);margin-left:6px}.history-diff{table-layout:fixed;min-width:570px}.history-diff pre{white-space:pre-wrap;overflow-wrap:anywhere;max-height:260px;overflow:auto;font-size:12px;margin:0}.history-diff td{vertical-align:top;overflow-wrap:anywhere}@media(max-width:700px){.backup-entry,.history-intro{align-items:flex-start;flex-direction:column}.backup-entry>.button{align-self:flex-end}.history-comparison{gap:10px;flex-wrap:wrap}}
</style>
