<script setup>
import { computed } from 'vue'
import Icon from '../Icon.vue'
import RoutePath from './RoutePath.vue'

const props = defineProps({
  services: { type: Array, default: () => [] }, gatewayIp: String,
  unappliedServiceIds: { type: Array, default: () => [] }, appliedRevision: { type: Number, default: 0 },
  appliedAt: String, unappliedElsewhere: Boolean,
})
const pendingIds = computed(() => new Set(props.unappliedServiceIds))
function stage(service) {
  if (!props.appliedRevision || pendingIds.value.has(service.id)) return 'draft'
  return service.enabled ? 'applied' : 'disabled'
}
function lane(services, id) {
  const members = services.filter(service => stage(service) === id)
  if (!members.length) return null
  return { id, count: members.length, disabled: members.filter(service => !service.enabled).length }
}
const routes = computed(() => [
  { id: 'direct', label: '本机直连', icon: 'globe', services: props.services.filter(service => !service.domains?.length || service.entryMode !== 'edge') },
  { id: 'edge', label: '边缘加速', icon: 'cloud', services: props.services.filter(service => service.domains?.length && service.entryMode === 'edge') },
].filter(route => route.services.length).map(route => ({ ...route,
  disabled: route.services.filter(service => !service.enabled).length,
  lanes: ['applied', 'draft', 'disabled'].map(id => lane(route.services, id)).filter(Boolean),
})))
</script>

<template>
  <section class="panel publish-topology">
    <div class="panel-header"><h2>发布路径</h2></div>
    <div v-if="!routes.length" class="compact-empty"><Icon name="route" :size="26" /><p>添加服务后显示对应的发布路径。</p></div>
    <div v-else class="topology-routes">
      <section v-for="route in routes" :key="route.id" class="topology-route" :data-route="route.id" :aria-label="route.label">
        <div class="route-heading"><strong><Icon :name="route.icon" :size="14" />{{ route.label }}</strong><span>{{ route.services.length }} 个服务<span v-if="route.disabled"> · {{ route.disabled }} 个已停用</span></span></div>
        <div v-for="item in route.lanes" :key="item.id" class="route-lane" :data-stage="item.id">
          <div class="lane-heading">
            <span :class="['lane-tag', item.id]">{{ item.id === 'applied' ? '本机配置已应用' : item.id === 'draft' ? '草稿待应用' : '已停用' }} · {{ item.count }}</span>
            <span v-if="item.id === 'draft' && item.disabled" class="lane-detail">含 {{ item.disabled }} 个停用配置</span>
          </div>
          <RoutePath :edge="route.id !== 'direct'" :show-edge-provider="false" :gateway-ip="gatewayIp" :gateway-pending="unappliedElsewhere" :stage="item.id" :show-connectivity="false" />
        </div>
      </section>
    </div>
  </section>
</template>

<style scoped>
.topology-routes{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:24px;padding:0 19px}.topology-routes:has(>.topology-route:only-child){grid-template-columns:1fr}
.topology-route+.topology-route{border-left:1px solid var(--line);padding-left:24px}
.route-heading{display:flex;justify-content:space-between;align-items:center;gap:10px;padding-top:14px;font-size:12px;color:var(--muted)}
.route-heading strong{display:flex;align-items:center;gap:6px;font-size:12px;font-weight:550;color:var(--ink)}
.route-heading svg{color:var(--blue)}
.route-lane+.route-lane{border-top:1px dashed var(--line)}
.lane-heading{display:flex;align-items:center;flex-wrap:wrap;gap:7px;padding-top:14px;font-size:12px;line-height:1.5}
.lane-tag{padding:2px 7px;border:1px solid var(--line);border-radius:5px;color:var(--muted);background:var(--surface)}
.lane-tag.applied{color:var(--blue);background:var(--blue-bg);border-color:color-mix(in srgb,var(--blue) 20%,transparent)}
.lane-tag.draft{border-style:dashed}.lane-detail{color:var(--muted)}
@media(max-width:1200px){.topology-routes{grid-template-columns:1fr;gap:0}.topology-route+.topology-route{border-left:0;padding-left:0;border-top:1px solid var(--line)}}
@media(max-width:760px){.topology-routes{padding:0 14px}}
</style>
