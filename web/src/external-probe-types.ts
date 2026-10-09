export interface ExternalQuota { type: 'ip'; limit: number; remaining: number; reset: number; checkedAt: string }
export interface ExternalTarget {
  key: string; serviceId: string; name: string; url: string; host: string; port: number
  protocol: 'HTTP' | 'HTTPS'; ipVersions: Array<4 | 6>; disabledReason: string
}
export interface ExternalInput { requestId: string; targetKey: string; ipVersion: 4 | 6; nodes: number; confirm: boolean }
export interface ExternalNode {
  country: string; city: string; network: string; asn: number
  status: 'ok' | 'failed' | 'reachable' | 'unknown'; httpStatus: number; latencyMs: number; message: string
}
export interface ExternalJob {
  id: string; target: ExternalTarget; ipVersion: 4 | 6; requestedNodes: number; probesCount: number
  measurementId: string; status: 'running' | 'ok' | 'partial' | 'failed' | 'unknown' | 'unavailable'
  message: string; startedAt: string; updatedAt: string; nodes: ExternalNode[]
}
export interface ExternalReport {
  provider: 'Globalping'; country: 'CN'; mode: 'anonymous'; staleAfterSeconds: number
  targets: ExternalTarget[]; jobs: ExternalJob[]; quota: ExternalQuota | null; error: string
}
