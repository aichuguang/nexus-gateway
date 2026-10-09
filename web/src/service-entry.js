export function upstreamPortOf(raw) {
  try {
    const url = new URL(raw)
    // URL.port removes an explicitly written protocol default (80/443), while
    // the server preserves it. Do not infer a port when the operator omitted it.
    const authority = String(raw).trim().match(/^[a-z][a-z\d+.-]*:\/\/([^/?#]*)/i)?.[1] || ''
    const port = Number(url.port || authority.match(/:(\d+)$/)?.[1])
    return Number.isInteger(port) && port > 0 && port <= 65535 ? port : 0
  } catch { return 0 }
}

export function serviceEntry(service, address = '') {
  if (service.domains?.length) return `https://${service.domains[0]}`
  const port = service.port || upstreamPortOf(service.upstream)
  if (!address) return `:${port}`
  const host = address.includes(':') && !address.startsWith('[') ? `[${address}]` : address
  return `http://${host}:${port}`
}
