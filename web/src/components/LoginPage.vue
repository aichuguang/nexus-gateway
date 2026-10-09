<script setup>
import { onBeforeUnmount, onMounted, ref } from 'vue'
import Icon from '../Icon.vue'
import BrandLogo from './BrandLogo.vue'
import UiTooltip from './ui/UiTooltip.vue'

const props = defineProps({
  projectName: { type: String, default: '澄光网关' },
  clientName: { type: String, default: '本地客户端' },
  logo: { type: String, default: '' },
  version: { type: String, default: '' },
  theme: { type: String, default: 'light' },
  busy: Boolean,
  error: { type: String, default: '' },
  password: { type: String, default: '' },
})
const emit = defineEmits(['update:password', 'login', 'toggle-theme'])
const copy = {
  lightTheme: '切换到浅色主题',
  darkTheme: '切换到深色主题',
  eyebrow: '你的服务，一个入口',
  title: '连接服务，',
  titleAccent: '让入口井然有序。',
  description: '从公网入口到内网上游，',
  descriptionDetail: '在一个清晰的工作空间里管理。',
  publicEntry: '公网入口',
  publicCaption: '统一接入管理',
  localCaption: '本地服务转发',
  networkCaption: '灵活组网',
  secureAccess: '安全接入',
  routing: '智能转发',
  management: '统一管理',
  formLabel: '管理登录',
  kicker: '管理工作空间',
  welcome: '欢迎登录',
  formDescription: '使用网关的管理密码进入工作空间。',
  password: '管理密码',
  passwordPlaceholder: '输入管理密码',
  hidePassword: '隐藏密码',
  showPassword: '显示密码',
  verifying: '正在验证…',
  submit: '进入工作空间',
  session: '使用安全管理会话，无需注册账号',
  security: 'HTTP 管理入口仅通过受信任的 EasyTier、其他加密 VPN 或 SSH 隧道使用。',
  footer: '管理台仅供受信任的内网使用',
}
const revealPassword = ref(false)
const pageHidden = ref(false)

function submitLogin() {
  if (props.busy || !props.password) return
  emit('login')
}
function updateVisibility() { pageHidden.value = document.visibilityState === 'hidden' }
onMounted(() => {
  updateVisibility()
  document.addEventListener('visibilitychange', updateVisibility)
})
onBeforeUnmount(() => document.removeEventListener('visibilitychange', updateVisibility))
</script>

