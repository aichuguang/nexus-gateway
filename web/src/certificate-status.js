import { formatDateTime } from './utils/datetime.ts'

const DAY = 86400000

// Presentation and selection must agree with the server's validity window.
// A missing start date is tolerated for older metadata; an explicit invalid
// date or missing expiry is not evidence that the certificate is usable.
export function certificateStatus(cert, now = Date.now()) {
  if (cert?.error) return { usable: false, label: '不可用', tone: 'amber', note: cert.error }
  if (!cert?.ready) return { usable: false, label: '等待导入', tone: 'amber', note: '还没有导入证书与私钥，应用时会被拒绝。' }

  const expiry = typeof cert.notAfter === 'string' ? Date.parse(cert.notAfter) : NaN
  const start = cert.notBefore ? (typeof cert.notBefore === 'string' ? Date.parse(cert.notBefore) : NaN) : null
  if (!Number.isFinite(expiry) || (start !== null && (!Number.isFinite(start) || start >= expiry))) {
    return { usable: false, label: '有效期待核对', tone: 'neutral', note: '证书有效期信息不完整或无效，请重新读取或导入材料。' }
  }
  if (now >= expiry) return { usable: false, label: '已过期', tone: 'amber', note: '证书已过期，按域名发布的服务无法握手。' }
  if (start !== null && now < start) {
    return { usable: false, label: '尚未生效', tone: 'amber', note: `将在 ${formatDateTime(cert.notBefore)} 生效，当前不能用于发布。` }
  }

  const days = Math.ceil((expiry - now) / DAY)
  if (expiry - now <= 14 * DAY) return { usable: true, label: '即将到期', tone: 'amber', note: `还有 ${days} 天到期；请通过 certd 续期部署，或手动更新证书。` }
  return { usable: true, label: '可用', tone: 'private', note: `还有 ${days} 天到期。` }
}

export function isCertificateUsable(cert) {
  return certificateStatus(cert).usable
}
