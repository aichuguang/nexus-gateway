<script setup>
import { computed } from 'vue'
import Icon from '../../Icon.vue'

const props = defineProps({ type: { type: String, default: 'info' }, message: String, progress: { type: Number, default: 1 } })
defineEmits(['dismiss'])
const types = {
  success: { title: '操作成功', path: 'M7 12l3.2 3.2L17 8.5' },
  error: { title: '操作失败', path: 'm8 8 8 8M16 8l-8 8' },
  info: { title: '提示信息', path: 'M12 11v6M12 7v.2' },
  warning: { title: '操作提醒', path: 'M12 6.5v7M12 17v.2' },
}
const kind = computed(() => types[props.type] || types.info)
</script>

<template>
  <article :class="['ui-toast', `ui-toast--${type}`]" :role="type === 'error' ? 'alert' : 'status'" aria-atomic="true">
    <span class="ui-toast-icon" aria-hidden="true">
      <svg viewBox="0 0 24 24" width="24" height="24" fill="none">
        <circle cx="12" r="11.5" cy="12" fill="currentColor" />
        <path :d="kind.path" class="ui-toast-symbol" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </span>
    <div class="ui-toast-copy"><strong>{{ kind.title }}</strong><p>{{ message }}</p></div>
    <button type="button" class="ui-toast-close" :aria-label="`关闭${kind.title}提示`" @click="$emit('dismiss')"><Icon name="close" :size="16" /></button>
    <span class="ui-toast-progress" :style="{ transform: `scaleX(${Math.max(0, Math.min(1, progress))})` }" aria-hidden="true"></span>
  </article>
</template>

<style scoped>
.ui-toast{--toast-color:#3983f5;--toast-icon-bg:#eaf2ff;--toast-icon-line:#d4e5ff;--toast-symbol:#fff;--toast-surface:#fff;--toast-title:#202c3b;--toast-text:#7b8798;--toast-close:#98a6b9;position:relative;overflow:hidden;display:flex;align-items:center;gap:12px;width:100%;min-height:80px;padding:16px 36px 16px 16px;border:1px solid #eaf0f780;border-radius:12px;background:var(--toast-surface);box-shadow:0 8px 28px #3045600a,0 2px 6px #30456003;animation:toast-enter .16s ease-out;pointer-events:auto;flex-shrink:0}
.ui-toast--success{--toast-color:#109c80;--toast-icon-bg:#e2f8f1;--toast-icon-line:#c8f1e7}
.ui-toast--error{--toast-color:#ec4545;--toast-icon-bg:#ffe8e8;--toast-icon-line:#ffd2d2}
.ui-toast--warning{--toast-color:#ffb000;--toast-icon-bg:#fff6e2;--toast-icon-line:#ffe9bc}
.ui-toast-icon{display:grid;place-items:center;flex-shrink:0;width:48px;height:48px;border:1px solid var(--toast-icon-line);border-radius:15px;background:var(--toast-icon-bg);color:var(--toast-color)}
.ui-toast-symbol{stroke:var(--toast-symbol)}
.ui-toast-copy{flex:1;min-width:0}.ui-toast-copy strong{display:block;color:var(--toast-title);font-size:14px;font-weight:650;line-height:1.4}
.ui-toast-copy p{margin:4px 0 0;color:var(--toast-text);font-size:13px;font-weight:400;line-height:1.55;overflow-wrap:anywhere;white-space:pre-wrap}
.ui-toast-close{position:absolute;top:12px;right:10px;display:grid;place-items:center;width:22px;height:22px;padding:0;border:0;border-radius:6px;background:transparent;color:var(--toast-close);cursor:pointer}
.ui-toast-close:hover{background:var(--toast-icon-bg);color:var(--toast-color)}
.ui-toast-close:focus-visible{outline:2px solid var(--toast-color);outline-offset:3px}
.ui-toast-progress{position:absolute;left:16px;right:16px;bottom:1px;height:3px;border-radius:999px;background:linear-gradient(90deg,color-mix(in srgb,var(--toast-color) 24%,transparent),var(--toast-color) 65%,color-mix(in srgb,var(--toast-color) 55%,#fff));transform-origin:left;will-change:transform;pointer-events:none}
:global([data-theme="dark"] .ui-toast){--toast-surface:#26323e;--toast-title:#f4f6fa;--toast-text:#a8b6c8;--toast-close:#a5b3c5;--toast-color:#4893ff;--toast-icon-bg:#2b4b75;--toast-icon-line:#2e527d;--toast-symbol:#223849;border-color:#ffffff06;box-shadow:0 8px 28px #00000020}
:global([data-theme="dark"] .ui-toast--success){--toast-color:#42c8ac;--toast-icon-bg:#255b55;--toast-icon-line:#29645c}
:global([data-theme="dark"] .ui-toast--error){--toast-color:#f04c43;--toast-icon-bg:#613d44;--toast-icon-line:#684048;--toast-symbol:#fff}
:global([data-theme="dark"] .ui-toast--warning){--toast-color:#ffb600;--toast-icon-bg:#65532d;--toast-icon-line:#735d2c;--toast-symbol:#fff}
@keyframes toast-enter{from{opacity:0;transform:translateY(-6px)}to{opacity:1;transform:translateY(0)}}
@media(prefers-reduced-motion:reduce){.ui-toast{animation:none}}
@media(max-width:480px){.ui-toast{padding:14px 34px 14px 14px}.ui-toast-icon{width:44px;height:44px;border-radius:14px}}
</style>
