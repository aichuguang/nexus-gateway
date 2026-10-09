<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import Icon from '../Icon.vue'
import { formatDateTime } from '../utils/datetime'
import { usePageScope } from '../page-scope'
import { errorDetails, isRequestCancelled } from '../request'
import type { SecuritySession } from '../system-types'
import UiOverlay from './ui/UiOverlay.vue'

const props = defineProps<{ busy?: string }>()
const emitEvent = defineEmits<{ busy: [value: string]; notice: [value: string]; 'session-lost': [message: string] }>()
const scope = usePageScope(), api = scope.api, emit = scope.guard(emitEvent)
const sessions = ref<SecuritySession[]>([]), loading = ref(false), error = ref(''), working = ref('')
const passwordOpen = ref(false), passwordError = ref(''), revokeError = ref(''), revokeAttempted = ref(false)
const form = reactive({ currentPassword: '', newPassword: '', confirmPassword: '' })
type RevokeTarget = { id?: string; scope?: 'others' | 'all'; current: boolean; label: string }
const target = ref<RevokeTarget | null>(null)
const locked = computed(() => Boolean(props.busy || working.value))
const passwordDirty = computed(() => Boolean(form.currentPassword || form.newPassword || form.confirmPassword))
const otherSessions = computed(() => sessions.value.filter(session => !session.current).length)
const passwordValid = computed(() => Boolean(form.currentPassword && Array.from(form.newPassword).length >= 12 && Array.from(form.newPassword).length <= 128 && new TextEncoder().encode(form.newPassword).length <= 512 && form.newPassword !== form.currentPassword && form.newPassword === form.confirmPassword))
function clearPasswords() { form.currentPassword = ''; form.newPassword = ''; form.confirmPassword = '' }
function openPassword() { if (!locked.value) { clearPasswords(); passwordError.value = ''; passwordOpen.value = true } }
function closePassword() { if (!locked.value) { passwordOpen.value = false; passwordError.value = ''; clearPasswords() } }
function askRevoke(selected: RevokeTarget) { if (!locked.value) { target.value = selected; revokeError.value = ''; revokeAttempted.value = false } }
function closeRevoke() { if (!locked.value) { target.value = null; revokeError.value = ''; revokeAttempted.value = false } }
async function load() {
  loading.value = true; error.value = ''
  try { sessions.value = (await api.securitySessions()).sessions || [] }
  catch (err) { if (!isRequestCancelled(err)) error.value = errorDetails(err).message }
  finally { loading.value = false }
}
async function changePassword() {
  if (locked.value || !passwordValid.value) return
  working.value = 'security-password'; emit('busy', working.value); passwordError.value = ''
  try {
    const pending = api.changePassword({ ...form })
    clearPasswords()
    const result = await pending
    passwordOpen.value = false
    if (result.relogin) emit('session-lost', '管理密码已修改，请使用新密码重新登录。')
  } catch (err) {
    if (!isRequestCancelled(err)) {
      const details = errorDetails(err)
      if (details.relogin) emit('session-lost', details.message)
      else passwordError.value = details.message
    }
  }
  finally { working.value = ''; emit('busy', '') }
}
async function revoke() {
  const selected = target.value
  if (locked.value || !selected || revokeAttempted.value) return
  revokeAttempted.value = true
  working.value = 'security-revoke'; emit('busy', working.value); revokeError.value = ''
  try {
    const result = selected.id ? await api.deleteSession(selected.id) : await api.revokeSessions(selected.scope || 'others')
    target.value = null
    if (selected.current || result.relogin) emit('session-lost', '当前会话已注销，请重新登录。')
    else { emit('notice', '选定会话已注销'); await load() }
  } catch (err) { if (!isRequestCancelled(err)) revokeError.value = errorDetails(err).message }
  finally { working.value = ''; emit('busy', '') }
}
onMounted(load)
onBeforeUnmount(clearPasswords)
</script>

