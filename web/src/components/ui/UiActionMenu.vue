<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, useId, watch } from 'vue'
import Icon from '../../Icon.vue'

const props = defineProps({ items: { type: Array, default: () => [] }, label: { type: String, default: '更多操作' }, disabled: Boolean })
const emit = defineEmits(['select'])
const uid = useId()
const trigger = ref(null)
const menu = ref(null)
const isOpen = ref(false)
const menuStyle = ref({})
const enabledIndices = computed(() => props.items.flatMap((item, index) => item.disabled ? [] : [index]))
const menuId = `action-menu-${uid}`

function positionMenu() {
  if (!isOpen.value || !trigger.value) return
  const rect = trigger.value.getBoundingClientRect()
  const width = Math.min(208, window.innerWidth - 16)
  const below = window.innerHeight - rect.bottom - 14
  const above = rect.top - 14
  const desiredHeight = props.items.length * 40 + 24
  const openAbove = below < desiredHeight && above > below
  menuStyle.value = {
    width: `${width}px`, left: `${Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8))}px`,
    top: openAbove ? 'auto' : `${rect.bottom + 6}px`,
    bottom: openAbove ? `${window.innerHeight - rect.top + 6}px` : 'auto',
    maxHeight: `${Math.max(40, openAbove ? above : below)}px`,
  }
}
function focusItem(index) {
  const item = menu.value?.querySelector(`[data-index="${index}"]`)
  item?.focus({ preventScroll: true })
  item?.scrollIntoView?.({ block: 'nearest' })
  if (!item) menu.value?.focus({ preventScroll: true })
}
async function open(last = false) {
  if (props.disabled) return
  isOpen.value = true
  positionMenu()
  await nextTick()
  if (isOpen.value) focusItem(last ? enabledIndices.value.at(-1) : enabledIndices.value[0])
}
function close(restoreFocus = false) {
  isOpen.value = false
  if (restoreFocus) trigger.value?.focus({ preventScroll: true })
}
function select(index) {
  const item = props.items[index]
  if (!isOpen.value || props.disabled || !item || item.disabled) return
  // Restore before emitting so a newly opened dialog can take focus afterwards.
  close(true)
  emit('select', item.id)
}
function onTriggerKey(event) {
  if (!['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(event.key)) return
  event.preventDefault()
  open(event.key === 'ArrowUp')
}
function onMenuKey(event) {
  if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); close(true); return }
  // Move back to the trigger before the browser performs normal Tab navigation;
  // otherwise the teleported menu would jump to the end of the document.
  if (event.key === 'Tab') { close(true); return }
  if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const indices = enabledIndices.value
  const index = Number(document.activeElement?.getAttribute('data-index'))
  const direction = event.key === 'ArrowDown' ? 1 : -1
  const next = event.key === 'Home' ? indices[0] : event.key === 'End' ? indices.at(-1)
    : indices[(indices.indexOf(index) + direction + indices.length) % indices.length]
  focusItem(next)
}
function onOutside(event) {
  if (!trigger.value?.contains(event.target) && !menu.value?.contains(event.target)) close()
}
watch(() => props.disabled, disabled => { if (disabled && isOpen.value) close(menu.value?.contains(document.activeElement)) })
watch(() => props.items, () => {
  if (!isOpen.value) return
  nextTick(() => {
    positionMenu()
    const index = Number(document.activeElement?.getAttribute('data-index') ?? -1)
    if (!enabledIndices.value.includes(index)) focusItem(enabledIndices.value[0])
  })
}, { deep: true })
onMounted(() => {
  document.addEventListener('pointerdown', onOutside, true)
  document.addEventListener('focusin', onOutside, true)
  window.addEventListener('resize', positionMenu)
  window.addEventListener('scroll', positionMenu, true)
})
onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onOutside, true)
  document.removeEventListener('focusin', onOutside, true)
  window.removeEventListener('resize', positionMenu)
  window.removeEventListener('scroll', positionMenu, true)
})
</script>

<template>
  <button :id="`${menuId}-trigger`" ref="trigger" type="button" class="icon-button action-menu-trigger" :class="{ open: isOpen }" :aria-label="label" aria-haspopup="menu" :aria-expanded="isOpen" :aria-controls="isOpen ? menuId : undefined" :disabled="disabled" @click="isOpen ? close() : open()" @keydown="onTriggerKey"><Icon name="more" :size="18" /></button>
  <Teleport to="body">
    <div v-if="isOpen" :id="menuId" ref="menu" class="action-menu" role="menu" :aria-labelledby="`${menuId}-trigger`" tabindex="-1" :style="menuStyle" @keydown="onMenuKey">
      <template v-for="(item, index) in items" :key="item.id">
        <div v-if="item.separator" class="action-menu-divider" role="separator"></div>
        <button type="button" role="menuitem" tabindex="-1" :data-index="index" class="action-menu-item" :class="{ danger: item.danger }" :disabled="item.disabled" :aria-disabled="Boolean(item.disabled)" @click="select(index)"><Icon :name="item.icon" :size="16" /><span>{{ item.label }}</span></button>
      </template>
    </div>
  </Teleport>
</template>

<style scoped>
.action-menu-trigger.open{background:var(--surface-2);color:var(--ink)}
.action-menu-trigger>svg{stroke-width:3}
.action-menu{position:fixed;z-index:3000;overflow-y:auto;overscroll-behavior:contain;padding:5px;border:1px solid var(--line);border-radius:12px;background:var(--surface);color:var(--ink);box-shadow:var(--shadow),inset 0 1px var(--control-highlight)}
.action-menu-item{display:flex;align-items:center;gap:10px;width:100%;min-height:38px;padding:9px 11px;border:0;border-radius:7px;background:transparent;color:var(--ink);font-size:12px;text-align:left;line-height:1.5}
.action-menu-item>svg{color:var(--muted)}
.action-menu-item:hover:not(:disabled),.action-menu-item:focus-visible{background:var(--surface-2);outline:2px solid transparent;outline-offset:-2px}
.action-menu-item:focus-visible{outline-color:var(--blue)}
.action-menu-item.danger,.action-menu-item.danger>svg{color:var(--danger)}
.action-menu-item.danger:hover:not(:disabled),.action-menu-item.danger:focus-visible{background:var(--danger-bg)}
.action-menu-divider{height:1px;margin:5px 7px;background:var(--line)}
@media(pointer:coarse){.action-menu-item{min-height:44px}}
</style>
