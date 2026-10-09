<script setup>
import { computed, ref } from 'vue'
import Icon from '../../Icon.vue'

const props = defineProps({ modelValue: { default: null }, options: { type: Array, default: () => [] }, disabled: Boolean, ariaLabel: String, label: String })
const emit = defineEmits(['update:modelValue', 'change'])
const group = ref(null)
const selectedIndex = computed(() => props.options.findIndex(option => Object.is(option.value, props.modelValue) && !option.disabled))
const tabIndex = computed(() => selectedIndex.value >= 0 ? selectedIndex.value : props.options.findIndex(option => !option.disabled))
function select(option) {
  if (props.disabled || option.disabled || Object.is(props.modelValue, option.value)) return
  emit('update:modelValue', option.value)
  emit('change', option.value)
}
function onKeydown(event, index) {
  if (props.disabled) return
  const enabled = props.options.map((option, i) => !option.disabled ? i : -1).filter(i => i >= 0)
  if (!enabled.length) return
  let next
  if (event.key === 'Home') next = enabled[0]
  else if (event.key === 'End') next = enabled.at(-1)
  else if (['ArrowRight', 'ArrowDown', 'ArrowLeft', 'ArrowUp'].includes(event.key)) {
    const direction = ['ArrowRight', 'ArrowDown'].includes(event.key) ? 1 : -1
    next = enabled[(enabled.indexOf(index) + direction + enabled.length) % enabled.length]
  } else return
  event.preventDefault()
  select(props.options[next])
  group.value?.querySelectorAll('button')[next]?.focus()
}
</script>

<template>
  <div ref="group" class="ui-segmented" role="radiogroup" :aria-label="ariaLabel || label" :aria-disabled="disabled">
    <button v-for="(option, index) in options" :key="index" type="button" class="segment" :class="{ selected: Object.is(option.value, modelValue) }" role="radio" :aria-checked="Object.is(option.value, modelValue)" :tabindex="index === tabIndex ? 0 : -1" :disabled="disabled || option.disabled" @click="select(option)" @keydown="onKeydown($event, index)">
      <Icon v-if="option.icon" :name="option.icon" :size="15" /><span>{{ option.label }}</span>
    </button>
  </div>
</template>

<style scoped>
.ui-segmented { display: inline-flex; flex-wrap: wrap; align-items: center; gap: 3px; max-width: 100%; padding: 4px; border: 1px solid var(--line); border-radius: 10px; background: var(--surface-2); }
.segment { appearance: none; display: inline-flex; align-items: center; justify-content: center; gap: 7px; min-height: 32px; padding: 6px 13px; border: 1px solid transparent; border-radius: 7px; color: var(--muted); background: transparent; font: inherit; font-size: 12px; font-weight: 600; line-height: 1.4; cursor: pointer; transition: color .15s, background .15s; }
.segment.selected { border-color: var(--line); background: var(--surface); color: var(--ink); box-shadow: 0 1px 3px #00000008; }
.segment:hover:not(:disabled):not(.selected) { color: var(--blue); background: var(--blue-bg); }
.segment:focus-visible { outline: 2px solid var(--blue); outline-offset: 1px; }
.segment:disabled { opacity: .5; cursor: not-allowed; }
</style>
