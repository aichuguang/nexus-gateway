<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, useId, watch } from 'vue'

const props = defineProps({ modelValue: { type: [String, Number, Boolean, Object], default: null }, options: { type: Array, default: () => [] }, id: String, placeholder: { type: String, default: '请选择' }, disabled: Boolean, ariaLabel: String, menuClass: String, menuWidth: Number, columns: { type: Number, default: 1 } })
const emit = defineEmits(['update:modelValue', 'change'])
const uid = useId()
const trigger = ref(null)
const menu = ref(null)
const isOpen = ref(false)
const activeIndex = ref(-1)
const menuStyle = ref({})
const triggerId = computed(() => props.id || `select-${uid}`)
const menuId = `select-menu-${uid}`
const selected = computed(() => props.options.find(option => Object.is(option.value, props.modelValue)))
const enabledIndices = computed(() => props.options.map((option, index) => option.disabled ? -1 : index).filter(index => index >= 0))

function positionMenu() {
  if (!isOpen.value || !trigger.value) return
  const rect = trigger.value.getBoundingClientRect()
  const viewportHeight = window.innerHeight
  const viewportWidth = window.innerWidth
  const below = viewportHeight - rect.bottom - 12
  const above = rect.top - 12
  const desiredHeight = Math.min(260, Math.ceil(props.options.length / props.columns) * (props.columns > 1 ? 72 : 38) + 12)
  const openAbove = below < desiredHeight && above > below
  const height = Math.max(40, Math.min(260, openAbove ? above : below))
  const width = Math.min(props.menuWidth || Math.max(rect.width, 160), viewportWidth - 16)
  menuStyle.value = {
    position: 'fixed', width: `${width}px`, maxHeight: `${height}px`,
    left: `${Math.max(8, Math.min(rect.left, viewportWidth - width - 8))}px`,
    top: openAbove ? 'auto' : `${rect.bottom + 6}px`,
    bottom: openAbove ? `${viewportHeight - rect.top + 6}px` : 'auto',
  }
}
function revealActive() {
  nextTick(() => menu.value?.querySelector(`[data-index="${activeIndex.value}"]`)?.scrollIntoView?.({ block: 'nearest' }))
}
function open(edge) {
  if (props.disabled) return
  isOpen.value = true
  const index = props.options.findIndex(option => Object.is(option.value, props.modelValue) && !option.disabled)
  activeIndex.value = edge === 'first' ? (enabledIndices.value[0] ?? -1) : edge === 'last' ? (enabledIndices.value.at(-1) ?? -1) : index >= 0 ? index : (enabledIndices.value[0] ?? -1)
  positionMenu()
  revealActive()
}
function close() { isOpen.value = false }
function select(index) {
  const option = props.options[index]
  if (props.disabled || !option || option.disabled) return
  if (!Object.is(option.value, props.modelValue)) {
    emit('update:modelValue', option.value)
    emit('change', option.value)
  }
  close()
  trigger.value?.focus()
}
function onKeydown(event) {
  if (props.disabled) return
  if (event.key === 'Tab') { close(); return }
  if (event.key === 'Escape') {
    if (isOpen.value) { event.preventDefault(); event.stopPropagation(); close() }
    return
  }
  const keys = ['ArrowDown', 'ArrowUp', 'Home', 'End', 'Enter', ' ']
  if (props.columns > 1) keys.push('ArrowLeft', 'ArrowRight')
  if (!keys.includes(event.key)) return
  event.preventDefault()
  if (event.key === 'Enter' || event.key === ' ') { if (isOpen.value) select(activeIndex.value); else open(); return }
  if (!isOpen.value) { open(event.key === 'End' ? 'last' : event.key === 'Home' ? 'first' : undefined); return }
  if (event.key === 'Home') activeIndex.value = enabledIndices.value[0] ?? -1
  else if (event.key === 'End') activeIndex.value = enabledIndices.value.at(-1) ?? -1
  else {
    const indices = enabledIndices.value
    const direction = event.key === 'ArrowDown' ? props.columns : event.key === 'ArrowUp' ? -props.columns : event.key === 'ArrowRight' ? 1 : -1
    if (indices.length) activeIndex.value = indices[(indices.indexOf(activeIndex.value) + direction + indices.length) % indices.length]
  }
  revealActive()
}
function onOutside(event) {
  if (!trigger.value?.contains(event.target) && !menu.value?.contains(event.target)) close()
}
watch(() => props.disabled, disabled => { if (disabled) close() })
watch(() => props.options, () => {
  if (!isOpen.value) return
  if (!enabledIndices.value.includes(activeIndex.value)) activeIndex.value = enabledIndices.value[0] ?? -1
  nextTick(positionMenu)
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
  <div class="ui-select">
    <button :id="triggerId" ref="trigger" type="button" class="select-trigger" :class="{ open: isOpen, placeholder: !selected }" role="combobox" aria-haspopup="listbox" :aria-expanded="isOpen" :aria-controls="menuId" :aria-label="ariaLabel" :aria-activedescendant="isOpen && activeIndex >= 0 ? `${menuId}-${activeIndex}` : undefined" :disabled="disabled" @click="isOpen ? close() : open()" @keydown="onKeydown">
      <slot name="trigger" :selected="selected"><span>{{ selected ? selected.label : placeholder }}</span></slot><svg class="select-chevron" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="m4 6 4 4 4-4" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" /></svg>
    </button>
    <Teleport to="body">
      <div v-if="isOpen" :id="menuId" ref="menu" class="ui-select-menu" :class="menuClass" role="listbox" :aria-label="ariaLabel" :aria-labelledby="ariaLabel ? undefined : triggerId" :style="menuStyle">
        <div v-for="(option, index) in options" :id="`${menuId}-${index}`" :key="index" class="select-option" :class="{ active: activeIndex === index, selected: Object.is(option.value, modelValue), disabled: option.disabled }" role="option" :aria-selected="Object.is(option.value, modelValue)" :aria-disabled="Boolean(option.disabled)" :data-index="index" @pointermove="!option.disabled && (activeIndex = index)" @mousedown.prevent @click="select(index)">
          <slot name="option" :option="option"><span>{{ option.label }}</span></slot><svg v-if="Object.is(option.value, modelValue)" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="m3.5 8 3 3 6-6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" /></svg>
        </div>
        <div v-if="!options.length" class="select-empty">暂无可选项</div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.ui-select { min-width: 0; width: 100%; }
.select-trigger { appearance: none; display: flex; align-items: center; justify-content: space-between; gap: 12px; width: 100%; min-height: 38px; padding: 8px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--ink); font: inherit; font-size: 13px; line-height: 1.5; text-align: left; cursor: pointer; }
.select-trigger > span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.select-trigger.placeholder { color: var(--muted); }
.select-trigger:hover:not(:disabled), .select-trigger.open { border-color: var(--blue); }
.select-trigger:focus-visible { outline: 2px solid var(--blue); outline-offset: 2px; }
.select-trigger:disabled { opacity: .5; cursor: not-allowed; }
.select-chevron { flex: 0 0 16px; width: 16px; height: 16px; color: var(--muted); transition: transform .15s; }
.open .select-chevron { transform: rotate(180deg); }
.ui-select-menu { box-sizing: border-box; z-index: 3000; overflow-y: auto; overscroll-behavior: contain; padding: 5px; border: 1px solid var(--line); border-radius: 10px; color: var(--ink); background: var(--surface); box-shadow: var(--shadow); font: inherit; font-size: 13px; }
.select-option { display: flex; align-items: center; justify-content: space-between; gap: 12px; min-height: 36px; padding: 7px 9px; border-radius: 6px; line-height: 1.5; cursor: pointer; }
.select-option > span { overflow-wrap: anywhere; }
.select-option.active:not(.disabled) { background: var(--surface-2); }
.select-option.selected { color: var(--blue); background: var(--blue-bg); font-weight: 600; }
.select-option.disabled { opacity: .45; cursor: not-allowed; }
.select-option svg { flex: 0 0 16px; width: 16px; height: 16px; }
.select-empty { padding: 12px; color: var(--muted); text-align: center; }
@media (prefers-reduced-motion: reduce) { .select-chevron { transition: none; } }
</style>
