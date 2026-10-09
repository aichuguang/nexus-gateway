import type { TrafficCounter, TrafficResponse } from './api-types'

export type TrafficPeriod = 'today' | 'totals'
export type TrafficServiceRow = TrafficResponse['services'][number] & {
  counter: TrafficCounter
  share: number
}
export interface TrafficTrendDay {
  date: string
  counter: TrafficCounter | null
  value: number | null
}

const DAY = 86400000
const SHANGHAI_OFFSET = 8 * 60 * 60 * 1000
const counterKeys: Array<keyof TrafficCounter> = ['requests', 'receivedBytes', 'sentBytes', 'totalBytes']
const byteUnits = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB']

function validNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}

export function formatTrafficBytes(value?: number): string {
  if (!validNumber(value)) return '—'
  if (value === 0) return '0 B'
  const index = Math.max(0, Math.min(Math.floor(Math.log(value) / Math.log(1024)), byteUnits.length - 1))
  return `${(value / 1024 ** index).toLocaleString('zh-CN', { maximumFractionDigits: index ? 2 : 0 })} ${byteUnits[index]}`
}

export function trafficShare(value?: number, total?: number): number {
  if (!validNumber(value) || !validNumber(total) || total === 0) return 0
  return Math.max(0, Math.min(100, value / total * 100))
}

// The input is a percentage, not a ratio. Tiny nonzero shares stay distinguishable
// from zero without suggesting that rounded display text is the chart geometry.
export function formatTrafficPercent(value?: number): string {
  if (!validNumber(value)) return '—'
  const percent = Math.min(100, value)
  if (percent > 0 && percent < 0.1) return '<0.1%'
  return `${percent.toLocaleString('zh-CN', { maximumFractionDigits: 1 })}%`
}

export function isRemovedTrafficService(service: TrafficResponse['services'][number]): boolean {
  return service.deleted === true || service.serviceStatus === 'deleted'
}

export function serviceTrafficRows(
  services: TrafficResponse['services'] | undefined,
  period: TrafficPeriod,
  totalBytes: number | undefined,
): TrafficServiceRow[] {
  return (services || []).map(service => {
    const counter = { ...service[period] }
    return { ...service, counter, share: trafficShare(counter.totalBytes, totalBytes) }
  }).sort((a, b) => {
    const left = validNumber(a.counter.totalBytes) ? a.counter.totalBytes : 0
    const right = validNumber(b.counter.totalBytes) ? b.counter.totalBytes : 0
    return right - left || (a.name || a.serviceId).localeCompare(b.name || b.serviceId, 'zh-CN') || a.serviceId.localeCompare(b.serviceId)
  })
}

function calendarDate(value: string): boolean {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) return false
  const milliseconds = Date.parse(`${value}T00:00:00Z`)
  return Number.isFinite(milliseconds) && new Date(milliseconds).toISOString().slice(0, 10) === value
}

function timestamp(value: unknown): number | null {
  // Reject locale-dependent or zone-less parsing, and dates that Date.parse
  // silently normalizes (for example February 30).
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) || !calendarDate(value.slice(0, 10))) return null
  const milliseconds = Date.parse(value)
  return Number.isFinite(milliseconds) ? milliseconds : null
}

function referenceDay(value: string | number | Date | undefined): string | null {
  if (typeof value === 'string' && calendarDate(value)) return value
  const milliseconds = value === undefined ? Date.now() : value instanceof Date ? value.getTime() : typeof value === 'number' ? value : timestamp(value)
  if (milliseconds === null || !Number.isFinite(milliseconds)) return null
  const shifted = new Date(milliseconds + SHANGHAI_OFFSET)
  return Number.isFinite(shifted.getTime()) ? shifted.toISOString().slice(0, 10) : null
}

// Date-only reference values already denote a Shanghai calendar day. Timestamp,
// Date and epoch-millisecond values are converted to UTC+8 before bucketing.
// An explicitly invalid reference returns no slots rather than inventing a day.
export function trendDays(
  days: TrafficResponse['days'] | undefined,
  referenceDate?: string | number | Date,
): TrafficTrendDay[] {
  const end = referenceDay(referenceDate)
  if (!end || !calendarDate(end)) return []
  const indexed = new Map<string, TrafficCounter | null>()
  for (const day of days || []) {
    if (!calendarDate(day.date)) continue
    const counter = counterKeys.every(key => validNumber(day[key]))
      ? { requests: day.requests, receivedBytes: day.receivedBytes, sentBytes: day.sentBytes, totalBytes: day.totalBytes }
      : null
    indexed.set(day.date, counter)
  }
  const endMilliseconds = Date.parse(`${end}T00:00:00Z`)
  return Array.from({ length: 30 }, (_, index) => {
    const date = new Date(endMilliseconds - (29 - index) * DAY).toISOString().slice(0, 10)
    const counter = indexed.get(date) ?? null
    return { date, counter, value: counter?.totalBytes ?? null }
  })
}

export function relativeTrafficTime(value: string | null | undefined, nowMs = Date.now()): string | null {
  const milliseconds = timestamp(value)
  if (milliseconds === null || !Number.isFinite(nowMs)) return null
  const elapsed = nowMs - milliseconds
  if (elapsed < 0 || !Number.isFinite(elapsed)) return null
  if (elapsed < 60000) return '刚刚'
  if (elapsed < 3600000) return `${Math.floor(elapsed / 60000)}分钟前`
  if (elapsed < DAY) return `${Math.floor(elapsed / 3600000)}小时前`
  if (elapsed < 30 * DAY) return `${Math.floor(elapsed / DAY)}天前`
  if (elapsed < 365 * DAY) return `${Math.floor(elapsed / (30 * DAY))}个月前`
  return `${Math.floor(elapsed / (365 * DAY))}年前`
}
