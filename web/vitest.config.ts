import { defineConfig } from 'vitest/config'

/**
 * Separate from vite.config.ts on purpose: the PWA plugin generates a service
 * worker on every config load, which is pure cost in a test run and writes into
 * dist/ as a side effect of running unit tests.
 */
export default defineConfig({
  resolve: {
    alias: { '~': new URL('./src', import.meta.url).pathname },
  },
  // The RDP pixel decoder is a .wasm file, which vite.config.ts never needs to
  // name -- the build emits it as an asset by its URL. Its test imports it
  // ?inline, which needs telling that .wasm is an asset and not a module.
  assetsInclude: ['**/*.wasm'],
  test: {
    environment: 'jsdom',
    // The console is gated behind LoginGate whenever a control plane is
    // configured, and .env.local configures one. Without this every route test
    // renders "Sign in required" instead of the page — which is the console
    // behaving correctly, and useless as a test. Empty means the in-memory
    // fixture, which is the state these tests are written against.
    env: { VITE_CONTROL_URL: '' },
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    globals: true,
    // See asyncUtilTimeout in src/test/setup.ts.
    testTimeout: 15_000,
  },
})
