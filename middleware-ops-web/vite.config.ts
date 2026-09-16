import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Vite 配置：
//   - 开发态通过 /api 代理到后端（默认 http://127.0.0.1:8080），避免跨域；
//   - 生产幂等构建输出 dist/，由 Nginx 托管并反向代理 /api 与 SSE。
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    host: '0.0.0.0',
    port: 5173,
    proxy: {
      '/api': {
        target: process.env.VITE_API_TARGET || 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
      '/healthz': {
        target: process.env.VITE_API_TARGET || 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
    // 不使用 manualChunks 强制分包。
    //
    // 原因（曾导致线上运行时崩溃）：
    //   element-plus 与 dayjs 之间存在互相引用（dayjs 的 locale/plugin 体系由 element-plus
    //   的日期组件引入），把 element-plus 与 dayjs 拆进不同的手工 chunk 后，
    //   chunk 之间会形成循环依赖：element -> vendor(dayjs) -> element。
    //   ES module 遇到循环依赖时依赖 Rollup 的变量提升来打破环，而 element-plus 顶层
    //   存在「导入后立即在模块初始化期求值」的常量/数组，结果触发 TDZ：
    //     Uncaught ReferenceError: Cannot access 'Pt' before initialization
    //   表现为整站白屏（#app 为空）。
    //   交给 Rollup 自动分包时，它按真实依赖图保证求值顺序，不会产生该问题。
    chunkSizeWarningLimit: 2000,
    rollupOptions: {
      output: {
        // 仅对文件名做稳定分类，不干预 chunk 划分。
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  },
})
