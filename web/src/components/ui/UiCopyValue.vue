<script setup>
import { computed, onBeforeUnmount, ref } from 'vue'
import Icon from '../../Icon.vue'
import UiTooltip from './UiTooltip.vue'

const props = defineProps({ value: { type: [String, Number], default: '' }, displayValue: String, label: { type: String, default: '内容' }, address: Boolean, truncate: Boolean, persistentCopy: Boolean })
const text = computed(() => String(props.value ?? ''))
// Split only the display. Keep the exact original URL for copying and tooltips,
// including IPv6 brackets, explicit ports, paths, queries and fragments.
const protocol = computed(() => props.address ? text.value.match(/^(https?):\/\/(?=\S)/i)?.[1].toUpperCase() || '' : '')
const displayText = computed(() => props.displayValue ?? (protocol.value ? text.value.slice(text.value.indexOf('://') + 3) : text.value))
const copyLabel = computed(() => props.label.startsWith('复制') ? props.label : `复制${props.label}`)
const tooltipLabel = computed(() => text.value ? `${copyLabel.value}：${text.value}` : copyLabel.value)
const status = ref('')
const copying = ref(false)
let timer

// The production management listener is HTTP. Clipboard API may be unavailable
// there; keep a user-initiated selection fallback, never claim a failed copy.
function copyBySelection(value) {
  const active = document.activeElement
  const selection = window.getSelection?.()
  const ranges = selection ? Array.from({ length: selection.rangeCount }, (_, i) => selection.getRangeAt(i).cloneRange()) : []
  const input = document.createElement('textarea')
  input.value = value
  input.setAttribute('readonly', '')
  input.setAttribute('aria-hidden', 'true')
  Object.assign(input.style, { position: 'fixed', left: '-10000px', top: '0', opacity: '0' })
  document.body.appendChild(input)
  try {
    input.select()
    if (!document.execCommand?.('copy')) throw new Error('clipboard unavailable')
  } finally {
    input.remove()
    active?.focus?.({ preventScroll: true })
    if (selection) { selection.removeAllRanges(); ranges.forEach(range => selection.addRange(range)) }
  }
}
async function copy() {
  if (!text.value || copying.value) return
  clearTimeout(timer)
  copying.value = true
  try {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable')
      await navigator.clipboard.writeText(text.value)
    } catch { copyBySelection(text.value) }
    status.value = '已复制'
  } catch { status.value = '复制失败，请选中文本手动复制' }
  finally { copying.value = false; timer = setTimeout(() => { status.value = '' }, 3000) }
}
onBeforeUnmount(() => clearTimeout(timer))
</script>

<template>
  <span class="copy-value" :class="{ copied: status === '已复制', 'address-value': Boolean(protocol), 'copy-truncate': truncate, 'copy-persistent': persistentCopy }">
    <span v-if="protocol" class="address-protocol"><Icon :name="protocol === 'HTTPS' ? 'lock' : 'link'" :size="11" />{{ protocol }}</span>
    <code>{{ displayText || '—' }}</code>
    <UiTooltip :text="status || tooltipLabel"><button type="button" class="copy-button" :aria-label="copyLabel" :aria-busy="copying" :disabled="!text" @click="copy"><Icon :name="status === '已复制' ? 'check' : 'copy'" :size="13" /></button></UiTooltip>
    <span class="sr-only" role="status" aria-live="polite">{{ status }}</span>
  </span>
</template>

<style scoped>
.copy-value{display:inline-flex;align-items:flex-start;gap:3px;max-width:100%;padding:2px 3px 2px 8px;border:1px solid color-mix(in srgb,var(--line) 60%,transparent);border-radius:6px;background:var(--data-bg);vertical-align:middle;box-shadow:inset 0 1px var(--control-highlight);color:var(--data-ink)}
.copy-value code{font-family:var(--mono);font-size:12px;font-weight:450;font-variant-numeric:tabular-nums;line-height:22px;letter-spacing:-.15px;overflow-wrap:anywhere;word-break:normal;user-select:text;min-width:0}
.address-value{gap:6px;padding-left:3px;border-color:color-mix(in srgb,var(--blue) 15%,var(--line));background:color-mix(in srgb,var(--data-bg) 72%,var(--surface))}
.address-protocol{display:inline-flex;align-items:center;align-self:center;gap:4px;flex-shrink:0;color:var(--muted);font-family:var(--mono);font-size:12px;font-weight:500;line-height:1.5}.address-protocol::after{content:"·";margin:0 3px}
.copy-button{display:grid;place-items:center;appearance:none;width:23px;height:22px;padding:0;border:0;border-radius:4px;background:transparent;color:var(--muted);opacity:0;cursor:pointer;transition:opacity .12s,background .12s}
.copy-value:hover .copy-button,.copy-value:focus-within .copy-button{opacity:1}
.copy-button:hover:not(:disabled){background:color-mix(in srgb,var(--blue) 9%,var(--surface));color:var(--blue)}
.copy-button:focus-visible{opacity:1;outline:2px solid var(--blue);outline-offset:1px}
.copy-button:disabled{cursor:not-allowed;opacity:.4}.copied .copy-button{opacity:1;color:var(--green)}
.copy-truncate{display:flex;align-items:center;width:100%;min-width:0;box-sizing:border-box}
.copy-truncate code{flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;overflow-wrap:normal}
.copy-truncate>.ui-tooltip{flex-shrink:0}.copy-persistent .copy-button{opacity:1}
.sr-only{position:absolute;width:1px;height:1px;margin:-1px;padding:0;border:0;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap}
@media(hover:none){.copy-button{opacity:1}}
@media(prefers-reduced-motion:reduce){.copy-button{transition:none}}
</style>
