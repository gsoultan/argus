import { test, expect } from '@playwright/test'

/**
 * The sign-in screen, in a browser, on every engine.
 *
 * The rest of this suite builds with VITE_CONTROL_URL empty, where
 * isConfigured() is false and LoginGate waves everything through. So the branch
 * every operator actually meets was the one part of the console no browser test
 * had rendered, and two bugs shipped through the gap: a console that landed on
 * its error boundary after every sign-in, and a session cookie Safari refused.
 *
 * These run against e2e/authstub.mjs, which serves a build with a control plane
 * configured and answers the handful of endpoints the screen talks to.
 */
test.use({ baseURL: 'http://localhost:5511' })

const EMAIL = 'dev@northwind.id'
const PASSWORD = 'stub-password'

async function signIn(page: import('@playwright/test').Page, password = PASSWORD) {
  await page.getByLabel(/^email$/i).fill(EMAIL)
  await page.getByLabel(/^password$/i).fill(password)
  await page.getByRole('button', { name: /^sign in$/i }).click()
}

test('a control plane with accounts offers the password form', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByLabel(/^email$/i)).toBeVisible()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeVisible()
  await expect(page.getByText(/could not be reached/i)).toBeHidden()
})

test('signing in renders the console rather than an error boundary', async ({ page }) => {
  // The bug this exists for: `/` runs its loader before anyone is signed in,
  // takes a 401, and the router commits that error to the match. Acquiring a
  // session has to re-run it, or the first screen after signing in is the
  // error boundary and only a reload gets past.
  await page.goto('/')
  await signIn(page)

  await expect(page.getByText('Dev Admin')).toBeVisible()
  await expect(page.getByText(/could not be rendered/i)).toBeHidden()
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible()

  // And the console does not warn about data it did not fall back to. The
  // banner belongs to a control plane that stopped answering; a banner shown
  // when one is answering fine is noise that teaches people to ignore it.
  await expect(page.getByText(/nothing below is your fleet/)).toBeHidden()
  await expect(page.locator('header .mantine-Badge-root').first()).toHaveText('localhost:5511')
})

test('the session survives a reload', async ({ page }) => {
  // A cookie the browser accepts but will not send back looks identical to a
  // successful sign-in until the next request. Safari did exactly that with a
  // Secure cookie on this plain-HTTP origin.
  await page.goto('/')
  await signIn(page)
  await expect(page.getByText('Dev Admin')).toBeVisible()

  await page.reload()
  await expect(page.getByText('Dev Admin')).toBeVisible()
  await expect(page.getByLabel(/^password$/i)).toBeHidden()
})

test('a refused password says so instead of doing nothing', async ({ page }) => {
  // No watchErrors here: the 401 is the point of the test, and the browser
  // logs every one of those to the console as a failed resource.
  await page.goto('/')
  await signIn(page, 'not-the-password')

  await expect(page.getByText(/invalid email or password/i)).toBeVisible()
  // Still on the form, and still able to try again.
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeEnabled()
  await expect(page.getByText('Dev Admin')).toBeHidden()
})

/**
 * The header badge reports which data is on screen: the control plane's host
 * once one has answered, `Local fixture` when none is configured, and a warning
 * if a configured one goes quiet. Between those it must not guess — claiming a
 * connection that has not been proven is the same class of mistake as the
 * hard-coded deployment name this badge replaced.
 *
 * Finding this state at all took some doing, and the route matters:
 *
 * - On `/` it is unreachable. The router's loader awaits `statsQuery`, so
 *   nothing renders — not even the shell — until that request has come back and
 *   already proved the control plane is there.
 * - `/connect` declares no loader, so the shell paints while the first data
 *   call is still in flight. That is the window, and it is a real one: an
 *   operator deep-linking to Connect against a slow control plane sees it.
 */