<template>
  <main class="gateway-login" :data-login-theme="theme" :data-motion-paused="pageHidden || busy">
    <header class="gateway-login-header">
      <div class="gateway-login-brand"><BrandLogo class="gateway-login-mark" :src="logo" :size="44" /><span>{{ projectName }}</span></div>
      <button class="gateway-theme-control" type="button" :aria-label="theme === 'dark' ? copy.lightTheme : copy.darkTheme" :aria-pressed="theme === 'dark'" @click="emit('toggle-theme')">
        <Icon name="sun" :size="19" /><span class="gateway-theme-track"><span></span></span><Icon name="moon" :size="19" />
      </button>
    </header>

    <div class="gateway-login-content">
      <section class="gateway-login-intro" aria-labelledby="gateway-login-title">
        <div class="gateway-login-eyebrow"><span></span>{{ copy.eyebrow }}</div>
        <h1 id="gateway-login-title">{{ copy.title }}<br /><strong>{{ copy.titleAccent }}</strong></h1>
        <p class="gateway-login-description">{{ copy.description }}<br />{{ copy.descriptionDetail }}</p>
        <div class="gateway-login-features">
          <div><span class="gateway-feature-icon"><Icon name="globe" :size="25" /></span><span><b>{{ copy.publicEntry }}</b><small>{{ copy.publicCaption }}</small></span></div>
          <div><span class="gateway-feature-icon"><Icon name="server" :size="25" /></span><span><UiTooltip :text="clientName"><b tabindex="0">{{ clientName }}</b></UiTooltip><small>{{ copy.localCaption }}</small></span></div>
          <div><span class="gateway-feature-icon"><Icon name="route" :size="25" /></span><span><b>EasyTier</b><small>{{ copy.networkCaption }}</small></span></div>
        </div>
      </section>

      <div class="gateway-login-art" aria-hidden="true">
        <svg viewBox="0 0 540 560" fill="none" xmlns="http://www.w3.org/2000/svg">
          <defs>
            <linearGradient id="gateway-glass" x1="167" y1="94" x2="354" y2="349" gradientUnits="userSpaceOnUse"><stop stop-color="#94bcff" stop-opacity=".12" /><stop offset=".55" stop-color="#4583ff" stop-opacity=".25" /><stop offset="1" stop-color="#7fbbff" stop-opacity=".65" /></linearGradient>
            <linearGradient id="gateway-platform" x1="260" y1="280" x2="260" y2="440" gradientUnits="userSpaceOnUse"><stop stop-color="#8fb7ff" stop-opacity=".85" /><stop offset="1" stop-color="#215bfa" stop-opacity=".95" /></linearGradient>
            <linearGradient id="gateway-edge" x1="169" y1="103" x2="355" y2="340" gradientUnits="userSpaceOnUse"><stop stop-color="#a8c9ff" stop-opacity=".35" /><stop offset=".5" stop-color="#a6cdff" /><stop offset="1" stop-color="#3e7dff" /></linearGradient>
            <radialGradient id="gateway-glow"><stop stop-color="#3679ff" stop-opacity=".35" /><stop offset="1" stop-color="#3679ff" stop-opacity="0" /></radialGradient>
            <linearGradient id="gateway-data-stream" x1="45" y1="390" x2="500" y2="335" gradientUnits="userSpaceOnUse"><stop stop-color="#80b2ff" stop-opacity=".1" /><stop offset=".48" stop-color="#85c1ff" /><stop offset="1" stop-color="#5086ff" stop-opacity=".15" /></linearGradient>
            <filter id="gateway-shadow" x="-50%" y="-50%" width="200%" height="200%"><feGaussianBlur stdDeviation="10" /></filter>
          </defs>
          <ellipse cx="268" cy="339" rx="242" ry="207" fill="url(#gateway-glow)" />
          <g stroke="currentColor" stroke-width="1" opacity=".14"><path d="M-20 298 278 470 563 305M-20 352 278 524 563 359M37 224 350 405M107 182 420 363M174 144 488 325M-4 420 361 209M60 456 425 246M132 497 497 286" /></g>
          <g class="gateway-data-rails" stroke-linecap="round" stroke-linejoin="round">
            <path class="gateway-rail-base" d="m1 303 104 60v43l74 43 70-40M521 332l-86 50v-46l-76-44M275 460v45l86 49" />
            <path class="gateway-rail-flow gateway-motion" pathLength="100" d="m1 303 104 60v43l74 43 70-40" />
            <path class="gateway-rail-flow gateway-motion" pathLength="100" d="M521 332l-86 50v-46l-76-44" />
            <path class="gateway-rail-flow gateway-motion" pathLength="100" d="M275 460v45l86 49" />
          </g>
          <path d="m120 386 149-85 150 85-150 87z" fill="#2865ee" opacity=".28" filter="url(#gateway-shadow)" />
          <g class="gateway-engine gateway-motion">
          <path d="m112 364 157-91 157 91v22l-157 91-157-91z" fill="url(#gateway-glass)" stroke="#679aff" stroke-opacity=".25" />
          <path d="m112 364 157 91 157-91M269 455v22" stroke="#8eb3ff" stroke-opacity=".6" />
          <path d="m137 345 132-76 132 76v18l-132 77-132-77z" fill="url(#gateway-platform)" />
          <path d="m137 345 132 76 132-76-132-76z" fill="#97bdff" fill-opacity=".26" stroke="#a8c9ff" stroke-opacity=".65" />
          <path d="m164 332 105-60 104 60-104 60z" fill="#b5d0ff" fill-opacity=".25" />
          <path d="m185 109 91 11 86 94v95c0 64-39 73-86 62-48-44-91-97-91-157z" fill="#5595ff" fill-opacity=".1" stroke="#99bfff" stroke-opacity=".3" stroke-width="9" />
          <path d="m171 100 91 12 86 93v94c0 65-39 74-86 63-49-44-91-97-91-157z" fill="url(#gateway-glass)" stroke="url(#gateway-edge)" stroke-width="3" />
          <path d="m185 119 77 11 71 81v85c0 53-29 61-71 51-40-37-77-85-77-144z" stroke="#c0dcff" stroke-opacity=".22" />
          <g transform="translate(216 183) skewY(29)" stroke="#82b5ff" stroke-width="7" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10" /><circle cx="76" cy="94" r="10" /><path d="M33 13h21c37 0 37 40 8 40H28C-1 53 0 94 32 94h23" /></g>
          </g>
          <g fill="url(#gateway-glass)" stroke="#8fb7ff" stroke-opacity=".45">
            <g class="gateway-satellite gateway-satellite-left gateway-motion"><path d="m62 337 32-19 32 19v37l-32 19-32-19zM62 337l32 19 32-19M94 356v37" /></g>
            <g class="gateway-satellite gateway-satellite-right gateway-motion"><path d="m407 418 34-20 34 20v39l-34 20-34-20zM407 418l34 20 34-20M441 438v39" /></g>
          </g>
          <g class="gateway-art-labels">
            <g transform="matrix(.866 .5 0 1 368 156)"><rect x="0" y="-18" width="101" height="31" rx="5" /><text x="12" y="3">{{ copy.secureAccess }}</text></g>
            <g transform="matrix(.866 .5 0 1 368 206)"><rect x="0" y="-18" width="101" height="31" rx="5" /><text x="12" y="3">{{ copy.routing }}</text></g>
            <g transform="matrix(.866 .5 0 1 368 256)"><rect x="0" y="-18" width="101" height="31" rx="5" /><text x="12" y="3">{{ copy.management }}</text></g>
          </g>
        </svg>
      </div>

      <section class="gateway-login-form-side" :aria-label="copy.formLabel">
        <form class="gateway-login-card" @submit.prevent="submitLogin">
          <span class="gateway-card-kicker">{{ copy.kicker }}</span>
          <h2>{{ copy.welcome }}</h2><div class="gateway-card-name">{{ projectName }}</div>
          <p class="gateway-card-description">{{ copy.formDescription }}</p>
          <label class="gateway-password-label" for="login-password">{{ copy.password }}</label>
          <div class="gateway-password-field" :class="{ 'has-error': error }">
            <Icon name="lock" :size="20" />
            <input id="login-password" :value="password" :type="revealPassword ? 'text' : 'password'" autocomplete="current-password" required :placeholder="copy.passwordPlaceholder" :disabled="busy" :aria-invalid="Boolean(error)" :aria-describedby="error ? 'gateway-login-error' : undefined" autofocus @input="emit('update:password', $event.target.value)" />
            <button type="button" :aria-label="revealPassword ? copy.hidePassword : copy.showPassword" :aria-pressed="revealPassword" :disabled="busy" @click="revealPassword = !revealPassword"><Icon :name="revealPassword ? 'lock' : 'eye'" :size="19" /></button>
          </div>
          <p v-if="error" id="gateway-login-error" class="gateway-login-error" role="alert"><Icon name="warning" :size="16" />{{ error }}</p>
          <button class="gateway-login-submit" type="submit" :disabled="busy || !password" :aria-busy="busy"><span v-if="busy" class="gateway-login-spinner"></span><span>{{ busy ? copy.verifying : copy.submit }}</span><Icon v-if="!busy" name="arrow" :size="21" /></button>
          <div class="gateway-session-note"><Icon name="lock" :size="14" /><span>{{ copy.session }}</span></div>
          <div class="gateway-login-security"><Icon name="shield" :size="18" /><span>{{ copy.security }}</span></div>
        </form>
        <div class="gateway-login-version"><span>{{ clientName }}</span><span v-if="version">v{{ version }}</span></div>
      </section>
    </div>
    <footer class="gateway-login-footer">
      <span class="gateway-footer-security"><Icon name="lock" :size="15" />{{ copy.footer }}</span>
    </footer>
  </main>
