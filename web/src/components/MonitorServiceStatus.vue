<script setup lang="ts">
import { computed, ref } from 'vue'
import UiOverlay from './ui/UiOverlay.vue'
import MonitorCheckDetails from './MonitorCheckDetails.vue'
import type { MonitorCheckStatus, MonitorService } from '../system-types'
import { statusLabel } from '../monitor-check-presentation'
const props = defineProps<{ result?: MonitorService | null; running?: boolean; pending?: boolean; disabled?: boolean }>()
const checks = ['ping', 'tcp', 'http', 'local', 'external'] as const
const titles = { ping: '上游 Ping', tcp: '上游 TCP', http: '上游 HTTP', local: '本机入口', external: '外部访问' }
const open = ref(false)
const status = computed(() => props.running && !props.result ? 'checking' : props.result?.status || 'unknown')
function label(value: MonitorCheckStatus) { return statusLabel({ status: value }) }
</script>

<template>
  <div class="monitor-service"><button type="button" class="monitor-trigger" :aria-label="`整体连通性：${label(status)}，查看检查明细`" aria-haspopup="dialog" :aria-expanded="open" @click="open = true"><span class="monitor-dot" :class="status"></span><span>{{ disabled && !result ? '未参与监测' : label(status) }}<template v-if="pending && result">（已应用配置）</template></span><span class="monitor-chevron" aria-hidden="true">›</span></button></div>
  <UiOverlay :open="open" :title="result?.name ? `整体连通性 · ${result.name}` : '整体连通性'" @close="open = false">
    <div class="monitor-details"><MonitorCheckDetails v-for="kind in checks" :key="kind" :label="titles[kind]" :check="result?.checks?.[kind]" class="monitor-check" /></div>
    <template #footer="{ close }"><button class="button secondary" @click="close">关闭详情</button></template>
  </UiOverlay>
</template>

<style scoped>
.monitor-service{font-size:12px;margin-top:5px}.monitor-trigger{display:flex;align-items:center;gap:6px;cursor:pointer;width:fit-content;color:var(--muted);border:0;background:none;padding:2px 0;font:inherit;text-align:left}.monitor-trigger:hover{color:var(--ink)}.monitor-dot{width:7px;height:7px;border-radius:50%;background:var(--muted);flex-shrink:0}.monitor-dot.ok{background:var(--green)}.monitor-dot.error{background:var(--amber)}.monitor-dot.checking,.monitor-dot.recovering,.monitor-dot.unknown{background:var(--blue)}.monitor-details{display:grid;gap:10px}.monitor-chevron{font-size:16px;line-height:1}
</style>
