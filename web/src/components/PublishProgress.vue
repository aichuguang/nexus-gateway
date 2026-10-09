<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import Icon from '../Icon.vue'
import ServicePublishStatus from './ServicePublishStatus.vue'
import { formatDateTime } from '../utils/datetime'
import { isPublishActive, publishStatus } from '../usePublishJobs.js'

const props = defineProps({ job: { type: Object, required: true }, error: String })
const emit = defineEmits(['close', 'refresh', 'goto', 'preview'])
const closeButton = ref(null)
const dialog = ref(null)
let returnFocus
const active = computed(() => isPublishActive(props.job))
const waiting = computed(() => props.job.status === 'waiting')
const labels = { pending: '等待执行', running: '进行中', waiting: '等待生效', succeeded: '已完成', failed: '未完成', skipped: '已跳过', unknown: '待核对', stopped: '未执行' }
const stages = computed(() => {
  const all = props.job.steps || []
  return [
    { kind: 'local', name: '本机配置', show: true },
    { kind: 'edgeone', name: 'EdgeOne', show: (props.job.edge && props.job.edgeProvider !== 'cloudflare' && !all.some(step => step.kind === 'cloudflare')) || all.some(step => step.kind === 'edgeone') },
    { kind: 'cloudflare', name: 'Cloudflare', show: props.job.edgeProvider === 'cloudflare' || all.some(step => step.kind === 'cloudflare') },
    { kind: 'dns', name: '域名解析', show: props.job.dns || all.some(step => step.kind === 'dns') },
    { kind: 'verification', name: '访问检查（独立）', show: all.some(step => step.kind === 'verification') },
  ].filter(stage => stage.show).map(stage => {
    const steps = all.filter(step => step.kind === stage.kind)
    let status = active.value ? 'pending' : 'stopped'
    if (steps.some(step => step.status === 'unknown')) status = 'unknown'
    else if (steps.some(step => step.status === 'waiting')) status = 'waiting'
    else if (steps.some(step => step.status === 'running')) status = 'running'
    else if (steps.some(step => step.status === 'failed')) status = 'failed'
    else if (steps.some(step => step.status === 'skipped')) status = 'skipped'
    else if (steps.length) status = props.job.status === 'running' && all[all.length - 1]?.kind === stage.kind ? 'running' : 'succeeded'
    return { ...stage, steps, status }
  })
})
const needsReview = computed(() => ['needs_review', 'interrupted'].includes(props.job.status))
const accessVerificationOnly = computed(() => waiting.value && stages.value.some(stage => stage.kind === 'verification' && stage.status === 'waiting') && ['cloudflare', 'dns'].every(kind => stages.value.some(stage => stage.kind === kind && stage.status === 'succeeded')))
const hasFailure = computed(() => !active.value && props.job.status !== 'succeeded')
const outcomeDismissed = ref(false)
const dismissalError = ref('')
const dismissalKey = computed(() => props.job.id ? `w103d-publish-outcome-v1:${props.job.id}` : '')
const outcomeVersion = computed(() => JSON.stringify([props.job.status, props.job.updatedAt]))
const dismissCopy = { hide: '不再提示该次结果', restore: '重新显示该次结果提示', help: '仅在当前浏览器隐藏这条历史提示，不删除记录，也不代表问题已解决。', error: '无法保存提示偏好，请检查浏览器存储后重试。' }
watch([dismissalKey, outcomeVersion], () => {
  outcomeDismissed.value = false
  dismissalError.value = ''
  try { outcomeDismissed.value = Boolean(dismissalKey.value && localStorage.getItem(dismissalKey.value) === outcomeVersion.value) } catch { /* Keep the outcome visible when storage is unavailable. */ }
}, { immediate: true })
function setOutcomeDismissed(hidden) {
  if (!hasFailure.value || !dismissalKey.value) return
  try {
    if (hidden) localStorage.setItem(dismissalKey.value, outcomeVersion.value)
    else localStorage.removeItem(dismissalKey.value)
    outcomeDismissed.value = hidden
    dismissalError.value = ''
    nextTick(() => closeButton.value?.focus())
  } catch { dismissalError.value = dismissCopy.error }
}
const failedSteps = computed(() => (props.job.steps || []).filter(step => ['failed', 'unknown'].includes(step.status)))
const outcomeCopy = { title: '该次发布结果：', help: '这是该次任务的执行结果，不代表当前云端状态；当前配置与解析请以下方重新核对结果为准。', retry: '重新预览' }
const completedCopy = { scope: '该次任务完成不代表网站可访问或 HTTPS 正常。访问检查独立进行，不作为解析切换门槛；当前 CNAME 以下方重新读取结果为准。' }
function statusLabel(kind, status) { return kind === 'verification' && status === 'waiting' ? '正在重查访问' : labels[status] }
function icon(status) { return status === 'succeeded' ? 'check' : ['failed', 'unknown'].includes(status) ? 'warning' : 'info' }
function trapFocus(event) {
  if (event.key !== 'Tab') return
  const targets = Array.from(dialog.value?.querySelectorAll('button, summary, a[href], [tabindex="0"]') || []).filter(target => !target.disabled)
  const first = targets[0], last = targets[targets.length - 1]
  if (!first) return
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus() }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
}
onMounted(() => { returnFocus = document.activeElement; nextTick(() => closeButton.value?.focus()) })
onBeforeUnmount(() => { if (document.contains(returnFocus)) returnFocus?.focus?.({ preventScroll: true }) })
</script>

