<script setup>
import { onBeforeUnmount, onMounted, ref } from 'vue'
import UiToastCard from './UiToastCard.vue'

const messages = ref([])
const duration = 3000
const clock = ref(Date.now())
const kinds = {
  success: { icon: 'check' },
  error: { icon: 'warning' },
  warning: { icon: 'warning' },
  info: { icon: 'info' },
}
let sequence = 0
const timers = new Map()
let progressFrame
function stopProgress() {
  if (progressFrame !== undefined) window.cancelAnimationFrame(progressFrame)
  progressFrame = undefined
}
function updateProgress() {
  clock.value = Date.now()
  for (const item of messages.value) {
    if (clock.value >= item.expiresAt) remove(item.id)
  }
}
function tick() {
  progressFrame = undefined
  updateProgress()
  if (messages.value.length) progressFrame = window.requestAnimationFrame(tick)
}
function remove(id) {
  clearTimeout(timers.get(id))
  timers.delete(id)
  messages.value = messages.value.filter(item => item.id !== id)
  if (!messages.value.length) stopProgress()
}
function clear() {
  for (const timer of timers.values()) clearTimeout(timer)
  timers.clear()
  messages.value = []
  stopProgress()
}
function show(input) {
  const options = typeof input === 'string' ? { message: input } : input || {}
  const message = String(options.message || '').trim()
  if (!message) return
  const type = Object.hasOwn(kinds, options.type) ? options.type : 'success'
  const existing = messages.value.find(item => item.message === message && item.type === type)
  if (existing) return existing.id
  if (messages.value.length === 5) remove(messages.value[0].id)
  clock.value = Date.now()
  const item = { id: ++sequence, message, type, expiresAt: clock.value + duration }
  messages.value.push(item)
  timers.set(item.id, setTimeout(() => remove(item.id), duration))
  if (progressFrame === undefined) tick()
  return item.id
}
onMounted(() => document.addEventListener('visibilitychange', updateProgress))
onBeforeUnmount(() => { clear(); document.removeEventListener('visibilitychange', updateProgress) })
defineExpose({ show, clear })
</script>

<template>
  <Teleport to="body">
    <section class="toast-stack" aria-label="操作消息">
      <UiToastCard v-for="item in messages" :key="item.id" :type="item.type" :message="item.message" :progress="(item.expiresAt - clock) / duration" @dismiss="remove(item.id)" />
    </section>
  </Teleport>
</template>

<style scoped>
.toast-stack{position:fixed;z-index:10000;top:78px;right:28px;display:flex;flex-direction:column;gap:10px;width:min(380px,calc(100vw - 32px));max-height:calc(100dvh - 96px);overflow-y:auto;pointer-events:none}
@media(max-width:760px){.toast-stack{top:70px;right:12px;width:calc(100vw - 24px);max-height:calc(100dvh - 86px)}}
</style>
