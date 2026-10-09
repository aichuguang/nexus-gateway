import { request, send, errorDetails } from './request'
import type { GatewayState, RequestOptions } from './api-types'

interface PendingCreation { requestId: string; kind: string }
const inFlight = new Map<string, Promise<GatewayState>>()

// Only request identity is persisted. In particular, input digests and cloud
// credentials must never enter browser storage, including after a failed POST.
export function createResource(path: string, input: unknown, options: RequestOptions = {}): Promise<GatewayState> {
  const active = inFlight.get(path)
  if (active) return active
  const action = (async () => {
    const key = `w103d-create-v1:${path}`
    let pending: PendingCreation | null = null
    try {
      const stored = localStorage.getItem(key)
      if (stored) {
        pending = JSON.parse(stored)
        if (!pending || pending.kind !== path || !/^[A-Za-z0-9_-]{16,80}$/.test(pending.requestId)) throw new Error('invalid recovery context')
      }
    } catch { throw new Error('无法读取创建恢复记录，未发送请求；请检查浏览器存储。') }
    if (pending) {
      try {
        const receipt = await request<{ status: string }>(`/api/operation-receipts/${encodeURIComponent(pending.requestId)}`, options)
        if (receipt.status === 'succeeded') {
          const state = await request<GatewayState>('/api/state', options)
          localStorage.removeItem(key)
          return { ...state, operationRecoveryMessage: '已恢复上次创建的成功结果；本次表单输入未提交。如需创建另一资源，请重新填写并保存。' }
        }
        if (receipt.status === 'rejected') {
          localStorage.removeItem(key)
          throw new Error('上次创建已被明确拒绝；请确认表单后再次保存。')
        }
      } catch (caught) {
        if (errorDetails(caught).status !== 404) throw caught
      }
    } else {
      const bytes = crypto.getRandomValues(new Uint8Array(16))
      pending = { requestId: `create_${Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('')}`, kind: path }
      try { localStorage.setItem(key, JSON.stringify(pending)) }
      catch { throw new Error('浏览器无法保存恢复标识，未发送创建请求。') }
    }
    try {
      const result = await send<GatewayState>('POST', path, { ...(input as object), requestId: pending.requestId }, options)
      localStorage.removeItem(key)
      return result
    } catch (caught) {
      const error = errorDetails(caught)
      // A conflicting identity may refer to a previous accepted operation. Keep
      // it until its receipt has been checked rather than inventing a new ID.
      if (error.status >= 400 && error.status < 500 && error.status !== 409) localStorage.removeItem(key)
      if (error.outcomeUnknown || !error.status) {
        error.message += ' 已保留原请求标识；再次保存会先核对原操作，刷新后请重新输入凭据。'
      }
      throw error
    }
  })().finally(() => inFlight.delete(path))
  inFlight.set(path, action)
  return action
}
