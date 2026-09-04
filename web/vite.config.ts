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
      // 'prompt', not 'autoUpdate': this console is used while watching a live
      // session. Swapping the running app out from under someone mid-stream
      // drops their socket with no explanation. Ask instead — see PWAUpdate.
      registerType: 'prompt',
      injectRegister: 'auto',
      workbox: {
        // Only the shell is precached. Route chunks are fetched on demand and
        // kept by runtimeCaching below; precaching all of them meant a first
        // visit downloaded xterm.js and every route the user never opened.
        //
        // Fonts ship as per-script subsets, so only the Latin ones are listed:
        // the rest exist for names this console may never render, and pulling
        // Cyrillic, Greek and Vietnamese up front costs a quarter of a megabyte
        // to no effect. They are still cached if a name ever needs them.
        globPatterns: [
          'index.html',
          'assets/index-*.js',
          'assets/*.css',
          'assets/*latin*.woff2',
          '**/*.svg',
        ],
        // Anything the control plane serves must reach the control plane.
        //
        // /auth/ is load-bearing and was missing: loginURL() navigates to
        // /auth/login, which is same-origin, so the navigation fallback served
        // cached index.html instead. Sign-in silently looped forever — and only
        // once the worker had installed, so the first visit always worked and
        // every visit after it did not.
        navigateFallbackDenylist: [/^\/api\//, /^\/auth\//],
        runtimeCaching: [
          // Session recordings and audit data are privileged — never cache them.
          {
            urlPattern: ({ url }) => url.pathname.startsWith('/api/'),
            handler: 'NetworkOnly',
          },
          {
            urlPattern: ({ url }) => url.pathname.startsWith('/auth/'),
            handler: 'NetworkOnly',
          },
          // Fonts are immutable and content-hashed; a subset fetched once for
          // one operator's name should not be fetched again.
          {
            urlPattern: ({ url }) => url.pathname.endsWith('.woff2'),
            handler: 'CacheFirst',
            options: {
              cacheName: 'argus-fonts',
              expiration: { maxEntries: 16, maxAgeSeconds: 60 * 60 * 24 * 365 },
            },
          },
          // Route chunks are content-hashed, so a cache hit is always correct.
          {
            urlPattern: ({ url }) => url.pathname.startsWith('/assets/'),
            handler: 'StaleWhileRevalidate',
            options: { cacheName: 'argus-assets' },
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
