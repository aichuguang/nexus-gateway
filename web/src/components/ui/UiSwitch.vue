<script setup>
const props = defineProps({ modelValue: Boolean, disabled: Boolean, id: String })
const emit = defineEmits(['update:modelValue', 'change'])
function toggle() {
  if (props.disabled) return
  const value = !props.modelValue
  emit('update:modelValue', value)
  emit('change', value)
}
</script>

<template>
  <button :id="id" type="button" class="ui-switch" :class="{ checked: modelValue }" role="switch" :aria-checked="modelValue" :disabled="disabled" @click="toggle">
    <span class="switch-track" aria-hidden="true"><span class="switch-thumb" /></span>
    <span v-if="$slots.default" class="switch-label"><slot /></span>
  </button>
</template>

<style scoped>
.ui-switch { appearance: none; display: inline-flex; align-items: center; gap: 9px; padding: 0; border: 0; background: transparent; color: var(--ink); font: inherit; line-height: 1.5; text-align: left; cursor: pointer; }
.switch-track { box-sizing: border-box; position: relative; display: block; flex: 0 0 36px; width: 36px; height: 21px; padding: 2px; border: 1px solid var(--line); border-radius: 20px; background: var(--surface-2); transition: background .18s, border-color .18s; }
.switch-thumb { display: block; width: 15px; height: 15px; border-radius: 50%; background: var(--muted); transition: transform .18s, background .18s; }
.checked .switch-track { background: var(--blue); border-color: var(--blue); }
.checked .switch-thumb { background: var(--surface); transform: translateX(15px); }
.ui-switch:hover:not(:disabled) .switch-track { border-color: var(--blue); }
.ui-switch:focus-visible { outline: 2px solid var(--blue); outline-offset: 4px; border-radius: 4px; }
.ui-switch:disabled { opacity: .5; cursor: not-allowed; }
@media (prefers-reduced-motion: reduce) { .switch-track, .switch-thumb { transition: none; } }
</style>
