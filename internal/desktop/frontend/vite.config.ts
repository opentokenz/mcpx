import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  // Wails 的 AssetServer 从根路径提供资源，用相对路径可以避免打包后
  // 资源引用被解析成绝对路径而 404。
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // 产物要提交进仓库，关掉 sourcemap 让 diff 小一些。
    sourcemap: false,
  },
})
