<script setup lang="ts">
import { ref } from 'vue'
import type { GatewayState, Profile } from './api-types'
import ProfileSettings from './components/ProfileSettings.vue'
import Icon from './Icon.vue'
import SecuritySettings from './components/SecuritySettings.vue'
import BackupSettings from './components/BackupSettings.vue'
import NotificationSettings from './components/NotificationSettings.vue'

defineProps<{ state: GatewayState; busy?: string }>()
const emit = defineEmits<{ busy: [value: string]; state: [value: GatewayState]; profile: [value: Profile]; notice: [value: string]; 'session-lost': [message: string] }>()
const tab = ref('overview')
const tabs = [
  { id: 'profile', label: '配置资料', icon: 'pen', description: '自定义项目名称、本地客户端名称和全局 Logo。' },
  { id: 'notifications', label: '通知渠道', icon: 'cloud', description: '管理接收端，查看最近投递结果。' },
  { id: 'backups', label: '备份与历史', icon: 'database', description: '下载加密备份，预览历史草稿差异。' },
  { id: 'security', label: '安全与会话', icon: 'shield', description: '查看登录设备，管理密码与会话。' },
]
</script>

<template>
  <section v-if="tab === 'overview'" class="system-intro">
    <div><span class="eyebrow">系统管理</span><h2>按需管理，不打断运行</h2><p>选择一项任务开始。敏感操作将在独立窗口中确认，不会因打开页面而执行。</p></div>
    <dl class="system-facts"><div><dt>当前草稿</dt><dd>v{{ state.revision }}</dd></div><div><dt>已应用版本</dt><dd>{{ state.appliedRevision ? `v${state.appliedRevision}` : '尚未应用' }}</dd></div></dl>
  </section>
  <nav class="system-tasks" :class="{ compact: tab !== 'overview' }" aria-label="系统管理任务">
    <button v-for="item in tabs" :key="item.id" type="button" class="system-task" :aria-pressed="tab === item.id" :disabled="Boolean(busy)" @click="tab = item.id">
      <span class="task-icon"><Icon :name="item.icon" /></span><span><strong>{{ item.label }}</strong><small v-if="tab === 'overview'">{{ item.description }}</small></span><Icon v-if="tab === 'overview'" name="chevron" :size="17" />
    </button>
  </nav>
  <div v-if="tab !== 'overview'" class="system-breadcrumb"><button type="button" class="text-button" :disabled="Boolean(busy)" @click="tab = 'overview'">返回系统概要</button><span>资料修改立即生效，敏感操作会再次确认。</span></div>
  <ProfileSettings v-if="tab === 'profile'" :busy="busy" @busy="emit('busy', $event)" @profile="emit('profile', $event)" @notice="emit('notice', $event)" />
  <NotificationSettings v-else-if="tab === 'notifications'" :busy="busy" @busy="emit('busy', $event)" @notice="emit('notice', $event)" />
  <BackupSettings v-else-if="tab === 'backups'" :state="state" :busy="busy" @busy="emit('busy', $event)" @state="emit('state', $event)" @notice="emit('notice', $event)" />
  <SecuritySettings v-else-if="tab === 'security'" :busy="busy" @busy="emit('busy', $event)" @notice="emit('notice', $event)" @session-lost="emit('session-lost', $event)" />
</template>

<style scoped>
.system-intro{display:flex;justify-content:space-between;align-items:center;gap:24px;padding:10px 0 26px}.system-intro h2{font-size:23px;margin:7px 0 9px}.system-intro p,.system-breadcrumb span{color:var(--muted);font-size:13px;line-height:1.7}.eyebrow{color:var(--blue);font-size:12px;font-weight:650}.system-facts{display:flex;gap:24px;flex-shrink:0}.system-facts dt{color:var(--muted);font-size:12px}.system-facts dd{font-size:21px;margin:7px 0 0;font-variant-numeric:tabular-nums}.system-tasks{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:14px;margin-bottom:18px}.system-task{display:flex;align-items:center;gap:14px;min-width:0;text-align:left;padding:24px 18px;border:1px solid var(--line);border-radius:var(--radius);color:var(--ink);background:var(--surface);cursor:pointer}.system-task>span:nth-child(2){flex:1}.system-task strong{font-size:14px}.system-task small{display:block;color:var(--muted);font-size:12px;line-height:1.7;margin-top:7px}.system-task:hover:not(:disabled),.system-task[aria-pressed=true]{border-color:var(--blue);background:var(--blue-bg)}.system-task:focus-visible{outline:2px solid var(--blue);outline-offset:3px}.system-task:disabled{opacity:.55;cursor:not-allowed}.task-icon{display:grid;place-items:center;width:38px;height:38px;border-radius:10px;color:var(--blue);background:var(--surface-2);flex-shrink:0}.compact{gap:8px;margin-bottom:12px}.compact .system-task{padding:12px 15px;gap:10px}.compact .task-icon{width:28px;height:28px}.system-breadcrumb{display:flex;align-items:center;gap:16px;margin-bottom:20px}@media(max-width:800px){.system-intro{align-items:flex-start;flex-direction:column}.system-tasks{grid-template-columns:1fr}.compact{grid-template-columns:repeat(4,minmax(0,1fr))}.compact .system-task{justify-content:center;padding:12px 5px}.compact .task-icon{display:none}.compact .system-task>span:nth-child(2){flex:0 1 auto}.system-breadcrumb span{display:none}}
</style>
