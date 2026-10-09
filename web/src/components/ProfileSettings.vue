<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import type { Profile } from '../api-types'
import { usePageScope } from '../page-scope'
import { errorDetails, isRequestCancelled } from '../request'
import { profileLogoLimits, readProfileLogo } from '../profile-logo'
import Icon from '../Icon.vue'
import BrandLogo from './BrandLogo.vue'

const copy = {
  title: '配置资料',
  refresh: '刷新',
  description: '统一设置登录页和管理工作区的 Logo 与显示名称。保存后立即生效，不需要发布，也不会修改系统主机名、网络配置或服务数据。',
  loading: '正在读取配置资料…',
  logo: '全局 Logo',
  logoHelp: '登录页与后台统一使用此图标。支持 PNG、JPEG、GIF，文件不超过 256 KiB，尺寸不超过 2048 × 2048 像素。',
  customLogo: '当前使用自定义 Logo',
  defaultLogo: '当前使用默认 Logo',
  chooseLogo: '选择图片',
  replaceLogo: '更换图片',
  resetLogo: '恢复默认',
  readingLogo: '正在读取图片…',
  projectName: '项目名称',
  projectHelp: '用于登录页、侧栏和浏览器标题，最多 60 个字符。',
  clientName: '本地客户端名称',
  clientHelp: '用于标识当前网关客户端，最多 60 个字符。不改变设备的系统主机名。',
  publicHelp: '名称和 Logo 会展示在登录页，请勿填写密码、密钥等敏感信息，也不要上传含敏感信息的图片。',
  invalidNames: '两个名称都不能为空，最多 60 个字符，且不能包含控制字符。',
  saving: '正在保存…',
  save: '保存资料',
  reset: '撤销编辑',
  dirty: '有未保存的修改',
  saved: '配置资料已保存，名称和 Logo 已立即更新',
  unknownOutcome: ' 保存结果尚未确认，可以重试保存相同资料，或撤销编辑后刷新核对。',
} as const

const props = defineProps<{ busy?: string }>()
const emitEvent = defineEmits<{ busy: [value: string]; notice: [value: string]; profile: [value: Profile] }>()
const scope = usePageScope(), api = scope.api, emit = scope.guard(emitEvent)
const form = reactive<Profile>({ projectName: '', clientName: '', logo: '' })
const saved = ref<Profile | null>(null), loading = ref(false), working = ref(false), readingLogo = ref(false), error = ref('')
const logoInput = ref<HTMLInputElement | null>(null)
const locked = computed(() => Boolean(props.busy || loading.value || working.value || readingLogo.value))
const dirty = computed(() => Boolean(saved.value && (form.projectName !== saved.value.projectName || form.clientName !== saved.value.clientName || (form.logo || '') !== (saved.value.logo || ''))))
const valid = computed(() => [form.projectName, form.clientName].every(value => value.trim().length > 0 && Array.from(value.trim()).length <= 60 && !/[\p{Cc}\p{Cf}\u2028\u2029]/u.test(value)))

function applyProfile(value: Profile) {
  saved.value = { ...value, logo: value.logo || '' }; Object.assign(form, saved.value)
}
async function load() {
  // A background read must never replace the user's unsaved branding.
  if (locked.value || dirty.value) return
  loading.value = true; error.value = ''
  try {
    const value = await api.profile()
    applyProfile(value)
  } catch (err) { if (!isRequestCancelled(err)) error.value = errorDetails(err).message }
  finally { loading.value = false }
}
function reset() {
  if (locked.value || !saved.value) return
  Object.assign(form, saved.value); error.value = ''
}
function chooseLogo() {
  if (!locked.value && saved.value) logoInput.value?.click()
}
function resetLogo() {
  if (locked.value || !saved.value) return
  form.logo = ''; error.value = ''
}
async function selectLogo(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file || locked.value || !saved.value) return
  readingLogo.value = true; error.value = ''
  try {
    const source = await readProfileLogo(file)
    if (scope.isActive()) form.logo = source
  } catch (err) { if (scope.isActive()) error.value = errorDetails(err).message }
  finally { readingLogo.value = false }
}
async function save() {
  if (locked.value || !saved.value || !dirty.value || !valid.value) return
  working.value = true; error.value = ''; emit('busy', 'profile-save')
  try {
    const value = await api.updateProfile({ projectName: form.projectName.trim(), clientName: form.clientName.trim(), logo: form.logo || '' })
    applyProfile(value)
    emit('profile', value); emit('notice', copy.saved)
  } catch (err) {
    if (!isRequestCancelled(err)) {
      const details = errorDetails(err)
      error.value = details.message + (details.outcomeUnknown ? copy.unknownOutcome : '')
    }
  } finally { working.value = false; emit('busy', '') }
}
onMounted(load)
</script>

