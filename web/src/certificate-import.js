const MAX_MATERIAL_BYTES = 2 * 1024 * 1024
const DEFAULT_GATEWAY_BINARY = '/opt/gateway/gateway'
const DEFAULT_GATEWAY_DATA_DIR = '/var/lib/gateway'

export function buildCertificateForm(mode, certificateText, privateKeyText, certificateFile, keyFile) {
  let certificate
  let privateKey
  if (mode === 'paste') {
    const certificateValue = String(certificateText ?? '').trim()
    const privateKeyValue = String(privateKeyText ?? '').trim()
    if (!certificateValue || !privateKeyValue) throw new Error('请完整填写证书链和私钥。')
    certificate = new Blob([certificateValue], { type: 'application/x-pem-file' })
    privateKey = new Blob([privateKeyValue], { type: 'application/x-pem-file' })
  } else if (mode === 'file') {
    certificate = certificateFile
    privateKey = keyFile
    if (!(certificate instanceof Blob) || !(privateKey instanceof Blob) || !certificate.size || !privateKey.size) {
      throw new Error('请选择非空的证书链文件和私钥文件。')
    }
  } else {
    throw new Error('请选择粘贴内容或上传文件。')
  }
  if (certificate.size > MAX_MATERIAL_BYTES || privateKey.size > MAX_MATERIAL_BYTES) {
    throw new Error('证书链和私钥每项不能超过 2 MiB。')
  }
  const form = new FormData()
  form.append('certificate', certificate, 'fullchain.pem')
  form.append('privateKey', privateKey, 'privkey.pem')
  return form
}

function shellArgument(value) {
  return `'${String(value).replace(/'/g, `'"'"'`)}'`
}

export function certificateInboxPath(cert, runtime) {
  if (!cert?.id) return ''
  if (runtime?.dataDir) return `${String(runtime.dataDir).replace(/\/+$/, '')}/cert-inbox/${cert.id}`
  return String(cert.inboxPath || '').replace(/\/+$/, '')
}

export function certificateDeployCommand(cert, runtime) {
  if (!cert?.id) return ''
  const inboxPath = certificateInboxPath(cert, runtime)
  const suffix = `/cert-inbox/${cert.id}`
  const dataDir = inboxPath.endsWith(suffix) ? inboxPath.slice(0, -suffix.length) || '/' : DEFAULT_GATEWAY_DATA_DIR
  return `${shellArgument(runtime?.binaryPath || DEFAULT_GATEWAY_BINARY)} certificate-apply ${shellArgument(cert.id)} --data-dir ${shellArgument(dataDir)}`
}
