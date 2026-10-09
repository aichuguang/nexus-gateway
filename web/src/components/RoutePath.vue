<script setup>
import { computed, inject } from 'vue'
import Icon from '../Icon.vue'
const props = defineProps({ edge: Boolean, edgeProvider: String, showEdgeProvider: { type: Boolean, default: true }, compact: Boolean, gatewayIp: String, gatewayPending: Boolean, stage: { type: String, default: 'draft' }, upstreamStatus: { type: String, default: 'unmonitored' }, motionEnabled: { type: Boolean, default: true }, showConnectivity: { type: Boolean, default: true } })
const profile = inject('gatewayProfile', { value: { clientName: '本地网关' } })
const routeCopy = { client: '客户端', edge: '边缘加速', network: 'VPN 内网' }
const upstreamLabels = { verified: '上游已验证', error: '上游异常', stale: '监测已过期', checking: '正在监测', unavailable: '监测不可用', unmonitored: '上游未验证', unconfirmed: '待核对配置', mixed: '上游结果不一致' }
const verified = computed(() => props.showConnectivity && !props.compact && props.stage === 'applied' && props.upstreamStatus === 'verified')
const flowing = computed(() => !props.compact && props.stage === 'applied')
const nodes = computed(() => [
  { id: 'client', icon: 'globe', label: routeCopy.client, note: '业务请求' },
  ...(props.edge ? [{ id: 'edge', icon: 'cloud', label: props.showEdgeProvider ? props.edgeProvider === 'cloudflare' ? 'Cloudflare' : 'EdgeOne' : routeCopy.edge, note: '边缘回源' }] : []),
  { id: 'primary', icon: 'server', label: props.compact ? '网关' : profile.value.clientName, note: props.gatewayPending ? '配置待核对' : props.gatewayIp || '未配置地址' },
  { id: 'upstream', icon: 'route', label: props.compact ? '上游' : '内网上游', note: !props.showConnectivity ? routeCopy.network : props.stage === 'applied' ? upstreamLabels[props.upstreamStatus] : props.stage === 'disabled' ? '服务已停用' : routeCopy.network },
])
</script>

<template>
  <div :class="['route-path', { compact, 'upstream-verified': verified, 'flow-running': flowing && motionEnabled }, !compact && `stage-${stage}`, !compact && `upstream-${upstreamStatus}`]" :aria-label="`配置路径：${nodes.map(node => node.label).join(' → ')}${compact ? '' : `；${stage === 'applied' ? '本机配置已应用，粒子仅示意传输方向，不代表实时流量' : stage === 'disabled' ? '已停用' : '草稿待应用'}，公网段未验证`}`">
    <template v-for="(node, index) in nodes" :key="node.id">
      <span v-if="index" :class="['flow-line', { 'upstream-line': node.id === 'upstream', 'flow-verified': node.id === 'upstream' && verified }]" aria-hidden="true">
        <svg v-if="flowing" class="flow-stream" viewBox="0 0 160 24" preserveAspectRatio="none" focusable="false">
          <line class="flow-ribbon" x1="2" y1="12" x2="158" y2="12" />
          <line class="flow-beads" x1="2" y1="12" x2="158" y2="12" />
        </svg>
        <Icon name="arrow" :size="14" :class="{ 'flow-arrow': !compact }" />
      </span>
      <div :class="['flow-node', node.id]"><span class="node-icon"><Icon :name="node.icon" :size="compact ? 14 : 20" /></span><strong>{{ node.label }}</strong><small v-if="!compact">{{ node.note }}</small></div>
    </template>
  </div>
</template>

