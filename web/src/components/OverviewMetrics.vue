<script setup>
import Icon from '../Icon.vue'

defineProps({ serviceCount: Number, enabledCount: Number, pendingCount: Number, domainCount: Number, edgeDomainCount: Number, certificateCount: Number, certificateTotal: Number })
</script>

<template>
  <div class="overview-metrics">
    <article class="metric-card metric-services" aria-label="服务入口概况">
      <span class="metric-symbol"><Icon name="grid" :size="32" /></span>
      <div class="metric-heading">服务入口</div>
      <div class="metric-value"><strong>{{ serviceCount }}</strong><span>个服务</span></div>
      <div class="metric-detail"><span :class="['metric-chip', { 'has-pending': pendingCount }]"><Icon name="power" :size="12" />{{ enabledCount }} 个启用</span><span v-if="pendingCount" class="metric-pending"><Icon name="edit" :size="12" />{{ pendingCount }} 项草稿改动</span></div>
    </article>
    <article class="metric-card metric-domains" aria-label="域名入口概况">
      <span class="metric-symbol"><Icon name="globe" :size="32" /></span>
      <div class="metric-heading">域名入口</div>
      <div class="metric-value"><strong>{{ domainCount }}</strong><span>个域名</span></div>
      <div class="metric-detail"><span><Icon name="route" :size="13" />直连 {{ domainCount - edgeDomainCount }}</span><span><Icon name="cloud" :size="13" />边缘 {{ edgeDomainCount }}</span></div>
    </article>
    <article class="metric-card metric-certificates" aria-label="有效证书概况">
      <span class="metric-symbol"><Icon name="shield" :size="32" /></span>
      <div class="metric-heading">有效证书</div>
      <div class="metric-value"><strong>{{ certificateCount }}</strong><span>张证书</span></div>
      <div class="metric-detail"><span><Icon name="file" :size="13" />共 {{ certificateTotal }} 个证书槽</span></div>
    </article>
  </div>
</template>

<style scoped>
.overview-metrics{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:16px;margin-bottom:20px}
.metric-card{display:grid;grid-template-columns:64px minmax(0,1fr);grid-template-rows:auto auto 1fr;column-gap:18px;min-width:0;padding:23px 22px;border:1px solid var(--card-line);border-radius:var(--radius);background:var(--surface);color:var(--ink);box-shadow:var(--card-shadow)}
.metric-heading{grid-column:2;color:var(--ink);font-size:14px;font-weight:550}
.metric-symbol{grid-column:1;grid-row:1/4;display:grid;place-items:center;align-self:center;width:64px;height:64px;border-radius:17px;background:var(--blue-bg);color:var(--blue);box-shadow:inset 0 1px var(--control-highlight)}
.metric-domains .metric-symbol{color:var(--purple);background:var(--purple-bg)}
.metric-certificates .metric-symbol{color:var(--green);background:var(--green-bg)}
.metric-value{grid-column:2;display:flex;align-items:baseline;gap:9px;margin:2px 0 8px}
.metric-value strong{font-size:35px;line-height:1.3;font-weight:650;font-variant-numeric:tabular-nums;letter-spacing:-1px}
.metric-value>span{font-size:12px;color:var(--muted)}
.metric-detail,.metric-detail>span{display:flex;align-items:center;gap:6px}
.metric-detail{grid-column:2;align-content:start;flex-wrap:wrap;gap:5px 12px;min-height:24px;color:var(--muted);font-size:12px}
.metric-detail>span{white-space:nowrap}
.metric-detail svg{opacity:.9}
.metric-chip{color:var(--green)}
.metric-chip.has-pending{color:var(--amber)}
.metric-pending{color:var(--muted)}
@media(max-width:1200px){.metric-card{grid-template-columns:52px minmax(0,1fr);column-gap:12px;padding:20px 16px}.metric-symbol{width:52px;height:52px;border-radius:14px}.metric-symbol svg{width:28px;height:28px}.metric-value strong{font-size:32px}}
@media(max-width:760px){.overview-metrics{grid-template-columns:1fr;gap:11px}.metric-card{column-gap:16px;padding:17px 18px}.metric-heading{font-size:13px}.metric-value{margin:0 0 3px}.metric-value strong{font-size:30px}.metric-detail{min-height:18px}}
</style>
