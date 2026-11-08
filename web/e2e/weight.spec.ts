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
  /**
   * The first screen. 233.6 kB when this was set, and 249.8 kB by the time the
   * first-run setup page landed, which bought 1.0 kB: the page itself, eager,
   * because it renders above the router. Lazy cost 3.4 kB instead -- it shares
   * Alert, Stack, Center and the icon factory with the shell, and splitting
   * those into chunks of their own cost more than the page did.
   *
   * 260.6 kB once the install wizard joined it, for the same reason: it renders
   * above the router on a fresh install. 9.5 kB, of which Grid and
   * SegmentedControl, which the shell did not load before, are 4.6 kB. Lazy
   * measured 258.0 kB and split the shared components into 11 more files.
   *
   * 264.6 kB with the redesign: the top menu's count badges and account menu
   * replaced the sidebar, and `/` now lands on Servers rather than Overview.
   *
   * 267.9 kB when Databases joined the menu: the item and its icon, the
   * command palette searching databases, and the query hooks, which every
   * route shares. The workspace itself is its own 9 kB chunk, paid only
   * when a database is opened.
   *
   * 271.9 kB with two-step sign-in: the account menu's two items and the
   * "sign out everywhere" call, and the hold screen a privileged account meets
   * before it has enrolled. The screen itself is lazy (27 kB raw, with the QR
   * encoder, paid once per account), but it shares Alert, Paper, Center and
   * the password and text inputs with the sign-in form, and the chunks those
   * now sit in were split to be shared. Eager measured 3 kB heavier again.
   *
   * 269.5 kB with organizations: the Organization field on the sign-in form,
   * which renders above the router, the switcher in the header, and the
   * sign-in client's organization calls. Measured against main at 267.9 kB
   * in the same build; two more chunk files account for about a third of it.
   * With both, 273.5 kB.
   *
   * The two-column sign-in page adds about 0.6 kB. It renders before the
   * router, so its layout, three icons and the brand panel's CSS are eager;
   * lazy would put a round trip in front of the first screen anyone sees.
   *
   * 276.0 kB with PasswordInput's stylesheet, which app.css had left out: every
   * password field rendered its input at the browser's default width. About
   * 0.3 kB, in the shared stylesheet, so every route pays it once.
   * 276.9 kB when the page header learned to put its actions under the title
   * on a phone and to carry a tab row: 0.9 kB of shared code, since every
   * route renders that header.
   * 282.1 kB with the alerts bell in the header (277.1 before): 4.6 kB of
   * JS for the bell, its count and the pop-ups, and 0.5 kB of CSS, most of
   * it the notifications stylesheet every toast had been missing. The list,
   * the settings, the stream and the fixture load only when needed.
   * 284.1 kB with the Agents card on Servers (282.9 before, same build): 1.2 kB
   * for the query that asks whether there are agents to show, its two
   * mutations and the API calls, which share the eager queries module. The
   * card itself is lazy and loads only for a fleet with enrolled agents.
   * 284.3 kB once `/` became Home rather than a redirect to Servers (284.0
   * before, same build). Home's operator half and its argus-control card are
   * chunks of their own, and nothing below the header is drawn until the
   * console knows who is signed in, so an admin's first visit does not fetch
   * the server cards it never shows.
   * 286.8 kB with light and dark themes (284.3 before, same build): the dark scheme's tokens in the
   * shared stylesheet, the per-scheme colour resolver and the theme menu in
   * the header with its three icons. All of it renders before the first
   * screen, so none of it can be lazy.
   * 290.8 kB with the full-width redesign (287.0 before, same build): Home
   * for the whole fleet became a command centre -- the band with its five
   * numbers, sessions by hour, the fleet map and the recording ring, Home's
   * own chunk plus its stylesheet. Server status helpers moved to
   * lib/reach.ts so the fleet map does not pull in the server cards.
   * 291.9 kB once the database page's SQL editor loaded its formatter
   * lazily (290.8 before): no editor code reaches this page, but a second chunk
   * needing the bundler's module helpers moves them into a small shared
   * chunk, fetched as one more file. Loading the formatter eagerly instead
   * would cost every database page 75 kB gzipped.
   * 293.2 kB with approval of changes per database (292.9 before, same build):
   * the query and the API call that say whether a database's changes wait and
   * who decides them, which share the eager queries and live modules. The
   * Safety card, the review, the banners and the impact views are all in
   * route or lazy chunks.
   * 295.1 kB with Brokers in the menu (293.5 before, same build): the item
   * and its icon, the two broker routes in the route map, and two more small
   * shared chunks, the copy button and the lock icon, split out because the
   * workspace now uses them from a chunk of its own. The workspace is lazy. It first measured 301.6 kB: a NavLink in
   * the exchange list split Menu's shared chunk into 13 files, and Chip added
   * its stylesheet to every route. Both were replaced.
   * 296.2 kB with Android devices: the device kind and its phone icon in
   * the server lists, "Watch only" and "Control" in the shared roles
   * module, and the device fields on the asset type. The wizard's QR code,
   * the device form and the phone toolbar are in route or lazy chunks.
   * 301.3 kB with the live Home (296.9 before, same build): six tiles with
   * their lines and bars, the stacked chart, Fleet health and the live store
   * replace the band, the fleet map and the ring, in Home's own chunk. In
   * fixture mode the sample dashboard is one more small chunk; the stream
   * code rides the bell's lazy one.
   * 302.7 kB with Cloud in the menu: the item and its icon, and the two
   * cloud routes, their role guards and the tab names in the route map.
   * The pages, the wizard and the cloud queries are in route chunks.
   * 303.8 kB with readable errors and sign-in on an expired session (302.7
   * before, same build): the plain-language description of each status, the
   * 401 listener the sign-in gate subscribes to, its "your session has ended"
   * note, and the error screen's sign-in button. All of it has to be in the
   * shell, because any page can be the one whose request finds the session gone.
   * 307.5 kB with cloud changes as well (303.8 before; 306.3 against the
   * build before readable errors). None of it is cloud code: with the launch page, the instance actions and the changes
   * list sharing the menu's icons, the bundler moved ten of those icons
   * (Home, Servers, Databases, Cloud, Settings and the like) out of the
   * entry into a small file each. The bytes are the same; the 3.6 kB is
   * the per-file overhead. The cloud pages themselves are in route chunks.
   * 308.1 kB with update notices (307.5 before, same build): the Updates
   * route in the route map, and the server cards' Needs updates filter and
   * badges, with the small query they read. The Updates page and the bell's
   * Updates tab are in route and lazy chunks.
   */
  overview: 309,
  /**
   * A deep link to a recording. 340.8 kB when this was set — the extra is
   * xterm.js and the replay player, which no other route pays for. This is the page most
   * likely to grow quietly, because everything about a session lands on it.
   * 370.6 kB with the redesign, which the new shell accounts for, as above.
   * 374.3 kB with database sessions: the shared Databases weight above plus
   * the branch that replays statements, itself lazy.
   * 378.4 kB with two-step sign-in, for the shared weight above.
   * 376.0 kB with organizations: the same shared 1.6 kB as the overview, above.
   * With both, 380.1 kB.
   * 383.2 kB with PasswordInput's stylesheet, the same 0.3 kB as the overview.
   * 384.1 kB with the phone-friendly page header, the same 0.9 kB as the
   * overview.
   * 389.6 kB with the alerts bell, the same shared 5 kB as the overview.
   * 390.0 kB with temporary passwords: the shell's gate for a password that
   * must be replaced and its "Change my password" item; the form is lazy.
   * 392.2 kB with Home (390.8 before, same build): its menu item and icon, the
   * link-typed Anchor every route can use, and the recent-audit and status
   * queries in the shared queries module.
   * 394.8 kB with light and dark themes, the same shared weight as the
   * overview.
   * 397.0 kB with the cut marker on long values: the shared results grid's
   * badge and note.
   * 397.03 kB with the query builder and Summarize, both lazy: what this
   * route pays is the entry's map of the two new chunks and what they need.
   * 399.5 kB with Brokers in the menu (397.9 before, same build), for the
   * shared weight given for the overview.
   * Operating system updates add about 0.1 kB here: the server page's
   * Updates tab name and the Settings entry. Its calls and views are in
   * chunks of their own, kept out of the shared live module.
   * 400.7 kB with Android devices, the same shared weight as the overview
   * plus the device toolbar in the desktop view, which Watch live loads.
   * 402.3 kB with the live Home (401.8 before): its two calls and types in
   * the shared live and api modules.
   * 403.7 kB with Cloud in the menu: the item and its icon, the two cloud
   * routes and their role guards in the route map, and the tab names the
   * page's search rule checks. The pages, the wizard and the cloud queries
   * are in route chunks.
   * 404.8 kB with readable errors and sign-in on an expired session, the
   * same shared weight as the overview.
   * 407.8 kB with cloud changes as well: the same icon files as the overview,
   * above.
   * 409.1 kB with cloud schedules: the schedule editor's route, role guard
   * and search rule in the route map, and the Schedules tab name the cloud
   * page's search rule checks. The tab, the editor and their queries are in
   * route chunks.
   */
  sessionDetail: 410,
}

