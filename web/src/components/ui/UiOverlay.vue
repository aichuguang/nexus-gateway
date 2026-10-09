<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'
import Icon from '../../Icon.vue'
import { acquireOverlay, overlayStack, releaseOverlay } from './overlay-stack'

const props = withDefaults(defineProps<{
  open: boolean; title: string; description?: string; variant?: 'dialog' | 'drawer'
  size?: 'normal' | 'wide'; busy?: boolean; dirty?: boolean
}>(), { variant: 'dialog', size: 'normal', busy: false, dirty: false })
const emit = defineEmits<{ close: [] }>()
const panel = ref<HTMLElement | null>(null)
const keepEditing = ref<HTMLButtonElement | null>(null)
const discarding = ref(false)
const id = Symbol('overlay'), uid = useId()
const titleID = `overlay-title-${uid}`, descriptionID = `overlay-description-${uid}`
const top = computed(() => overlayStack.at(-1) === id)
const layerIndex = computed(() => 2000 + Math.max(0, overlayStack.indexOf(id)) * 2)
let returnFocus: HTMLElement | null = null
let active = false

function focusable() {
  if (!panel.value) return []
  return [...panel.value.querySelectorAll<HTMLElement>('button,input,select,textarea,a[href],[tabindex]')].filter(element => {
    if (element.hasAttribute?.('disabled') || (element as HTMLButtonElement).disabled || String(element.getAttribute('tabindex')) === '-1') return false
    for (let current: HTMLElement | null = element; current && current !== panel.value; current = current.parentNode as HTMLElement | null) {
      if (current.style?.display === 'none' || current.getAttribute?.('hidden') !== null) return false
    }
    return typeof element.getClientRects !== 'function' || element.getClientRects().length > 0
  })
}
function focusFirst() {
  if (!top.value || !props.open) return
  const items = focusable()
  const field = items.find(element => element.getAttribute('autofocus') !== null || ['INPUT', 'SELECT', 'TEXTAREA'].includes(element.tagName))
  ;(discarding.value ? keepEditing.value : field || items[0] || panel.value)?.focus({ preventScroll: true })
}
function requestClose() {
  if (props.busy || !top.value) return
  if (props.dirty && !discarding.value) {
    discarding.value = true
    nextTick(focusFirst)
  } else if (!discarding.value) emit('close')
}
function continueEditing() {
  discarding.value = false
  nextTick(focusFirst)
}
function onKeydown(event: KeyboardEvent) {
  if (!top.value || event.defaultPrevented) return
  if (event.key === 'Escape') {
    event.preventDefault(); event.stopPropagation()
    if (props.busy) return
    if (discarding.value) continueEditing()
    else requestClose()
  } else if (event.key === 'Tab') {
    const items = focusable()
    const first = items[0], last = items.at(-1)
    if (!first) { event.preventDefault(); panel.value?.focus(); return }
    if (event.shiftKey && (document.activeElement === first || !panel.value?.contains(document.activeElement))) {
      event.preventDefault(); last?.focus()
    } else if (!event.shiftKey && (document.activeElement === last || !panel.value?.contains(document.activeElement))) {
      event.preventDefault(); first.focus()
    }
  }
}
function onFocus(event: FocusEvent) {
  if (top.value && props.open && panel.value && !panel.value.contains(event.target as Node)) focusFirst()
}
function activate() {
  if (active) return
  active = true
  returnFocus = document.activeElement as HTMLElement | null
  acquireOverlay(id)
  document.addEventListener('keydown', onKeydown)
  document.addEventListener('focusin', onFocus)
  nextTick(focusFirst)
}
function deactivate() {
  if (!active) return
  active = false
  const wasTop = top.value
  document.removeEventListener('keydown', onKeydown)
  document.removeEventListener('focusin', onFocus)
  releaseOverlay(id)
  discarding.value = false
  if (wasTop && returnFocus && document.contains(returnFocus)) returnFocus.focus({ preventScroll: true })
  returnFocus = null
}
watch(() => props.open, open => { if (open) activate(); else deactivate() }, { immediate: true, flush: 'post' })
onBeforeUnmount(deactivate)
defineExpose({ requestClose })
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="ui-overlay" :class="`ui-overlay--${variant}`" :style="{ zIndex: layerIndex }" :aria-hidden="!top || undefined" :inert="!top || undefined" @click.self="requestClose">
      <section ref="panel" class="ui-overlay-panel" :class="{ 'ui-overlay-panel--wide': size === 'wide' }" :role="discarding ? 'alertdialog' : 'dialog'" aria-modal="true" :aria-labelledby="titleID" :aria-describedby="description || discarding ? descriptionID : undefined" :aria-busy="busy" tabindex="-1">
        <header class="ui-overlay-header">
          <div><span class="ui-overlay-kind">{{ discarding ? '未保存的更改' : variant === 'drawer' ? '详细信息 / 设置' : '当前操作' }}</span><h2 :id="titleID">{{ discarding ? '放弃未保存的内容？' : title }}</h2><p v-if="description && !discarding" :id="descriptionID">{{ description }}</p></div>
          <button type="button" class="icon-button" :aria-label="`关闭${title}`" :disabled="busy" @click="discarding ? continueEditing() : requestClose()"><Icon name="close" :size="19" /></button>
        </header>
        <template v-if="discarding">
          <div class="ui-overlay-body"><p :id="descriptionID">当前输入尚未保存。放弃后将关闭操作面板，已保存的数据不会改变。</p></div>
          <footer class="ui-overlay-footer"><button ref="keepEditing" type="button" class="button secondary" @click="continueEditing">继续编辑</button><button type="button" class="button danger" @click="emit('close')">放弃修改</button></footer>
        </template>
        <div v-show="!discarding" class="ui-overlay-content">
          <div class="ui-overlay-body"><slot :close="requestClose" /></div>
          <footer v-if="$slots.footer" class="ui-overlay-footer"><slot name="footer" :close="requestClose" /></footer>
        </div>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.ui-overlay{position:fixed;inset:0;display:flex;align-items:center;justify-content:center;padding:24px;background:#101a2b70;backdrop-filter:blur(3px);animation:overlay-in .14s ease-out;overscroll-behavior:contain}
.ui-overlay-panel{display:flex;flex-direction:column;min-width:0;width:min(520px,100%);max-height:calc(100dvh - 48px);color:var(--ink);background:var(--surface);border:1px solid var(--line);border-radius:16px;box-shadow:0 28px 90px #09132840;outline:none;animation:panel-in .16s ease-out}
.ui-overlay-panel--wide{width:min(860px,100%)}
.ui-overlay--drawer{justify-content:flex-end;align-items:stretch;padding:0}
.ui-overlay--drawer .ui-overlay-panel{width:min(640px,94vw);max-height:100dvh;border-radius:16px 0 0 16px;border-right:0;animation:drawer-in .18s ease-out}
.ui-overlay--drawer .ui-overlay-panel--wide{width:min(900px,94vw)}
.ui-overlay-header{flex:none;display:flex;justify-content:space-between;align-items:flex-start;gap:20px;padding:22px 24px;border-bottom:1px solid var(--line)}
.ui-overlay-header h2{margin:3px 0 0;font-size:19px;line-height:1.45;overflow-wrap:anywhere}.ui-overlay-kind{font-size:11px;color:var(--muted);letter-spacing:.04em}.ui-overlay-header p{margin:7px 0 0;font-size:12px;color:var(--muted)}
.ui-overlay-body{flex:1;min-height:0;overflow-y:auto;overscroll-behavior:contain;padding:24px;scrollbar-gutter:stable}.ui-overlay-body>p:first-child{margin-top:0}
.ui-overlay-content{display:flex;flex-direction:column;min-height:0;flex:1}
.ui-overlay-footer{flex:none;display:flex;align-items:center;justify-content:flex-end;gap:10px;flex-wrap:wrap;padding:16px 24px;border-top:1px solid var(--line);background:var(--surface-2);border-radius:0 0 16px 16px}
.ui-overlay--drawer .ui-overlay-footer{border-radius:0}
@keyframes overlay-in{from{opacity:0}to{opacity:1}}@keyframes panel-in{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:none}}@keyframes drawer-in{from{transform:translateX(28px);opacity:0}to{transform:none;opacity:1}}
@media(max-width:600px){.ui-overlay{padding:12px}.ui-overlay--drawer{padding:0}.ui-overlay--drawer .ui-overlay-panel,.ui-overlay--drawer .ui-overlay-panel--wide{width:100%;border-radius:0}.ui-overlay-header{padding:18px}.ui-overlay-body{padding:18px}.ui-overlay-footer{padding:14px 18px}.ui-overlay-panel{max-height:calc(100dvh - 24px)}}
@media(prefers-reduced-motion:reduce){.ui-overlay,.ui-overlay-panel,.ui-overlay--drawer .ui-overlay-panel{animation:none}}
</style>
