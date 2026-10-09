<script setup lang="ts">
import type { MonitorCheck } from '../system-types'
import { checkTone, latencyLabel, statusLabel } from '../monitor-check-presentation'
defineProps<{ check?: MonitorCheck | null; label?: string }>()
</script>

<template>
  <div class="monitor-check-result" :class="{ 'has-label': label }">
    <span v-if="label" class="monitor-check-name">{{ label }}</span>
    <span :class="['pill', 'monitor-check-status', checkTone(check)]" role="status">{{ statusLabel(check) }}</span>
    <span class="monitor-check-latency mono">{{ latencyLabel(check) }}</span>
  </div>
</template>

<style scoped>
.monitor-check-result{display:grid;grid-template-columns:1fr auto;align-items:center;gap:16px;min-width:0;padding:14px 16px;border:1px solid var(--line);border-radius:10px;background:var(--surface-2)}
.monitor-check-result.has-label{grid-template-columns:minmax(0,1fr) auto minmax(70px,auto)}
.monitor-check-name{font-size:13px;font-weight:550;color:var(--ink)}
.monitor-check-status{justify-self:start;min-width:56px;justify-content:center;padding:4px 10px;font-size:11px;font-weight:600;white-space:nowrap}
.monitor-check-status.private{background:var(--green-bg);color:var(--green)}
.monitor-check-status.amber{background:var(--amber-bg);color:var(--amber)}
.monitor-check-status.blue{background:var(--blue-bg);color:var(--blue)}
.monitor-check-latency{justify-self:end;color:var(--ink);font-family:var(--mono);font-size:13px;font-variant-numeric:tabular-nums;white-space:nowrap;text-align:right}
@media(max-width:480px){.monitor-check-result{padding:12px;gap:10px}.monitor-check-result.has-label{grid-template-columns:minmax(0,1fr) auto minmax(56px,auto)}.monitor-check-status{min-width:50px;padding:4px 8px}.monitor-check-name,.monitor-check-latency{font-size:12px}}
</style>
