import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
  chmodSync, copyFileSync, existsSync, lstatSync, mkdirSync, mkdtempSync,
  readFileSync, readdirSync, rmSync, writeFileSync,
} from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { DEPLOYMENT_ASSETS, LICENSE_ASSETS, RELEASE_BINARY, RELEASE_FILES, RELEASE_TARGET } from './release-layout.mjs'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const versionPattern = /^\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?$/
const hashPattern = /^[a-f0-9]{64}$/
const commitPattern = /^[a-f0-9]{40}$/

function sha256(file) {
  return createHash('sha256').update(readFileSync(file)).digest('hex')
}

function fields(value, names, label) {
  assert(value && typeof value === 'object' && !Array.isArray(value), `Invalid ${label}`)
  assert.deepEqual(Object.keys(value).sort(), [...names].sort(), `Unexpected fields in ${label}`)
}

function asset(value, label) {
  fields(value, ['path', 'sha256'], label)
  assert(typeof value.path === 'string' && value.path.length > 0, `Invalid path in ${label}`)
  assert(!value.path.includes('\\') && !path.posix.isAbsolute(value.path)
    && value.path.split('/').every(part => part && part !== '.' && part !== '..')
    && !/[\x00-\x1f\x7f:]/.test(value.path), `Unsafe path in ${label}`)
  assert(typeof value.sha256 === 'string' && hashPattern.test(value.sha256), `Invalid checksum in ${label}`)
}

export function validateRelease(input, { tag, requireClean = false, expectedCommit } = {}) {
  const inputStat = lstatSync(input)
  assert(inputStat.isDirectory() && !inputStat.isSymbolicLink(), 'Release input must be a regular directory')
  assert.deepEqual(readdirSync(input).sort(), [...RELEASE_FILES].sort(),
    'Release directory must contain exactly the allowlisted files; use a fresh build directory')
  for (const name of RELEASE_FILES) {
    const stat = lstatSync(path.join(input, name))
    assert(stat.isFile() && !stat.isSymbolicLink(), `Release asset is not a regular file: ${name}`)
  }
  const manifest = JSON.parse(readFileSync(path.join(input, 'build-manifest.json'), 'utf8'))
  fields(manifest, ['version', 'commit', 'dirty', 'buildTime', 'target', 'checks', 'binary', 'frontend', 'deployment', 'licenses'], 'manifest')
  assert(typeof manifest.version === 'string' && versionPattern.test(manifest.version), 'Invalid release version')
  assert(manifest.commit === 'unknown' || (typeof manifest.commit === 'string' && commitPattern.test(manifest.commit)), 'Invalid build commit')
  assert(['true', 'false', 'unknown'].includes(manifest.dirty), 'Invalid dirty status')
  assert(typeof manifest.buildTime === 'string' && !Number.isNaN(Date.parse(manifest.buildTime))
    && new Date(manifest.buildTime).toISOString() === manifest.buildTime, 'Invalid build time')
  assert.equal(manifest.target, RELEASE_TARGET, 'Unsupported release target')
  fields(manifest.checks, ['typecheck', 'frontendBuild', 'goBuild', 'tests'], 'build checks')
  assert.equal(manifest.checks.typecheck, true, 'Typecheck was not completed')
  assert.equal(manifest.checks.frontendBuild, true, 'Frontend build was not completed')
  assert.equal(manifest.checks.goBuild, true, 'Go build was not completed')
  assert.equal(manifest.checks.tests, 'not run by build script', 'Unexpected test provenance')
  if (tag !== undefined) assert.equal(tag, `v${manifest.version}`, 'Tag must equal v + the canonical build version')
  if (requireClean) {
    assert.equal(manifest.dirty, 'false', 'Public release requires a clean build')
    assert(commitPattern.test(manifest.commit), 'Public release requires a known Git commit')
  }
  if (expectedCommit !== undefined) {
    assert(commitPattern.test(expectedCommit), 'Invalid expected commit')
    assert.equal(manifest.commit, expectedCommit, 'Build commit does not match the requested commit')
  }
  asset(manifest.binary, 'binary')
  assert.equal(manifest.binary.path, RELEASE_BINARY, 'Unexpected binary name')
  assert(Array.isArray(manifest.deployment), 'Missing deployment assets')
  for (const entry of manifest.deployment) asset(entry, 'deployment asset')
  assert.deepEqual(manifest.deployment.map(entry => entry.path).sort(), [...DEPLOYMENT_ASSETS].sort(), 'Unexpected deployment assets')
  assert(Array.isArray(manifest.licenses), 'Missing redistribution licenses')
  for (const entry of manifest.licenses) asset(entry, 'license asset')
  assert.deepEqual(manifest.licenses.map(entry => entry.path).sort(), LICENSE_ASSETS.map(entry => entry.path).sort(), 'Unexpected license assets')
  assert(Array.isArray(manifest.frontend) && manifest.frontend.length > 0, 'Missing frontend provenance')
  for (const entry of manifest.frontend) asset(entry, 'frontend asset')
  assert.equal(new Set(manifest.frontend.map(entry => entry.path)).size, manifest.frontend.length, 'Duplicate frontend assets')
  for (const entry of [manifest.binary, ...manifest.deployment, ...manifest.licenses]) {
    assert.equal(sha256(path.join(input, entry.path)), entry.sha256, `Checksum mismatch: ${entry.path}`)
  }

  const binary = readFileSync(path.join(input, RELEASE_BINARY))
  assert(binary.length >= 64 && binary.subarray(0, 4).equals(Buffer.from([0x7f, 0x45, 0x4c, 0x46]))
    && binary[4] === 2 && binary[5] === 1 && binary.readUInt16LE(18) === 183,
  'Expected a Linux ARM64 ELF binary')
  const checksums = new Map()
  for (const line of readFileSync(path.join(input, 'SHA256SUMS'), 'utf8').trimEnd().split('\n')) {
    const match = /^([a-f0-9]{64})  ([A-Za-z0-9_.-]+)$/.exec(line)
    assert(match, 'Invalid SHA256SUMS line')
    assert(!checksums.has(match[2]), 'Duplicate SHA256SUMS path')
    checksums.set(match[2], match[1])
  }
  assert.deepEqual([...checksums.keys()].sort(), RELEASE_FILES.filter(name => name !== 'SHA256SUMS').sort(),
    'SHA256SUMS must cover exactly the public release assets')
  for (const [name, hash] of checksums) assert.equal(sha256(path.join(input, name)), hash, `SHA256SUMS mismatch: ${name}`)
  return manifest
}

