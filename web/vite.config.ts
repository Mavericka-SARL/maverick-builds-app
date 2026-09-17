import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [tailwindcss(), react()],
  server: {
    proxy: {
      '/api': {
        // A second gateway on another port (a real-auth check next to the
        // dev one, say) is reached with API_PROXY=http://localhost:8090.
        target: process.env.API_PROXY ?? 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
