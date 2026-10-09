import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { readFileSync } from 'node:fs'

const version = readFileSync(new URL('../internal/buildinfo/version.txt', import.meta.url), 'utf8').trim()

export default defineConfig({
  plugins: [vue()],
  define: { __APP_VERSION__: JSON.stringify(version) },
  server: { proxy: { '/api': 'http://127.0.0.1:18080' } },
  build: { outDir: 'dist', emptyOutDir: true },
})
