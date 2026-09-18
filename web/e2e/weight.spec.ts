import { test, expect } from '@playwright/test'

/**
 * What a first visit costs, asserted rather than noted.
 *
 * Measured once by hand against `6916bcca` while reviewing a console rewrite:
 * 228.0 kB of JS and CSS before, 233.6 kB after. A number arrived at that way
 * decays — it is the same shape as every "fast, low memory" claim this suite
 * exists to keep true, and the per-component Mantine stylesheet list is one
 * careless edit away from becoming the 267 kB monolith again.
 *
 * Deterministic to a tenth of a kilobyte across runs, so the budgets are tight
 * on purpose. Raise them deliberately, with the reason, rather than nudging
 * them to make a build pass.
 */

// The service worker serves from cache once it is controlling, which would
// measure the cache rather than the download. Blocking it also keeps
// `page.route`-free specs honest about what actually crossed the network.
test.use({ serviceWorkers: 'block' })

/** JS + CSS on a cold load of `/`. Fonts are excluded — see below. */
const CODE_BUDGET_KB = 250

/**
 * The stylesheet alone.
 *
 * `app.css` imports ~47 individual Mantine component stylesheets instead of the
 * concatenated one. Measured by swapping it back: 26.6 kB becomes 39.6 kB.
 * cascade.spec.ts checks that list is in the right *order*; this checks nobody
 * gave up and swapped the monolith back in.
 */
const CSS_BUDGET_KB = 32

test('a first visit stays inside its budget', async ({ page, browserName }) => {
  // Chromium only: the assets are identical on every engine, so a second
  // measurement adds nothing but a second way for resource timing to disagree.
  test.skip(browserName !== 'chromium', 'one engine is enough to weigh bytes')

  await page.goto('/')
  await page.waitForLoadState('networkidle')
  // Links preload on intent, and a stray hover would pull in a route chunk that
  // a real first visit never fetches.
  await page.mouse.move(0, 0)
  await page.waitForTimeout(400)

  const weight = await page.evaluate(() => {
    const entries = performance.getEntriesByType('resource') as PerformanceResourceTiming[]
    // encodedBodySize is the compressed body. `content-length` is absent from
    // vite preview's responses, so summing headers reports zero.
    const sum = (match: (e: PerformanceResourceTiming) => boolean) =>
      entries.filter(match).reduce((total, e) => total + (e.encodedBodySize || 0), 0) / 1024
    return {
      js: sum((e) => e.name.endsWith('.js')),
      css: sum((e) => e.name.endsWith('.css')),
      // Excluded from the budget: the two self-hosted variable fonts are a
      // fixed cost that has nothing to do with how the console is written.
      fonts: sum((e) => e.name.endsWith('.woff2')),
      files: entries.filter((e) => e.name.endsWith('.js')).length,
    }
  })

  const code = weight.js + weight.css
  const detail =
    `js ${weight.js.toFixed(1)} kB across ${weight.files} files, `
    + `css ${weight.css.toFixed(1)} kB, fonts ${weight.fonts.toFixed(1)} kB`

  expect(
    code,
    `A first visit now downloads ${code.toFixed(1)} kB of code (${detail}).\n`
      + 'If this is deliberate, say what bought the weight and raise the budget.',
  ).toBeLessThan(CODE_BUDGET_KB)

  expect(
    weight.css,
    `The stylesheet is ${weight.css.toFixed(1)} kB. Around 40 kB means app.css `
      + 'went back to @mantine/core/styles.layer.css; anything else means a lot of '
      + 'new CSS.',
  ).toBeLessThan(CSS_BUDGET_KB)

  // A budget nothing is measured against passes silently.
  expect(code, 'measured nothing — did the page load?').toBeGreaterThan(50)
})
