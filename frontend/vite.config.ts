import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 前端由 native 宿主 WebView2 加载，资源由 Go 经本机 HTTP 提供（ADR-013）。
// 生产环境下页面与 API **同源**（都由 Go 提供），所以代码里一律用相对路径，
// 不硬编码端口——硬编码会在开发期造成跨域、在换端口时静默失效。
//
// 开发期：Vite 起在 5273，/api 反向代理到 Go，前端代码完全不用改。
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5273,
    strictPort: true,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8791',
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    // 桌面应用不需要 sourcemap：它会让 dist 体积增加约 7 倍，
    // 而这些文件只在本机回环上由 Go 提供给 WebView2，不会外发。
    sourcemap: false,
  },
})
