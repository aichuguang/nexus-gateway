<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { defaultLogo } from '../branding'

const props = withDefaults(defineProps<{ src?: string; size?: number }>(), { src: '', size: 34 })
const failed = ref(false)
const custom = computed(() => Boolean(props.src && !failed.value))
const source = computed(() => custom.value ? props.src : defaultLogo)
watch(() => props.src, () => { failed.value = false })
</script>

<template>
  <span class="brand-logo" :class="{ 'brand-logo-custom': custom }" :style="{ '--logo-size': `${size}px` }" aria-hidden="true">
    <img :key="source" :src="source" alt="" decoding="async" @error="failed = true" />
  </span>
</template>

<style scoped>
.brand-logo {
  display: inline-grid;
  place-items: center;
  flex-shrink: 0;
  width: var(--brand-logo-size, var(--logo-size));
  height: var(--brand-logo-size, var(--logo-size));
  border-radius: 29.4%;
  vertical-align: middle;
  box-shadow: 0 2px 4px #172b4310;
}
.brand-logo img { display: block; width: 100%; height: 100%; object-fit: contain; border-radius: inherit; }
.brand-logo-custom { border-radius: 0; box-shadow: none; }
</style>
