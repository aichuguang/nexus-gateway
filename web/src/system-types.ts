import type { GatewayState, JSONValue } from './api-types'

export interface SecuritySession {
  id: string; current: boolean; createdAt: string; lastSeenAt: string; expiresAt: string; client: string; ip: string
}
export interface PasswordChange { currentPassword: string; newPassword: string; confirmPassword: string }
export interface BackupInput { password: string; passphrase: string; confirmPassphrase: string }
export interface DownloadFile { blob: Blob; filename: string }
export interface HistoryEntry { id: string; createdAt: string; revision: number; summary: string; serviceCount: number }
export interface HistoryPreview {
  id: string; revision: number; token: string; changes: Array<{ path: string; before: JSONValue; after: JSONValue }>
  warnings: string[]; canRestore: boolean
}
export interface HistoryRestore { revision: number; token: string; confirm: true; password: string }
export interface HistoryRestored { state: GatewayState; message: string }
export interface MonitorSettings { enabled: boolean; intervalSeconds: number; checkPing: boolean; checkTCP: boolean; checkHTTP: boolean; checkExternal?: boolean }
export type MonitorCheckStatus = 'ok' | 'error' | 'checking' | 'recovering' | 'unknown' | 'not-applicable'
export interface MonitorCheck { classification?: string; status: MonitorCheckStatus; latencyMs?: number; statusCode?: number; message?: string; checkedAt?: string; details?: Array<{ url: string; ipVersion?: number; status: string; statusCode?: number; message?: string; nodes?: import('./external-probe-types').ExternalNode[] }> }
export interface MonitorService {
  serviceId: string; name: string; status: MonitorCheckStatus; lastCheckedAt: string
  consecutiveFailures: number; consecutiveSuccesses: number; checks: { ping: MonitorCheck; tcp: MonitorCheck; http: MonitorCheck; local?: MonitorCheck; external?: MonitorCheck }
}
export interface MonitorStatus {
  enabled: boolean; running: boolean; lastCheckedAt: string; error: string; failureThreshold: number; recoveryThreshold: number
  services: MonitorService[]
  externalQuota?: import('./external-probe-types').ExternalQuota
}
export type NotificationType = 'wecom' | 'feishu' | 'dingtalk' | 'smtp' | 'webhook'
export type SMTPTLS = 'starttls' | 'tls'
export interface NotificationChannel {
  id: string; name: string; type: NotificationType; enabled: boolean; endpointHint: string; hasEndpoint: boolean; hasSecret: boolean
  smtpHost: string; smtpPort: number; smtpUsername: string; smtpFrom: string; smtpTo: string[]; smtpTLS: SMTPTLS | ''; hasPassword: boolean
  lastStatus: 'ok' | 'error' | 'unknown'; lastAttemptAt: string; lastError: string
}
export interface NotificationInput {
  name: string; type: NotificationType; enabled: boolean; endpoint?: string; secret?: string
  smtpHost?: string; smtpPort?: number; smtpUsername?: string; smtpPassword?: string; smtpFrom?: string; smtpTo?: string[]; smtpTLS?: SMTPTLS
}
