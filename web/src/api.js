// @ts-check
// Every management call the dashboard makes, in one place.
//
// The two halves of this API are compiled separately: the server registers
// routes in internal/app/server.go, and this file is bundled into web/dist and
// embedded in the binary. Keep each path and HTTP method aligned with the
// corresponding server registration when changing this API contract.
//
// Template literals keep parameterized paths and their identifiers readable.

import { request, send } from './request.ts'
import { createResource } from './operation-create.ts'
export { onSessionLost, invalidateSessionRequests } from './request.ts'

/** @type {import('./api-types').GatewayAPI} */
export const api = {
  profile: (options = {}) => request('/api/profile', options),
  updateProfile: (profile, options = {}) => send('PUT', '/api/profile', profile, options),
  session: (options = {}) => request('/api/session', options),
  login: (password, options = {}) => send('POST', '/api/login', { password }, options),
  logout: (options = {}) => send('POST', '/api/logout', undefined, options),
  state: (options = {}) => request('/api/state', options),
  exportConfig: (options = {}) => request('/api/export', options),
  preview: (options = {}) => request('/api/preview', options),
  apply: (revision, options = {}) => send('POST', '/api/apply', { revision }, options),
  createService: (service, options = {}) => createResource('/api/services', service, options),
  updateService: (id, service, options = {}) => send('PUT', `/api/services/${encodeURIComponent(id)}`, service, options),
  deleteService: (id, input = { deleteDNS: false, deleteEdgeOne: false }, options = {}) => send('DELETE', `/api/services/${encodeURIComponent(id)}`, input, options),
  probeService: (id, options = {}) => send('POST', `/api/services/${encodeURIComponent(id)}/probe`, undefined, { ...options, readOnly: true }),
  checkService: (id, options = {}) => send('POST', `/api/services/${encodeURIComponent(id)}/public-check`, undefined, { ...options, readOnly: true }),
  checkServiceExternal: (id, options = {}) => send('POST', `/api/services/${encodeURIComponent(id)}/external-check`, { confirm: true }, options),
  // The cloud half of publishing. Applying the draft is a separate call and has
  // to come first: every value this writes is derived from the address that is
  // being served, so the two are one action with an order in it, not two
  // independent buttons.
  publishService: (id, options = {}) => send('POST', `/api/services/${encodeURIComponent(id)}/publish`, undefined, options),
  startPublishJob: (id, input, options = {}) => send('POST', `/api/services/${encodeURIComponent(id)}/publish-jobs`, input, options),
  publishJobs: (options = {}) => request('/api/publish-jobs', options),
  operationReceipt: (id, options = {}) => request(`/api/operation-receipts/${encodeURIComponent(id)}`, options),
  createCertificate: (name, options = {}) => createResource('/api/certificates', { name }, options),
  importCertificate: (id, form, options = {}) => request(`/api/certificates/${encodeURIComponent(id)}/import`, { ...options, method: 'POST', body: form }),
  deleteCertificate: (id, options = {}) => request(`/api/certificates/${encodeURIComponent(id)}`, { ...options, method: 'DELETE' }),
  diagnostics: (options = {}) => request('/api/diagnostics', options),
  traffic: (options = {}) => request('/api/traffic', options),
  externalProbes: (options = {}) => request('/api/external-probes', options),
  externalLimits: (options = {}) => request('/api/external-probes/limits', options),
  startExternalProbe: (input, options = {}) => send('POST', '/api/external-probes', input, options),
  publicPreflight: (options = {}) => request('/api/public/preflight', options),
  publicAddress: (options = {}) => request('/api/public/address', options),
  dnsAccounts: (options = {}) => request('/api/dns/accounts', options),
  createDNSAccount: (account, options = {}) => createResource('/api/dns/accounts', account, options),
  updateDNSAccount: (id, account, options = {}) => send('PATCH', `/api/dns/accounts/${encodeURIComponent(id)}`, account, options),
  dnsBindings: (options = {}) => request('/api/dns/bindings', options),
  dnsRecordAccounts: (host, options = {}) => request(`/api/dns/records/${encodeURIComponent(host)}/accounts`, options),
  selectDNSRecordAccount: (host, input, options = {}) => send('POST', `/api/dns/records/${encodeURIComponent(host)}/account`, input, options),
  saveDNSBindings: (input, options = {}) => send('PUT', '/api/dns/bindings', input, options),
  deleteDNSAccount: (id, options = {}) => request(`/api/dns/accounts/${encodeURIComponent(id)}`, { ...options, method: 'DELETE' }),
  dnsRecords: (options = {}) => request('/api/dns/records', options),
  dnsObservation: (host, options = {}) => request(`/api/dns/records/${encodeURIComponent(host)}/observation`, options),
  checkCloudflareEntry: (host, options = {}) => send('POST', `/api/cloudflare/hosts/${encodeURIComponent(host)}/check`, { confirm: true }, { ...options, readOnly: true }),
  startCloudflareHTTPValidation: (host, input, options = {}) => send('POST', `/api/cloudflare/hosts/${encodeURIComponent(host)}/http-validation`, input, { timeoutMs: 90000, ...options }),
  previewDNS: (host, options = {}) => send('POST', `/api/dns/records/${encodeURIComponent(host)}/preview`, undefined, { ...options, readOnly: true }),
  applyDNS: (confirmation, options = {}) => send('POST', `/api/dns/records/${encodeURIComponent(confirmation.host)}/apply`, confirmation, options),
  recoverDNSOverwrite: (host, input, options = {}) => send('POST', `/api/dns/records/${encodeURIComponent(host)}/recover-overwrite`, input, options),
  dnsStatus: (options = {}) => request('/api/dns/status', options),
  edgeAccounts: (options = {}) => request('/api/edgeone/accounts', options),
  createEdgeAccount: (account, options = {}) => createResource('/api/edgeone/accounts', account, options),
  updateEdgeAccount: (id, account, options = {}) => send('POST', `/api/edgeone/accounts/${encodeURIComponent(id)}`, account, options),
  deleteEdgeAccount: (id, options = {}) => request(`/api/edgeone/accounts/${encodeURIComponent(id)}`, { ...options, method: 'DELETE' }),
  edgeStatus: (options = {}) => request('/api/edgeone/status', options),
  previewEdge: (host, options = {}) => send('POST', `/api/edgeone/hosts/${encodeURIComponent(host)}/preview`, undefined, { ...options, readOnly: true }),
  applyEdge: (confirmation, options = {}) => send('POST', `/api/edgeone/hosts/${encodeURIComponent(confirmation.host)}/apply`, confirmation, options),
  cloudflareAccounts: (options = {}) => request('/api/cloudflare/accounts', options),
  discoverCloudflareAccount: (input, options = {}) => send('POST', '/api/cloudflare/accounts/discover', input, { timeoutMs: 60000, ...options, readOnly: true }),
  createCloudflareAccount: (account, options = {}) => createResource('/api/cloudflare/accounts', account, options),
  updateCloudflareAccount: (id, account, options = {}) => send('PATCH', `/api/cloudflare/accounts/${encodeURIComponent(id)}`, account, options),
  deleteCloudflareAccount: (id, options = {}) => request(`/api/cloudflare/accounts/${encodeURIComponent(id)}`, { ...options, method: 'DELETE' }),
  cloudflareHosts: (options = {}) => request('/api/cloudflare/hosts', options),
  previewCloudflare: (host, options = {}) => request(`/api/cloudflare/preview?host=${encodeURIComponent(host)}`, options),
  applyCloudflare: (confirmation, options = {}) => send('POST', '/api/cloudflare/apply', confirmation, options),
  edgeWorkspaceAccounts: (options = {}) => request('/api/edge/accounts', options),
  edgeWorkspaceHosts: (options = {}) => request('/api/edge/hosts', { timeoutMs: 42000, ...options }),
  createEdgeWorkspaceAccount: (account, options = {}) => createResource('/api/edge/accounts', account, options),
  updateEdgeWorkspaceAccount: (provider, id, account, options = {}) => send('PATCH', `/api/edge/accounts/${encodeURIComponent(provider)}/${encodeURIComponent(id)}`, account, options),
  deleteEdgeWorkspaceAccount: (provider, id, options = {}) => request(`/api/edge/accounts/${encodeURIComponent(provider)}/${encodeURIComponent(id)}`, { ...options, method: 'DELETE' }),
  previewEdgeWorkspace: (provider, host, options = {}) => request(`/api/edge/hosts/${encodeURIComponent(host)}/preview?provider=${encodeURIComponent(provider)}`, options),
  applyEdgeWorkspace: (confirmation, options = {}) => send('POST', `/api/edge/hosts/${encodeURIComponent(confirmation.host)}/apply`, confirmation, options),
  saveSettings: (settings, options = {}) => send('PUT', '/api/settings', settings, options),
  securitySessions: (options = {}) => request('/api/security/sessions', options),
  changePassword: (input, options = {}) => send('POST', '/api/security/password', input, options),
  deleteSession: (id, options = {}) => request(`/api/security/sessions/${encodeURIComponent(id)}`, { ...options, method: 'DELETE' }),
  revokeSessions: (scope, options = {}) => send('POST', '/api/security/sessions/revoke', { scope }, options),
  history: (options = {}) => request('/api/history', options),
  previewHistory: (id, revision, options = {}) => send('POST', `/api/history/${encodeURIComponent(id)}/preview`, { revision }, { ...options, readOnly: true }),
  restoreHistory: (id, input, options = {}) => send('POST', `/api/history/${encodeURIComponent(id)}/restore`, input, options),
  downloadBackup: (input, options = {}) => send('POST', '/api/backups/download', input, { ...options, readOnly: true, responseType: 'blob' }),
  monitorSettings: (options = {}) => request('/api/monitor/settings', options),
  saveMonitorSettings: (settings, options = {}) => send('PUT', '/api/monitor/settings', settings, options),
  monitorStatus: (options = {}) => request('/api/monitor/status', options),
  checkMonitor: (options = {}) => send('POST', '/api/monitor/check', { confirmExternal: true }, options),
  notificationChannels: (options = {}) => request('/api/notifications/channels', options),
  createNotification: (input, options = {}) => send('POST', '/api/notifications/channels', input, options),
  updateNotification: (id, input, options = {}) => send('PUT', `/api/notifications/channels/${encodeURIComponent(id)}`, input, options),
  deleteNotification: (id, options = {}) => request(`/api/notifications/channels/${encodeURIComponent(id)}`, { ...options, method: 'DELETE' }),
  testNotification: (id, options = {}) => send('POST', `/api/notifications/channels/${encodeURIComponent(id)}/test`, undefined, options),
}
