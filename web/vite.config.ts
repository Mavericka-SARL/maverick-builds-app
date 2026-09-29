import { createReadStream, existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { defineConfig, type Connect, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// The manuals the web image serves at /docs/ (web/Dockerfile copies them,
// web/nginx.conf serves them), read here straight from the repo's docs/ so a
// link in the console works under `npm run dev` too. The allowlist mirrors the
// image's paths, though the image copies all of img/ and this admits only .png
// (every picture there today), so another image type needs adding here. It
// keeps a request inside docs/: [\w-]+ admits neither "." nor "/". Anything
// else under /docs/ is a 404, as in nginx, rather than the console's index.html.
const BUNDLED_DOC = /^\/docs\/((?:formulas|developer)-manual\/manual\.html|developer-manual\/img\/[\w-]+\.png)$/

function bundledDocs(): Plugin {
  const root = fileURLToPath(new URL('../docs/', import.meta.url))
  const serve: Connect.NextHandleFunction = (req, res, next) => {
    const path = (req.url ?? '').split(/[?#]/)[0]
    if (!path.startsWith('/docs/')) return next()
    const m = BUNDLED_DOC.exec(path)
    const file = m ? root + m[1] : null
    if (!file || !existsSync(file)) {
      res.statusCode = 404
      res.end()
      return
    }
    res.setHeader('Content-Type', file.endsWith('.png') ? 'image/png' : 'text/html; charset=utf-8')
    res.setHeader('Cache-Control', 'no-cache')
    createReadStream(file).on('error', next).pipe(res)
  }
  return {
    name: 'bundled-docs',
    configureServer: (server) => { server.middlewares.use(serve) },
    configurePreviewServer: (server) => { server.middlewares.use(serve) },
  }
}

export default defineConfig({
  plugins: [tailwindcss(), react(), bundledDocs()],
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
