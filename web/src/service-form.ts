import type { EdgeProvider, EntryMode, Service } from './api-types'
export interface ServiceForm {
  id: string; name: string; icon: string; upstream: string; mode: 'name' | 'port'
  domains: string[]; certificateId: string; entryMode: EntryMode; port: string; enabled: boolean
  edgeProvider: EdgeProvider; edgeAccountId: string; usePreferred: boolean; preferredDomain: string
  healthPath: string; healthCodes: string
}
export function serviceForm(service?: Service | null): ServiceForm {
  return {
    id: service?.id || '', name: service?.name || '', icon: service?.icon || '', upstream: service?.upstream || '',
    mode: service?.domains?.length ? 'name' : 'port',
    domains: service?.domains?.length ? [...service.domains] : [''], certificateId: service?.certificateId || '',
    entryMode: service?.entryMode === 'edge' ? 'edge' : 'direct', port: service?.port ? String(service.port) : '',
    enabled: service?.enabled !== false,
    edgeProvider: service?.edgeProvider === 'cloudflare' ? 'cloudflare' : 'edgeone',
    edgeAccountId: service?.edgeAccountId || '', usePreferred: service?.usePreferred === true,
    preferredDomain: service?.preferredDomain || '',
    healthPath: service?.healthCheck?.path || '', healthCodes: service?.healthCheck?.expectedStatusCodes?.join(', ') || '',
  }
}
export function setServiceMode(form: ServiceForm, mode: ServiceForm['mode']) {
  form.mode = mode
  if (mode === 'name') form.port = ''
  else { form.domains = ['']; form.certificateId = ''; form.entryMode = 'direct' }
}
export function servicePayload(form: ServiceForm): Service {
  const port = form.mode === 'port' && form.port ? Number(form.port) : 0
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error('发布端口必须为 1 到 65535 的整数，或留空沿用上游端口。')
  const path = form.healthPath.trim()
  if (path && (!path.startsWith('/') || path.startsWith('//') || /[\r\n\\#]/.test(path))) throw new Error('健康路径必须是同源相对路径，例如 /health。')
  const codes = form.healthCodes.trim() ? form.healthCodes.split(/[,，\s]+/).filter(Boolean).map(Number) : []
  if (codes.some(code => !Number.isInteger(code) || code < 100 || code > 599)) throw new Error('预期状态码须为 100–599，以逗号分隔。')
  return {
    id: form.id, name: form.name.trim(), icon: form.icon, upstream: form.upstream.trim(),
    domains: form.mode === 'name' ? form.domains.map(domain => domain.trim().toLowerCase()).filter(Boolean) : [],
    certificateId: form.mode === 'name' ? form.certificateId : '', port, enabled: form.enabled,
    healthCheck: path || codes.length ? { path, expectedStatusCodes: [...new Set(codes)] } : null,
    ...(form.mode === 'name' && form.entryMode === 'edge' ? {
      entryMode: 'edge' as const, edgeProvider: form.edgeProvider,
      ...(form.edgeProvider === 'cloudflare' ? { edgeAccountId: form.edgeAccountId, usePreferred: form.usePreferred, preferredDomain: form.preferredDomain.trim().toLowerCase() } : {}),
    } : {}),
  }
}
