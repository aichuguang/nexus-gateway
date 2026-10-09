import { onBeforeUnmount } from 'vue'
import { api as gatewayAPI } from './api.js'
import { cancelledRequest } from './request'
import type { GatewayAPI, RequestOptions } from './api-types'

const reads = new Set<keyof GatewayAPI>(['profile', 'session', 'state', 'exportConfig', 'preview', 'publishJobs',
  'operationReceipt',
  'dnsObservation', 'checkCloudflareEntry', 'discoverCloudflareAccount',
  'diagnostics', 'traffic', 'externalProbes', 'externalLimits', 'publicPreflight', 'publicAddress', 'dnsAccounts', 'dnsRecords', 'dnsStatus',
  'edgeAccounts', 'edgeStatus', 'probeService', 'checkService', 'previewDNS', 'previewEdge', 'dnsBindings',
  'cloudflareAccounts', 'cloudflareHosts', 'previewCloudflare', 'dnsRecordAccounts',
  'edgeWorkspaceAccounts', 'edgeWorkspaceHosts', 'previewEdgeWorkspace',
  'securitySessions', 'history', 'previewHistory', 'downloadBackup', 'monitorSettings', 'monitorStatus', 'notificationChannels'])
const argumentsBeforeOptions: Record<keyof GatewayAPI, number> = {
  profile: 0, updateProfile: 1,
  session: 0, login: 1, logout: 0, state: 0, exportConfig: 0, preview: 0, apply: 1,
  createService: 1, updateService: 2, deleteService: 2, probeService: 1, checkService: 1,
  checkServiceExternal: 1,
  publishService: 1, startPublishJob: 2, publishJobs: 0, createCertificate: 1, importCertificate: 2,
  operationReceipt: 1,
  dnsObservation: 1, recoverDNSOverwrite: 2, checkCloudflareEntry: 1, discoverCloudflareAccount: 1,
  startCloudflareHTTPValidation: 2,
  deleteCertificate: 1, diagnostics: 0, traffic: 0, publicPreflight: 0, publicAddress: 0, dnsAccounts: 0,
  externalProbes: 0, externalLimits: 0, startExternalProbe: 1,
  createDNSAccount: 1, deleteDNSAccount: 1, dnsRecords: 0, previewDNS: 1, applyDNS: 1, dnsStatus: 0,
  updateDNSAccount: 2, dnsBindings: 0, saveDNSBindings: 1,
  dnsRecordAccounts: 1, selectDNSRecordAccount: 2,
  cloudflareAccounts: 0, createCloudflareAccount: 1, updateCloudflareAccount: 2, deleteCloudflareAccount: 1,
  cloudflareHosts: 0, previewCloudflare: 1, applyCloudflare: 1,
  edgeWorkspaceAccounts: 0, edgeWorkspaceHosts: 0, createEdgeWorkspaceAccount: 1,
  updateEdgeWorkspaceAccount: 3, deleteEdgeWorkspaceAccount: 2, previewEdgeWorkspace: 2, applyEdgeWorkspace: 1,
  edgeAccounts: 0, createEdgeAccount: 1, updateEdgeAccount: 2, deleteEdgeAccount: 1,
  edgeStatus: 0, previewEdge: 1, applyEdge: 1, saveSettings: 1,
  securitySessions: 0, changePassword: 1, deleteSession: 1, revokeSessions: 1,
  history: 0, previewHistory: 2, restoreHistory: 2, downloadBackup: 1,
  monitorSettings: 0, saveMonitorSettings: 1, monitorStatus: 0, checkMonitor: 0,
  notificationChannels: 0, createNotification: 1, updateNotification: 2, deleteNotification: 1, testNotification: 1,
}

// A page owns only its reads and delivery of results, never server-side writes.
// The proxy preserves the GatewayAPI signatures; the cast is limited to its
// dynamic dispatch boundary, not exposed to consumers as an untyped API.
export function createPageScope(source: GatewayAPI = gatewayAPI) {
  let active = true
  let epoch = 0
  const pending = new Map<string, AbortController>()
  const api = new Proxy(source, {
    get(target, property: keyof GatewayAPI) {
      const method = target[property]
      if (typeof method !== 'function') return method
      return async (...args: unknown[]) => {
        if (!active) throw cancelledRequest()
        const generation = epoch
        const optionsIndex = argumentsBeforeOptions[property]
        const controller = new AbortController()
        const read = reads.has(property)
        // Sensitive write payloads must not become retained map keys.
        const key = `${property}:${read && property !== 'downloadBackup' && property !== 'discoverCloudflareAccount' ? JSON.stringify(args.slice(0, optionsIndex)) : ''}`
        if (read) { pending.get(key)?.abort(); pending.set(key, controller) }
        const original = (args[optionsIndex] ?? {}) as RequestOptions
        const forwardAbort = () => controller.abort()
        original.signal?.addEventListener('abort', forwardAbort, { once: true })
        if (original.signal?.aborted) controller.abort()
        args[optionsIndex] = { ...original, signal: controller.signal }
        try {
          const result = await (method as (...values: unknown[]) => Promise<unknown>)(...args)
          if (!active || generation !== epoch || (read && pending.get(key) !== controller)) throw cancelledRequest()
          return result
        } catch (error) {
          if (!active || generation !== epoch || (read && pending.get(key) !== controller)) throw cancelledRequest()
          throw error
        } finally {
          original.signal?.removeEventListener('abort', forwardAbort)
          if (pending.get(key) === controller) pending.delete(key)
        }
      }
    },
  }) as GatewayAPI
  function reset() {
    epoch++
    for (const controller of pending.values()) controller.abort()
    pending.clear()
  }
  function guard<T extends (...args: never[]) => unknown>(handler: T): T {
    return ((...args: Parameters<T>) => active ? handler(...args) : undefined) as T
  }
  return { api, guard, reset, isActive: () => active, generation: () => epoch,
    isCurrent: (value: number) => active && epoch === value,
    dispose: () => { active = false; reset() } }
}
export function usePageScope() {
  const scope = createPageScope()
  onBeforeUnmount(scope.dispose)
  return scope
}

export function isReadBusy(value: string) {
  return ['dns-refresh', 'edge-refresh', 'backup-download'].includes(value) || value.startsWith('probe-') || value.startsWith('check-')
}
