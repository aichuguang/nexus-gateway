<script setup>
import { formatDateTime } from './utils/datetime'
import { computed, ref } from 'vue'
import Icon from './Icon.vue'
import UiCopyValue from './components/ui/UiCopyValue.vue'
import UiFileUpload from './components/ui/UiFileUpload.vue'
import UiOverlay from './components/ui/UiOverlay.vue'
import UiTooltip from './components/ui/UiTooltip.vue'
import { usePageScope } from './page-scope.ts'
import { isRequestCancelled } from './request.ts'
import { certificateStatus as certState } from './certificate-status.js'
import { buildCertificateForm, certificateDeployCommand, certificateInboxPath } from './certificate-import.js'

const props = defineProps({ state: Object, busy: String })
const emitEvent = defineEmits(['state', 'notice', 'busy'])
const scope = usePageScope(), api = scope.api, emit = scope.guard(emitEvent)
const creating = ref(false), name = ref(''), mode = ref('certd'), saving = ref(false)
const certificateText = ref(''), privateKeyText = ref('')
const importing = ref(null), importError = ref(''), certificateFile = ref(null), keyFile = ref(null)
const uploadBusy = computed(() => Boolean(props.busy) || saving.value)
const uploadDirty = computed(() => Boolean((!importing.value && name.value) || certificateText.value || privateKeyText.value || certificateFile.value || keyFile.value))
const canSubmit = computed(() => (importing.value || name.value.trim()) && (mode.value === 'certd' || (mode.value === 'paste' ? certificateText.value.trim() && privateKeyText.value.trim() : certificateFile.value && keyFile.value)))
const inboxPath = cert => certificateInboxPath(cert, props.state?.runtime)
const deployCommand = computed(() => importing.value ? certificateDeployCommand(importing.value, props.state?.runtime) : '')
const deployHelp = '两份文件上传完成后，再执行上方应用命令。命令优先使用当前网关的实际程序和数据目录，新安装默认程序为 /opt/gateway/gateway；未提供运行信息的自定义安装请核对程序路径。网关服务必须运行，命令需要 root 或明确的最小授权。'
const deleting = ref(null), selectedId = ref('')
function certificatePriority(cert) {
  const status = certState(cert)
  return !status.usable ? 0 : status.tone === 'amber' ? 1 : 2
}
const certificates = computed(() => [...(props.state?.certificates || [])].sort((a, b) => certificatePriority(a) - certificatePriority(b)))
const selected = computed(() => certificates.value.find(cert => cert.id === selectedId.value) || null)
// Deletion also checks the applied snapshot on the server, not only this draft.
function usage(cert) { return (props.state?.services || []).filter(service => service.certificateId === cert.id) }
function edgeSummary(cert) {
  const bindings = cert.edgeOne?.bindings || []
  if (!bindings.length) return { label: '尚无绑定记录', tone: 'neutral' }
  if (bindings.some(binding => binding.error || binding.fingerprint !== cert.fingerprint)) return { label: '绑定待核对', tone: 'amber' }
  return { label: '绑定已提交', tone: 'private' }
}
function openCreate() { if (!uploadBusy.value) { closeImport(); mode.value = 'certd'; creating.value = true } }
function openImport(cert) {
  if (uploadBusy.value) return
  closeImport(); importing.value = cert; name.value = cert.name; mode.value = 'file'; creating.value = true
}
function closeImport() {
  creating.value = false; importing.value = null; name.value = ''; certificateFile.value = null; keyFile.value = null
  certificateText.value = ''; privateKeyText.value = ''; importError.value = ''
}
async function submitImport() {
  if (uploadBusy.value || !canSubmit.value) return
  saving.value = true; emit('busy', 'certificate-import'); importError.value = ''
  try {
    // Validate inputs before creating a slot; both entry methods share the server's PEM validator.
    const form = mode.value === 'certd' ? null : buildCertificateForm(mode.value, certificateText.value, privateKeyText.value, certificateFile.value, keyFile.value)
    if (!importing.value) {
      if (new TextEncoder().encode(name.value.trim()).length > 100) throw new Error('证书名称不能超过 100 个 UTF-8 字节')
      const previous = new Set(certificates.value.map(cert => cert.id))
      const next = await api.createCertificate(name.value.trim())
      emit('state', next)
      // Recovery can return a different earlier creation: never upload secrets into a guessed slot.
      if (next.operationRecoveryMessage) { closeImport(); emit('notice', next.operationRecoveryMessage); return }
      const added = next.certificates.filter(cert => !previous.has(cert.id))
      if (added.length !== 1) throw new Error('无法确定新建槽位，请关闭窗口后在证书列表中选择对应槽位继续。')
      importing.value = added[0]
    }
    if (mode.value === 'certd') { emit('notice', '证书槽已创建，请按下方流程配置 certd 推送'); return }
    const next = await api.importCertificate(importing.value.id, form)
    emit('state', next); emit('notice', '证书已导入；若已被运行中的服务使用则立即生效，边缘绑定同步结果请查看详情。'); closeImport()
  } catch (err) {
    if (!isRequestCancelled(err)) importError.value = err.message + (importing.value && !importing.value.ready ? ' 当前证书槽已保留，可在此修正后重试，无需重复创建。' : '')
  } finally { saving.value = false; emit('busy', '') }
}
function openDelete(cert) { if (!props.busy) deleting.value = cert }
async function remove(cert) {
  if (props.busy) return
  emit('busy', 'certificate-delete')
  try {
    emit('state', await api.deleteCertificate(cert.id))
    if (selectedId.value === cert.id) selectedId.value = ''
    deleting.value = null
    emit('notice', '证书槽已删除；磁盘上的证书文件被保留以便恢复')
  } catch (err) { if (isRequestCancelled(err)) return; emit('notice', { type: 'error', message: err.message }); deleting.value = null }
  finally { emit('busy', '') }
}
defineExpose({ openCreate })
</script>

