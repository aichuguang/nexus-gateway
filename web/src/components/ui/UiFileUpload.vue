<script setup>
import { computed, ref, useId, watch } from 'vue'
import Icon from '../../Icon.vue'

const props = defineProps({ modelValue: { default: null }, id: String, accept: { type: String, default: '' }, disabled: Boolean, label: { type: String, default: '选择文件' } })
const emit = defineEmits(['update:modelValue', 'change'])
const uid = useId()
const input = ref(null)
const dragging = ref(false)
const error = ref('')
let dragDepth = 0
const inputId = computed(() => props.id || `upload-${uid}`)
const errorId = `upload-error-${uid}`
const sizeLabel = computed(() => {
  const size = props.modelValue?.size || 0
  return size < 1024 ? `${size} B` : size < 1024 * 1024 ? `${(size / 1024).toFixed(1)} KB` : `${(size / (1024 * 1024)).toFixed(1)} MB`
})
function accepts(file) {
  const rules = props.accept.split(',').map(rule => rule.trim().toLowerCase()).filter(Boolean)
  if (!rules.length) return true
  const name = file.name.toLowerCase()
  const type = (file.type || '').toLowerCase()
  return rules.some(rule => rule === '*/*' || (rule.startsWith('.') ? name.endsWith(rule) : rule.endsWith('/*') ? type.startsWith(rule.slice(0, -1)) : type === rule))
}
function update(file) {
  if (props.disabled || !file) return
  if (!accepts(file)) { error.value = `文件类型不支持，请选择 ${props.accept} 文件。`; return }
  error.value = ''
  emit('update:modelValue', file)
  emit('change', file)
}
function choose() { if (!props.disabled) input.value?.click() }
function onInput(event) {
  update(event.target.files?.[0])
  event.target.value = ''
}
function clear() {
  if (props.disabled) return
  error.value = ''
  if (input.value) input.value.value = ''
  emit('update:modelValue', null)
  emit('change', null)
}
function onDragEnter() {
  if (props.disabled) return
  dragDepth += 1
  dragging.value = true
}
function onDragLeave() {
  dragDepth = Math.max(0, dragDepth - 1)
  if (!dragDepth) dragging.value = false
}
function onDrop(event) {
  dragDepth = 0
  dragging.value = false
  if (!props.disabled) update(event.dataTransfer?.files?.[0])
}
watch(() => props.disabled, disabled => { if (disabled) { dragging.value = false; dragDepth = 0 } })
watch(() => props.modelValue, () => { error.value = '' })
</script>

<template>
  <div class="ui-file-upload" :class="{ dragging, disabled, 'has-file': modelValue, invalid: error }" @dragenter.prevent="onDragEnter" @dragover.prevent @dragleave.prevent="onDragLeave" @drop.prevent="onDrop">
    <input :id="inputId" ref="input" class="file-input" type="file" :accept="accept" :disabled="disabled" tabindex="-1" aria-hidden="true" @change="onInput" />
    <button type="button" class="upload-card" :disabled="disabled" :aria-label="`${label}：${modelValue ? '更换文件' : '选择文件'}`" :aria-describedby="error ? errorId : undefined" @click="choose">
      <span class="upload-icon"><Icon :name="modelValue ? 'file' : 'upload'" :size="21" /></span>
      <span class="upload-content"><strong>{{ label }}</strong><span v-if="modelValue" class="file-name">{{ modelValue.name }}</span><span v-else class="upload-help">点击选择或拖入文件</span><span v-if="modelValue" class="upload-help">{{ sizeLabel }} · 点击更换</span></span>
      <Icon v-if="modelValue" name="check" :size="16" class="upload-check" />
    </button>
    <button v-if="modelValue" type="button" class="upload-clear" :disabled="disabled" :aria-label="`清除${label}`" @click="clear"><Icon name="close" :size="14" />清除</button>
    <p v-if="error" :id="errorId" class="upload-error" role="alert">{{ error }}</p>
  </div>
</template>

<style scoped>
.ui-file-upload { position: relative; min-width: 0; padding: 0; border: 1px dashed var(--line); border-radius: 10px; background: var(--surface); transition: border-color .15s, background .15s; }
.file-input { display: none !important; }
.upload-card { appearance: none; display: flex; align-items: center; gap: 13px; width: 100%; min-height: 112px; padding: 19px 17px; border: 0; border-radius: 10px; background: transparent; color: var(--ink); font: inherit; text-align: left; cursor: pointer; }
.upload-icon { display: flex; flex: 0 0 40px; align-items: center; justify-content: center; width: 40px; height: 40px; border: 1px solid var(--line); border-radius: 10px; background: var(--surface-2); color: var(--muted); }
.upload-content { display: flex; flex: 1; flex-direction: column; gap: 6px; min-width: 0; }
.upload-content strong { font-size: 12px; font-weight: 600; overflow-wrap: anywhere; }
.upload-help { font-size:12px; line-height: 1.5; color: var(--muted); }
.file-name { color: var(--ink); font-size: 12px; overflow-wrap: anywhere; }
.upload-check { flex-shrink: 0; color: var(--green); }
.ui-file-upload:hover:not(.disabled), .ui-file-upload.dragging { border-color: var(--blue); background: var(--blue-bg); }
.ui-file-upload:focus-within { outline: 2px solid var(--blue); outline-offset: 2px; }
.upload-card:focus-visible, .upload-clear:focus-visible { outline: 2px solid var(--blue); outline-offset: -3px; }
.upload-clear { appearance: none; display: flex; align-items: center; gap: 4px; margin: -3px 17px 12px auto; padding: 3px 5px; border: 0; border-radius: 4px; background: transparent; color: var(--muted); font: inherit; font-size:12px; cursor: pointer; }
.upload-clear:hover:not(:disabled) { color: var(--danger); }
.ui-file-upload.disabled { opacity: .5; }
.upload-card:disabled, .upload-clear:disabled { cursor: not-allowed; }
.ui-file-upload.invalid { border-color: var(--danger); }
.upload-error { margin: 0; padding: 0 17px 12px; color: var(--danger); font-size: 12px; line-height: 1.5; overflow-wrap: anywhere; }
</style>
