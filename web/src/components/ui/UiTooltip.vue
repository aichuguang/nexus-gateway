<script setup>
import { nextTick, onBeforeUnmount, onMounted, ref, useId } from 'vue'

const props = defineProps({ text: { type: String, default: '' } })
const anchor = ref(null)
const bubble = ref(null)
const visible = ref(false)
const position = ref({})
const id = `tooltip-${useId()}`
let focused = null
let previousDescription = null

function place() {
  if (!visible.value || !anchor.value) return
  const rect = anchor.value.getBoundingClientRect()
  const width = bubble.value?.getBoundingClientRect().width || 160
  const height = bubble.value?.getBoundingClientRect().height || 32
  const below = rect.top < height + 12
  position.value = {
    left: `${Math.max(8, Math.min(rect.left + rect.width / 2 - width / 2, window.innerWidth - width - 8))}px`,
    top: `${below ? rect.bottom + 7 : rect.top - height - 7}px`,
  }
}
function show() { if (props.text) { visible.value = true; nextTick(place) } }
function hide() { visible.value = false }
function focusIn(event) {
  focused = event.target
  previousDescription = focused.getAttribute?.('aria-describedby') || null
  focused.setAttribute?.('aria-describedby', [previousDescription, id].filter(Boolean).join(' '))
  show()
}
function focusOut() {
  if (previousDescription) focused?.setAttribute?.('aria-describedby', previousDescription)
  else focused?.removeAttribute?.('aria-describedby')
  focused = null
  hide()
}
onMounted(() => { window.addEventListener('resize', hide); window.addEventListener('scroll', hide, true) })
onBeforeUnmount(() => { focusOut(); window.removeEventListener('resize', hide); window.removeEventListener('scroll', hide, true) })
</script>

<template>
  <span ref="anchor" class="ui-tooltip" @mouseenter="show" @mouseleave="hide" @focusin="focusIn" @focusout="focusOut" @keydown.esc="hide">
    <slot />
    <Teleport to="body"><span v-if="visible && text" :id="id" ref="bubble" class="tooltip-bubble" role="tooltip" :style="position">{{ text }}</span></Teleport>
  </span>
</template>

<style scoped>
.ui-tooltip{display:inline-flex;vertical-align:middle;min-width:0}
.tooltip-bubble{position:fixed;z-index:4000;max-width:min(280px,calc(100vw - 16px));padding:7px 10px;border:1px solid color-mix(in srgb,var(--surface) 15%,transparent);border-radius:7px;background:var(--ink);color:var(--surface);box-shadow:var(--shadow);font:inherit;font-size:12px;line-height:1.6;pointer-events:none;white-space:normal;overflow-wrap:anywhere}
</style>
