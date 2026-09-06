import { expect, type Page } from '@playwright/test'

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
  return {
    assertClean: () => expect(errors, errors.join('\n')).toEqual([]),
  }
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
