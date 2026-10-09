<script setup>
import Icon from '../Icon.vue'

defineProps({ checks: { type: Array, default: () => [] }, busy: Boolean, error: String })
function label(check, busy, error) {
  if (check.external) return check.label
  return busy ? '检查中' : error ? '读取失败' : check.label
}
function tone(check, busy, error) {
  if (check.external) return 'neutral'
  return busy ? 'neutral' : error ? 'amber' : check.tone
}
function statusIcon(check, busy, error) {
  if (check.external) return 'external'
  if (busy) return 'clock'
  if (error || check.tone === 'amber') return 'warning'
  return check.tone === 'private' ? 'check' : check.skipped ? 'minus' : 'clock'
}
</script>

<template>
  <section class="diagnostic-stages" aria-label="入口核验阶段" :aria-busy="busy">
    <div class="stages-heading"><h2><Icon name="route" :size="17" />入口核验</h2><span>配置与本机检查 / 独立外部验证</span></div>
    <ol class="stages-list">
      <li v-for="(check, index) in checks" :key="check.title" class="diagnostic-stage" :class="[{ 'stage-external': check.external }, tone(check, busy, error)]">
        <div class="stage-top"><span class="stage-icon"><Icon :name="check.icon" :size="21" /></span></div>
        <h3>{{ check.title }}</h3>
        <p>{{ !check.external && busy ? '正在重新核对，请稍候…' : !check.external && error ? '本轮未取得核验结果' : check.note }}</p>
        <span :class="['stage-status', tone(check, busy, error)]"><Icon :name="statusIcon(check, busy, error)" :size="12" />{{ label(check, busy, error) }}</span>
        <span class="stage-link" :class="{ terminal: index === checks.length - 1 }" aria-hidden="true"><span class="stage-number">{{ String(index + 1).padStart(2, '0') }}</span><span v-if="index < checks.length - 1" class="stage-connector" :class="{ boundary: checks[index + 1].external }"></span></span>
      </li>
    </ol>
  </section>
</template>

<style scoped>
.diagnostic-stages{margin-bottom:20px;border:1px solid var(--card-line);border-radius:var(--radius);background:var(--surface);box-shadow:var(--card-shadow)}
.stages-heading{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:15px 19px;border-bottom:1px solid var(--line)}
.stages-heading h2{display:flex;align-items:center;gap:8px;font-size:15px}
.stages-heading h2 svg{color:var(--blue)}
.stages-heading>span{font-size:12px;color:var(--muted)}
.stages-list{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));list-style:none;padding:0;margin:0}
.diagnostic-stage{position:relative;display:flex;flex-direction:column;align-items:center;text-align:center;min-width:0;padding:19px 20px 18px}
.diagnostic-stage+.diagnostic-stage{border-left:1px solid var(--line)}
.diagnostic-stage.stage-external{border-left-style:dashed;background:color-mix(in srgb,var(--surface-2) 50%,var(--surface));border-radius:0 0 var(--radius) 0}
.stage-top{position:relative;display:flex;align-items:center;justify-content:center;align-self:stretch;min-height:40px;margin-bottom:13px}
.stage-icon{display:grid;place-items:center;flex-shrink:0;width:40px;height:40px;border-radius:10px;border:1px solid var(--line);background:var(--surface-2);color:var(--muted);box-shadow:inset 0 1px var(--control-highlight)}
.stage-icon svg{display:block;width:21px;height:21px}
.private .stage-icon{color:var(--green);border-color:color-mix(in srgb,var(--green) 20%,transparent);background:var(--green-bg)}
.amber .stage-icon{color:var(--amber);border-color:color-mix(in srgb,var(--amber) 20%,transparent);background:var(--amber-bg)}
.stage-number{font-family:var(--mono);font-size:12px;color:var(--muted)}
.diagnostic-stage h3{font-size:13px;font-weight:600;margin-bottom:6px}
.diagnostic-stage p{font-size:12px;line-height:1.7;color:var(--muted);min-height:41px;margin-bottom:12px}
.stage-status{display:inline-flex;align-items:center;justify-content:center;gap:5px;min-height:26px;margin-top:auto;padding:3px 7px;border:0;border-radius:5px;font-size:12px;color:var(--muted);background:transparent}
.stage-status.private{padding:3px 7px;color:var(--green);border-color:color-mix(in srgb,var(--green) 18%,transparent);background:var(--green-bg)}
.stage-status.amber{padding:3px 7px;color:var(--amber);border-color:color-mix(in srgb,var(--amber) 18%,transparent);background:var(--amber-bg)}
.stage-link{position:absolute;right:-9px;top:50%;transform:translateY(-50%);z-index:1;display:flex;align-items:center;gap:11px}
.stage-link.terminal{right:20px}
.stage-connector{position:relative;flex-shrink:0;width:18px;border-top:1.5px solid var(--soft);box-shadow:0 0 0 5px var(--surface)}
.stage-connector::after{content:'';position:absolute;right:0;top:-3.5px;width:6px;height:6px;border-top:1.5px solid var(--soft);border-right:1.5px solid var(--soft);transform:rotate(45deg)}
.stage-connector.boundary{border-top-style:dashed}
@media(max-width:1050px) and (min-width:601px){.stages-list{grid-template-columns:repeat(2,minmax(0,1fr))}.diagnostic-stage:nth-child(3){border-left:0}.diagnostic-stage:nth-child(n+3){border-top:1px solid var(--line)}.diagnostic-stage:nth-child(2) .stage-connector{display:none}.diagnostic-stage:nth-child(2) .stage-link{right:20px}}
@media(max-width:600px){.stages-heading{align-items:flex-start;flex-direction:column;padding:14px 16px;gap:4px}.stages-list{grid-template-columns:1fr}.diagnostic-stage{display:grid;grid-template-columns:40px minmax(0,1fr) auto;column-gap:12px;align-items:center;text-align:left;padding:17px 16px}.diagnostic-stage+.diagnostic-stage{border-left:0;border-top:1px solid var(--line)}.diagnostic-stage.stage-external{border-top-style:dashed;border-radius:0 0 var(--radius) var(--radius)}.stage-top{grid-column:1;grid-row:1/3;width:40px;margin:0;align-self:center;justify-self:center}.stage-number{display:none}.diagnostic-stage h3{grid-column:2;margin-bottom:3px}.diagnostic-stage p{grid-column:2/4;min-height:0;margin-bottom:0;font-size:12px}.stage-status{grid-column:3;grid-row:1;justify-self:end;margin-top:0;font-size:12px;white-space:nowrap}.stage-link{right:auto;top:auto;bottom:-9px;left:50%;transform:translateX(-50%)}.stage-link.terminal{display:none}.stage-connector{width:0;height:18px;border-top:0;border-left:1.5px solid var(--soft)}.stage-connector::after{right:auto;top:auto;bottom:0;left:-3.5px;transform:rotate(135deg)}.stage-connector.boundary{border-left-style:dashed}}
</style>
