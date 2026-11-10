import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 開發與建置共用 Vue 外掛，讓 Vite 編譯單檔元件。
export default defineConfig({
  plugins: [vue()],
})
