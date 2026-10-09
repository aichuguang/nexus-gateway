// @ts-check
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api } from './api.js'
import { errorDetails } from './request.ts'
/** @typedef {import('./api-types').PublishJob} PublishJob */
/** @typedef {import('./api-types').PublishJobsResponse} PublishJobsResponse */
/** @typedef {import('./api-types').PublishSubmission} PublishSubmission */

/** @param {PublishJob | null | undefined} job */
export function publishStatus(job) {
  if (!job) return '状态未知'
  if (job.status === 'succeeded' && job.edgeProvider === 'cloudflare' && job.dns) return '配置与云端解析已更新'
  return ({ running: '发布中', waiting: '等待生效', succeeded: '配置已完成', partial_failed: '部分未完成', failed: '发布未完成', needs_review: '结果待确认', interrupted: '执行中断，需核对' })[job.status] || '状态未知'
}

/** @param {PublishJob | null | undefined} job */
export function isPublishActive(job) { return Boolean(job && ['running', 'waiting'].includes(job.status)) }

export function publishRequestId() {
  const bytes = new Uint8Array(16)
  globalThis.crypto.getRandomValues(bytes)
  return `publish_${Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('')}`
}

// The server owns execution and history. This hook only reconnects the view; no
// browser timer, storage entry or successful toast authorizes a cloud write.
/**
 * @param {import('vue').Ref<boolean>} enabled
 * @param {(job: PublishJob) => void | Promise<void>} onFinished
 */
