/**
 * Serves the reference build for cascade.spec.ts.
 *
 * This build is produced by the webServer entry above it, and `vite preview`
 * exits immediately if its outDir does not exist — so wait for the build rather
 * than racing it.
 *
 * Playwright starts webServer entries in order, waiting for each url before
 * launching the next, so in practice the build has already finished by the time
 * this runs and the loop below falls straight through. The wait is here for the
 * case where that stops being true: it costs nothing and it is the difference
 * between a clear failure and a confusing one.
 *
 * Staleness is handled upstream — the entry above removes dist-monolith before
 * it builds, so a marker that exists belongs to this run. Two earlier attempts
 * tried to establish that here instead, by requiring the marker to be newer
 * than this process and then by watching it disappear and return. Both deadlock
 * under sequential startup, because the build is already complete and final
 * before this file is executed.
 */
import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'

const ROOT = new URL('..', import.meta.url).pathname
const MARKER = new URL('../dist-monolith/.build-complete', import.meta.url).pathname
const DEADLINE = Date.now() + 150_000

/** @param {number} ms */
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

while (!existsSync(MARKER)) {
  if (Date.now() > DEADLINE) {
    // Exiting beats hanging until Playwright's own timeout, which reports only
    // that *a* webServer did not come up.
    console.error(
      'preview-monolith: dist-monolith/.build-complete never appeared.\n'
        + 'The reference build is produced by the webServer entry above this one '
        + 'in playwright.config.ts — check that it ran.',
    )
    process.exit(1)
  }
  await sleep(200)
}

spawn(
  'bunx',
  ['vite', 'preview', '--outDir', 'dist-monolith', '--port', '5512', '--strictPort'],
  { stdio: 'inherit', cwd: ROOT },
).on('exit', (code) => process.exit(code ?? 0))