<style scoped>
.route-path{display:flex;align-items:flex-start;justify-content:space-between;gap:6px;padding:18px 0;min-width:0}
.flow-node{display:flex;flex-direction:column;align-items:center;gap:6px;min-width:0;flex:0 1 110px;text-align:center}
.node-icon{display:grid;place-items:center;width:40px;height:40px;border:1px solid color-mix(in srgb,var(--blue) 18%,transparent);border-radius:12px;background:var(--blue-bg);color:var(--blue)}
.route-path:not(.compact) .node-icon{color:var(--muted);background:var(--surface);border-color:var(--line)}
.route-path.stage-applied .node-icon{color:var(--blue);background:var(--blue-bg);border-color:color-mix(in srgb,var(--blue) 18%,transparent)}
.route-path.stage-applied .upstream .node-icon{color:var(--muted);background:var(--surface);border-color:var(--line)}
.route-path.upstream-verified .upstream .node-icon{color:var(--green);background:var(--green-bg);border-color:color-mix(in srgb,var(--green) 18%,transparent)}
.route-path.upstream-error .upstream .node-icon,.route-path.upstream-mixed .upstream .node-icon,.route-path.upstream-unavailable .upstream .node-icon,.route-path.upstream-unconfirmed .upstream .node-icon{color:var(--amber);background:var(--surface);border-color:var(--line)}
.flow-node strong{overflow-wrap:anywhere;font-size:12px;font-weight:600}.flow-node small{font-size:12px;line-height:1.5;color:var(--muted);overflow-wrap:anywhere}
.flow-line{position:relative;display:flex;align-items:center;flex:1;min-width:48px;height:24px;margin-top:8px;color:var(--muted)}
.flow-line::before{content:'';flex:1;border-top:1px solid var(--line)}
.route-path:not(.compact) .flow-line::before{margin-right:10px}
.flow-arrow{position:absolute;right:0;top:5px;flex-shrink:0;pointer-events:none}
.stage-draft .flow-line::before{border-top-style:dashed}
.stage-applied .flow-line{color:var(--blue)}.stage-applied .flow-line::before{border-top-color:color-mix(in srgb,var(--blue) 65%,transparent)}
.stage-applied .upstream-line{color:var(--blue)}
.stage-applied .flow-verified{color:var(--green)}.stage-applied .flow-verified::before{border-top-color:var(--green)}
.upstream-error .upstream-line,.upstream-mixed .upstream-line,.upstream-unavailable .upstream-line,.upstream-unconfirmed .upstream-line{color:var(--amber)}
.upstream-error .upstream-line::before,.upstream-mixed .upstream-line::before,.upstream-unavailable .upstream-line::before,.upstream-unconfirmed .upstream-line::before{border-top-color:var(--amber)}
.stage-applied .flow-line::before{border-top-width:2.5px;opacity:.18}
.flow-stream{position:absolute;inset:0;width:calc(100% - 10px);height:24px;overflow:hidden;pointer-events:none;opacity:.5}
.flow-ribbon,.flow-beads{fill:none;stroke:currentColor;stroke-linecap:round;vector-effect:non-scaling-stroke}
.flow-ribbon{stroke-width:2.5;stroke-dasharray:10 12;filter:drop-shadow(0 0 2px color-mix(in srgb,currentColor 35%,transparent))}
.flow-beads{stroke-width:3.75;stroke-dasharray:1 21;stroke-dashoffset:-9;opacity:.85}
/* The requested direction effect is local to these lines, independent of monitoring results. */
.flow-running .flow-stream{opacity:1}
.flow-running .flow-ribbon{animation:ribbon-current 1.25s linear infinite!important}
.flow-running .flow-beads{animation:bead-current 1.25s linear infinite!important}
@keyframes ribbon-current{from{stroke-dashoffset:44}to{stroke-dashoffset:0}}
@keyframes bead-current{from{stroke-dashoffset:35}to{stroke-dashoffset:-9}}
@media(prefers-reduced-motion:reduce){.route-path:not(.flow-running) .flow-ribbon,.route-path:not(.flow-running) .flow-beads{animation:none}}
.compact{padding:0;gap:6px;justify-content:flex-start;align-items:center;flex-wrap:nowrap;white-space:nowrap;color:var(--muted)}
.compact .flow-node{flex:none;flex-direction:row;gap:4px}.compact .node-icon{width:auto;height:auto;border:0;border-radius:0;background:none;color:inherit}.compact .flow-node strong{font-weight:400}.compact .flow-line{flex:none;min-width:0;height:auto;margin:0}.compact .flow-line::before{display:none}
.compact .flow-line{width:14px;align-self:center}
@media(max-width:480px){.route-path:not(.compact){gap:3px}.route-path:not(.compact) .flow-line{min-width:28px;margin-top:5px}.flow-node{flex-basis:72px}.node-icon{width:34px;height:34px}.flow-node small{font-size:12px}}
</style>
