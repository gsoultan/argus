import { expect, type Page } from '@playwright/test'

const HAZARD =
  'A service worker is intercepting this page\'s requests, so failed assets are not '
  + 'reported to page-level listeners and "no failed asset" proves nothing. Use one '
  + 'navigation in a fresh context, or test.use({ serviceWorkers: "block" }).'

/** Attaches error collectors before navigation; call `assertClean` after. */
export function watchErrors(page: Page) {
  const errors: string[] = []
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(`console: ${m.text()}`)
  })
  page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`))
  page.on('requestfailed', (r) => {
    // The service worker's own probes and aborted navigations are noise;
    // a failed asset is not.
    if (/\.(js|css|woff2)(\?|$)/.test(r.url())) errors.push(`asset failed: ${r.url()}`)
  })

  // Document loads, not SPA transitions: pushState does not fire this, a reload
  // does. One is the condition under which the listeners above see everything.
  let loads = 0
  page.on('load', () => { loads += 1 })
  return {
    /**
     * Also asserts that no service worker took control.
     *
     * `page.on('requestfailed')` does not see requests a service worker makes
     * on the page's behalf, so once one is controlling, "no failed asset" is a
     * statement about nothing: an aborted font produced zero page-level events,
     * measured. What keeps these tests honest is that each runs in a fresh
     * context with a single navigation, and vite-plugin-pwa is configured
     * `registerType: 'prompt'` — no skipWaiting — so the worker registers but
     * does not claim the page until the next one.
     *
     * That is a real property and it is invisible, which is the kind that stops
     * being true without anyone noticing. Adding a reload to a test using this
     * helper would silently weaken it to nothing; now it fails instead.
     */
    assertClean: async () => {
      const sw = await page.evaluate(async () => ({
        controlling: Boolean(navigator.serviceWorker?.controller),
        registered: (await navigator.serviceWorker?.getRegistrations?.())?.length ?? 0,
      }))

      // A worker that is already controlling is a definite problem: the
      // listeners above are no longer being told about the page's requests.
      expect(sw.controlling, HAZARD).toBe(false)

      // And a second document load with one registered is a problem whether or
      // not it won the race to claim the page on this particular run — which is
      // why the count is checked rather than only the outcome. Both are skipped
      // when the context blocks service workers, because then nothing can
      // intercept and any number of loads is fine.
      if (sw.registered > 0) expect(loads, `${HAZARD} (loaded ${loads} times)`).toBeLessThanOrEqual(1)

      expect(errors, errors.join('\n')).toEqual([])
    },
  }
}

/**
 * Serves a generated recording to the app, matched on pathname.
 *
 * Deliberately not a glob: a `**` wildcard followed by `/e2e.cast` also matches
 * the page's own URL, because the recording is passed as `?cast=/e2e.cast` and
 * the query string ends with it — so Playwright fulfils the navigation with the
 * browser renders an 8 MB asciicast as plain text. A service worker masked that
 * for as long as one was serving navigations, which is why it survived.
 *
 * Route anything by pathname when the same path can appear in a query string.
 */
export async function serveFile(
  page: Page,
  pathname: string,
  body: string | Buffer,
  contentType: string,
): Promise<void> {
  await page.route(
    (url) => url.pathname === pathname,
    (route) => route.fulfill({ status: 200, contentType, body }),
  )
}

export async function heapMB(page: Page): Promise<number> {
  return page.evaluate(() => {
    const m = (performance as unknown as { memory?: { usedJSHeapSize: number } }).memory
    if (!m) throw new Error('performance.memory unavailable; is this Chromium with --enable-precise-memory-info?')
    return Math.round(m.usedJSHeapSize / 1048576)
  })
}

/** First session link on the sessions page, as an operator would click it. */
export async function firstSessionPath(page: Page, filter?: RegExp): Promise<string> {
  await page.goto('/sessions')
  const rows = page.locator('tbody tr')
  await expect(rows.first()).toBeVisible()
  const n = await rows.count()
  for (let i = 0; i < n; i++) {
    const row = rows.nth(i)
    if (filter && !filter.test(await row.innerText())) continue
    const href = await row.locator('a[href^="/sessions/"]').first().getAttribute('href')
    if (href) return href
  }
  throw new Error('no session row matched')
}

/** Builds an asciicast v2 of roughly `bytes` of output with per-key typing. */
export function makeCast(bytes: number): string {
  const ESC = String.fromCharCode(27)
  const lines = [JSON.stringify({ version: 2, width: 120, height: 34 })]
  let t = 0
  let out = 0
  const cmds = ['tail -f /var/log/syslog', 'journalctl -u api -n 200', 'dmesg']
  let seed = 7
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff
  while (out < bytes) {
    const cmd = cmds[Math.floor(rnd() * cmds.length)]!
    for (const ch of cmd) {
      lines.push(JSON.stringify([+t.toFixed(3), 'i', ch]))
      lines.push(JSON.stringify([+t.toFixed(3), 'o', ch]))
      t += 0.03
    }
    lines.push(JSON.stringify([+t.toFixed(3), 'o', '\r\n']))
    const n = 100 + Math.floor(rnd() * 400)
    for (let i = 0; i < n; i++) {
      const line = `${ESC}[2m${t.toFixed(3).padStart(10, '0')}${ESC}[0m ${['INFO', 'WARN', 'ERROR'][Math.floor(rnd() * 3)]} worker: batch=${Math.floor(rnd() * 99999)} ${'x'.repeat(20 + Math.floor(rnd() * 80))}\r\n`
      lines.push(JSON.stringify([+t.toFixed(3), 'o', line]))
      out += line.length
      t += 0.02
    }
    t += 1
  }
  return lines.join('\n')
}