export function usePublishJobs(enabled, onFinished = () => {}) {
  const jobs = ref(/** @type {PublishJob[]} */ ([]))
  const ready = ref(false)
  const error = ref('')
  const submitError = ref('')
  const starting = ref(false)
  const uncertain = ref(/** @type {PublishSubmission | null} */ (null))
  const storageKey = 'w103d-publish-pending-v1'
  function clearPending() { uncertain.value = null; localStorage.removeItem(storageKey) }
  function restorePending() {
    try {
      const value = JSON.parse(localStorage.getItem(storageKey) || 'null')
      if (value && typeof value.serviceId === 'string' && /^[A-Za-z0-9_-]{16,80}$/.test(value.requestId) && Number.isSafeInteger(value.revision) && /^[a-f0-9]{64}$/.test(value.publishFingerprint)) uncertain.value = value
    } catch { submitError.value = '无法读取发布恢复上下文，请检查浏览器存储。' }
  }
  const selectedId = ref('')
  const active = computed(() => jobs.value.find(job => job.status === 'running') || jobs.value.find(job => job.status === 'waiting') || null)
  const selected = computed(() => jobs.value.find(job => job.id === selectedId.value) || null)
  // A missing progress API is a local capability failure, not evidence that a
  // publish is running. Only a known task or our own unresolved POST blocks edits.
  const locked = computed(() => Boolean(jobs.value.some(job => job.status === 'running') || starting.value || uncertain.value))
  const available = computed(() => ready.value && !error.value)
  let unsupported = false
  /** @type {ReturnType<typeof setTimeout> | undefined} */
  let timer
  /** @type {AbortController | null} */
  let readController = null
  /** @type {Promise<PublishJobsResponse | null> | null} */
  let inFlight = null
  let epoch = 0
  let acceptedVersion = 0
  let failures = 0
  let disposed = false
  const notifiedJobs = new Set()

  /** @param {PublishJob} job */
  function notifyFinished(job) {
    if (!job.id || !['succeeded', 'partial_failed', 'failed', 'needs_review', 'interrupted'].includes(job.status) || notifiedJobs.has(job.id)) return
    notifiedJobs.add(job.id)
    const current = epoch
    Promise.resolve().then(() => {
      if (!disposed && enabled.value && current === epoch) return onFinished(job)
    }).catch(() => {})
  }

  function clearTimer() { clearTimeout(timer); timer = undefined }
  function schedule() {
    clearTimer()
    if (disposed || !enabled.value || document.visibilityState === 'hidden') return
    if (unsupported && !active.value && !uncertain.value) return
    if (active.value || error.value || uncertain.value) {
      timer = setTimeout(refresh, Math.min(2000 * (2 ** failures), 30000))
    }
  }

  /** @returns {Promise<PublishJobsResponse | null>} */
  async function refresh() {
    if (!enabled.value || disposed) return null
    if (inFlight) return inFlight
    clearTimer()
    const current = epoch
    const version = acceptedVersion
    const controller = new AbortController()
    readController = controller
    const request = (async () => {
      try {
        const result = await Promise.resolve().then(() => api.publishJobs({ signal: controller.signal }))
        if (current !== epoch || !enabled.value) return null
        // A read started before a successful POST may contain an older history.
        // It must not erase the accepted task and stop its polling/write guard.
        if (version !== acceptedVersion) return null
        if (!Array.isArray(result.jobs)) throw new Error('发布记录格式不正确，请重新读取')
        const previous = new Map(jobs.value.map(job => [job.id, job.status]))
        jobs.value = result.jobs
        error.value = result.error || ''
        ready.value = true
        unsupported = false
        failures = error.value ? Math.min(failures + 1, 4) : 0
        if (uncertain.value) {
          const found = jobs.value.find(job => job.requestId === uncertain.value?.requestId)
          if (found) {
            selectedId.value = found.id
            clearPending()
            submitError.value = ''
            notifyFinished(found)
          } else if (api.operationReceipt) {
            try {
              const receipt = await api.operationReceipt(uncertain.value.requestId, { signal: controller.signal })
              if (current !== epoch || !enabled.value || version !== acceptedVersion) return null
              if (receipt.job) {
                jobs.value = [receipt.job, ...jobs.value.filter(job => job.id !== receipt.job?.id)]
                selectedId.value = receipt.job.id
                clearPending()
                submitError.value = ''
                notifyFinished(receipt.job)
              }
            } catch (caught) {
              if (errorDetails(caught).status !== 404) submitError.value = '暂时无法核对原发布收据；未重复提交。'
            }
          }
        }
        for (const job of jobs.value) {
          if (!isPublishActive(job) && ['running', 'waiting'].includes(previous.get(job.id) || '')) {
            notifyFinished(job)
          }
        }
        return result
      } catch (caught) {
        const err = errorDetails(caught)
        if (current === epoch && enabled.value) {
          unsupported = [403, 404, 501].includes(err.status)
          error.value = unsupported
            ? '发布进度接口暂不可用；其他配置可继续操作。'
            : `暂时无法读取发布进度：${err.message}`
          failures = Math.min(failures + 1, 4)
        }
        return null
      } finally {
        if (current === epoch) { inFlight = null; schedule() }
      }
    })()
    inFlight = request
    return request
  }

  /** @param {PublishSubmission} input */
  async function create(input, recovering = false) {
    const existing = jobs.value.find(job => job.serviceId === input.serviceId && isPublishActive(job) && (job.status === 'running' || job.publishFingerprint === input.publishFingerprint))
    if (enabled.value && existing && !recovering) { selectedId.value = existing.id; return existing }
    if (!enabled.value || (locked.value && !(recovering && uncertain.value?.requestId === input.requestId && !jobs.value.some(job => job.status === 'running') && !starting.value))) return null
    if (!available.value) {
      submitError.value = '发布进度服务暂不可用，请稍后重新查询；不影响编辑和保存配置。'
      return null
    }
    const current = epoch
    starting.value = true
    submitError.value = ''
    try {
      localStorage.setItem(storageKey, JSON.stringify(input))
      const result = await api.startPublishJob(input.serviceId, {
        revision: input.revision, publishFingerprint: input.publishFingerprint, requestId: input.requestId,
      })
      if (current !== epoch || !enabled.value) return null
      if (!result.job?.id) throw new Error('网关未返回发布记录')
      acceptedVersion++
      jobs.value = [result.job, ...jobs.value.filter(job => job.id !== result.job.id)]
      selectedId.value = result.job.id
      clearPending()
      schedule()
      notifyFinished(result.job)
      return result.job
    } catch (caught) {
      const err = errorDetails(caught)
      if (current !== epoch || !enabled.value) return null
      if (err.status === 409) {
        // A duplicate may arrive while the original request owns the write
        // gate but has not persisted its receipt yet. Conflict is not proof
        // that the original failed; only its durable receipt can settle it.
        uncertain.value = { ...input }
        submitError.value = '原请求仍需核对；冲突响应不代表原发布失败，已保留原请求标识。'
        await refresh()
      } else if (err.status >= 400 && err.status < 500) {
        clearPending()
        submitError.value = err.message
        await refresh()
      } else {
        // The POST may have succeeded even if its response was lost. Query it;
        // never automatically send another write with a fresh request ID.
        uncertain.value = { ...input }
        submitError.value = '提交结果待确认，正在查询发布记录；请勿重复发布。'
        await refresh()
        const found = jobs.value.find(job => job.requestId === input.requestId)
        if (found) return found
      }
      return null
    } finally {
      if (current === epoch) { starting.value = false; schedule() }
    }
  }

  async function recover() {
    if (uncertain.value) return create({ ...uncertain.value }, true)
    return null
  }

  /** @param {PublishJob} job */
  function view(job) {
    selectedId.value = job.id
    // Opening an old result is a new read, not permission to replay the job.
    // The result dialog separately observes today's service/DNS state.
    return refresh()
  }
  function close() { selectedId.value = '' }
  function visibilityChanged() {
    if (document.visibilityState === 'hidden') clearTimer()
    else refresh()
  }
  const stop = watch(enabled, value => {
    epoch++
    readController?.abort()
    readController = null
    clearTimer()
    inFlight = null
    jobs.value = []
    ready.value = false
    error.value = ''
    submitError.value = ''
    selectedId.value = ''
    uncertain.value = null
    starting.value = false
    failures = 0
    unsupported = false
    acceptedVersion = 0
    notifiedJobs.clear()
    if (value) { restorePending(); refresh() }
  }, { immediate: true, flush: 'sync' })
  onMounted(() => document.addEventListener('visibilitychange', visibilityChanged))
  onBeforeUnmount(() => {
    disposed = true
    epoch++
    readController?.abort()
    readController = null
    stop()
    clearTimer()
    document.removeEventListener('visibilitychange', visibilityChanged)
  })

  return { jobs, ready, available, active, selected, selectedId, locked, error, submitError, uncertain, starting, refresh, create, recover, view, close }
}