<template>
  <section class="card">
    <div class="card-head"><div class="card-title"><Icon name="shield" /><h2>登录会话<span class="badge-count">{{ sessions.length }}</span></h2></div><div class="actions"><button type="button" class="button secondary small" :disabled="loading || locked" @click="load"><Icon name="refresh" :size="15" />刷新</button><button type="button" class="button primary small" :disabled="locked" @click="openPassword"><Icon name="lock" :size="15" />修改管理密码</button></div></div>
    <div class="session-summary"><p>查看仍有效的登录。会话固定 12 小时到期，服务重启后失效。</p><span v-if="!loading">{{ otherSessions }} 个其他会话</span></div>
    <div v-if="error" class="card-body"><div class="alert error" role="alert">{{ error }}</div></div>
    <div v-if="loading && !sessions.length" class="compact-empty"><span class="spinner"></span>正在读取会话…</div>
    <div v-else-if="!sessions.length && !error" class="compact-empty"><Icon name="shield" :size="28" /><h3>没有可显示的会话</h3><p>刷新列表以确认当前登录状态。</p></div>
    <div v-else class="table-wrap"><table class="data-table"><thead><tr><th>浏览器 / 客户端</th><th>来源</th><th>最近活动 / 到期</th><th class="align-right">操作</th></tr></thead><tbody><tr v-for="session in sessions" :key="session.id"><td><strong>{{ session.client || '未知客户端' }}</strong><span v-if="session.current" class="pill blue">当前会话</span><span class="secondary">创建于 {{ formatDateTime(session.createdAt) }}</span></td><td class="mono">{{ session.ip || '—' }}</td><td><span>{{ formatDateTime(session.lastSeenAt) }}</span><span class="secondary">到期 {{ formatDateTime(session.expiresAt) }}</span></td><td><button type="button" class="text-button" :disabled="locked" :aria-label="'注销' + (session.current ? '当前' : '其他') + '会话：' + (session.client || '未知客户端')" @click="askRevoke({ id: session.id, current: session.current, label: session.current ? '注销当前会话' : '注销这个会话' })">注销</button></td></tr></tbody></table></div>
    <div class="session-footer"><span>注销不会取消已受理的后台发布任务。</span><div class="actions"><button type="button" class="button secondary small" :disabled="locked || !otherSessions" @click="askRevoke({ scope: 'others', current: false, label: '注销所有其他会话' })">注销其他会话</button><button type="button" class="text-button danger-text" :disabled="locked" @click="askRevoke({ scope: 'all', current: true, label: '注销全部会话并退出登录' })">注销全部会话</button></div></div>
  </section>

  <UiOverlay :open="passwordOpen" title="修改管理密码" description="修改成功后，所有会话都会注销，包括当前浏览器。" :busy="locked" :dirty="passwordDirty" @close="closePassword">
    <form id="security-password-form" class="form-body" @submit.prevent="changePassword">
      <div v-if="passwordError" class="alert error" role="alert">{{ passwordError }}</div>
      <div class="form-field"><label class="form-label" for="security-current-password">当前管理密码</label><input id="security-current-password" v-model="form.currentPassword" type="password" autocomplete="current-password" required :disabled="locked" /></div>
      <div class="form-field"><label class="form-label" for="security-new-password">新密码</label><input id="security-new-password" v-model="form.newPassword" type="password" autocomplete="new-password" required :disabled="locked" /><span class="field-caption">12–128 个字符，不超过 512 字节，且不能与当前密码相同。</span></div>
      <div class="form-field"><label class="form-label" for="security-confirm-password">再次输入新密码</label><input id="security-confirm-password" v-model="form.confirmPassword" type="password" autocomplete="new-password" required :disabled="locked" /><span v-if="form.confirmPassword && form.confirmPassword !== form.newPassword" class="field-caption error-text">两次密码不一致。</span></div>
      <p class="field-caption">密码仅用于本次请求，提交或关闭后清空。</p>
    </form>
    <template #footer="{ close }"><button type="button" class="button secondary" :disabled="locked" @click="close">取消</button><button type="submit" form="security-password-form" class="button primary" :disabled="locked || !passwordValid">{{ working === 'security-password' ? '正在修改…' : '修改密码并重新登录' }}</button></template>
  </UiOverlay>

  <UiOverlay :open="Boolean(target)" :title="target?.label || '注销会话'" :description="target?.current ? '当前浏览器也会退出，需要重新登录。' : '选定的其他设备将需要重新登录，当前会话保持有效。'" :busy="locked" @close="closeRevoke">
    <div v-if="revokeError" class="alert error" role="alert">{{ revokeError }}<p>未自动重试。请关闭后刷新会话，先核对结果。</p></div><p v-else class="field-caption">此操作立即使选定会话失效，不影响网关已发布入口。</p>
    <template #footer><button type="button" class="button secondary" :disabled="locked" @click="closeRevoke">{{ revokeError ? '关闭' : '取消' }}</button><button type="button" class="button danger" :disabled="locked || revokeAttempted" @click="revoke">{{ working === 'security-revoke' ? '正在注销…' : '确认注销' }}</button></template>
  </UiOverlay>
</template>

<style scoped>
.session-summary,.session-footer{display:flex;align-items:center;justify-content:space-between;gap:18px;padding:16px 20px;color:var(--muted);font-size:12px;line-height:1.7}.session-summary{background:var(--surface-2);border-bottom:1px solid var(--line)}.session-summary p{margin:0}.session-summary>span{white-space:nowrap}.session-footer{border-top:1px solid var(--line)}.danger-text{color:var(--danger)}@media(max-width:720px){.session-summary,.session-footer{align-items:flex-start;flex-direction:column;gap:10px}.card-head{align-items:flex-start;gap:12px;flex-wrap:wrap}}
</style>
