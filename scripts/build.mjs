import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { copyFileSync, existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { DEPLOYMENT_ASSETS, LICENSE_ASSETS, RELEASE_BINARY, RELEASE_TARGET } from './release-layout.mjs'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
let output = path.join(root, 'dist', 'release')
let install = true
const args = process.argv.slice(2)
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--out-dir' && args[i + 1]) output = path.resolve(args[++i])
  else if (args[i] === '--skip-install') install = false
  else throw new Error(`Unknown or incomplete build option: ${args[i]}`)
}

function run(command, args, cwd = root, env = process.env) {
  const result = spawnSync(command, args, {
    cwd, env, stdio: 'inherit', shell: process.platform === 'win32' && command === 'npm.cmd',
  })
  if (result.error) throw result.error
  if (result.status !== 0) throw new Error(`${command} failed (${result.status ?? result.signal})`)
}

function git(args, fallback) {
  const result = spawnSync('git', args, { cwd: root, encoding: 'utf8' })
  return result.status === 0 ? result.stdout.trim() : fallback
}

function sha256(file) {
  return createHash('sha256').update(readFileSync(file)).digest('hex')
}

function assets(dir, prefix = '') {
  return readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name)).flatMap(entry => {
    const file = path.join(dir, entry.name)
    const name = `${prefix}${entry.name}`
    if (entry.isDirectory()) return assets(file, `${name}/`)
    if (!entry.isFile()) throw new Error(`Unexpected non-regular build asset: ${name}`)
    return [{ path: name, sha256: sha256(file) }]
  })
}

const version = readFileSync(path.join(root, 'internal/buildinfo/version.txt'), 'utf8').trim()
if (!/^\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?$/.test(version)) throw new Error('Invalid canonical version')
const startupTemplate = JSON.parse(readFileSync(path.join(root, 'config.example.json'), 'utf8'))
if (Object.keys(startupTemplate).sort().join(',') !== 'gatewayIp,initialKey,port'
  || startupTemplate.gatewayIp !== '' || startupTemplate.initialKey !== ''
  || !Number.isInteger(startupTemplate.port) || startupTemplate.port < 1 || startupTemplate.port > 65535) {
  throw new Error('config.example.json must remain a blank public template; fill only the private config.json')
}
const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm'
const bundledGo = path.join(root, '.tools', 'go', 'bin', process.platform === 'win32' ? 'go.exe' : 'go')
const go = process.env.GO_BINARY || (existsSync(bundledGo) ? bundledGo : 'go')
const commit = git(['rev-parse', 'HEAD'], 'unknown')
const status = git(['status', '--porcelain', '--untracked-files=normal'], null)
const dirty = status === null ? 'unknown' : String(status !== '')
const buildTime = new Date().toISOString()

if (install) run(npm, ['ci'], path.join(root, 'web'))
run(npm, ['run', 'typecheck'], path.join(root, 'web'))
run(npm, ['run', 'build'], path.join(root, 'web'))
mkdirSync(output, { recursive: true })
const binaryName = RELEASE_BINARY
const binary = path.join(output, binaryName)
const flags = `-X w103d/gateway/internal/buildinfo.Commit=${commit} -X w103d/gateway/internal/buildinfo.BuildTime=${buildTime} -X w103d/gateway/internal/buildinfo.Dirty=${dirty}`
run(go, ['build', '-trimpath', '-ldflags', flags, '-o', binary, '.'], root,
  { ...process.env, GOOS: 'linux', GOARCH: 'arm64', CGO_ENABLED: '0' })

const manifest = {
  version, commit, dirty, buildTime, target: RELEASE_TARGET,
  checks: { typecheck: true, frontendBuild: true, goBuild: true, tests: 'not run by build script' },
  binary: { path: binaryName, sha256: sha256(binary) },
  frontend: assets(path.join(root, 'web/dist')),
}
manifest.deployment = DEPLOYMENT_ASSETS.map(name => {
  const source = name === 'config.json' ? path.join(root, 'config.example.json') : path.join(root, 'deploy', name)
  copyFileSync(source, path.join(output, name))
  return { path: name, sha256: sha256(path.join(output, name)) }
})
manifest.licenses = LICENSE_ASSETS.map(asset => {
  copyFileSync(path.join(root, asset.source), path.join(output, asset.path))
  return { path: asset.path, sha256: sha256(path.join(output, asset.path)) }
})
const manifestPath = path.join(output, 'build-manifest.json')
writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`, 'utf8')
writeFileSync(path.join(output, 'SHA256SUMS'), `${manifest.binary.sha256}  ${binaryName}\n${sha256(manifestPath)}  build-manifest.json\n${[...manifest.deployment, ...manifest.licenses].map(asset => `${asset.sha256}  ${asset.path}\n`).join('')}`, 'utf8')
console.log(`Built ${version} (${commit}, dirty=${dirty}) at ${output}. Tests and device deployment are separate.`)
