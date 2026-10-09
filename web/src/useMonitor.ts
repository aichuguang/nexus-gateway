import { onBeforeUnmount, onMounted, ref } from 'vue'
import type { GatewayAPI } from './api-types'
import type { MonitorStatus } from './system-types'
import { errorDetails, isRequestCancelled } from './request'

export function useMonitor(api: GatewayAPI) {
  const status = ref<MonitorStatus | null>(null), error = ref(''), loading = ref(false), starting = ref(false)
  let disposed = false, timer: ReturnType<typeof setTimeout> | undefined
  let version = 0, refreshAgain = false
  function stop() { clearTimeout(timer); timer = undefined }
  function schedule() {
    stop()
    if (disposed || starting.value || document.visibilityState === 'hidden') return
    const delay = status.value?.running ? 2000 : status.value?.enabled || error.value ? 15000 : 0
    if (delay) timer = setTimeout(refresh, delay)
  }
  async function refresh() {
    if (disposed || loading.value) return
    const current = version
    stop(); loading.value = true; error.value = ''
    try {
      const result = await api.monitorStatus()
      if (!disposed && current === version) { status.value = result; error.value = result.error || '' }
    }
    catch (err) { if (!disposed && current === version && !isRequestCancelled(err)) error.value = errorDetails(err).message }
    finally {
      loading.value = false
      if (refreshAgain && !disposed) { refreshAgain = false; void refresh() }
      else schedule()
    }
  }
  async function check() {
    if (disposed || starting.value || status.value?.running) return
    starting.value = true; error.value = ''; stop()
    try { const result = await api.checkMonitor(); if (!disposed) { version++; status.value = result; error.value = result.error || '' } }
    catch (err) { if (!disposed && !isRequestCancelled(err)) error.value = errorDetails(err).message }
    finally { starting.value = false; schedule() }
  }
  function changed() { version++; if (loading.value) refreshAgain = true; else void refresh() }
  function visibility() { if (document.visibilityState === 'hidden') stop(); else void refresh() }
  onMounted(() => { document.addEventListener('visibilitychange', visibility); void refresh() })
  onBeforeUnmount(() => { disposed = true; stop(); document.removeEventListener('visibilitychange', visibility) })
  return { status, error, loading, starting, refresh, check, changed }
}
