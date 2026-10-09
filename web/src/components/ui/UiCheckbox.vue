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
  <button :id="id" type="button" class="ui-checkbox" :class="{ checked: modelValue }" role="checkbox" :aria-checked="modelValue" :disabled="disabled" @click="toggle">
    <span class="checkbox-mark" aria-hidden="true"><svg v-if="modelValue" viewBox="0 0 16 16" fill="none"><path d="m3.5 8 3 3 6-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" /></svg></span>
    <span v-if="$slots.default" class="checkbox-label"><slot /></span>
  </button>
</template>

<style scoped>
.ui-checkbox { appearance: none; display: inline-flex; align-items: flex-start; gap: 9px; padding: 0; border: 0; background: transparent; color: var(--ink); font: inherit; line-height: 1.5; text-align: left; cursor: pointer; }
.checkbox-mark { display: inline-flex; flex: 0 0 18px; width: 18px; height: 18px; margin-top: 2px; border: 1.5px solid var(--line); border-radius: 5px; background: var(--surface); transition: border-color .15s, background .15s; }
.checkbox-mark svg { width: 100%; height: 100%; }
.checked .checkbox-mark { background: var(--blue); border-color: var(--blue); color: var(--surface); }
.ui-checkbox:hover:not(:disabled) .checkbox-mark { border-color: var(--blue); }
.ui-checkbox:focus-visible { outline: 2px solid var(--blue); outline-offset: 4px; border-radius: 4px; }
.ui-checkbox:disabled { opacity: .5; cursor: not-allowed; }
.checkbox-label { min-width: 0; }
</style>