/**
 * The stylesheet, on any route.
 *
 * `app.css` imports ~47 individual Mantine component stylesheets instead of the
 * concatenated one. Measured by swapping it back: 26.6 kB becomes 39.6 kB.
 * cascade.spec.ts checks that list is in the right *order*; this checks nobody
 * gave up and swapped the concatenated one back in, which would leave the
 * cascade correct and the bundle fat.
 *
 * 32.1 kB on a session detail once PasswordInput's stylesheet joined the list.
 * It had been missing, which left every password field's input at the
 * browser's default width. Still 7 kB under the concatenated one.
 *
 * 33.2 kB on Home with the full-width redesign: its command-centre stylesheet
 * loads with Home. The filter rails' and split views' styles load with their
 * own routes and are not paid here.
 *
 * 33.6 kB on Home with the live dashboard (33.4 before): tiles, the live
 * pill, Fleet health's meters and the reduced-motion rules.
 */
const CSS_BUDGET_KB = 33.8

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

  // Reported on every run, not only on failure. The figures quoted in the
  // comments above date the moment a budget is legitimately raised — which is
  // the decay these tests exist to stop, one level up — so the live number
  // belongs in the output rather than only in prose.
  const line = `${what}: ${code.toFixed(1)} kB of code / ${codeBudget} budget — ${detail}`
  test.info().annotations.push({ type: 'weight', description: line })
  // Logged as well as annotated: the list reporter shows annotations only for
  // tests that did not pass, and a number nobody sees while it is healthy is
  // the same as no number.
  console.log(`  ▸ ${line}`)

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
    `${what} fetched ${w.fontFiles} font files. Two is IBM Plex Sans and JetBrains Mono, `
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
