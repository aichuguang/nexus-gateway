const formatter = new Intl.DateTimeFormat('en-GB', {
  timeZone: 'Asia/Shanghai',
  year: 'numeric', month: '2-digit', day: '2-digit',
  hour: '2-digit', minute: '2-digit', second: '2-digit',
  hourCycle: 'h23',
})

const goZeroTime = Date.parse('0001-01-01T00:00:00Z')

/** Format an instant in Shanghai time. Strings must include an explicit timezone. */
export function formatDateTime(value: string | number | Date | null | undefined, fallback = '—'): string {
  if (value == null || value === '') return fallback

  let date: Date
  if (typeof value === 'string') {
    const input = value.trim()
    const parts = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/i.exec(input)
    if (!parts) return fallback
    const [, year, month, day, hour, minute, second, fraction, zone] = parts
    const y = Number(year), m = Number(month), d = Number(day)
    const leap = y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0)
    const monthDays = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
    if (y < 1 || m < 1 || m > 12 || d < 1 || d > monthDays[m - 1]
      || Number(hour) > 23 || Number(minute) > 59 || Number(second) > 59
      || (zone.toUpperCase() !== 'Z' && (Number(zone.slice(1, 3)) > 23 || Number(zone.slice(4)) > 59))) return fallback
    // RFC3339Nano precision exceeds Date's milliseconds; truncate explicitly for portability.
    date = new Date(`${year}-${month}-${day}T${hour}:${minute}:${second}${fraction ? `.${fraction.slice(0, 3).padEnd(3, '0')}` : ''}${zone.toUpperCase()}`)
  } else {
    date = value instanceof Date ? value : new Date(value)
  }

  if (!Number.isFinite(date.getTime()) || date.getTime() === goZeroTime) return fallback
  const parts = Object.fromEntries(formatter.formatToParts(date).map(part => [part.type, part.value]))
  return `${parts.year.padStart(4, '0')}-${parts.month}-${parts.day} ${parts.hour}:${parts.minute}:${parts.second}`
}