<template>
  <div class="modal-overlay" @click.self="emit('close')" @keydown.esc.stop="emit('close')" @keydown="trapFocus">
    <div ref="dialog" class="modal wide publish-progress" role="dialog" aria-modal="true" aria-labelledby="publish-progress-title">
      <div class="modal-header"><div><span class="eyebrow">PUBLISH STATUS</span><h2 id="publish-progress-title">{{ job.serviceName }} · {{ active ? publishStatus(job) : '发布状态' }}</h2></div><button ref="closeButton" class="icon-button" aria-label="关闭发布进度" @click="emit('close')"><Icon name="close" :size="18" /></button></div>
      <div class="modal-body">
        <section v-if="hasFailure && !outcomeDismissed" class="alert warning publish-outcome" role="status" data-publish-outcome>
          <strong>{{ outcomeCopy.title }}{{ publishStatus(job) }}</strong>
          <p v-if="job.message">{{ job.message }}</p>
          <ul v-if="failedSteps.length" class="publish-failures">
            <li v-for="(step, index) in failedSteps" :key="index">
              <strong>{{ step.role || stages.find(stage => stage.kind === step.kind)?.name || step.kind }}</strong>
              <code v-if="step.host">{{ step.host }}</code>
              <span class="small">{{ statusLabel(step.kind, step.status) }}</span>
              <p v-if="step.message">{{ step.message }}</p>
            </li>
          </ul>
          <p class="field-caption">{{ outcomeCopy.help }}</p>
          <button v-if="['failed', 'partial_failed'].includes(job.status) && !error" class="button secondary small" @click="emit('preview', job.serviceId)">{{ outcomeCopy.retry }}</button>
          <button v-if="dismissalKey" class="button secondary small" data-dismiss-publish-outcome @click="setOutcomeDismissed(true)">{{ dismissCopy.hide }}</button>
          <p class="field-caption">{{ dismissCopy.help }}</p>
        </section>
        <p v-if="dismissalError" class="error-text" role="alert">{{ dismissalError }}</p>
        <section v-if="job.status === 'succeeded' && job.edgeProvider === 'cloudflare'" class="alert info publish-outcome" role="status" data-publish-completed>
          <strong>{{ outcomeCopy.title }}{{ publishStatus(job) }}</strong>
          <p v-if="job.message">{{ job.message }}</p>
          <p class="field-caption">{{ completedCopy.scope }}</p>
        </section>
        <ServicePublishStatus v-if="!active" :service-id="job.serviceId" />
        <section v-if="waiting" class="publish-waiting" role="status">
          <h3>{{ accessVerificationOnly ? '证书已生效，正在核验访问' : '等待自动发布完成' }}</h3>
          <p>{{ job.message || '正在等待域名验证、边缘证书或回源就绪。' }}</p>
          <p>{{ accessVerificationOnly ? '仅重查实际访问，不会重新创建 CF 接入或改写 DNS；可以关闭窗口。' : '系统会自动检查并继续发布，无需操作；可以关闭窗口。' }}</p>
          <p v-if="job.nextAttemptAt" class="field-caption">下次自动检查：{{ formatDateTime(job.nextAttemptAt) }}</p>
        </section>
        <p v-if="job.status === 'running'" class="page-help">可以关闭此窗口，发布会继续。请稍后查看结果。</p>
        <div v-if="error" class="alert warning" role="alert"><Icon name="warning" /><span>{{ error }}。页面断连不代表发布失败。</span><button class="text-button" @click="emit('refresh')">重新读取</button></div>
        <details class="execution-history" :open="job.status === 'running'">
        <summary>{{ active ? (waiting ? '查看执行详情' : '实时发布进度') : '本次发布历史（非当前状态）' }}</summary>
        <p v-if="!active" class="field-caption">以下记录保留当时的执行结果，不代表当前状态；在解析页等位置完成的后续操作，请以上方重新核对结果为准。</p>
        <p v-if="!active" class="field-caption">当时结果：{{ publishStatus(job) }}</p>
        <button v-if="hasFailure && outcomeDismissed" class="text-button" data-restore-publish-outcome @click="setOutcomeDismissed(false)">{{ dismissCopy.restore }}</button>
        <div v-if="job.message && !waiting" :class="['alert', hasFailure ? 'warning' : 'info']" role="status"><Icon :name="hasFailure ? 'warning' : 'info'" /><span>{{ job.message }}</span></div>
        <ol class="progress-stages">
          <li v-for="stage in stages" :key="stage.kind" :data-status="stage.status">
            <div class="progress-stage-title"><span v-if="stage.status === 'running'" class="spinner small"></span><Icon v-else :name="icon(stage.status)" :size="18" /><strong>{{ stage.name }}</strong><span :class="['pill', stage.status === 'succeeded' ? 'private' : stage.status === 'failed' ? 'amber' : 'neutral']">{{ statusLabel(stage.kind, stage.status) }}</span></div>
            <div v-for="step in stage.steps" :key="step.host || stage.kind" class="progress-step">
              <div><strong>{{ step.role || stage.name }}</strong><code v-if="step.host">{{ step.host }}</code><span class="small muted">{{ statusLabel(stage.kind, step.status) }}</span></div>
              <p v-if="step.message" :class="{ 'error-text': ['failed', 'unknown'].includes(step.status) }">{{ step.message }}</p>
            </div>
          </li>
        </ol>
        <p v-if="needsReview" class="field-caption">请先核对实际配置。中断的云端请求可能已经生效，此处不会直接重试或自动接管同名资源。</p>
        <div v-if="hasFailure" class="actions"><button v-if="job.edge || needsReview" class="button secondary small" @click="emit('goto', 'edgeone')">去边缘加速核对</button><button v-if="job.dns || needsReview" class="button secondary small" @click="emit('goto', 'resolve')">去解析页核对</button></div>
        <details class="raw-details"><summary>记录详情</summary><dl class="definition"><div><dt>任务</dt><dd class="mono">{{ job.id }}</dd></div><div><dt>配置版本</dt><dd>{{ job.revision }}</dd></div><div><dt>开始时间</dt><dd>{{ formatDateTime(job.createdAt) }}</dd></div><div><dt>最后更新</dt><dd>{{ formatDateTime(job.updatedAt) }}</dd></div></dl></details>
        <p class="field-caption">以上是配置执行结果，不代表公网可达性已验证。</p>
        </details>
      </div>
      <div class="modal-footer"><button class="button primary" @click="emit('close')">{{ active ? '关闭，后台继续' : '关闭' }}</button></div>
    </div>
  </div>