</template>

<style scoped>
.gateway-login {
  --login-bg: #f3f7ff;
  --login-ink: #17233b;
  --login-muted: #647593;
  --login-blue: #285df6;
  --login-line: #d9e3f3;
  --login-error: #b14e5f;
  --feature-gap: 24px;
  position: relative;
  isolation: isolate;
  min-height: 100svh;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  padding: 34px clamp(28px, 5.5vw, 110px) 28px;
  background: radial-gradient(ellipse at 39% 47%, #d9e7ff90, transparent 55%), linear-gradient(125deg, #f9fbff, var(--login-bg));
  color: var(--login-ink);
  color-scheme: light;
}
.gateway-login[data-login-theme="dark"] {
  --login-bg: #0c1424;
  --login-ink: #eef3ff;
  --login-muted: #99aac8;
  --login-blue: #7ca4ff;
  --login-line: #34425b;
  --login-error: #f0a1ae;
  color-scheme: dark;
  background: radial-gradient(ellipse at 40% 49%, #16387466, transparent 56%), linear-gradient(125deg, #101b2d, #0a111e);
}
.gateway-login::before {
  content: "";
  position: absolute;
  z-index: -1;
  width: 90vw;
  height: 120px;
  top: 30%;
  left: 0;
  transform: rotate(-32deg);
  background: linear-gradient(transparent, #83aaff0a, transparent);
  border-top: 1px solid #83aaff0b;
  border-bottom: 1px solid #83aaff0b;
  pointer-events: none;
}
.gateway-login-header { display: flex; align-items: center; justify-content: space-between; gap: 25px; }
.gateway-login-brand { display: flex; gap: 13px; align-items: center; min-width: 0; max-width: 75%; font-size: 23px; font-weight: 750; line-height: 1.4; overflow-wrap: anywhere; }
.gateway-theme-control { display: flex; align-items: center; gap: 10px; padding: 8px 0; border: 0; background: none; color: var(--login-muted); cursor: pointer; }
.gateway-theme-track { width: 37px; height: 21px; border-radius: 20px; background: #d3e1fc; border: 1px solid #8cacf1; padding: 2px; display: block; }
.gateway-theme-track span { display: block; width: 15px; height: 15px; border-radius: 50%; background: #fff; box-shadow: 0 1px 4px #14367644; transition: transform .2s; }
.gateway-login[data-login-theme="dark"] .gateway-theme-track { background: #285df6; }
.gateway-login[data-login-theme="dark"] .gateway-theme-track span { transform: translateX(15px); }

.gateway-login-content { flex: 1; display: grid; grid-template-columns: minmax(315px, 1fr) minmax(250px, .9fr) minmax(340px, 1.05fr); align-items: center; gap: 12px; max-width: 1700px; width: 100%; margin: 40px auto; }
.gateway-login-intro { position: relative; z-index: 1; padding-bottom: 16px; }
.gateway-login-eyebrow { display: flex; align-items: center; gap: 9px; margin-bottom: 23px; color: var(--login-muted); font-size: 12px; letter-spacing: 3px; }
.gateway-login-eyebrow span { width: 6px; height: 6px; border-radius: 50%; background: #4d80f6; box-shadow: 0 0 0 4px #5f8fff15; }
.gateway-login-intro h1 { font-size: clamp(32px, 3.15vw, 55px); line-height: 1.4; letter-spacing: -1.8px; margin: 0; white-space: nowrap; font-weight: 750; color: var(--login-ink); }
.gateway-login-intro strong { font-weight: 750; color: var(--login-blue); }
.gateway-login-description { color: var(--login-muted); font-size: 17px; line-height: 1.8; margin: 22px 0 42px; }
.gateway-login-features { display: flex; align-items: center; gap: var(--feature-gap); }
.gateway-login-features > div { position: relative; display: flex; align-items: center; gap: 10px; min-width: 0; }
.gateway-login-features > div:not(:last-child)::after {
  content: "";
  position: absolute;
  right: calc(4px - var(--feature-gap));
  top: 50%;
  width: calc(var(--feature-gap) - 8px);
  height: 1px;
  background: linear-gradient(90deg, #75a4ff12, #75a4ff70, #75a4ff12);
  pointer-events: none;
}
.gateway-login-features > div > span:last-child { min-width: 0; }
.gateway-feature-icon { flex-shrink: 0; display: grid; place-items: center; width: 43px; height: 46px; border: 1px solid #739fff18; border-radius: 12px; background: linear-gradient(145deg, #e8f0ffd9, #dce8ff94); color: var(--login-blue); box-shadow: inset 0 1px #ffffff80; }
.gateway-login[data-login-theme="dark"] .gateway-feature-icon { background: linear-gradient(130deg, #263759, #16233b); border-color: #8eb3ff1c; box-shadow: inset 0 1px #b5d0ff0b; }
.gateway-login-features b { display: block; max-width: 120px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; font-weight: 650; line-height: 22px; letter-spacing: .01em; }
.gateway-login-features b:focus-visible { outline: 2px solid var(--login-blue); outline-offset: 2px; border-radius: 2px; }
.gateway-login-features small { display: block; font-size: 11px; color: var(--login-muted); white-space: nowrap; line-height: 18px; letter-spacing: .02em; margin-top: 2px; }

.gateway-login-art { width: 144%; max-width: min(700px, 66svh); margin-left: -26%; pointer-events: none; color: var(--login-muted); }
.gateway-login-art svg { display: block; width: 100%; overflow: visible; }
.gateway-rail-base { stroke: #578fff; stroke-width: 1.4; opacity: .3; }
.gateway-rail-flow { stroke: url(#gateway-data-stream); stroke-width: 2.2; stroke-dasharray: 9 91; stroke-dashoffset: 35; opacity: .7; }
.gateway-rail-flow:nth-of-type(3) { stroke-dashoffset: 65; }
.gateway-rail-flow:nth-of-type(4) { stroke-dashoffset: 85; }
.gateway-art-labels rect { fill: #e8f1ff75; stroke: #84a8ea40; stroke-width: 1; }
.gateway-art-labels text { fill: currentColor; font-size: 12px; font-weight: 500; letter-spacing: 1.2px; opacity: .78; }
.gateway-login[data-login-theme="dark"] .gateway-art-labels rect { fill: #1b31564a; stroke: #81abff26; }
.gateway-login[data-login-theme="dark"] .gateway-rail-base { opacity: .36; }
.gateway-login[data-motion-paused="true"] .gateway-motion { animation-play-state: paused; }

.gateway-login-form-side { position: relative; z-index: 1; width: 100%; max-width: 480px; justify-self: end; }
.gateway-login-card {
  position: relative;
  padding: 38px 36px 30px;
  background: linear-gradient(145deg, #fffffff2, #f9fcffd9);
  border: 1px solid #6a8dd528;
  border-radius: 24px;
  box-shadow: 0 28px 64px -24px #2a498338, 0 4px 16px -6px #2563eb14, inset 0 1px #fffffff2;
  backdrop-filter: blur(18px) saturate(120%);
}
.gateway-login-card::before { content: ""; position: absolute; top: -1px; left: 28px; right: 28px; height: 1px; background: linear-gradient(90deg, transparent, #afc9ff8c, transparent); pointer-events: none; }
.gateway-login[data-login-theme="dark"] .gateway-login-card { background: linear-gradient(145deg, #1b2940e8, #111d30e0); border-color: #9db8e32b; box-shadow: 0 28px 70px -24px #00000073, 0 0 28px -12px #447bea1f, inset 0 1px #c0d8ff0c; }
.gateway-login[data-login-theme="dark"] .gateway-login-card::before { background: linear-gradient(90deg, transparent, #8eafff52, transparent); }
.gateway-card-kicker { display: block; font-size: 10px; letter-spacing: 3px; color: var(--login-muted); margin-bottom: 14px; }
.gateway-login-card h2 { font-size: 28px; line-height: 1.4; margin: 0 0 1px; font-weight: 750; letter-spacing: -.5px; color: var(--login-ink); }
.gateway-card-name { font-size: 19px; font-weight: 650; line-height: 1.55; color: var(--login-blue); overflow-wrap: anywhere; }
.gateway-card-description { font-size: 13px; color: var(--login-muted); margin: 18px 0 27px; line-height: 1.7; }
.gateway-password-label { display: block; font-size: 12px; font-weight: 600; margin-bottom: 8px; color: var(--login-ink); }
.gateway-password-field { display: flex; align-items: center; gap: 10px; min-width: 0; border: 1px solid var(--login-line); background: #ffffff70; border-radius: 10px; padding: 0 13px; color: var(--login-blue); transition: border-color .2s, box-shadow .2s; }
.gateway-password-field:focus-within { border-color: #5889f8; box-shadow: 0 0 0 3px #5889f81c; }
.gateway-password-field.has-error { border-color: #ce6471; }
.gateway-password-field > svg { flex-shrink: 0; }
.gateway-password-field input { flex: 1; min-width: 0; width: 100%; height: 49px; background: transparent !important; border: 0 !important; box-shadow: none !important; padding: 0; color: var(--login-ink); outline: none !important; font-size: 13px; border-radius: 0; }
.gateway-password-field input::placeholder { color: var(--login-muted); opacity: .85; }
.gateway-password-field button { flex-shrink: 0; display: grid; place-items: center; width: 30px; height: 34px; padding: 0; border: 0; background: transparent; color: var(--login-muted); cursor: pointer; }
.gateway-login[data-login-theme="dark"] .gateway-password-field { background: #10192760; }
.gateway-login-submit { display: flex; justify-content: center; align-items: center; gap: 10px; position: relative; width: 100%; min-height: 49px; border: 0; border-radius: 10px; margin-top: 20px; color: #fff; background: linear-gradient(110deg, #4c91fc, #2457f4); box-shadow: 0 5px 12px #285df624, inset 0 1px 0 #ffffff35; font-size: 14px; font-weight: 650; cursor: pointer; transition: filter .2s, transform .2s; }
.gateway-login-submit > svg { position: absolute; right: 19px; }
.gateway-login-submit:hover:not(:disabled) { filter: brightness(1.08); transform: translateY(-1px); }
.gateway-login-submit:disabled { opacity: .65; cursor: not-allowed; }
.gateway-session-note { display: flex; justify-content: center; align-items: center; gap: 6px; color: var(--login-muted); font-size: 11px; margin-top: 15px; }
.gateway-login-security { display: flex; align-items: flex-start; gap: 9px; border-top: 1px solid var(--login-line); margin-top: 27px; padding-top: 21px; color: var(--login-muted); font-size: 12px; line-height: 1.8; }
.gateway-login-security svg { flex-shrink: 0; margin-top: 2px; }
.gateway-login-version { display: flex; flex-wrap: wrap; justify-content: center; gap: 8px 14px; padding: 20px 14px 0; font-size: 11px; color: var(--login-muted); overflow-wrap: anywhere; }
.gateway-login-version span { min-width: 0; }
.gateway-login-footer { display: flex; align-items: center; gap: 16px; color: var(--login-muted); font-size: 12px; }
.gateway-footer-security { display: flex; align-items: center; gap: 9px; }
.gateway-footer-security > svg { flex-shrink: 0; }
.gateway-login-error { display: flex; align-items: flex-start; gap: 7px; color: var(--login-error); font-size: 12px; line-height: 1.7; margin: 10px 0 0; overflow-wrap: anywhere; }
.gateway-login-error svg { flex-shrink: 0; margin-top: 2px; }
.gateway-login-spinner { width: 15px; height: 15px; border: 2px solid #ffffff55; border-top-color: #fff; border-radius: 50%; animation: gateway-spin .8s linear infinite; }
.gateway-login button:focus-visible { outline: 3px solid #6a96fc; outline-offset: 4px; }

@keyframes gateway-spin { to { transform: rotate(360deg); } }
@keyframes gateway-engine-float { 0%, 100% { transform: translateY(0); } 50% { transform: translateY(-6px); } }
@keyframes gateway-rail-pulse { from { stroke-dashoffset: 100; } to { stroke-dashoffset: 0; } }
@keyframes gateway-satellite-float { 0%, 100% { transform: translate(0, 0); opacity: .8; } 50% { transform: translate(1px, -4px); opacity: 1; } }
@media (prefers-reduced-motion: no-preference) and (min-width: 761px) {
  .gateway-engine { animation: gateway-engine-float 6s ease-in-out infinite; }
  .gateway-rail-flow { animation: gateway-rail-pulse 6s linear infinite; }
  .gateway-rail-flow:nth-of-type(3) { animation-delay: -2s; }
  .gateway-rail-flow:nth-of-type(4) { animation-delay: -4s; }
  .gateway-satellite { animation: gateway-satellite-float 7s ease-in-out infinite; }
  .gateway-satellite-right { animation-delay: -3.5s; }
}
@media (min-width: 1200px) {
  .gateway-login-form-side { transform: translateX(clamp(-96px, calc(144px - 12vw), 0px)); }
  .gateway-login-art { transform: translateX(clamp(-42px, calc(60px - 5vw), 0px)); }
}
@media (min-width: 1600px) {
  .gateway-login { --feature-gap: 32px; }
  .gateway-login-content { gap: 25px; }
  .gateway-login-features b { font-size: 15px; }
  .gateway-login-features small { font-size: 12px; }
  .gateway-feature-icon { width: 49px; height: 51px; }
  .gateway-login-card { padding: 44px 40px 35px; }
  .gateway-login-card h2 { font-size: 31px; }
}
@media (max-width: 1190px) {
  .gateway-login { --feature-gap: 17px; padding: 28px 4vw; }
  .gateway-login-content { grid-template-columns: minmax(330px, 1fr) minmax(340px, .85fr); gap: 45px; margin-top: 55px; margin-bottom: 55px; }
  .gateway-login-art { position: absolute; width: 470px; max-width: none; left: 23%; top: 21%; opacity: .35; margin: 0; z-index: -1; }
  .gateway-login-intro h1 { font-size: clamp(31px, 3.6vw, 44px); }
  .gateway-login-features > div { gap: 7px; }
  .gateway-feature-icon { width: 35px; height: 40px; }
  .gateway-login-features b { max-width: 92px; }
  .gateway-login-card { padding: 31px 28px 27px; }
}
@media (max-width: 760px) {
  .gateway-login { padding: 23px 22px; min-height: 100svh; }
  .gateway-login-header { gap: 15px; }
  .gateway-login-brand { font-size: 19px; gap: 10px; }
  .gateway-login-mark { --brand-logo-size: 37px; }
  .gateway-theme-control { gap: 6px; }
  .gateway-theme-control > svg { width: 16px; }
  .gateway-login-content { display: flex; flex-direction: column; gap: 28px; margin: 42px auto 25px; max-width: 450px; justify-content: center; }
  .gateway-login-intro { align-self: stretch; padding: 0; }
  .gateway-login-eyebrow { display: none; }
  .gateway-login-intro h1 { font-size: 30px; line-height: 1.4; letter-spacing: -1px; }
  .gateway-login-intro h1 br { display: none; }
  .gateway-login-intro h1 strong { display: block; }
  .gateway-login-description { font-size: 13px; margin: 12px 0 0; line-height: 1.8; }
  .gateway-login-description br { display: none; }
  .gateway-login-features { display: none; }
  .gateway-login-art { width: 360px; top: 40px; left: auto; right: -140px; opacity: .26; }
  .gateway-login-form-side { max-width: none; }
  .gateway-login-card { padding: 27px 25px 25px; border-radius: 20px; }
  .gateway-card-kicker { display: none; }
  .gateway-login-card h2 { font-size: 24px; }
  .gateway-card-name { font-size: 17px; }
  .gateway-card-description { margin: 13px 0 22px; }
  .gateway-login-version { padding-top: 17px; }
  .gateway-login-footer { justify-content: center; font-size: 11px; margin-top: auto; }
}
@media (prefers-reduced-motion: reduce) {
  .gateway-login *, .gateway-login *::before, .gateway-login *::after { animation: none !important; transition: none !important; }
}
</style>
