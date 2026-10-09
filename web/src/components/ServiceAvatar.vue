<script setup>
import { computed } from 'vue'
import Icon from '../Icon.vue'
import { serviceAppearance } from '../service-icons.js'

const props = defineProps({ service: { type: Object, default: () => ({}) } })
const appearance = computed(() => serviceAppearance(props.service))
</script>

<template>
  <span class="service-avatar" :class="appearance.tone" aria-hidden="true"><Icon :name="appearance.icon" :size="22" /></span>
</template>

<style scoped>
.service-avatar{display:grid;place-items:center;flex-shrink:0;width:40px;height:40px;border-radius:11px;border:1px solid color-mix(in srgb,var(--blue) 13%,transparent);background:var(--blue-bg);color:var(--blue);box-shadow:inset 0 1px var(--control-highlight)}
.service-avatar.purple{color:var(--purple);background:var(--purple-bg);border-color:color-mix(in srgb,var(--purple) 13%,transparent)}
.service-avatar.green{color:var(--green);background:var(--green-bg);border-color:color-mix(in srgb,var(--green) 13%,transparent)}
.service-avatar.amber{color:var(--amber);background:var(--amber-bg);border-color:color-mix(in srgb,var(--amber) 13%,transparent)}
.service-avatar.rose{color:var(--rose);background:var(--rose-bg);border-color:color-mix(in srgb,var(--rose) 13%,transparent)}
.service-avatar.cyan{color:var(--cyan);background:var(--cyan-bg);border-color:color-mix(in srgb,var(--cyan) 13%,transparent)}
.service-avatar.orange{color:var(--orange);background:var(--orange-bg);border-color:color-mix(in srgb,var(--orange) 13%,transparent)}
.service-avatar.slate{color:var(--slate);background:var(--slate-bg);border-color:color-mix(in srgb,var(--slate) 13%,transparent)}
.service-avatar.teal{color:var(--icon-teal);background:var(--icon-teal-bg);border-color:color-mix(in srgb,var(--icon-teal) 13%,transparent)}
.service-avatar.indigo{color:var(--indigo);background:var(--indigo-bg);border-color:color-mix(in srgb,var(--indigo) 13%,transparent)}
.service-avatar.lime{color:var(--lime);background:var(--lime-bg);border-color:color-mix(in srgb,var(--lime) 13%,transparent)}
</style>