</template>

<style scoped>
.progress-stages { list-style: none; padding: 0; margin: 20px 0; display: grid; gap: 12px; }
.progress-stages > li { border: 1px solid var(--line); border-radius: 12px; padding: 16px; min-width: 0; }
.progress-stage-title { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.progress-stage-title .pill { margin-left: auto; }
.progress-step { margin-top: 12px; padding-top: 12px; border-top: 1px solid var(--line); }
.progress-step > div { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.progress-step code, .progress-step p, .definition dd { overflow-wrap: anywhere; }
.progress-step p { margin: 6px 0 0; font-size: 13px; }
.publish-progress .raw-details { margin-top: 20px; }
.execution-history { margin-top: 20px; min-width: 0; }
.publish-outcome { display: block; overflow-wrap: anywhere; }
.publish-outcome p { margin: 8px 0; }
.publish-failures { margin: 12px 0; padding-left: 20px; }
.publish-failures li + li { margin-top: 12px; }
.publish-failures code, .publish-failures .small { margin-left: 8px; }
.execution-history > summary { cursor: pointer; color: var(--muted); font-size: 13px; line-height: 1.6; }
.publish-waiting { padding: 16px; border: 1px solid var(--line); border-radius: 12px; }
.publish-waiting h3 { margin: 0; font-size: 16px; }
@media (max-width: 540px) { .progress-stages > li { padding: 12px; } .publish-progress .actions { flex-wrap: wrap; } }
</style>
