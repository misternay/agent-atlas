import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// The dashboard is served by atlasd (embedded from web/dist). For `pnpm dev`,
// API and WebSocket calls are proxied to a running atlasd.
const target = process.env.ATLAS_ADDR ?? 'http://127.0.0.1:7878'

export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true, sourcemap: false, chunkSizeWarningLimit: 2000 },
  server: {
    proxy: {
      '/api': { target, changeOrigin: false, ws: true },
      '/auth': { target },
    },
  },
  test: { globals: true, environment: 'node' },
})
