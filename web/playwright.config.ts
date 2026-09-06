import { defineConfig, devices } from '@playwright/test'

/**
 * Browser checks against the production build.
 *
 * Every "blazingly fast, low memory" result for this console was measured by
 * hand in a headless browser. That is a claim that decays: the next change to
 * the worker boundary or the terminal buffer undoes it silently, and nothing
 * short of a browser can tell. These run the built app, served the way it is
 * deployed, and assert the properties directly.
 *
 * Built against the in-memory fixture, which is what the route tests use and
 * the only mode with no control plane in the loop.
 */
export default defineConfig({
  testDir: './e2e',
  // Memory measurements must not share a renderer with another test.
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['github']] : 'list',
  timeout: 90_000,
  use: {
    baseURL: 'http://localhost:5510',
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        // performance.memory is quantised to defeat fingerprinting unless the
        // browser is told this is a measurement.
        launchOptions: { args: ['--enable-precise-memory-info'] },
      },
    },
  ],
  webServer: {
    command:
      'VITE_CONTROL_URL= bunx vite build --logLevel error && bunx vite preview --port 5510 --strictPort',
    url: 'http://localhost:5510',
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
  },
})
