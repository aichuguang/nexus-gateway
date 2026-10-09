import type { MonitorCheck } from './system-types'

export type MonitorCheckTone = 'private' | 'amber' | 'blue' | 'neutral'
type Check = Partial<MonitorCheck> | null | undefined

export function statusLabel(check: Check = undefined): string {
  if (check?.classification === 'auth-required') return '可达·需鉴权'
  if (check?.classification === 'tls-error') return 'TLS 异常'
  if (check?.classification === 'timeout') return '请求超时'
  if (check?.statusCode === 429) return '次数超限'
  switch (check?.status) {
    case 'ok': return '正常'
    case 'error': return '异常'
    case 'checking': return '检查中'
    case 'recovering': return '恢复确认中'
    case 'not-applicable': return '不适用'
    default: return '待确认'
  }
}

export function checkTone(check: Check = undefined): MonitorCheckTone {
  if (check?.classification === 'auth-required') return 'blue'
  if (check?.statusCode === 429 || check?.status === 'error') return 'amber'
  if (check?.status === 'ok') return 'private'
  if (check?.status === 'not-applicable') return 'neutral'
  return 'blue'
}

export function latencyMS(check: Check = undefined): number | null {
  if (!check || check.statusCode === 429 || check.status === 'checking' || check.status === 'not-applicable') return null
  const value = check.latencyMs
  const completed = check.status === 'ok' || check.status === 'error'
  if (completed && typeof value === 'number' && Number.isFinite(value) && value > 0) return value

  // Old external results keep the real timings on their HTTP-response nodes.
  // Unknown/transport-failed nodes with a default zero are not measurements.
  let measured: number | null = null
  for (const detail of check.details || []) {
    for (const node of detail.nodes || []) {
      if (node.country !== 'CN' || !['ok', 'reachable', 'failed'].includes(node.status)
        || !Number.isInteger(node.httpStatus) || node.httpStatus < 100 || node.httpStatus > 599
        || typeof node.latencyMs !== 'number' || !Number.isFinite(node.latencyMs) || node.latencyMs < 0) continue
      measured = measured === null ? node.latencyMs : Math.max(measured, node.latencyMs)
    }
  }
  if (measured !== null) return measured
  if (check.details?.some(detail => detail.nodes?.length)) return null
  return check.status === 'ok' && value === 0 ? 0 : null
}

export function latencyLabel(check: Check = undefined): string {
  const value = latencyMS(check)
  if (value === null) return '—'
  if (value > 0 && value < 0.1) return '<0.1 ms'
  return `${value.toLocaleString('zh-CN', { maximumFractionDigits: 1 })} ms`
}