<template>
  <section v-if="!certificates.length" class="card empty-state">
    <div class="empty-icon"><Icon name="shield" :size="34" /></div><h3>还没有证书槽</h3><p>通过 certd 部署、粘贴内容或上传文件添加证书，再关联到域名服务。</p><button class="button primary" :disabled="Boolean(busy)" @click="openCreate">上传证书</button>
  </section>
  <section v-else class="card certificate-list">
    <div class="card-head"><div><h2>证书资源 <span class="badge-count">{{ certificates.length }}</span></h2><p class="field-caption">待处理与即将到期的证书优先显示，选择证书查看详情。</p></div></div>
    <div class="table-wrap"><table class="data-table certificate-table">
      <thead><tr><th>证书</th><th>覆盖域名</th><th>到期日期</th><th>关联服务</th><th class="certificate-status-cell">状态</th><th class="align-right">操作</th></tr></thead>
      <tbody><tr v-for="cert in certificates" :key="cert.id">
        <td><button class="certificate-name" :aria-label="`查看证书详情：${cert.name}`" @click="selectedId = cert.id"><Icon name="shield" :size="18" /><strong>{{ cert.name }}</strong></button></td>
        <td><span v-if="cert.domains?.length" class="mono certificate-domain">{{ cert.domains[0] }}</span><span v-else class="muted">等待导入材料</span><span v-if="cert.domains?.length > 1" class="secondary">另有 {{ cert.domains.length - 1 }} 个域名</span></td>
        <td class="mono">{{ formatDateTime(cert.notAfter, '待导入') }}</td><td>{{ usage(cert).length }} 个服务</td>
        <td class="certificate-status-cell"><UiTooltip v-if="certState(cert).label === '可用'" :text="`可用 · ${certState(cert).note}`"><span class="certificate-ready" role="img" aria-label="可用" tabindex="0"><Icon name="check" :size="19" /></span></UiTooltip><span v-else :class="['pill', certState(cert).tone]">{{ certState(cert).label }}</span></td>
        <td><div class="actions certificate-actions"><button class="button secondary small" :disabled="Boolean(busy)" @click="openImport(cert)">{{ cert.ready ? '重新导入' : '导入证书' }}</button><button class="icon-button danger-hover" :disabled="Boolean(busy)" :aria-label="`删除证书槽：${cert.name}`" @click="openDelete(cert)"><Icon name="trash" :size="17" /></button></div></td>
      </tr></tbody>
    </table></div><div class="table-meta">共 {{ certificates.length }} 个证书槽 · 域名服务使用校验通过的证书材料</div>
  </section>

  <UiOverlay :open="Boolean(selected)" :title="selected?.name || '证书详情'" description="证书材料、当前草稿关联与部署记录" variant="drawer" @close="selectedId = ''">
    <template v-if="selected">
      <section class="certificate-detail-section">
        <div class="certificate-detail-heading"><h3>材料状态</h3><span :class="['pill', certState(selected).tone]">{{ certState(selected).label }}</span></div><p :class="['field-caption', { 'error-text': certState(selected).tone === 'amber' }]">{{ certState(selected).note }}</p>
        <dl class="definition"><div><dt>生效日期</dt><dd>{{ formatDateTime(selected.notBefore, '待导入') }}</dd></div><div><dt>到期日期</dt><dd>{{ formatDateTime(selected.notAfter, '待导入') }}</dd></div></dl>
        <h4>域名覆盖范围</h4><div v-if="selected.domains?.length" class="certificate-domains"><UiCopyValue v-for="domain in selected.domains" :key="domain" :value="domain" /></div><p v-else class="field-caption">等待导入证书材料。</p>
        <h4>完整指纹</h4><div class="certificate-fingerprint"><UiCopyValue v-if="selected.fingerprint" :value="selected.fingerprint" :label="selected.name" /><span v-else class="muted">尚未生成证书指纹</span></div>
      </section>
      <section class="certificate-detail-section">
        <div class="certificate-detail-heading"><h3>关联服务</h3><span class="small muted">当前草稿 · {{ usage(selected).length }} 个</span></div><p v-if="!usage(selected).length" class="field-caption">还没有服务使用这张证书。</p>
        <div v-for="service in usage(selected)" :key="service.id" class="certificate-service"><div><strong>{{ service.name }}</strong><span class="secondary mono">{{ (service.domains || []).join('、') }}</span></div><span :class="['pill', service.enabled ? 'private' : 'neutral']">{{ service.enabled ? '已启用' : '已停用' }}</span></div><p class="field-caption">删除时还会核对已应用服务的引用。</p>
      </section>
      <section class="certificate-detail-section">
        <h3>证书部署</h3><dl class="definition"><div><dt>边缘绑定</dt><dd><span :class="['pill', edgeSummary(selected).tone]">{{ edgeSummary(selected).label }}</span></dd></div></dl><div v-if="inboxPath(selected)" class="address-block">证书收件目录<UiCopyValue :value="inboxPath(selected)" :label="selected.name" /></div><p v-if="selected.edgeOne?.bindings?.length" class="field-caption">外部可达性未验证。</p><div v-for="(binding, index) in (selected.edgeOne?.bindings || []).filter(item => item.error)" :key="index" class="alert warning"><Icon name="warning" :size="15" /><span>{{ binding.zoneName || binding.zoneId }}：{{ binding.error }}</span></div>
      </section>
    </template>
    <template #footer="{ close }"><button class="button secondary" @click="close">返回列表</button><button v-if="selected" class="button primary" :disabled="Boolean(busy)" @click="openImport(selected)"><Icon name="upload" :size="15" />{{ selected.ready ? '重新导入' : '导入证书' }}</button></template>
  </UiOverlay>

  <UiOverlay :open="creating" :title="importing ? '证书部署与更新' : '上传证书'" description="选择一种方式，使用同一个证书槽管理证书材料。" :busy="uploadBusy" :dirty="uploadDirty" @close="closeImport">
    <fieldset class="certificate-methods" :disabled="uploadBusy">
      <legend class="form-label">添加方式</legend>
      <label v-for="option in [{ value: 'certd', label: '通过 certd 部署' }, { value: 'paste', label: '粘贴证书' }, { value: 'file', label: '上传文件' }]" :key="option.value" :class="{ active: mode === option.value }">
        <input v-model="mode" type="radio" name="certificate-method" :value="option.value" @change="importError = ''" />{{ option.label }}
      </label>
    </fieldset>
    <div v-if="importError" class="alert error" role="alert"><Icon name="warning" /><span>{{ importError }}</span></div>
    <div class="form-field"><label class="form-label" for="certificate-name">{{ mode === 'certd' ? '证书槽名称' : '证书名称' }}</label><input id="certificate-name" v-model="name" :disabled="uploadBusy || Boolean(importing)" maxlength="100" placeholder="例如：example.com 通配符证书" autocomplete="off" /></div>
    <template v-if="mode === 'certd'">
      <p class="field-caption">证书槽是固定收件位置。certd 续期后向同一个槽推送，已有服务关联保持不变。</p>
      <div v-if="inboxPath(importing)" class="certificate-deploy-paths">
        <div class="address-block">证书槽 ID<UiCopyValue :value="importing.id" /></div>
        <div class="address-block">证书收件目录<UiCopyValue :value="inboxPath(importing)" /></div>
        <div class="address-block">证书链目标路径<UiCopyValue :value="`${inboxPath(importing)}/fullchain.pem`" /></div>
        <div class="address-block">私钥目标路径<UiCopyValue :value="`${inboxPath(importing)}/privkey.pem`" /></div>
        <div v-if="deployCommand" class="address-block">上传后执行的命令<UiCopyValue :value="deployCommand" /></div>
      </div>
      <p v-else class="hint-panel">填写名称并创建后，这里会显示真实收件目录、文件路径和应用命令。</p>
      <ol class="certificate-deploy-steps">
        <li>在 certd 证书流水线中添加「主机 - 部署证书到 SSH 主机」任务，配置网关服务器的 SSH 地址和授权。凭据只填写在 certd 中。</li>
        <li>选择 PEM 格式，将完整证书链（crt，不是仅叶证书 oc）推送到上方 fullchain.pem 路径，将匹配的私钥推送到 privkey.pem 路径。</li>
        <li>{{ deployHelp }}</li>
        <li>先手动运行一次 certd 流水线，再刷新本页面核对域名、有效期和状态。首次使用还需在域名服务中选择证书并应用配置；仅上传文件不会自动生效。</li>
      </ol>
    </template>
    <template v-else>
      <div v-if="mode === 'paste'" class="certificate-paste">
        <label class="form-label" for="certificate-pem">完整证书链（PEM）</label><textarea id="certificate-pem" v-model="certificateText" :disabled="uploadBusy" rows="6" spellcheck="false" autocomplete="off" placeholder="-----BEGIN CERTIFICATE-----" />
        <label class="form-label" for="certificate-private-key">私钥（PEM）</label><textarea id="certificate-private-key" v-model="privateKeyText" :disabled="uploadBusy" rows="5" spellcheck="false" autocomplete="off" placeholder="-----BEGIN PRIVATE KEY-----" />
      </div>
      <div v-else class="form-row certificate-upload-files"><UiFileUpload id="certificate-file" v-model="certificateFile" label="fullchain.pem · 完整证书链" accept=".pem,.crt,.cer,application/x-pem-file" :disabled="uploadBusy" /><UiFileUpload id="certificate-key" v-model="keyFile" label="privkey.pem · PEM 私钥" accept=".pem,.key,application/x-pem-file" :disabled="uploadBusy" /></div>
      <p class="field-caption">每项不超过 2 MiB，仅支持 PEM 编码及未加密私钥，不支持 PFX / P12 / DER。私钥不会回显，关闭窗口后清空输入。</p>
      <p class="field-caption">手动导入不自动续期；如果 certd 仍向同一槽推送，后续推送可能覆盖手动证书。</p>
      <div v-if="importing?.ready" class="alert warning"><Icon name="warning" :size="16" /><span>如果此证书正被已应用的服务使用，更新将立即生效，并自动同步已有边缘绑定，不必等待发布草稿。</span></div>
    </template>
    <template #footer="{ close }"><button class="button secondary" :disabled="uploadBusy" @click="close">{{ mode === 'certd' && importing ? '完成' : '取消' }}</button><button v-if="mode !== 'certd' || !importing" class="button primary" :disabled="uploadBusy || !canSubmit" @click="submitImport">{{ saving ? '处理中…' : mode === 'certd' ? '创建并查看部署方法' : '导入并校验' }}</button></template>
  </UiOverlay>
  <UiOverlay :open="Boolean(deleting)" title="删除证书槽" :busy="Boolean(busy)" @close="deleting = null">
    <template v-if="deleting"><p class="delete-message">确认删除证书槽「{{ deleting.name }}」？若草稿或已应用的服务仍引用它，删除会被拒绝。</p><div v-if="usage(deleting).length" class="hint-panel"><Icon name="warning" :size="17" />仍被 {{ usage(deleting).map(service => service.name).join('、') }} 引用；请先解除引用并应用配置。</div></template>
    <template #footer="{ close }"><button class="button secondary" :disabled="Boolean(busy)" @click="close">取消</button><button class="button danger" :disabled="Boolean(busy)" @click="remove(deleting)">{{ busy === 'certificate-delete' ? '删除中…' : '删除槽位' }}</button></template>
  </UiOverlay>