test.describe('the data-source badge', () => {
  // page.route cannot see requests a service worker re-issues on the page's
  // behalf, and this console registers one — so with it live the delay below
  // silently does nothing and the test passes against an instant response.
  test.use({ serviceWorkers: 'block' })

  test('does not claim a connection before one has answered', async ({ page }) => {
    await page.goto('/')
    await signIn(page)
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible()

    // A control plane that is up — /auth/me still answers, so the login gate
    // lets go — but slow on the endpoints the console draws figures from.
    await page.route('**/api/v1/**', async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 3000))
      await route.continue()
    })

    await page.goto('/connect', { waitUntil: 'commit' })

    // Scoped to the header badge: `getByText` matches substrings, and the
    // fallback banner's title carries the host too.
    const badge = page.locator('header .mantine-Badge-root').first()
    await expect(badge).toHaveText(/^Connecting/)
    // And once something answers, it names what it reached.
    await expect(badge).toHaveText('localhost:5511', { timeout: 15_000 })
  })

  /**
   * The state the badge exists for.
   *
   * `orFallback` serves the in-memory fixture whenever a data call fails, so
   * the console keeps drawing a complete, plausible fleet that belongs to
   * nobody. Without this warning that is indistinguishable from a healthy
   * deployment — and it is the reading an operator would act on.
   *
   * Aborted rather than answered with a 503: `get()` raises ControlPlaneError
   * on a refused status and `orFallback` rethrows it into the error boundary,
   * which is a different screen. Nothing answering at all is what live.ts calls
   * the only thing "unreachable" should mean, and it is the case that reaches
   * the fallback.
   */
  test('says the control plane is not answering rather than serving its fallback silently', async ({
    page,
  }) => {
    await page.goto('/')
    await signIn(page)
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible()

    // Up for auth, so the login gate still lets go; dead for everything the
    // console draws figures from.
    await page.route('**/api/v1/**', (route) => route.abort())
    await page.reload()

    await expect(page.locator('header .mantine-Badge-root').first()).toHaveText('Not answering')

    // And says it at the size of the problem. The corner badge alone left the
    // page underneath announcing "13 hosts would not record a bypass", three
    // agents gone silent and six live sessions — every figure invented, because
    // orFallback serves the fixture rather than showing nothing.
    await expect(page.getByText(/nothing below is your fleet/)).toBeVisible()
  })
})

/**
 * Coverage states a number, then links to the inventory filtered to what it
 * counted. Getting that pairing wrong is silent: the page renders, the link
 * works, it just lands on a different set of hosts than the one advertised.
 *
 * It shipped twice. An alert headed "5 hosts would not record a bypass" linked
 * to `?agent=absent` and landed on 2, because the count is
 * `bypassPosture === 'open'` while the filter asked about the agent — caught
 * only by pointing the console at a real control plane. The rule it violates
 * was already written down after the same mistake was avoided on the Overview:
 * **a counter links to a filter only when the two mean the same set.**
 *
 * authstub serves a deliberately mixed fleet with its coverage counters derived
 * from it, so no two of these numbers agree by accident.
 */
