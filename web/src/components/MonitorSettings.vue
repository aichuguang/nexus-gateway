<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import type { MonitorSettings } from '../system-types'
import { usePageScope } from '../page-scope'
import { useMonitor } from '../useMonitor'
import { errorDetails, isRequestCancelled } from '../request'
import { statusLabel, latencyLabel, checkTone } from '../monitor-check-presentation'
import Icon from '../Icon.vue'
import MonitorServiceStatus from './MonitorServiceStatus.vue'

const props = defineProps<{ busy?: string }>()
const emitEvent = defineEmits<{ busy: [value: string]; notice: [value: string] }>()
const scope = usePageScope(), api = scope.api, emit = scope.guard(emitEvent)
const monitor = reactive(useMonitor(api))
const loaded = ref(false), loading = ref(false), saving = ref(false), error = ref('')
const savedSettings = ref<MonitorSettings | null>(null)
const filter = ref('all')
const services = computed(() => [...(monitor.status?.services || [])].sort((a, b) => Number(b.status === 'error') - Number(a.status === 'error')))
const issues = computed(() => services.value.filter(service => service.status === 'error'))
const visibleServices = computed(() => filter.value === 'issues' ? issues.value : services.value)
const actionLocked = computed(() => Boolean(props.busy || saving.value || loading.value || monitor.starting))
const locked = computed(() => Boolean(actionLocked.value || monitor.status?.running))
const legacyExternalPending = computed(() => Boolean(savedSettings.value?.enabled && !savedSettings.value.checkExternal))
const summary = computed(() => `每 15分钟 · 上游 Ping / TCP / HTTP · ${legacyExternalPending.value ? '本机（外网未启用）' : '本机/外网'}`)
const tableChecks = ['http', 'local', 'external'] as const

async function load() {
  loading.value = true; loaded.value = false; error.value = ''
  try { savedSettings.value = await api.monitorSettings(); loaded.value = true }
  catch (err) { if (!isRequestCancelled(err)) error.value = errorDetails(err).message }
  finally { loading.value = false }
}
function fixedSettings(enabled: boolean, checkExternal: boolean): MonitorSettings {
  return { enabled, intervalSeconds: 900, checkPing: true, checkTCP: true, checkHTTP: true, checkExternal }
}
async function enable() {
  if (locked.value || !loaded.value) return
  // The explicit enable action opts into the complete check. Merely reading
  // an older upstream-only setting must never broaden its external scope.
  await persist(fixedSettings(true, true))
}
async function pause() {
  if (actionLocked.value || !loaded.value || !savedSettings.value?.enabled) return
  await persist(fixedSettings(false, Boolean(savedSettings.value.checkExternal)))
}
async function check() {
  if (locked.value || !loaded.value) return
  await monitor.check()
}
async function persist(next: MonitorSettings) {
  saving.value = true; emit('busy', 'monitor-settings'); error.value = ''
  try {
    savedSettings.value = await api.saveMonitorSettings(next)
    emit('notice', next.enabled ? '自动连通检测已启用' : '自动连通检测已暂停')
    monitor.changed()
  } catch (err) { if (!isRequestCancelled(err)) error.value = errorDetails(err).message }
  finally { saving.value = false; emit('busy', '') }
}
onMounted(load)
</script>

<template>
  <section class="card monitor-panel">
    <div class="card-head"><div class="card-title"><Icon name="pulse" /><h2>服务连通检测</h2></div><div class="actions"><button class="button primary small" :disabled="locked || !loaded" @click="check">{{ monitor.status?.running || monitor.starting ? '检查中…' : '连通检测' }}</button></div></div>
    <div class="monitor-summary">
      <div><span :class="['pill', savedSettings?.enabled ? 'private' : 'neutral']">{{ savedSettings?.enabled ? '定时监测中' : '定时监测关闭' }}</span><span v-if="loaded" class="small muted">{{ summary }}</span></div>
      <div class="monitor-summary-actions"><button v-if="!savedSettings?.enabled || legacyExternalPending" class="button secondary small" :disabled="locked || !loaded" @click="enable">{{ saving ? '保存中…' : '启用自动连通检测' }}</button><button v-if="savedSettings?.enabled" class="text-button" :disabled="actionLocked || !loaded" @click="pause">暂停监测</button></div>
    </div>
    <div v-if="error || monitor.error || loading || !loaded" class="card-body"><div v-if="error || monitor.error" class="alert error" role="alert">{{ error || monitor.error }}</div><p v-if="loading" class="field-caption">读取中…</p><button v-if="!loaded && !loading" class="button secondary small" @click="load">重新读取</button></div>
    <div class="monitor-toolbar"><div class="actions" role="group" aria-label="筛选监测服务"><button class="monitor-filter" :class="{ selected: filter === 'all' }" :aria-pressed="filter === 'all'" @click="filter = 'all'">全部 {{ services.length }}</button><button class="monitor-filter" :class="{ selected: filter === 'issues' }" :aria-pressed="filter === 'issues'" @click="filter = 'issues'">异常 {{ issues.length }}</button></div><button class="text-button" :disabled="monitor.loading" @click="monitor.refresh"><Icon name="refresh" :size="14" />刷新结果</button></div>
    <div v-if="visibleServices.length" class="table-wrap"><table class="data-table"><thead><tr><th>已发布服务</th><th>上游</th><th>本机入口</th><th>外部访问</th><th class="align-right">整体状态</th></tr></thead><tbody><tr v-for="service in visibleServices" :key="service.serviceId"><td><strong>{{ service.name }}</strong></td><td v-for="kind in tableChecks" :key="kind"><div class="monitor-result"><span :class="['pill', checkTone(service.checks[kind])]">{{ statusLabel(service.checks[kind]) }}</span><span class="mono monitor-latency">{{ latencyLabel(service.checks[kind]) }}</span></div></td><td class="align-right"><MonitorServiceStatus :result="service" :running="monitor.status?.running" /></td></tr></tbody></table></div>
    <div v-else class="compact-empty"><span v-if="monitor.loading" class="spinner"></span><h3>{{ monitor.loading ? '读取中…' : filter === 'issues' && services.length ? '暂无异常' : '暂无记录' }}</h3></div>
  </section>
</template>

<style scoped>
.monitor-panel .data-table{min-width:670px}
.monitor-summary,.monitor-toolbar{display:flex;justify-content:space-between;align-items:center;gap:16px;padding:14px 20px}
.monitor-summary{border-bottom:1px solid var(--line);background:var(--surface-2)}
.monitor-summary>div{display:flex;align-items:center;gap:12px;flex-wrap:wrap;min-width:0}
.monitor-summary-actions{flex-shrink:0}
.monitor-toolbar{padding-top:12px;padding-bottom:12px}
.monitor-filter{border:0;background:transparent;color:var(--muted);padding:6px 9px;border-radius:6px;font:inherit;font-size:12px;cursor:pointer}
.monitor-filter.selected{color:var(--blue);background:var(--blue-bg)}
.monitor-result{display:flex;align-items:center;gap:10px;white-space:nowrap}
.monitor-result .pill{font-weight:550;flex-shrink:0}
.monitor-latency{font-size:12px;color:var(--ink);font-variant-numeric:tabular-nums}
@media(max-width:760px){.monitor-summary{align-items:flex-start;flex-direction:column;gap:12px}.monitor-summary-actions{align-self:flex-end}.monitor-summary,.monitor-toolbar{padding-left:14px;padding-right:14px}.monitor-summary>div{gap:8px}.monitor-result{gap:7px}}
</style>