</template>

<style scoped>
.certificate-upload-files{margin-top:20px}
.certificate-methods{display:flex;flex-wrap:wrap;gap:8px;border:0;padding:0;margin:0 0 20px}
.certificate-methods legend{margin-bottom:10px}
.certificate-methods label{display:flex;align-items:center;gap:7px;flex:1;white-space:nowrap;border:1px solid var(--line);border-radius:8px;padding:11px 10px;cursor:pointer;font-size:12px}
.certificate-methods label.active{border-color:var(--blue);background:var(--blue-bg);color:var(--blue)}
.certificate-methods input{appearance:none;width:14px;height:14px;flex:0 0 14px;margin:0;border:1px solid var(--line);border-radius:50%;background:var(--surface);cursor:inherit}
.certificate-methods input:checked{border-color:var(--blue);background:var(--blue);box-shadow:inset 0 0 0 3px var(--surface)}
.certificate-methods input:focus-visible{outline:2px solid var(--blue);outline-offset:2px}
.certificate-paste textarea{font-family:monospace;margin-bottom:16px;resize:vertical}
.certificate-deploy-steps{padding-left:21px;font-size:13px;line-height:1.8;color:var(--muted)}
.certificate-deploy-steps li+li{margin-top:10px}
.certificate-deploy-paths .address-block{margin:10px 0;overflow-wrap:anywhere}
.certificate-deploy-paths :deep(.copy-value){white-space:normal;overflow-wrap:anywhere;word-break:break-all}
.certificate-table{min-width:700px}
.certificate-name{display:flex;align-items:center;gap:8px;border:0;background:none;color:var(--ink);padding:0;text-align:left;cursor:pointer}
.certificate-status-cell{text-align:center;white-space:nowrap;width:100px}
.certificate-ready{display:inline-flex;align-items:center;justify-content:center;vertical-align:middle;color:var(--green);padding:4px;border-radius:4px}
.certificate-ready:focus-visible{outline:2px solid var(--blue);outline-offset:2px}
.certificate-name:hover strong{color:var(--blue)}
.certificate-name strong,.certificate-domain{overflow-wrap:anywhere}
.certificate-actions{justify-content:flex-end;flex-wrap:nowrap}
.certificate-detail-section+.certificate-detail-section{border-top:1px solid var(--line);margin-top:24px;padding-top:24px}
.certificate-detail-section h3{margin:0;font-size:15px}
.certificate-detail-section h4{font-size:12px;margin:18px 0 8px}
.certificate-detail-heading{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:10px}
.certificate-domains{display:grid;gap:8px}
.certificate-fingerprint{overflow-wrap:anywhere;min-width:0}
.certificate-fingerprint :deep(.copy-value){max-width:100%;white-space:normal;word-break:break-all}
.certificate-service{display:flex;align-items:center;justify-content:space-between;gap:16px;padding:12px 0;border-bottom:1px solid var(--line)}
.certificate-service>div{min-width:0;overflow-wrap:anywhere}
.certificate-detail-section .definition{margin-top:14px}
.certificate-list .card-head .field-caption{margin:5px 0 0}
@media(max-width:640px){.certificate-list .card-head{align-items:flex-start;gap:12px}.certificate-detail-heading{align-items:flex-start;flex-wrap:wrap}}
</style>