test.describe('coverage links land on exactly what they counted', () => {
  // These navigate more than once, so a service worker claims the page on the
  // second load and re-issues its requests -- invisibly to page.route, which
  // one test here relies on to stub a coverage payload. Scoped to this block so
  // the sign-in and reload tests above still exercise the worker.
  test.use({ serviceWorkers: 'block' })

  const claim = async (page: import('@playwright/test').Page, heading: RegExp) =>
    Number(((await page.getByText(heading).first().textContent()) ?? '').match(/\d+/)?.[0])

  test('the bypass alert', async ({ page }) => {
    await page.goto('/')
    await signIn(page)
    await page.goto('/coverage')

    const counted = await claim(page, /would not record a bypass/)
    expect(counted, 'the stub fleet should have hosts with an open posture').toBeGreaterThan(0)

    await page.getByRole('link', { name: /show these hosts/i }).click()
    await expect(page.getByRole('heading', { name: 'Assets', exact: true })).toBeVisible()
    await expect(
      page.locator('tbody tr[role="link"]'),
      `The alert counted ${counted} hosts; its link selects a different set.`,
    ).toHaveCount(counted)
  })

  /**
   * The same rule, on the counter that was wrong for nineteen days.
   *
   * An active session whose gateway was killed rather than drained never
   * reports the end, so it stayed `active` forever and every surface counted it
   * as running. The Overview now says how many are unaccounted for and links to
   * exactly those -- which is the pairing that has already shipped broken twice
   * elsewhere, and is invisible when it does: the page renders, the link works,
   * it just lands on a different set.
   */
  test('the unaccounted-sessions alert', async ({ page }) => {
    await page.goto('/')
    await signIn(page)

    const counted = await claim(page, /sessions? unaccounted for/)
    expect(counted, 'the stub log should have silent sessions').toBeGreaterThan(0)

    await page.getByRole('link', { name: /review sessions/i }).click()
    await expect(page.getByRole('heading', { name: 'Sessions', exact: true })).toBeVisible()
    await expect(
      page.locator('tbody tr[role="link"]'),
      `The alert counted ${counted} sessions; its link selects a different set.`,
    ).toHaveCount(counted)

    // And the set it landed on is the one it named: every row unknown, none
    // live. Counting the rows alone would pass on any filter of the same size.
    // Scoped to the table body, because "unknown" also appears in the filter's
    // own label and in the header pill.
    const body = page.locator('tbody')
    await expect(body.getByText('unknown')).toHaveCount(counted)
    await expect(body.getByText('live', { exact: true })).toHaveCount(0)
  })

  /**
   * The headline number, which is the one an operator glances at to decide
   * whether anything is happening at all.
   */
  test('the header live pill counts only what is still being reported', async ({ page }) => {
    await page.goto('/')
    await signIn(page)

    const pill = page.getByText(/\d+ live/)
    await expect(pill).toBeVisible()
    const live = Number(((await pill.textContent()) ?? '').match(/\d+/)?.[0])

    // Mantine's SegmentedControl hides the real radio behind a label, so the
    // input is never clickable; the label is what a user actually hits.
    await page.goto('/sessions')
    const segment = (name: RegExp) => page.locator('label').filter({ hasText: name })

    await segment(/^Live/).click()
    await expect(
      page.locator('tbody tr[role="link"]'),
      `The header claimed ${live} live sessions; the Live filter selects a different set.`,
    ).toHaveCount(live)

    // The silent ones are somewhere, not simply dropped.
    const unknown = Number(
      ((await segment(/^Unknown/).textContent()) ?? '').match(/\d+/)?.[0],
    )
    expect(unknown).toBeGreaterThan(0)
    expect(live).not.toBe(unknown)

    await segment(/^Unknown/).click()
    await expect(page.locator('tbody tr[role="link"]')).toHaveCount(unknown)
  })

  /**
   * Two numbers on one page describing the same thing, and they disagreed.
   *
   * The Overview's "Live sessions" section fetches `state = 'active'`, which
   * includes the sessions nothing has reported -- so its badge counted every
   * one of them while the Stat tile two rows above counted only the live ones.
   */
  test('the Overview agrees with itself about how many sessions are live', async ({ page }) => {
    await page.goto('/')
    await signIn(page)

    const tile = page.locator('.argus-stat', { hasText: 'LIVE SESSIONS' })
    await expect(tile).toContainText(/\d/)
    const claimed = Number((await tile.innerText()).match(/\n\s*(\d+)/)?.[1])
    expect(claimed).toBeGreaterThan(0)

    // Scoped to the card the heading belongs to, so this cannot accidentally
    // count rows from the requests table beside it.
    const card = page
      .getByRole('heading', { name: 'Live sessions' })
      .locator('xpath=ancestor::div[contains(@class,"mantine-Card-root")][1]')
    await expect(
      card.locator('tbody tr'),
      `The tile says ${claimed} live sessions; the table below it lists a different number.`,
    ).toHaveCount(claimed)
  })

  /**
   * A session that ended at a time nobody recorded must not be given one.
   * `duration(startedAt, endedAt)` counts to now on a null end time, which had
   * 26 terminated sessions in dev rendering as still running.
   */
  test('a session with no recorded end time is not given a running clock', async ({ page }) => {
    await page.goto('/')
    await signIn(page)
    await page.goto('/sessions')

    const row = page.locator('tbody tr', { hasText: 'terminated' }).first()
    await expect(row).toBeVisible()
    await expect(
      row,
      'a terminated session with no end time should show no duration at all',
    ).toContainText('—')
  })

  /**
   * Every tile on the Overview, against the page it opens.
   *
   * All four linked unfiltered, so clicking "2 live sessions" opened a log of
   * twelve thousand rows and clicking "1 bypassed today" opened every bypass
   * ever recorded. The stub is built so a naive link lands on a visibly
   * different number in each case: two host keys not pinned across *two*
   * different states, and two direct sessions of which only one is inside the
   * 24-hour window.
   */
  const TILES = [
    { label: 'LIVE SESSIONS', heading: 'Sessions', rows: 'tbody tr' },
    { label: 'UNVERIFIED HOSTS', heading: 'Assets', rows: 'tbody tr' },
    { label: 'BYPASSED GATEWAY', heading: 'Sessions', rows: 'tbody tr' },
    // Requests are cards, not a table.
    { label: 'PENDING APPROVALS', heading: 'Access requests', rows: '.argus-request' },
  ]

  for (const t of TILES) {
    test(`the ${t.label.toLowerCase()} tile`, async ({ page }) => {
      await page.goto('/')
      await signIn(page)

      const tile = page.locator('.argus-stat', { hasText: t.label })
      // Waited for: Stat renders a skeleton rather than a zero until the count
      // arrives, so reading too early gets a card with no digit in it.
      await expect(tile).toContainText(/\d/)
      const claimed = Number((await tile.innerText()).match(/\n\s*(\d+)/)?.[1])
      expect(claimed, `the stub should give ${t.label} something to count`).toBeGreaterThan(0)

      await tile.click()
      await expect(page.getByRole('heading', { name: t.heading, exact: true })).toBeVisible()
      await expect(
        page.locator(t.rows),
        `${t.label} counted ${claimed}; the page it opens lists a different number.`,
      ).toHaveCount(claimed)
    })
  }

  /**
   * A tile's colour has to come from the number on it.
   *
   * "Linux assets with an agent" showed `assetsWithAgent / sshAssets` and took
   * its tone from `assetsUnmonitored` -- a different set. A host with no agent
   * but a `monitored` posture is counted by one and not the other, so a fleet
   * could show "1 / 6" in the calm colour while five Linux assets were
   * recording nothing at all.
   *
   * The coverage payload is stubbed per-test rather than shaped in authstub,
   * because the case that was broken needs `assetsUnmonitored === 0` and the
   * bypass alert above needs it above zero.
   */
  test('a coverage tile takes its colour from its own figure', async ({ page }) => {
    // Registered before the first load, not after signing in: the shell reads
    // coverage on mount for its nav badge, so by the time /coverage renders the
    // answer is already in the query cache and no second request is made.
    await page.route('**/api/v1/coverage', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          // Deliberately a shape the stub fleet cannot produce, so a failure to
          // intercept shows up as a wrong figure rather than passing by luck.
          assets: 9, assetsWithAgent: 2, assetsAgentStale: 0,
          // Nothing can be reached around Argus, and seven Linux assets still
          // have no agent. The old rule called that calm.
          assetsUnmonitored: 0,
          unreviewedHosts: 0, ignoredHosts: 0,
          sshAssets: 9, rdpAssets: 0, rdpAwaitingAgent: 0,
        }),
      }),
    )
    await page.goto('/')
    await signIn(page)
    await page.goto('/coverage')

    const tile = page.locator('.argus-stat', { hasText: 'LINUX ASSETS WITH AN AGENT' })
    await expect(tile, 'the coverage payload was not intercepted').toContainText('2 / 9')
    await expect(
      tile,
      'seven Linux assets have no agent, so this tile must not read as calm',
    ).toHaveAttribute('data-tone', 'warn')
  })

  /**
   * "Move hosts to certificate auth where you can" is an instruction, and
   * until the filter existed there was nowhere to carry it out -- the only way
   * to find the hosts still on a vaulted secret was to read every row.
   *
   * The set spans both injected modes, so no single-mode filter selects it.
   */
  test('the standing-credential card lands on the hosts it counted', async ({ page }) => {
    await page.goto('/')
    await signIn(page)

    const card = page.locator('.mantine-Card-root', { hasText: 'Zero standing privilege' })
    await expect(card).toContainText(/The remaining \d+ use vaulted keys/)
    const counted = Number(((await card.innerText()).match(/The remaining (\d+)/) ?? [])[1])
    expect(counted, 'the stub fleet should have hosts on a standing credential').toBeGreaterThan(0)

    await card.getByRole('link', { name: /show these hosts/i }).click()
    await expect(page.getByRole('heading', { name: 'Assets', exact: true })).toBeVisible()
    await expect(
      page.locator('tbody tr[role="link"]'),
      `The card counted ${counted} hosts; its link selects a different set.`,
    ).toHaveCount(counted)
  })

  test('the agents-gone-quiet counter', async ({ page }) => {
    await page.goto('/')
    await signIn(page)
    await page.goto('/coverage')

    // Scoped to the card, not the label: the figure is a sibling of the text.
    const card = page.locator('.argus-stat', { hasText: 'AGENTS GONE QUIET' })
    // And waited for: Stat renders a skeleton rather than a zero until the
    // count arrives, so reading too early gets a card with no digit in it.
    await expect(card).toContainText(/\d/)
    const quiet = Number((await card.innerText()).match(/\n\s*(\d+)/)?.[1])
    expect(quiet, 'the stub fleet should have stale agents').toBeGreaterThan(0)

    await card.click()
    await expect(page.getByRole('heading', { name: 'Assets', exact: true })).toBeVisible()
    await expect(
      page.locator('tbody tr[role="link"]'),
      `The card counted ${quiet} quiet agents; its link selects a different set.`,
    ).toHaveCount(quiet)
  })
})