export function packageRelease({ input = path.join(root, 'dist', 'release'), output = path.join(root, 'dist', 'packages'), ...options } = {}) {
  input = path.resolve(input)
  output = path.resolve(output)
  const manifest = validateRelease(input, options)
  const name = `gateway-${manifest.version}-linux-arm64.tar.gz`
  const outputFiles = [name, 'SHA256SUMS']
  if (existsSync(output)) {
    const stat = lstatSync(output)
    assert(stat.isDirectory() && !stat.isSymbolicLink(), 'Package output must be a regular directory')
    for (const file of readdirSync(output)) {
      const fileStat = lstatSync(path.join(output, file))
      assert(outputFiles.includes(file) && fileStat.isFile() && !fileStat.isSymbolicLink(),
        'Package output contains unexpected files; choose a fresh --out-dir')
    }
  }
  const temporary = mkdtempSync(path.join(os.tmpdir(), 'gateway-package-'))
  try {
    const stage = path.join(temporary, 'release')
    mkdirSync(stage)
    for (const file of RELEASE_FILES) {
      const destination = path.join(stage, file)
      copyFileSync(path.join(input, file), destination)
      chmodSync(destination, file === RELEASE_BINARY || file.endsWith('.sh') || file.endsWith('.py') ? 0o755 : 0o644)
    }
    // Validate the copied bytes, not only the potentially changing source files.
    validateRelease(stage, options)
    const archive = path.join(temporary, name)
    const env = { ...process.env, COPYFILE_DISABLE: '1' }
    delete env.TAR_OPTIONS
    delete env.TAPE
    delete env.GZIP
    const tar = spawnSync('tar', ['-czf', archive, '-C', stage, '--', ...RELEASE_FILES], { env, encoding: 'utf8' })
    if (tar.error) throw tar.error
    assert.equal(tar.status, 0, `tar failed: ${tar.stderr || tar.signal || tar.status}`)
    mkdirSync(output, { recursive: true })
    copyFileSync(archive, path.join(output, name))
    writeFileSync(path.join(output, 'SHA256SUMS'), `${sha256(archive)}  ${name}\n`, 'utf8')
    return { name, output, manifest }
  } finally {
    // Delete only the unique temporary directory that this invocation created.
    assert(path.dirname(temporary) === path.resolve(os.tmpdir()) && path.basename(temporary).startsWith('gateway-package-'))
    rmSync(temporary, { recursive: true, force: true })
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const options = {}
    const args = process.argv.slice(2)
    for (let i = 0; i < args.length; i++) {
      if (args[i] === '--input-dir' && args[i + 1]) options.input = args[++i]
      else if (args[i] === '--out-dir' && args[i + 1]) options.output = args[++i]
      else if (args[i] === '--tag' && args[i + 1]) options.tag = args[++i]
      else if (args[i] === '--expected-commit' && args[i + 1]) options.expectedCommit = args[++i]
      else if (args[i] === '--require-clean') options.requireClean = true
      else throw new Error(`Unknown or incomplete package option: ${args[i]}`)
    }
    const result = packageRelease(options)
    console.log(`Packaged ${result.name} at ${result.output}. Runtime data and source directories are excluded.`)
  } catch (error) {
    console.error(`Packaging failed: ${error.message}`)
    process.exitCode = 1
  }
}
