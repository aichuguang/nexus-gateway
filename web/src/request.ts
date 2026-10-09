import type { RequestOptions } from './api-types'

export class RequestError extends Error {
  relogin = false
  operationStage?: string
  cloudChanges?: boolean
  constructor(message: string, public status = 0, public outcomeUnknown = false, public cancelled = false) { super(message); this.name = 'RequestError' }
}
export function errorDetails(error: unknown): RequestError {
  if (error instanceof RequestError) return error
  const details = new RequestError(error instanceof Error ? error.message : String(error),
    typeof error === 'object' && error !== null && 'status' in error && typeof error.status === 'number' ? error.status : 0)
  details.relogin = typeof error === 'object' && error !== null && 'relogin' in error && error.relogin === true
  if (typeof error === 'object' && error !== null) {
    if ('operationStage' in error && typeof error.operationStage === 'string') details.operationStage = error.operationStage
    if ('cloudChanges' in error && typeof error.cloudChanges === 'boolean') details.cloudChanges = error.cloudChanges
  }
  return details
}
export function cancelledRequest() { return new RequestError('请求已取消或已过期', 0, false, true) }
export function isRequestCancelled(error: unknown) { return error instanceof RequestError && error.cancelled }

let sessionEpoch = 0
let unauthorized = () => {}
const pendingReads = new Set<AbortController>()
export function onSessionLost(handler: () => void) { unauthorized = handler }
export function invalidateSessionRequests() {
  sessionEpoch++
  for (const controller of pendingReads) controller.abort()
  pendingReads.clear()
}

type Options = RequestInit & RequestOptions & { readOnly?: boolean; responseType?: 'json' | 'blob' }
function attachmentFilename(header: string | null) {
  const encoded = header?.match(/filename\*=UTF-8''([^;]+)/i)?.[1]
  const plain = header?.match(/filename="?([^";]+)"?/i)?.[1]
  let value = plain || 'gateway-backup.enc'
  if (encoded) { try { value = decodeURIComponent(encoded) } catch { /* use plain filename */ } }
  return value.replace(/[\\/<>:"|?*\x00-\x1f]/g, '_').replace(/^\.+/, '').slice(0, 120) || 'gateway-backup.enc'
}
// Reads can be physically aborted. A timed-out write only detaches its caller:
// aborting the HTTP request is not a rollback and cannot cancel a server job.
export async function request<T>(path: string, options: Options = {}): Promise<T> {
  const { timeoutMs, signal, readOnly = false, responseType = 'json', ...init } = options
  const write = Boolean(init.method && !['GET', 'HEAD'].includes(init.method))
  const mutation = write && !readOnly
  if (signal?.aborted) throw cancelledRequest()
  const epoch = sessionEpoch
  const controller = new AbortController()
  if (!mutation) pendingReads.add(controller)
  const headers = new Headers(init.headers)
  if (write) {
    headers.set('X-Gateway-Request', '1')
    if (!(init.body instanceof FormData)) headers.set('Content-Type', 'application/json')
  }
  let timer: ReturnType<typeof setTimeout> | undefined
  let abortListener: (() => void) | undefined
  const boundary = new Promise<never>((_, reject) => {
    abortListener = () => {
      if (!mutation) controller.abort()
      reject(mutation ? new RequestError('已停止等待响应，操作结果待确认；请先核对状态，不要重复提交。', 0, true) : cancelledRequest())
    }
    signal?.addEventListener('abort', abortListener, { once: true })
    timer = setTimeout(() => {
      if (!mutation) controller.abort()
      reject(new RequestError(mutation
        ? '请求超时，操作结果待确认；服务端可能仍在执行，请先核对状态，不要重复提交。'
        : '读取超时，请稍后重试。', 0, mutation))
    }, timeoutMs ?? (mutation ? 90_000 : 20_000))
  })
  try {
    const result = await Promise.race([boundary, (async () => {
      const response = await fetch(path, { ...init, credentials: 'same-origin', cache: 'no-store', headers, signal: controller.signal })
      if (response.ok && responseType === 'blob') {
        const blob = await response.blob()
        if (epoch !== sessionEpoch) throw cancelledRequest()
        return { blob, filename: attachmentFilename(response.headers.get('Content-Disposition')) } as T
      }
      const data: unknown = await response.json().catch(() => null)
      // An old 401 must never sign a newly authenticated session out.
      if (epoch !== sessionEpoch) throw cancelledRequest()
      if (!response.ok) {
        if (response.status === 401 && path !== '/api/login') { invalidateSessionRequests(); unauthorized() }
        const message = typeof data === 'object' && data !== null && 'error' in data && typeof data.error === 'string'
          ? data.error : `请求失败（HTTP ${response.status}）`
        const error = new RequestError(message, response.status, mutation && response.status >= 500)
        error.relogin = typeof data === 'object' && data !== null && 'relogin' in data && data.relogin === true
        if (typeof data === 'object' && data !== null) {
          if ('operationStage' in data && typeof data.operationStage === 'string') error.operationStage = data.operationStage
          if ('cloudChanges' in data && typeof data.cloudChanges === 'boolean') error.cloudChanges = data.cloudChanges
        }
        throw error
      }
      if (data === null || typeof data !== 'object') throw new RequestError(mutation
        ? '响应格式异常，操作结果待确认；请先核对状态，不要重复提交。' : '响应格式异常，请重新读取。', 0, mutation)
      return data as T
    })()])
    if (epoch !== sessionEpoch) throw cancelledRequest()
    return result
  } catch (error) {
    if (epoch !== sessionEpoch) throw cancelledRequest()
    if (error instanceof RequestError) throw error
    if (controller.signal.aborted && !mutation) throw cancelledRequest()
    throw new RequestError(mutation
      ? '连接中断，操作结果待确认；请先核对状态，不要重复提交。'
      : errorDetails(error).message, 0, mutation)
  } finally {
    clearTimeout(timer)
    if (abortListener) signal?.removeEventListener('abort', abortListener)
    pendingReads.delete(controller)
  }
}

export function send<T>(method: string, path: string, body?: unknown, options: Options = {}): Promise<T> {
  return request<T>(path, { ...options, method, body: JSON.stringify(body ?? {}) })
}
