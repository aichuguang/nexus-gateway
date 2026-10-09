// Dependency-free, redacted pre-commit guard. This complements, not replaces,
// provider-side key rotation and a dedicated full-history secret scanner.
import { spawnSync } from 'node:child_process'
import { lstatSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const MAX_FILE_BYTES = 16 * 1024 * 1024
const privateDirectories = /(?:^|\/)(?:\.codex|\.claude|\.local|\.tools|node_modules|dist|design)(?:\/|$)/i
const privateDocuments = /^docs\/(?:audit-2026-09-28|implementation-plan|network-security-audit-2026-10-02|project-review-2026-10-03|review-remediation-2026-10-03|task-status)\.md$/
const privateNames = new Set([
  'initial-password', 'password.sha256', 'state.json', 'applied.json',
  'control.sock', 'runtime.lock', 'profile.json', 'runtime-maintenance.json',
  'runtime-proxy.json', 'idempotency.json', 'state-migration.json',
  'runtime-transaction.json', 'cloudflare-dcv-delegations.json',
  'cloudflare-http-validations.json', 'nginx.pid',
  'edge-origins.json', 'managed-nginx.conf', 'gateway-security.js',
  'gateway-config.json', 'dns-credentials.json', 'dns-managed.json',
  'dns-pending.json', 'publish-jobs.json', 'service-deletions.json',
  'config-history.json', 'monitor-settings.json', 'notifications.json',
  'traffic-state.json', 'external-probes.json', 'cloudflare-ledger.json',
  'dns-verification.json', 'id_rsa', 'id_ed25519', 'known_hosts',
])

function privatePath(name) {
  const base = path.posix.basename(name).toLowerCase()
  if (name === 'config.json') return true
  if (privateDirectories.test(name) || privateDocuments.test(name) || privateNames.has(base)) return true
  if (base === 'cookie_guard.js' && name !== 'internal/engine/cookie_guard.js') return true
  if (/(?:^|\/)(?:operation-receipts|certificates|cert-inbox|schema-backup-[^/]+|[^/]+\.json\.versions|dns-migrations|cloud-operation-history|dns-txt-cleanup|dns-txt-archive)\//i.test(name)) return true
  if (/(?:^|\/)(?:cloudflare|dns|edgeone)-accounts\//i.test(name)) return true
  if (/\.(?:pem|key|p12|pfx|crt|cer|jks|keystore|db|sqlite3?|bak|backup|enc|exe|tar\.gz|zip|log)$/i.test(base)) return true
  if (/^\.env(?:\.|$)/i.test(base)) return base !== '.env.example'
  if (/\.env$/i.test(base)) return !['deploy/gateway.env', 'deploy/w103d-gateway.env'].includes(name)
  return /^(?:gateway|w103d-gateway)(?:-linux-[a-z0-9]+)?$/.test(base)
}

function entropy(value) {
  const counts = new Map()
  for (const c of value) counts.set(c, (counts.get(c) || 0) + 1)
  let bits = 0
  for (const count of counts.values()) {
    const ratio = count / value.length
    bits -= ratio * Math.log2(ratio)
  }
  return bits
}

function placeholder(value) {
  return /^(?:<[^>]+>|\$\{[\s\S]+\}|\$[A-Z_]+|REDACTED|YOUR_[A-Z_]+|CHANGE_ME|REPLACE_ME)$/i.test(value)
    || /^(?:test|fake|dummy|example|sample|mock|placeholder|not-a-real)[-_]/i.test(value)
    || /^(?:x{4,}|0{4,}|a{4,}|\.{3})$/i.test(value)
}

const tokenRules = [
  ['github-token', /\bgh[pousr]_[A-Za-z0-9]{30,255}\b/g],
  ['github-fine-grained-token', /\bgithub_pat_[A-Za-z0-9_]{60,255}\b/g],
  ['aws-access-key', /\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/g],
  ['tencent-access-key-id', /\bAKID[A-Za-z0-9]{28,40}\b/g],
  ['alicloud-access-key-id', /\bLTAI[A-Za-z0-9]{12,30}\b/g],
  ['slack-token', /\bxox[baprs]-[A-Za-z0-9-]{20,}\b/g],
  ['stripe-secret-key', /\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b/g],
  ['jwt-token', /\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b/g],
  ['secret-webhook', /https:\/\/(?:qyapi\.weixin\.qq\.com\/cgi-bin\/webhook\/send\?key=|open\.feishu\.cn\/open-apis\/bot\/v2\/hook\/|oapi\.dingtalk\.com\/robot\/send\?access_token=)([A-Za-z0-9_-]{12,})/g],
]

export function inspectFile(name, bytes) {
  name = name.replaceAll('\\', '/')
  const findings = []
  const add = (rule, index = 0, source = '') => findings.push({ file: name, line: source.slice(0, index).split('\n').length, rule })
  if (privatePath(name)) add('private-or-generated-file')
  if (bytes.length > MAX_FILE_BYTES) {
    add('file-exceeds-scan-limit')
    return { findings, binary: false }
  }
  if (bytes.includes(0)) {
    if (/\.(?:md|go|[cm]?js|ts|vue|json|ya?ml|sh|py|html|css|svg|txt)$/.test(name)) add('nul-in-text-file')
    return { findings, binary: true }
  }
  let source
  try {
    source = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes)
  } catch {
    if (/\.(?:md|go|[cm]?js|ts|vue|json|ya?ml|sh|py|html|css|svg|txt)$/.test(name)) add('invalid-utf8')
    return { findings, binary: true }
  }
  if (source.charCodeAt(0) === 0xfeff) add('utf8-bom')
  if (name === 'config.example.json') {
    try {
      const config = JSON.parse(source)
      if (config.initialKey !== '') add('initial-key-in-public-template')
    } catch { add('invalid-config-template') }
  }
  for (const [rule, regex] of tokenRules) {
    for (const match of source.matchAll(new RegExp(regex))) {
      if (!placeholder(match[1] || match[0])) add(rule, match.index, source)
    }
  }
  // A UI placeholder displaying only a PEM header is not private key material.
  const header = /-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----/g
  for (const match of source.matchAll(header)) {
    const tail = source.slice(match.index + match[0].length).replaceAll('\\n', '\n').replaceAll('\\r', '\r')
    if (/^\s*[A-Za-z0-9+/=\s]{40,}/.test(tail)) add('private-key-material', match.index, source)
  }
  const assignment = /(?:["']?\b(?:api[_-]?(?:key|token)|access[_-]?(?:token|key(?:[_-]?id)?)|auth[_-]?token|token|client[_-]?secret|secret(?:[_-]?(?:key|id))?|password|passwd|smtp[_-]?pass(?:word)?|(?:gateway|w103d)[_-]?ssh[_-]?pass)["']?\s*[:=]\s*["'])([^"'\r\n]{8,})(?:["'])/gi
  for (const match of source.matchAll(assignment)) {
    const value = match[1]
    if (!placeholder(value) && value.length >= 16 && entropy(value) >= 3.5 && !/\s/.test(value)) {
      add('hardcoded-credential-candidate', match.index, source)
    }
  }
  const basicAuth = /https?:\/\/[^\s:/"'<>]+:([^\s@/"'<>]+)@([^\s/"'<>]+)/g
  for (const match of source.matchAll(basicAuth)) {
    if (!placeholder(match[1]) && !/(?:^|\.)(?:example\.(?:com|net|org)|invalid|test)(?::\d+)?$/i.test(match[2])) {
      add('url-embedded-password', match.index, source)
    }
  }
  const homes = /\b[A-Z]:[\\/](?:Users|Documents and Settings)[\\/][A-Za-z0-9_.-]+[\\/]/g
  for (const match of source.matchAll(homes)) add('personal-machine-path', match.index, source)
  return { findings: findings.filter((item, index, all) => all.findIndex(other => other.rule === item.rule && other.line === item.line) === index), binary: false }
}

function git(cwd, args, options = {}) {
  const result = spawnSync('git', args, { cwd, maxBuffer: 128 * 1024 * 1024, ...options })
  // Git's stderr may contain private file contents: never forward it.
  if (result.error || result.status !== 0) throw new Error(`Git operation failed: ${args[0]}`)
  return result.stdout
}

function indexFiles(cwd) {
  const entries = git(cwd, ['ls-files', '--stage', '-z']).toString('utf8').split('\0').filter(Boolean).map(record => {
    const split = record.indexOf('\t')
    const [mode, oid, stage] = record.slice(0, split).split(' ')
    if (stage !== '0') throw new Error('Resolve unmerged index entries before scanning.')
    return { name: record.slice(split + 1), mode, oid }
  })
  const ids = [...new Set(entries.map(entry => entry.oid))]
  if (ids.length === 0) return []
  const batch = git(cwd, ['cat-file', '--batch'], { input: `${ids.join('\n')}\n` })
  const blobs = new Map()
  let offset = 0
  for (const expected of ids) {
    const end = batch.indexOf(10, offset)
    if (end < 0) throw new Error('Incomplete Git blob response.')
    const [oid, type, length] = batch.subarray(offset, end).toString('ascii').split(' ')
    const size = Number(length)
    if (oid !== expected || type !== 'blob' || !Number.isSafeInteger(size) || size < 0 || end + 2 + size > batch.length) throw new Error('Unexpected Git object in index.')
    blobs.set(oid, batch.subarray(end + 1, end + 1 + size))
    offset = end + size + 2
  }
  return entries.map(entry => ({ ...entry, bytes: blobs.get(entry.oid) }))
}

export function scanRepository(cwd, staged = false) {
  let files
  if (staged) {
    files = indexFiles(cwd)
  } else {
    const names = git(cwd, ['ls-files', '--cached', '--others', '--exclude-standard', '-z']).toString('utf8').split('\0').filter(Boolean)
    files = [...new Set(names)].flatMap(name => {
      const filename = path.join(cwd, name)
      let stat
      try { stat = lstatSync(filename) } catch (error) {
        if (error.code === 'ENOENT') return []
        throw new Error('Cannot inspect a source file; scan stopped without exposing its contents.')
      }
      if (!stat.isFile()) return [{ name, mode: '120000', bytes: Buffer.alloc(0) }]
      if (stat.size > MAX_FILE_BYTES) return [{ name, oversized: true, bytes: Buffer.alloc(0) }]
      return [{ name, bytes: readFileSync(filename) }]
    })
  }
  const findings = []
  let binaries = 0
  for (const file of files) {
    if (file.mode === '120000' || file.mode === '160000') {
      findings.push({ file: file.name, line: 1, rule: 'unreviewed-symlink-or-submodule' })
      continue
    }
    if (file.oversized) {
      findings.push({ file: file.name, line: 1, rule: 'file-exceeds-scan-limit' })
      continue
    }
    const result = inspectFile(file.name, file.bytes)
    findings.push(...result.findings)
    binaries += Number(result.binary)
  }
  return { mode: staged ? 'index' : 'working-tree', files: files.length, binaryFiles: binaries, findings }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const args = process.argv.slice(2)
    if (args.length > 1 || args.some(arg => !['--staged', '--working-tree'].includes(arg))) throw new Error('Usage: node scripts/scan-secrets.mjs [--staged|--working-tree]')
    const report = scanRepository(process.cwd(), args.includes('--staged'))
    for (const finding of report.findings) console.error(`${JSON.stringify(finding.file)}:${finding.line}: ${finding.rule} (content redacted)`)
    console.log(`Secret guard: ${report.files} files (${report.mode}), ${report.binaryFiles} binary files not content-scanned, ${report.findings.length} findings. History is not scanned by this command.`)
    if (report.findings.length) process.exitCode = 1
  } catch (error) {
    console.error(error instanceof Error ? error.message : 'Secret scan failed.')
    process.exitCode = 2
  }
}
