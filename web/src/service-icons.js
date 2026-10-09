// Stable built-in IDs only: no external image requests or user-provided SVG.
export const serviceIconOptions = [
  { value: 'server', label: '服务器', icon: 'server', tone: 'blue' },
  { value: 'folder', label: '文件', icon: 'folder', tone: 'amber' },
  { value: 'film', label: '影视', icon: 'film', tone: 'purple' },
  { value: 'music', label: '音乐', icon: 'music', tone: 'rose' },
  { value: 'globe', label: '网站', icon: 'globe', tone: 'cyan' },
  { value: 'code', label: '代码', icon: 'code', tone: 'orange' },
  { value: 'terminal', label: '终端', icon: 'terminal', tone: 'slate' },
  { value: 'shield', label: '证书', icon: 'shield', tone: 'green' },
  { value: 'cloud', label: '云', icon: 'cloud', tone: 'blue' },
  { value: 'grid', label: '应用', icon: 'grid', tone: 'purple' },
  { value: 'drive', label: '存储', icon: 'drive', tone: 'teal' },
  { value: 'wrench', label: '工具', icon: 'wrench', tone: 'orange' },
  { value: 'pen', label: '博客', icon: 'pen', tone: 'indigo' },
  { value: 'image', label: '相册', icon: 'image', tone: 'rose' },
  { value: 'download', label: '下载', icon: 'download', tone: 'lime' },
  { value: 'database', label: '数据库', icon: 'database', tone: 'cyan' },
]

export function serviceKind(service = {}) {
  if (!service.domains?.length) return { label: '端口', icon: 'link', tone: 'blue' }
  return service.entryMode === 'edge'
    ? { label: service.edgeProvider === 'cloudflare' ? 'Cloudflare' : 'EdgeOne', icon: 'bolt', tone: 'green' }
    : { label: '直连', icon: 'server', tone: 'blue' }
}

export function serviceAppearance(service = {}) {
  return serviceIconOptions.find(option => option.value === service.icon) || serviceKind(service)
}
