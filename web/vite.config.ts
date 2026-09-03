import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import { VitePWA } from 'vite-plugin-pwa'

export default defineConfig({
  plugins: [
    tanstackRouter({ target: 'react', autoCodeSplitting: true }),
    react(),
    tailwindcss(),
    VitePWA({
      registerType: 'autoUpdate',
      injectRegister: 'auto',
      workbox: {
        globPatterns: ['**/*.{js,css,html,svg,woff2}'],
        // Session recordings and audit data are privileged — never cache them.
        navigateFallbackDenylist: [/^\/api\//],
        runtimeCaching: [
          {
            urlPattern: ({ url }) => url.pathname.startsWith('/api/'),
            handler: 'NetworkOnly',
          },
        ],
      },
      manifest: {
        name: 'Argus — Privileged Access Management',
        short_name: 'Argus',
        description: 'Brokered, recorded and audited privileged access to Linux infrastructure.',
        theme_color: '#0a0e14',
        background_color: '#0a0e14',
        display: 'standalone',
        start_url: '/',
        icons: [
          { src: '/icon.svg', sizes: 'any', type: 'image/svg+xml', purpose: 'any maskable' },
        ],
      },
    }),
  ],
  resolve: {
    alias: { '~': new URL('./src', import.meta.url).pathname },
  },
  worker: { format: 'es' },
  server: {
    port: 5273,
    // Proxy the control plane so the console and the API share an origin.
    //
    // This is not a dev convenience: a session cookie with SameSite=Lax is not
    // sent on cross-site fetches, so a split-origin console cannot stay signed
    // in. Production fronts both with one hostname; this mirrors that rather
    // than papering over it with SameSite=None, which would weaken the cookie
    // everywhere to fix a local-only problem.
    proxy: {
      // The control plane serves HTTPS whenever a certificate is configured,
      // which the dev config does. Proxying to http:// fails every request,
      // and because whoami() falls back silently the console then reports
      // "no identity provider configured" — which sends you to check Dex
      // instead of the one line that is actually wrong.
      //
      // secure:false accepts the dev certificate. It applies only to the dev
      // server's own proxy, never to anything shipped.
      '/api': { target: 'https://127.0.0.1:8080', changeOrigin: false, secure: false },
      '/auth': { target: 'https://127.0.0.1:8080', changeOrigin: false, secure: false },
    },
  },
})
