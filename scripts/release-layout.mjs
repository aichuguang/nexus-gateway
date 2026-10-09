// Keep build output and public archives on the same explicit allowlist.
export const RELEASE_TARGET = 'linux/arm64'
export const RELEASE_BINARY = 'gateway-linux-arm64'
export const DEPLOYMENT_ASSETS = Object.freeze([
  'config.json', 'install.sh', 'replace.sh', 'verify-release.py',
  'gateway.env', 'gateway.service', 'gateway-proxy.service', 'gateway.logrotate', 'gateway-traffic.logrotate',
  // Existing installations retain their legacy paths and unit names.
  'w103d-gateway.env', 'w103d-gateway.service', 'w103d-gateway-proxy.service', 'w103d-gateway.logrotate', 'w103d-gateway-traffic.logrotate',
])
export const LICENSE_ASSETS = Object.freeze([
  { path: 'LICENSE', source: 'LICENSE' },
  { path: 'THIRD_PARTY_NOTICES.md', source: 'THIRD_PARTY_NOTICES.md' },
  { path: 'Inter-OFL.txt', source: 'web/public/licenses/inter-OFL.txt' },
  { path: 'JetBrainsMono-OFL.txt', source: 'web/public/licenses/jetbrains-mono-OFL.txt' },
])
export const RELEASE_FILES = Object.freeze([
  RELEASE_BINARY, 'build-manifest.json', 'SHA256SUMS', ...DEPLOYMENT_ASSETS, ...LICENSE_ASSETS.map(asset => asset.path),
])
