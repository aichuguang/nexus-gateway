<script setup>
import { computed } from 'vue'
import ServiceAvatar from './ServiceAvatar.vue'
import UiSelect from './ui/UiSelect.vue'
import { serviceIconOptions } from '../service-icons.js'

const props = defineProps({ modelValue: { type: String, default: '' }, service: { type: Object, default: () => ({}) }, disabled: Boolean })
const emit = defineEmits(['update:modelValue'])
const options = [{ value: '', label: '自动' }, ...serviceIconOptions]
const selected = computed(() => options.find(option => option.value === props.modelValue) || options[0])
</script>

<template>
  <UiSelect class="service-icon-picker" :model-value="modelValue" :options="options" :disabled="disabled" aria-label="服务图标" menu-class="service-icon-menu" :menu-width="272" :columns="4" @update:model-value="emit('update:modelValue', $event)">
    <template #trigger><ServiceAvatar :service="{ ...service, icon: selected.value }" /></template>
    <template #option="{ option }"><span class="icon-choice"><ServiceAvatar :service="{ ...service, icon: option.value }" /><span>{{ option.label }}</span></span></template>
  </UiSelect>
</template>

<style scoped>
.service-icon-picker{flex:0 0 58px;width:58px}
.service-icon-picker :deep(.select-trigger){height:40px;min-height:40px;gap:4px;padding:4px;border-radius:9px}
.service-icon-picker :deep(.select-chevron){width:12px;height:12px;flex-basis:12px}
.service-icon-picker :deep(.service-avatar){width:30px;height:30px;border-radius:7px}
.icon-choice{display:flex;flex-direction:column;align-items:center;gap:5px;width:100%;font-size:12px;font-weight:500;color:var(--muted)}
.icon-choice :deep(.service-avatar){width:34px;height:34px;border-radius:9px}
:global(.service-icon-menu){display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:4px;padding:7px}
:global(.service-icon-menu .select-option){position:relative;justify-content:center;min-height:68px;padding:5px 3px;border:1px solid transparent;border-radius:9px}
:global(.service-icon-menu .select-option.active){border-color:var(--line)}
:global(.service-icon-menu .select-option.selected){border-color:color-mix(in srgb,var(--blue) 30%,var(--line))}
:global(.service-icon-menu .select-option>svg){position:absolute;right:3px;top:3px;width:12px;height:12px}
</style>
