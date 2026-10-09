<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import type { NotificationChannel, NotificationInput, NotificationType, SMTPTLS } from '../system-types'
import { usePageScope } from '../page-scope'
import { errorDetails, isRequestCancelled } from '../request'
import Icon from '../Icon.vue'
import { formatDateTime } from '../utils/datetime'
import UiSelect from './ui/UiSelect.vue'
import UiSwitch from './ui/UiSwitch.vue'
import UiOverlay from './ui/UiOverlay.vue'

const props = defineProps<{ busy?: string }>()
const emitEvent = defineEmits<{ busy: [value: string]; notice: [value: string] }>()
const scope = usePageScope(), api = scope.api, emit = scope.guard(emitEvent)
const channels = ref<NotificationChannel[]>([]), loading = ref(false), working = ref(''), error = ref(''), formError = ref('')
const formBaseline = ref(''), actionError = ref(''), actionAttempted = ref(false), saveUncertain = ref(false)
const open = ref(false), editing = ref<NotificationChannel | null>(null)
const action = ref<{ type: 'delete' | 'test'; channel: NotificationChannel } | null>(null)
const emptyForm = { name: '', type: 'wecom' as NotificationType, enabled: true, endpoint: '', secret: '', smtpHost: '', smtpPort: 587, smtpUsername: '', smtpPassword: '', smtpFrom: '', smtpTo: '', smtpTLS: 'starttls' as SMTPTLS }
const form = reactive({ ...emptyForm })
const types = [{ value: 'wecom', label: '企业微信' }, { value: 'feishu', label: '飞书' }, { value: 'dingtalk', label: '钉钉' }, { value: 'smtp', label: 'SMTP 邮件' }, { value: 'webhook', label: '通用 Webhook' }]
const tlsModes = [{ value: 'starttls', label: 'STARTTLS' }, { value: 'tls', label: 'TLS' }]
const locked = computed(() => Boolean(props.busy || working.value || loading.value))
const recipients = computed(() => form.smtpTo.split(/[\s,;，；]+/).map(value => value.trim()).filter(Boolean))
const valid = computed(() => Boolean(form.name.trim() && (form.type === 'smtp'
  ? form.smtpHost.trim() && form.smtpFrom.trim() && recipients.value.length > 0 && recipients.value.length <= 10 && Number.isInteger(Number(form.smtpPort)) && Number(form.smtpPort) > 0 && Number(form.smtpPort) <= 65535
  : form.endpoint.trim() || editing.value?.hasEndpoint)))
