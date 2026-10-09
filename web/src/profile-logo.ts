export const profileLogoLimits = {
  bytes: 256 * 1024,
  dimension: 2048,
  accept: 'image/png,image/jpeg,image/gif',
} as const

const copy = {
  type: '仅支持 PNG、JPEG 或 GIF 图片，不支持 SVG 或网络地址。',
  size: 'Logo 文件不能超过 256 KiB。',
  empty: '图片文件为空，请重新选择。',
  read: '无法读取图片，请重新选择文件。',
  image: '图片内容无效或已损坏，请选择有效的 PNG、JPEG 或 GIF 图片。',
  dimensions: '图片尺寸不能超过 2048 × 2048 像素。',
} as const

function imageType(bytes: Uint8Array): string {
  if ([137, 80, 78, 71, 13, 10, 26, 10].every((byte, index) => bytes[index] === byte)) return 'image/png'
  if (bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255) return 'image/jpeg'
  const header = String.fromCharCode(...bytes.subarray(0, 6))
  return header === 'GIF87a' || header === 'GIF89a' ? 'image/gif' : ''
}

function verifyDimensions(source: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const image = new Image()
    const finish = (error?: Error) => {
      image.onload = null; image.onerror = null
      if (error) reject(error)
      else resolve()
    }
    image.onload = () => {
      if (!image.naturalWidth || !image.naturalHeight) finish(new Error(copy.image))
      else if (image.naturalWidth > profileLogoLimits.dimension || image.naturalHeight > profileLogoLimits.dimension) finish(new Error(copy.dimensions))
      else finish()
    }
    image.onerror = () => finish(new Error(copy.image))
    image.src = source
  })
}

export async function readProfileLogo(file: File): Promise<string> {
  if (!file.size) throw new Error(copy.empty)
  if (file.size > profileLogoLimits.bytes) throw new Error(copy.size)
  const declaredType = file.type.toLowerCase()
  if (declaredType && !profileLogoLimits.accept.split(',').includes(declaredType)) throw new Error(copy.type)
  let bytes: Uint8Array
  try { bytes = new Uint8Array(await file.arrayBuffer()) }
  catch { throw new Error(copy.read) }
  if (!bytes.length) throw new Error(copy.empty)
  if (bytes.length > profileLogoLimits.bytes) throw new Error(copy.size)
  const type = imageType(bytes)
  if (!type || (declaredType && declaredType !== type)) throw new Error(copy.image)
  // Chunking avoids exceeding the argument limit on a valid 256 KiB image.
  const chunks: string[] = []
  for (let start = 0; start < bytes.length; start += 8192) chunks.push(String.fromCharCode(...bytes.subarray(start, start + 8192)))
  const source = `data:${type};base64,${btoa(chunks.join(''))}`
  await verifyDimensions(source)
  return source
}
