import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

/*
 * The secrets screens, built on their own into /mf/secrets/<contract>/index.js
 * beside the console (src/mf/contract.ts). React, Mantine, TanStack Query and
 * the console's page shapes are pointed at the console's own copies through
 * src/mf/shared, so the bundle carries only the screens and their icons.
 *
 *   MF_OUT=dist bunx vite build -c vite.mf.config.ts
 */
const here = (p: string) => new URL(p, import.meta.url).pathname
const CONTRACT = 1

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: [
      { find: /^react$/, replacement: here('./src/mf/shared/react.ts') },
      { find: /^react\/jsx-runtime$/, replacement: here('./src/mf/shared/jsx-runtime.ts') },
      { find: /^react\/jsx-dev-runtime$/, replacement: here('./src/mf/shared/jsx-runtime.ts') },
      { find: /^@mantine\/core$/, replacement: here('./src/mf/shared/mantine.ts') },
      { find: /^@tanstack\/react-query$/, replacement: here('./src/mf/shared/query.ts') },
      { find: '@argus/shell', replacement: here('./src/mf/shared/shell.ts') },
      { find: '~', replacement: here('./src') },
    ],
  },
  build: {
    outDir: `${process.env.MF_OUT ?? 'dist'}/mf/secrets/${CONTRACT}`,
    emptyOutDir: true,
    copyPublicDir: false,
    lib: { entry: here('./src/mf-secrets/index.tsx'), formats: ['es'], fileName: () => 'index.js' },
  },
})