const formDirty = computed(() => JSON.stringify(form) !== formBaseline.value)
const typeDescription = computed(() => ({
  wecom: '填写企业微信群机器人的 Webhook 地址。',
  feishu: '填写飞书机器人地址；启用签名时补充签名密钥。',
  dingtalk: '填写钉钉机器人地址；启用加签时补充签名密钥。',
  smtp: '通过加密 SMTP 投递邮件，支持最多 10 个收件人。',
  webhook: '向 HTTPS 接收端发送事件，可选 Bearer 身份验证。',
})[form.type])
function label(type: NotificationType) { return types.find(item => item.value === type)?.label || type }
function deliveryLabel(status: string) { return ({ ok: '投递成功', error: '投递失败', unknown: '尚未投递' } as Record<string, string>)[status] || '尚未投递' }
function clearSecrets() { form.endpoint = ''; form.secret = ''; form.smtpPassword = '' }
watch(() => form.type, clearSecrets)
function edit(channel: NotificationChannel | null = null) {
  if (locked.value || (!channel && channels.value.length >= 10)) return
  editing.value = channel; formError.value = ''; action.value = null; saveUncertain.value = false
  Object.assign(form, { name: channel?.name || '', type: channel?.type || 'wecom', enabled: channel?.enabled ?? true,
    endpoint: '', secret: '', smtpPassword: '', smtpHost: channel?.smtpHost || '', smtpPort: channel?.smtpPort || 587,
    smtpUsername: channel?.smtpUsername || '', smtpFrom: channel?.smtpFrom || '', smtpTo: (channel?.smtpTo || []).join('\n'), smtpTLS: channel?.smtpTLS || 'starttls' })
  formBaseline.value = JSON.stringify(form)
  open.value = true
}
function close() { if (!locked.value) { open.value = false; editing.value = null; formError.value = ''; saveUncertain.value = false; Object.assign(form, emptyForm); formBaseline.value = JSON.stringify(form) } }
function askAction(type: 'delete' | 'test', channel: NotificationChannel) {
  if (!locked.value) { action.value = { type, channel }; actionError.value = ''; actionAttempted.value = false }
}
function closeAction() { if (!locked.value) { action.value = null; actionError.value = ''; actionAttempted.value = false } }
async function load() {
  loading.value = true; error.value = ''
  try { channels.value = (await api.notificationChannels()).channels || [] }
  catch (err) { if (!isRequestCancelled(err)) error.value = errorDetails(err).message }
  finally { loading.value = false }
}
async function save() {
  if (locked.value || !valid.value || saveUncertain.value) return
  formError.value = ''
  if (form.type !== 'smtp' && form.endpoint) {
    try { const endpoint = new URL(form.endpoint.trim()); if (endpoint.protocol !== 'https:' || endpoint.username || endpoint.password || endpoint.hash) throw new Error() }
    catch { formError.value = '通知地址必须是有效的 HTTPS URL。'; return }
  }
  const input: NotificationInput = { name: form.name.trim(), type: form.type, enabled: form.enabled }
  if (form.type === 'smtp') Object.assign(input, { smtpHost: form.smtpHost.trim(), smtpPort: Number(form.smtpPort), smtpUsername: form.smtpUsername.trim(), smtpPassword: form.smtpPassword, smtpFrom: form.smtpFrom.trim(), smtpTo: [...recipients.value], smtpTLS: form.smtpTLS })
  else Object.assign(input, { endpoint: form.endpoint.trim(), secret: form.secret })
  working.value = 'notification-save'; emit('busy', working.value)
  try {
    const pending = editing.value ? api.updateNotification(editing.value.id, input) : api.createNotification(input)
    clearSecrets()
    channels.value = (await pending).channels || []
    open.value = false; editing.value = null; emit('notice', '通知渠道已保存')
  } catch (err) { if (!isRequestCancelled(err)) { const details = errorDetails(err); formError.value = details.message; saveUncertain.value = details.outcomeUnknown } }
  finally { working.value = ''; emit('busy', '') }
}
async function confirmAction() {
  const selected = action.value
  if (!selected || locked.value || actionAttempted.value) return
  actionAttempted.value = true; actionError.value = ''
  working.value = `notification-${selected.type}`; emit('busy', working.value); error.value = ''
  try {
    if (selected.type === 'delete') channels.value = (await api.deleteNotification(selected.channel.id)).channels || []
    else {
      await api.testNotification(selected.channel.id)
      await load()
    }
    action.value = null; emit('notice', selected.type === 'delete' ? '通知渠道已删除' : '渠道已接受测试通知，请在接收端核对')
  } catch (err) {
    if (!isRequestCancelled(err)) {
      // A failed test is recorded by the server too; keep both its result and
      // the operation error visible instead of showing the previous success.
      if (selected.type === 'test') await load()
      actionError.value = errorDetails(err).message
    }
  }
  finally { working.value = ''; emit('busy', '') }
}
onMounted(load)
onBeforeUnmount(clearSecrets)
</script>

