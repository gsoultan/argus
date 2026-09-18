import { test, expect, type Browser, type Page } from '@playwright/test'

/**
 * What a visit costs, asserted rather than noted.
 *
 * Measured once by hand against `6916bcca` while reviewing a console rewrite:
 * 228.0 kB of JS and CSS before, 233.6 kB after. A number arrived at that way
 * decays — it is the same shape as every "fast, low memory" claim this suite
 * exists to keep true, and the per-component Mantine stylesheet list is one
 * careless edit away from becoming the concatenated one again.
 *
 * Every figure below is deterministic to a tenth of a kilobyte across runs, so
 * the budgets are tight on purpose. Raise them deliberately, with the reason,
 * rather than nudging them to make a build pass.
 */

// The service worker serves from cache once it is controlling, which would
// measure the cache rather than the download.
test.use({ serviceWorkers: 'block' })

/** JS + CSS, compressed. */
const BUDGET_KB = {
  /** The first screen. Currently 233.6 kB. */
  overview: 250,
  /**
   * A deep link to a recording. Currently 340.8 kB — the extra is xterm.js and
   * the replay player, which no other route pays for. This is the page most
   * likely to grow quietly, because everything about a session lands on it.
   */
  sessionDetail: 370,
}

/**
 * The stylesheet, on any route.
 *
 * `app.css` imports ~47 individual Mantine component stylesheets instead of the
 * concatenated one. Measured by swapping it back: 26.6 kB becomes 39.6 kB.
 * cascade.spec.ts checks that list is in the right *order*; this checks nobody
 * gave up and swapped the concatenated one back in, which would leave the
 * cascade correct and the bundle fat.
 */
const CSS_BUDGET_KB = 32

/**
 * The two self-hosted variable fonts, latin subsets.
 *
 * A third of a first visit and previously the only part of it nothing guarded.
 * The count matters as much as the size: @fontsource ships a subset per script,
 * and the build lists only `*latin*` in the worker's precache because the rest
 * exist for names this console may never render. A Cyrillic or Greek subset
 * appearing here means something started pulling them eagerly.
 */
const FONT_BUDGET_KB = 100
const FONT_FILES = 2

interface Weight {
  js: number
  css: number
  fonts: number
  jsFiles: number
  fontFiles: number
}

async function weigh(page: Page, url: string): Promise<Weight> {
  await page.goto(url)
  await page.waitForLoadState('networkidle')
  // Links preload on intent, and a stray hover would pull in a route chunk a
  // real visit never fetches.
  await page.mouse.move(0, 0)
  await page.waitForTimeout(600)

  return page.evaluate(() => {
    const entries = performance.getEntriesByType('resource') as PerformanceResourceTiming[]
    // encodedBodySize is the compressed body. `content-length` is absent from
    // vite preview's responses, so summing headers reports zero.
    const sum = (match: (e: PerformanceResourceTiming) => boolean) =>
      entries.filter(match).reduce((total, e) => total + (e.encodedBodySize || 0), 0) / 1024
    const count = (match: (e: PerformanceResourceTiming) => boolean) =>
      entries.filter(match).length
    const js = (e: PerformanceResourceTiming) => e.name.endsWith('.js')
    const css = (e: PerformanceResourceTiming) => e.name.endsWith('.css')
    const font = (e: PerformanceResourceTiming) => e.name.endsWith('.woff2')
    return {
      js: sum(js), css: sum(css), fonts: sum(font),
      jsFiles: count(js), fontFiles: count(font),
    }
  })
}

function expectWithin(what: string, w: Weight, codeBudget: number) {
  const code = w.js + w.css
  const detail =
    `js ${w.js.toFixed(1)} kB across ${w.jsFiles} files, css ${w.css.toFixed(1)} kB, `
    + `fonts ${w.fonts.toFixed(1)} kB across ${w.fontFiles} files`

  // A budget nothing was measured against passes silently.
  expect(code, `measured nothing on ${what} — did the page load?`).toBeGreaterThan(50)

  expect(
    code,
    `${what} now downloads ${code.toFixed(1)} kB of code (${detail}).\n`
      + 'If this is deliberate, say what bought the weight and raise the budget.',
  ).toBeLessThan(codeBudget)

  expect(
    w.css,
    `The stylesheet on ${what} is ${w.css.toFixed(1)} kB. Around 40 kB means `
      + 'app.css went back to @mantine/core/styles.layer.css; anything else means a '
      + 'lot of new CSS.',
  ).toBeLessThan(CSS_BUDGET_KB)

  expect(
    w.fonts,
    `${what} downloads ${w.fonts.toFixed(1)} kB of fonts (${detail}).`,
  ).toBeLessThan(FONT_BUDGET_KB)

  expect(
    w.fontFiles,
    `${what} fetched ${w.fontFiles} font files. Two is Inter and JetBrains Mono, `
      + 'latin. More means a non-latin subset is being pulled eagerly.',
  ).toBe(FONT_FILES)
}

test('a first visit stays inside its budget', async ({ page, browserName }) => {
  // Chromium only: the assets are identical on every engine, so a second
  // measurement adds nothing but a second way for resource timing to disagree.
  test.skip(browserName !== 'chromium', 'one engine is enough to weigh bytes')

  expectWithin('the overview', await weigh(page, '/'), BUDGET_KB.overview)
})

test('a deep link to a recording stays inside its budget', async ({ page, browser, browserName }) => {
  test.skip(browserName !== 'chromium', 'one engine is enough to weigh bytes')

  // The id comes from following the list, the way an operator reaches it.
  await page.goto('/sessions')
  await page.waitForLoadState('networkidle')
  await page.locator('tbody tr[role="link"]').first().click()
  await page.waitForURL(/\/sessions\/[^/]+$/)
  const path = new URL(page.url()).pathname

  // A fresh context, because the browser cache is warm from the navigation
  // above and a deep link is by definition somebody's first request.
  await withColdContext(browser, async (cold) => {
    expectWithin('a session detail', await weigh(cold, path), BUDGET_KB.sessionDetail)
  })
})

async function withColdContext(browser: Browser, run: (page: Page) => Promise<void>) {
  const context = await browser.newContext({ serviceWorkers: 'block' })
  try {
    await run(await context.newPage())
  } finally {
    await context.close()
  }
}
