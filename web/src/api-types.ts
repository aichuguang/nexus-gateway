// Core wire contracts mirror internal/model and the publish-jobs API. Optional
// fields correspond to Go's omitempty; transport JSON is untrusted at runtime.
import type { BackupInput, DownloadFile, HistoryEntry, HistoryPreview, HistoryRestore, HistoryRestored, MonitorSettings, MonitorStatus, NotificationChannel, NotificationInput, PasswordChange, SecuritySession } from './system-types'
import type { ExternalInput, ExternalJob, ExternalQuota, ExternalReport } from './external-probe-types'
export interface RequestOptions { signal?: AbortSignal; timeoutMs?: number }
export interface Profile { projectName: string; clientName: string; logo?: string }
export type EntryMode = 'direct' | 'edge'
export type DNSProvider = 'dnspod' | 'cloudflare' | 'alidns'
export type EdgeProvider = 'edgeone' | 'cloudflare'
export interface Service {
  id: string; name: string; icon?: string; upstream: string; domains: string[] | null
  certificateId?: string; entryMode?: EntryMode; port?: number; enabled: boolean
  edgeProvider?: EdgeProvider; edgeAccountId?: string; usePreferred?: boolean; preferredDomain?: string
  healthCheck?: { path?: string; expectedStatusCodes?: number[] } | null
}
export interface Certificate {
  id: string; name: string; domains: string[] | null; notBefore: string; notAfter: string
  fingerprint: string; ready: boolean; error?: string; inboxPath?: string; materialVersion?: string
  edgeOne?: {
    uploads?: Array<{ accountId: string; certId: string; fingerprint: string; at?: string }>
    bindings?: Array<{ accountId: string; zoneId: string; zoneName?: string; certId: string; fingerprint: string; hosts: string[]; error?: string; at?: string }>
  }
}
export interface Settings {
  gatewayIp: string; certdUrl: string; autoResolve: boolean; baseDomain?: string; baseTtl?: number
}
export interface CloudAccount {
  id: string; name: string; secretIdHint: string; credentialVersion: number; enabled: boolean
  originMode?: 'address' | 'base'; provider?: string
}
export interface DNSBinding { host: string; accountId: string; zone: string; exact?: boolean; automatic?: boolean }
export interface DNSRecordAccounts {
  host: string; revision: number; status: 'automatic' | 'choice-required' | 'selected' | 'unavailable'
  choices: Array<{ accountId: string; accountName: string; provider: string; zone: string }>
  selectedAccountId?: string; message: string
}
export interface CloudflareAccount {
  id: string; name: string; zoneId: string; zoneName?: string; fallbackDomain: string; standardTarget: string
  tokenHint?: string; credentialVersion: number; enabled: boolean; autoSSLStrict?: boolean
}
export interface CloudflareAccountInput { name: string; apiToken: string; zoneId: string; fallbackDomain: string; standardTarget: string; enabled?: boolean; autoConfigure?: boolean; autoSSLStrict?: boolean; fallbackMode?: 'create' | 'reuse' | 'replace'; existingFallback?: string }
export interface CloudflareAccountDiscovery {
  zones: Array<{ zoneId: string; zoneName: string; eligible: boolean; message: string; fallbackDomain: string; standardTarget: string; fallbackMode?: 'create' | 'reuse' | 'replace'; existingFallback?: string; existingFallbackTarget?: string; desiredBaseDomain?: string; fallbackStatus?: string; sslMode?: string; requiresSSLStrict?: boolean; sslStrictImpact?: string }>
  recommendedZoneId: string; warnings: string[]
}
export interface CloudflareHost {
  host: string; serviceId: string; serviceName: string; accountId: string; standardTarget: string
  preferredDomain?: string; usePreferred?: boolean; target: string; hostnameStatus: string; certificateStatus: string
  originStatus: string; status: string; message?: string; ready: boolean
  validationRecords?: Array<{ type: string; name: string; value: string }>
}
export interface CloudflarePreview extends CloudflareHost {
  token: string; revision: number; warnings?: string[]
  steps?: Array<{ action: string; target: string; detail: string; from?: string; to?: string }>
}
export interface CloudflareHTTPValidationResult {
  ok: true; host: string; target: string; jobId: string; dnsPublished: true; certificateReady: boolean; message: string
}
export interface EdgeWorkspaceAccount {
  key: string; provider: EdgeProvider; id: string; name: string; enabled: boolean; credentialVersion: number
  secretIdHint?: string; originMode?: 'address' | 'base'; tokenHint?: string; zoneId?: string; zoneName?: string
  fallbackDomain?: string; standardTarget?: string; autoSSLStrict?: boolean
}
export interface EdgeWorkspaceHost {
  key: string; provider: EdgeProvider; host: string; serviceId: string; serviceName: string; enabled: boolean
  accountId?: string; accountName?: string; status: string; message?: string; target: string; ready: boolean
  standardTarget?: string; usePreferred?: boolean; origin?: string; originMode?: string; certificateStatus?: string
  originStatus?: string; hostnameStatus?: string; recordSays?: string; detail: JSONRecord
}
export type EdgeWorkspaceAccountInput = ({ provider: 'edgeone' } & AccountInput) | ({ provider: 'cloudflare' } & CloudflareAccountInput)
export interface GatewayState {
  operationRecoveryMessage?: string
  runtime?: { binaryPath: string; dataDir: string }
  schemaVersion: number; revision: number; appliedRevision: number; appliedAt: string
  // These Go slices do not use omitempty. Loading persisted state preserves nil,
  // so the keys are present but their values may be null rather than arrays.
  appliedPublicAddress?: string; settings: Settings; services: Service[] | null; certificates: Certificate[] | null
  dnsAccounts: CloudAccount[] | null; edgeOneAccounts: CloudAccount[] | null
  cloudflareAccounts?: CloudflareAccount[] | null; dnsBindings?: DNSBinding[] | null
  edgeOrigins: Array<{ accountId: string; zoneId: string; zoneName?: string; groupId: string; value?: string }> | null
  unappliedServiceIds?: string[]; unappliedElsewhere?: boolean
}
export type PublishStatus = 'running' | 'waiting' | 'succeeded' | 'partial_failed' | 'failed' | 'needs_review' | 'interrupted'
export type PublishStepStatus = 'running' | 'waiting' | 'succeeded' | 'failed' | 'skipped' | 'unknown'
export interface PublishJob {
  id: string; requestId: string; serviceId: string; serviceName: string
  revision: number; publishFingerprint: string; status: PublishStatus
  edge: boolean; edgeProvider?: EdgeProvider; dns: boolean; createdAt: string; updatedAt: string; message?: string
  nextAttemptAt?: string; waitStartedAt?: string; attempts?: number
  steps: Array<{
    kind: 'local' | 'edgeone' | 'cloudflare' | 'dns' | 'verification'; host: string; role?: string; action?: string
    ok: boolean; changes: boolean; message?: string; status: PublishStepStatus; updatedAt: string
    steps?: Array<{ action: string; target: string; detail: string; from?: string; to?: string }>
  }>
}
export interface PublishInput { revision: number; publishFingerprint: string; requestId: string }
export interface PublishSubmission extends PublishInput { serviceId: string }
export interface PublishJobsResponse { jobs: PublishJob[]; error?: string }
export interface Preview {
  revision: number; publishFingerprint: string; publicAddress: string; config: string
  dryRun: boolean; warnings?: string[]; impact?: string[]
  services: Array<Service & { entry: string; change: 'create' | 'update' | 'unchanged'; certificate: string; port: number; domains: string[] }>
}
export interface Confirmation { host: string; token: string; revision: number; confirm: boolean; confirmOverwrite?: boolean; confirmFallbackReplacement?: boolean }
export interface DNSConflictRecord {
  recordId: string; type: string; value: string; ttl: number
  name?: string; line?: string; lineId?: string; status?: string; remark?: string; weight?: number; proxied?: boolean
}
export interface DNSPreview {
  host: string; token: string; revision: number
  action: 'create' | 'update' | 'delete' | 'replace' | 'adopt' | 'release' | 'noop'
  type?: string; value?: string; ttl?: number; zone?: string; accountId?: string; subDomain?: string
  previous?: { recordId: number; opaqueId?: string; type: string; value: string; ttl: number } | null
  conflicts?: DNSConflictRecord[]; warnings?: string[]
  requiresOverwrite?: boolean
  recovery?: { before: DNSConflictRecord[]; current: DNSConflictRecord[]; phase?: string }
}
export interface ServiceDeleteInput { deleteDNS: boolean; deleteEdgeOne: boolean; deleteCloudflare?: boolean; revision?: number }
export interface AccountInput { name: string; secretId: string; secretKey: string; originMode?: 'address' | 'base' }
export interface AccountUpdate { name?: string; originMode?: 'address' | 'base'; enabled?: boolean }
export interface DNSAccountInput { name: string; provider?: DNSProvider; secretId?: string; secretKey?: string; apiToken?: string; enabled?: boolean }
export interface TrafficCounter { requests: number; receivedBytes: number; sentBytes: number; totalBytes: number }
export interface TrafficResponse {
  status: 'collecting' | 'waiting' | 'needs-apply' | 'disabled' | 'error'
  startedAt: string | null; updatedAt: string | null; lastRecordAt: string | null; timezone: 'Asia/Shanghai'
  totals: TrafficCounter; today: TrafficCounter
  days: Array<TrafficCounter & { date: string }>
  services: Array<{ serviceId: string; name: string; deleted: boolean; enabled: boolean; serviceStatus?: 'active' | 'disabled' | 'draft' | 'deleted' | 'unknown'; totals: TrafficCounter; today: TrafficCounter; lastSeenAt: string | null }>
  warnings: string[]; incomplete?: boolean; catchingUp?: boolean
}
// Secondary diagnostic/cloud result shapes remain opaque during this staged
// migration. They are not `any`: checked consumers must narrow before using them.
export type JSONValue = string | number | boolean | null | JSONValue[] | { [key: string]: JSONValue }
export type JSONRecord = { [key: string]: JSONValue }
type Read<T> = (options?: RequestOptions) => Promise<T>
type ByID<T> = (id: string, options?: RequestOptions) => Promise<T>
export interface GatewayAPI {
  profile: Read<Profile>
  updateProfile: (profile: Profile, options?: RequestOptions) => Promise<Profile>
  session: Read<{ authenticated: boolean }>
  login: (password: string, options?: RequestOptions) => Promise<{ authenticated?: boolean; ok?: boolean }>
  logout: Read<{ ok?: boolean }>
  state: Read<GatewayState>; exportConfig: Read<GatewayState>; preview: Read<Preview>
  apply: (revision: number, options?: RequestOptions) => Promise<{ ok: boolean; publicAddress: string; state: GatewayState }>
  createService: (service: Service, options?: RequestOptions) => Promise<GatewayState>
  updateService: (id: string, service: Service, options?: RequestOptions) => Promise<GatewayState>
  deleteService: (id: string, input?: ServiceDeleteInput, options?: RequestOptions) => Promise<GatewayState>
  probeService: ByID<JSONRecord>; checkService: ByID<JSONRecord>; publishService: ByID<JSONRecord>
  checkServiceExternal: ByID<MonitorStatus>
  startPublishJob: (id: string, input: PublishInput, options?: RequestOptions) => Promise<{ job: PublishJob }>
  publishJobs: Read<PublishJobsResponse>
  operationReceipt: ByID<{ status: string; requestId: string; resourceId?: string; job?: PublishJob }>
  createCertificate: (name: string, options?: RequestOptions) => Promise<GatewayState>
  importCertificate: (id: string, form: FormData, options?: RequestOptions) => Promise<GatewayState>
  deleteCertificate: ByID<GatewayState>
  diagnostics: Read<JSONRecord>; publicPreflight: Read<JSONRecord>; publicAddress: Read<JSONRecord>
  traffic: Read<TrafficResponse>
  externalProbes: Read<ExternalReport>
  externalLimits: Read<ExternalQuota>
  startExternalProbe: (input: ExternalInput, options?: RequestOptions) => Promise<ExternalJob>
  dnsAccounts: Read<JSONRecord>; dnsRecords: Read<JSONRecord>; dnsStatus: Read<JSONRecord>
  dnsObservation: ByID<JSONRecord>
  checkCloudflareEntry: ByID<JSONRecord>
  startCloudflareHTTPValidation: (host: string, input: { confirm: true; revision: number }, options?: RequestOptions) => Promise<CloudflareHTTPValidationResult>
  createDNSAccount: (account: DNSAccountInput, options?: RequestOptions) => Promise<GatewayState>
  updateDNSAccount: (id: string, account: Partial<DNSAccountInput>, options?: RequestOptions) => Promise<GatewayState>
  dnsBindings: Read<{ bindings: DNSBinding[] }>
  saveDNSBindings: (input: { bindings: DNSBinding[] }, options?: RequestOptions) => Promise<{ bindings: DNSBinding[] }>
  dnsRecordAccounts: ByID<DNSRecordAccounts>
  selectDNSRecordAccount: (host: string, input: { revision: number; accountId: string; zone: string }, options?: RequestOptions) => Promise<DNSRecordAccounts>
  deleteDNSAccount: ByID<GatewayState>; previewDNS: ByID<DNSPreview>
  applyDNS: (confirmation: Confirmation, options?: RequestOptions) => Promise<JSONRecord>
  recoverDNSOverwrite: (host: string, input: { confirmRecovery: boolean }, options?: RequestOptions) => Promise<JSONRecord>
  edgeAccounts: Read<JSONRecord>; edgeStatus: Read<JSONRecord>
  createEdgeAccount: (account: AccountInput, options?: RequestOptions) => Promise<GatewayState>
  updateEdgeAccount: (id: string, account: AccountUpdate, options?: RequestOptions) => Promise<GatewayState>
  deleteEdgeAccount: ByID<GatewayState>; previewEdge: ByID<JSONRecord>
  applyEdge: (confirmation: Confirmation, options?: RequestOptions) => Promise<JSONRecord>
  cloudflareAccounts: Read<{ accounts: CloudflareAccount[] }>
  discoverCloudflareAccount: (input: { apiToken: string }, options?: RequestOptions) => Promise<CloudflareAccountDiscovery>
  createCloudflareAccount: (account: CloudflareAccountInput, options?: RequestOptions) => Promise<GatewayState>
  updateCloudflareAccount: (id: string, account: { name?: string; apiToken?: string; enabled?: boolean; autoSSLStrict?: boolean }, options?: RequestOptions) => Promise<GatewayState>
  deleteCloudflareAccount: ByID<GatewayState>
  cloudflareHosts: Read<{ hosts: CloudflareHost[] }>
  previewCloudflare: ByID<CloudflarePreview>
  applyCloudflare: (confirmation: Confirmation, options?: RequestOptions) => Promise<JSONRecord>
  edgeWorkspaceAccounts: Read<{ accounts: EdgeWorkspaceAccount[]; baseDomain: string; originModes: Array<{ value: string; label: string }> }>
  edgeWorkspaceHosts: Read<{ hosts: EdgeWorkspaceHost[]; errors: Array<{ provider: EdgeProvider; message: string }> }>
  createEdgeWorkspaceAccount: (account: EdgeWorkspaceAccountInput, options?: RequestOptions) => Promise<GatewayState>
  updateEdgeWorkspaceAccount: (provider: EdgeProvider, id: string, account: AccountUpdate & { apiToken?: string }, options?: RequestOptions) => Promise<GatewayState>
  deleteEdgeWorkspaceAccount: (provider: EdgeProvider, id: string, options?: RequestOptions) => Promise<GatewayState>
  previewEdgeWorkspace: (provider: EdgeProvider, host: string, options?: RequestOptions) => Promise<JSONRecord>
  applyEdgeWorkspace: (confirmation: Confirmation & { provider: EdgeProvider }, options?: RequestOptions) => Promise<JSONRecord>
  saveSettings: (settings: Partial<Settings>, options?: RequestOptions) => Promise<GatewayState>
  securitySessions: Read<{ sessions: SecuritySession[] }>
  changePassword: (input: PasswordChange, options?: RequestOptions) => Promise<{ ok: true; relogin: true }>
  deleteSession: ByID<{ ok: boolean; relogin?: boolean }>
  revokeSessions: (scope: 'others' | 'all', options?: RequestOptions) => Promise<{ ok: boolean; relogin?: boolean }>
  history: Read<{ entries: HistoryEntry[]; error?: string }>
  previewHistory: (id: string, revision: number, options?: RequestOptions) => Promise<HistoryPreview>
  restoreHistory: (id: string, input: HistoryRestore, options?: RequestOptions) => Promise<HistoryRestored>
  downloadBackup: (input: BackupInput, options?: RequestOptions) => Promise<DownloadFile>
  monitorSettings: Read<MonitorSettings>
  saveMonitorSettings: (settings: MonitorSettings, options?: RequestOptions) => Promise<MonitorSettings>
  monitorStatus: Read<MonitorStatus>
  checkMonitor: Read<MonitorStatus>
  notificationChannels: Read<{ channels: NotificationChannel[] }>
  createNotification: (input: NotificationInput, options?: RequestOptions) => Promise<{ channels: NotificationChannel[] }>
  updateNotification: (id: string, input: NotificationInput, options?: RequestOptions) => Promise<{ channels: NotificationChannel[] }>
  deleteNotification: ByID<{ channels: NotificationChannel[] }>
  testNotification: ByID<{ ok: boolean }>
}
