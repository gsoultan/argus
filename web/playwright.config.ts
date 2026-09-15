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
    {
      // A second engine, because "works in Chrome" is not the claim. Safari
      // silently dropped the dev session cookie -- it was marked Secure and
      // served over plain HTTP -- and sign-in did nothing, with five green CI
      // jobs behind it, because everything here ran Chromium.
      //
      // The memory specs stay Chromium-only: performance.memory does not exist
      // in WebKit, so running them here would fail on the measurement rather
      // than on the thing being measured.
      name: 'webkit',
      testIgnore: /.*-memory\.spec\.ts/,
      use: { ...devices['Desktop Safari'] },
    },
  ],
  webServer: [
    {
      // Both bundles are built here, in one command, in sequence. Splitting
      // them across the two entries below ran two vite builds concurrently in
      // one project -- Playwright starts webServers in parallel -- and a
      // preview server handed out index.html while the hashed font assets it
      // referenced were still being written. Twenty-three tests failed on
      // missing fonts rather than on anything real.
      //
      // globalSetup is not the place for it either: Playwright starts its
      // webServers before globalSetup runs, so the preview would find no dist.
      command:
        'VITE_CONTROL_URL= bunx vite build --logLevel error'
        + ' && VITE_CONTROL_URL=/ bunx vite build --outDir dist-live --logLevel error'
        + ' && bunx vite preview --port 5510 --strictPort',
      url: 'http://localhost:5510',
      reuseExistingServer: !process.env.CI,
      timeout: 180_000,
    },
    {
      // Serves the control-plane build, so the sign-in screen exists at all --
      // with VITE_CONTROL_URL empty, LoginGate waves everything through and
      // there is nothing to sign in to. Starts immediately and answers 404
      // until the build above lands, which is what Playwright polls this url
      // for.
      command: 'node e2e/authstub.mjs',
      url: 'http://localhost:5511',
      reuseExistingServer: !process.env.CI,
      timeout: 180_000,
    },
  ],
})
