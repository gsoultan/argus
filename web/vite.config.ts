import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import { VitePWA } from 'vite-plugin-pwa'

// `process` is declared once for the whole project in e2e/node-globals.d.ts.

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
        // Fonts ship as per-script subsets, and only the plain Latin ones are
        // precached. The rest exist for names this console may never render,
        // and pulling them up front costs a quarter of a megabyte to no effect.
        // They are still cached the moment a name needs one — the CacheFirst
        // rule below does that.
        //
        // `*latin*` used to be the pattern, which also matched `latin-ext` and
        // pushed 98 kB of accented-Latin coverage at every first visit. Two
        // font files are fetched to render this console and four were being
        // stored; e2e/weight.spec.ts asserts the two, precache.spec.ts the
        // four-now-two. An accented name still renders, it just fetches its
        // subset on demand like Cyrillic and Greek always have.
        //
        // No '**/*.svg' here: the only SVG in the build is icon.svg, and the
        // manifest already precaches it as the app icon. Listing both put it in
        // the manifest twice, with the same revision.
        globPatterns: [
          'index.html',
          'assets/index-*.js',
          'assets/*.css',
          'assets/*-latin-wght-*.woff2',
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
    // Array form, because order decides which entry wins and the monolith
    // override has to be matched before the general '~' prefix.
    alias: [
      // The reference build for e2e/cascade.spec.ts: identical application,
      // Mantine's concatenated stylesheet instead of the ~47 per-component
      // imports whose order app.css maintains by hand. Never set in a shipped
      // build — see src/app.monolith.css.
      ...(process.env.ARGUS_CSS === 'monolith'
        ? [{
            find: '~/app.css',
            replacement: new URL('./src/app.monolith.css', import.meta.url).pathname,
          }]
        : []),
      { find: '~', replacement: new URL('./src', import.meta.url).pathname },
    ],
  },
  worker: { format: 'es' },
  server: {
    port: 5290,
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
      // and because whoami() falls back silently the console then reports the
      // deployment as unreachable — which sends you hunting for a dead control
      // plane instead of the one line here that is actually wrong.
      //
      // secure:false accepts the dev certificate. It applies only to the dev
      // server's own proxy, never to anything shipped.
      '/api': { target: 'https://127.0.0.1:8480', changeOrigin: false, secure: false },
      '/auth': { target: 'https://127.0.0.1:8480', changeOrigin: false, secure: false },
    },
  },
})