<template>
  <section class="card"><div class="card-head"><div class="card-title"><Icon name="cloud" /><h2>通知渠道<span class="badge-count">{{ channels.length }} / 10</span></h2></div><div class="actions"><button type="button" class="button secondary small" :disabled="loading || locked" @click="load"><Icon name="refresh" :size="15" />刷新</button><button type="button" class="button primary small" :disabled="locked || channels.length >= 10" @click="edit()"><Icon name="plus" :size="15" />添加渠道</button></div></div>
    <div class="notification-summary"><p>异常或恢复时发送通知。添加、编辑或启用渠道不会发送测试消息。</p><span v-if="!loading">{{ channels.filter(channel => channel.enabled).length }} 个已启用</span></div>
    <div v-if="error" class="card-body"><div class="alert error" role="alert">{{ error }}</div></div>
    <div v-if="loading && !channels.length" class="compact-empty"><span class="spinner"></span>正在读取渠道…</div><div v-else-if="!channels.length && !error" class="compact-empty"><Icon name="cloud" :size="28" /><h3>尚未配置通知渠道</h3><p>添加机器人、邮件或 Webhook，接收监测异常和恢复通知。</p><button type="button" class="button secondary" :disabled="locked" @click="edit()">添加第一个渠道</button></div>
    <div v-else class="table-wrap"><table class="data-table"><thead><tr><th>渠道</th><th>接收端</th><th>最近投递</th><th class="align-right">操作</th></tr></thead><tbody><tr v-for="channel in channels" :key="channel.id"><td><strong>{{ channel.name }}</strong><span class="secondary">{{ label(channel.type) }} · {{ channel.enabled ? '已启用' : '已停用' }}</span></td><td class="mono">{{ channel.type === 'smtp' ? channel.smtpHost : channel.endpointHint }}<span v-if="channel.type === 'smtp'" class="secondary">{{ (channel.smtpTo || []).join('、') }}</span></td><td><span>{{ deliveryLabel(channel.lastStatus) }}</span><span v-if="channel.lastAttemptAt" class="secondary">{{ formatDateTime(channel.lastAttemptAt) }}</span><span v-if="channel.lastError" class="secondary error-text">{{ channel.lastError }}</span></td><td><div class="actions"><button type="button" class="text-button" :disabled="locked" :aria-label="'编辑通知渠道：' + channel.name" @click="edit(channel)">编辑</button><button type="button" class="text-button" :disabled="locked" :aria-label="'测试通知渠道：' + channel.name" @click="askAction('test', channel)">测试</button><button type="button" class="text-button danger-text" :disabled="locked" :aria-label="'删除通知渠道：' + channel.name" @click="askAction('delete', channel)">删除</button></div></td></tr></tbody></table></div>
  </section>

  <UiOverlay :open="open" :title="editing ? '编辑通知渠道' : '添加通知渠道'" description="先选择渠道类型，再填写对应接收端信息。密钥不会回显。" variant="drawer" :busy="locked" :dirty="formDirty" @close="close">
    <form id="notification-form" class="form-body" @submit.prevent="save">
      <div v-if="formError" class="alert error" role="alert">{{ formError }}<p v-if="saveUncertain">结果尚未确认。请关闭并刷新渠道列表，核对后再操作，不要重复提交。</p></div>
      <div class="form-field"><label class="form-label" for="notification-type">渠道类型</label><UiSelect id="notification-type" v-model="form.type" :options="types" :disabled="locked || Boolean(editing)" /><span class="field-caption">{{ typeDescription }}</span></div>
      <div class="form-field"><label class="form-label" for="notification-name">渠道名称</label><input id="notification-name" v-model="form.name" maxlength="80" required :disabled="locked" placeholder="例如：运维通知" /></div>
      <div class="channel-config">
        <h3>{{ label(form.type) }}接收端</h3>
        <template v-if="form.type !== 'smtp'">
          <div class="form-field"><label class="form-label" for="notification-endpoint">HTTPS 通知地址</label><input id="notification-endpoint" v-model="form.endpoint" type="password" autocomplete="new-password" :placeholder="editing?.hasEndpoint ? '留空保留已保存地址' : 'https://…'" :disabled="locked" /><span v-if="editing?.endpointHint" class="field-caption">已保存主机：{{ editing.endpointHint }}。完整地址不会回显。</span></div>
          <div v-if="form.type !== 'wecom'" class="form-field"><label class="form-label" for="notification-secret">{{ form.type === 'webhook' ? 'Bearer 密钥（可选）' : '签名密钥（可选）' }}</label><input id="notification-secret" v-model="form.secret" type="password" autocomplete="new-password" :placeholder="editing?.hasSecret ? '留空保留已保存密钥' : '可选'" :disabled="locked" /></div>
          <p class="field-caption">仅支持 HTTPS，不接受带用户信息或片段的地址。</p>
        </template>
        <template v-else>
          <div class="form-field"><label class="form-label" for="smtp-host">SMTP 主机</label><input id="smtp-host" v-model="form.smtpHost" required :disabled="locked" placeholder="smtp.example.com" /></div>
          <div class="form-row"><div class="form-field"><label class="form-label" for="smtp-port">端口</label><input id="smtp-port" v-model.number="form.smtpPort" type="number" min="1" max="65535" required :disabled="locked" /></div><div class="form-field"><label class="form-label" for="smtp-tls">连接加密</label><UiSelect id="smtp-tls" v-model="form.smtpTLS" :options="tlsModes" :disabled="locked" /></div></div>
          <div class="form-field"><label class="form-label" for="smtp-username">用户名</label><input id="smtp-username" v-model="form.smtpUsername" autocomplete="off" :disabled="locked" /></div>
          <div class="form-field"><label class="form-label" for="smtp-password">SMTP 密码</label><input id="smtp-password" v-model="form.smtpPassword" type="password" autocomplete="new-password" :placeholder="editing?.hasPassword ? '留空保留已保存密码' : 'SMTP 密码或授权码'" :disabled="locked" /></div>
          <div class="form-field"><label class="form-label" for="smtp-from">发件人邮箱</label><input id="smtp-from" v-model="form.smtpFrom" type="email" required :disabled="locked" /></div>
          <div class="form-field"><label class="form-label" for="smtp-to">收件人邮箱</label><textarea id="smtp-to" v-model="form.smtpTo" rows="3" required :disabled="locked" placeholder="每行一个邮箱，最多 10 个"></textarea><span class="field-caption">已填写 {{ recipients.length }} / 10 个收件人。</span></div>
        </template>
      </div>
      <div class="switch-field"><div><strong>启用渠道</strong><span class="field-caption">关闭后不接收自动监测通知。</span></div><UiSwitch v-model="form.enabled" :disabled="locked" aria-label="启用通知渠道" /></div>
    </form>
    <template #footer="{ close: requestClose }"><button type="button" class="button secondary" :disabled="locked" @click="requestClose">取消</button><button type="submit" form="notification-form" class="button primary" :disabled="locked || !valid || saveUncertain">{{ working === 'notification-save' ? '保存中…' : '保存渠道' }}</button></template>
  </UiOverlay>

  <UiOverlay :open="Boolean(action)" :title="action?.type === 'test' ? '发送测试通知' : '删除通知渠道'" :description="action ? '渠道：' + action.channel.name : ''" :busy="locked" @close="closeAction">
    <div v-if="actionError" class="alert error" role="alert">{{ actionError }}<p>未自动重试。请核对接收端及最近投递状态，关闭后再决定下一步。</p></div>
    <p v-else-if="action?.type === 'test'" class="action-description">确认后将发送一条真实测试通知，停用渠道也可以测试。每个渠道至少间隔 30 秒，请勿重复发送。</p>
    <p v-else class="action-description">删除后不再通过该渠道通知。已发送消息不会被撤回。</p>
    <template #footer><button type="button" class="button secondary" :disabled="locked" @click="closeAction">{{ actionError ? '关闭' : '取消' }}</button><button type="button" class="button" :class="action?.type === 'delete' ? 'danger' : 'primary'" :disabled="locked || actionAttempted" @click="confirmAction">{{ working ? '正在处理…' : action?.type === 'test' ? '确认发送' : '确认删除' }}</button></template>
  </UiOverlay>
</template>

<style scoped>
.notification-summary{display:flex;align-items:center;justify-content:space-between;gap:18px;padding:15px 20px;background:var(--surface-2);border-bottom:1px solid var(--line);font-size:12px;color:var(--muted);line-height:1.7}.notification-summary p{margin:0}.notification-summary>span{white-space:nowrap}.channel-config{display:grid;gap:17px;border-top:1px solid var(--line);padding-top:20px}.channel-config h3{font-size:13px;margin:0;color:var(--ink)}.action-description{font-size:13px;line-height:1.8;color:var(--muted);margin:0}.danger-text{color:var(--danger)}@media(max-width:700px){.notification-summary{align-items:flex-start;flex-direction:column;gap:7px}.card-head{gap:12px;flex-wrap:wrap}}
</style>
