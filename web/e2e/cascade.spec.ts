import { test, expect } from '@playwright/test'

/**
 * The per-component stylesheet list in app.css, checked against Mantine's own.
 *
 * app.css imports ~47 individual Mantine stylesheets instead of the 267 kB
 * monolith — 12 kB gzipped smaller, at the cost of putting the cascade order
 * into a list a human maintains. Two things can go wrong there, and neither is
 * visible to any other test in this repo:
 *
 *   1. A reordered import. Several Mantine components are built on others at
 *      the same specificity — Card on Paper, NavLink on UnstyledButton — so a
 *      base that lands after the thing built on it loses the cascade. Sorted by
 *      name, Card computes `display: block` rather than `flex`. That is a broken
 *      layout which compiles, type-checks, and passes every test that does not
 *      measure rendered geometry.
 *   2. A component added to the app but not to the list. It renders unstyled.
 *
 * app.monolith.css is the same application with Mantine's concatenated
 * stylesheet, which is in Mantine's own order by construction. Both are built
 * and served; this walks every route in both and compares getComputedStyle
 * property by property. Screenshot diffing is noisier and pixel-identical
 * output is not the same as identical cascade.
 *
 * The probe set is derived from the DOM rather than hand-listed, so a component
 * added tomorrow is covered without anyone remembering to add it here.
 */

// The live-session dot pulses, so a sampled opacity is whatever the animation
// happened to be at. The app already collapses animations under this setting,
// which makes every probed value deterministic. Scoped to this file rather than
// the config: the memory specs measure the app as it actually behaves.
test.use({ reducedMotion: 'reduce' })

const SHIPPED = 'http://localhost:5510'
const REFERENCE = 'http://localhost:5512'

const ROUTES = ['/', '/sessions', '/assets', '/requests', '/audit', '/users', '/settings', '/connect']

/**
 * Longhands, not shorthands: `border-color` resolves to the empty string when
 * the four sides disagree, which would silently skip the comparison.
 */
const PROPS = [
  'display', 'position', 'flex-direction', 'align-items', 'justify-content',
  'flex-grow', 'flex-shrink', 'flex-basis', 'gap',
  'width', 'height', 'min-height', 'max-width',
  'padding-top', 'padding-right', 'padding-bottom', 'padding-left',
  'margin-top', 'margin-right', 'margin-bottom', 'margin-left',
  'background-color', 'color', 'opacity',
  'border-top-width', 'border-right-width', 'border-bottom-width', 'border-left-width',
  'border-top-color', 'border-bottom-color', 'border-left-color', 'border-right-color',
  'border-top-left-radius', 'border-bottom-right-radius',
  'font-size', 'font-weight', 'line-height', 'letter-spacing', 'text-transform',
  'text-align', 'white-space', 'overflow-x', 'overflow-y', 'box-shadow', 'z-index',
]

type Snapshot = Record<string, Record<string, string>>

/**
 * Samples until two consecutive reads agree.
 *
 * A fixed settle time is not enough. Styles arrive asynchronously — the
 * stylesheet, then the web fonts, then whatever React mounts last — and this
 * compares widths and heights, which move as each lands. Sampling too early
 * caught a table scroll container at the browser's default 16px on WebKit and
 * reported it as a cascade fault, which it was not.
 *
 * Waiting for stability rather than for a duration keeps that noise out without
 * hiding anything: a genuine difference between the two builds is stable, so it
 * survives this and still fails.
 */
async function snapshot(page: import('@playwright/test').Page, origin: string, route: string) {
  await page.goto(origin + route)
  await page.waitForLoadState('networkidle')
  // Layout depends on the web fonts, and they are compared by pixel here.
  await page.evaluate(() => document.fonts.ready)
  // Nothing hovered, so no tooltip or hover rule is sampled on one side only.
  await page.mouse.move(0, 0)

  let previous: Snapshot | null = null
  for (let attempt = 0; attempt < 8; attempt++) {
    await page.waitForTimeout(250)
    const current = await collect(page)
    if (previous && JSON.stringify(previous) === JSON.stringify(current)) return current
    previous = current
  }
  return previous as Snapshot
}

function collect(page: import('@playwright/test').Page): Promise<Snapshot> {
  return page.evaluate((props) => {
    const names = new Set<string>()
    for (const el of document.querySelectorAll('*')) {
      for (const c of el.classList) {
        if (c.startsWith('mantine-') || c.startsWith('argus-')) names.add(c)
      }
    }
    const out: Record<string, Record<string, string>> = {}
    for (const name of [...names].sort()) {
      const el = document.querySelector('.' + CSS.escape(name))
      if (!el) continue
      const cs = getComputedStyle(el)
      const rec: Record<string, string> = {}
      for (const p of props) rec[p] = cs.getPropertyValue(p)
      out[name] = rec
    }
    return out
  }, PROPS)
}

function diff(shipped: Snapshot, reference: Snapshot): string[] {
  const problems: string[] = []

  for (const name of Object.keys(reference)) {
    if (!(name in shipped)) {
      problems.push(`${name}: present in the reference build, absent in the shipped one`)
    }
  }
  for (const name of Object.keys(shipped)) {
    if (!(name in reference)) {
      problems.push(`${name}: present in the shipped build, absent in the reference one`)
      continue
    }
    for (const [prop, want] of Object.entries(reference[name]!)) {
      const got = shipped[name]![prop]
      if (got !== want) {
        problems.push(`${name} { ${prop}: ${got} } — Mantine's own order gives ${want}`)
      }
    }
  }
  return problems
}

for (const route of ROUTES) {
  test(`${route} computes the same styles as Mantine's own stylesheet order`, async ({ page }) => {
    const shipped = await snapshot(page, SHIPPED, route)
    const reference = await snapshot(page, REFERENCE, route)

    // A page that probed nothing would pass silently, which is the one result
    // this test must never give.
    expect(Object.keys(reference).length).toBeGreaterThan(20)

    const problems = diff(shipped, reference)
    expect(
      problems,
      `app.css disagrees with @mantine/core/styles.layer.css on ${route}.\n`
        + 'Either an import is out of order or a component has no stylesheet — '
        + 'regenerate the list from the byte offsets in styles.layer.css.\n\n'
        + problems.slice(0, 40).join('\n'),
    ).toEqual([])
  })
}