<template>
  <section class="card profile-settings">
    <div class="card-head"><div class="card-title"><Icon name="pen" /><h2>{{ copy.title }}</h2></div><button type="button" class="button secondary small" :disabled="locked || dirty" @click="load"><Icon name="refresh" :size="15" />{{ copy.refresh }}</button></div>
    <div class="card-body">
      <p class="profile-description">{{ copy.description }}</p>
      <div v-if="error" class="alert error" role="alert">{{ error }}</div>
      <div v-if="loading && !saved" class="compact-empty" role="status"><span class="spinner"></span>{{ copy.loading }}</div>
      <form v-if="saved" class="profile-form" @submit.prevent="save">
        <div class="form-field">
          <label class="form-label" for="profile-logo-file">{{ copy.logo }}</label>
          <div class="profile-logo-editor">
            <BrandLogo :src="form.logo" :size="56" />
            <div class="profile-logo-content">
              <span class="profile-logo-status" role="status">{{ readingLogo ? copy.readingLogo : form.logo ? copy.customLogo : copy.defaultLogo }}</span>
              <div class="profile-logo-actions">
                <input id="profile-logo-file" ref="logoInput" type="file" class="profile-logo-input" :accept="profileLogoLimits.accept" :disabled="locked" tabindex="-1" aria-hidden="true" @change="selectLogo" />
                <button type="button" class="button secondary small" :disabled="locked" aria-describedby="profile-logo-help" @click="chooseLogo"><Icon name="upload" :size="15" />{{ form.logo ? copy.replaceLogo : copy.chooseLogo }}</button>
                <button type="button" class="button secondary small" :disabled="locked || !form.logo" @click="resetLogo">{{ copy.resetLogo }}</button>
              </div>
            </div>
          </div>
          <span id="profile-logo-help" class="field-caption">{{ copy.logoHelp }}</span>
        </div>
        <div class="form-field"><label class="form-label" for="profile-project-name">{{ copy.projectName }}</label><input id="profile-project-name" v-model="form.projectName" maxlength="60" required :disabled="locked" autocomplete="off" aria-describedby="profile-project-help" /><span id="profile-project-help" class="field-caption">{{ copy.projectHelp }}</span></div>
        <div class="form-field"><label class="form-label" for="profile-client-name">{{ copy.clientName }}</label><input id="profile-client-name" v-model="form.clientName" maxlength="60" required :disabled="locked" autocomplete="off" aria-describedby="profile-client-help" /><span id="profile-client-help" class="field-caption">{{ copy.clientHelp }}</span></div>
        <p class="field-caption">{{ copy.publicHelp }}</p>
        <p v-if="dirty && !valid" class="error-text" role="alert">{{ copy.invalidNames }}</p>
        <div class="profile-actions"><button type="submit" class="button primary" :disabled="locked || !dirty || !valid">{{ working ? copy.saving : copy.save }}</button><button type="button" class="button secondary" :disabled="locked || !dirty" @click="reset">{{ copy.reset }}</button><span v-if="dirty" class="field-caption" role="status">{{ copy.dirty }}</span></div>
      </form>
    </div>
  </section>
</template>

<style scoped>
.profile-description{margin:0 0 24px;color:var(--muted);font-size:13px;line-height:1.8;max-width:760px}.profile-form{max-width:640px;display:grid;gap:20px}.profile-form .form-field{margin:0}.profile-actions{display:flex;align-items:center;flex-wrap:wrap;gap:12px;padding-top:8px}.profile-form p{margin:0}.profile-settings .alert{margin-bottom:18px}
.profile-logo-editor{display:flex;align-items:center;gap:18px;padding:18px;border:1px solid var(--line);border-radius:10px;background:var(--surface-2)}.profile-logo-content{min-width:0;display:grid;gap:10px}.profile-logo-status{font-size:13px;color:var(--ink)}.profile-logo-actions{display:flex;flex-wrap:wrap;gap:8px}.profile-logo-input{display:none!important}
@media(max-width:480px){.profile-logo-editor{gap:12px;padding:14px}.profile-logo-actions .button{padding-inline:10px}}
</style>
