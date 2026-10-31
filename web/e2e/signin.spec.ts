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
  await expect(page.getByRole('heading', { name: 'Servers', exact: true })).toBeVisible()

  // And the console does not warn about data it did not fall back to. The
  // banner belongs to a control plane that stopped answering; a banner shown
  // when one is answering fine is noise that teaches people to ignore it.
  await expect(page.getByText(/nothing below is your fleet/)).toBeHidden()
  // Which control plane answered is in the account menu, not on every screen.
  await page.getByRole('button', { name: 'Account menu' }).click()
  await expect(page.locator('.argus-source-title')).toHaveText('Connected to localhost:5511')
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
 * The source line reports which data is on screen: the control plane's host
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
    await expect(page.getByRole('heading', { name: 'Servers', exact: true })).toBeVisible()

    // A control plane that is up — /auth/me still answers, so the login gate
    // lets go — but slow on the endpoints the console draws figures from.
    await page.route('**/api/v1/**', async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 3000))
      await route.continue()
    })

    await page.goto('/connect', { waitUntil: 'commit' })

    // Scoped to the source line in the account menu: `getByText` matches
    // substrings, and the fallback banner's title carries the host too.
    await page.getByRole('button', { name: 'Account menu' }).click()
    const line = page.locator('.argus-source-title')
    await expect(line).toHaveText(/^Connecting/)
    // And once something answers, it names what it reached.
    await expect(line).toHaveText('Connected to localhost:5511', { timeout: 15_000 })
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
    await expect(page.getByRole('heading', { name: 'Servers', exact: true })).toBeVisible()

    // Up for auth, so the login gate still lets go; dead for everything the
    // console draws figures from.
    await page.route('**/api/v1/**', (route) => route.abort())
    await page.reload()

    await expect(page.getByText('Not answering', { exact: true })).toBeVisible()

    // And says it at the size of the problem. The corner badge alone left the
    // page underneath announcing "13 hosts would not record a bypass", three
    // agents gone silent and six live sessions — every figure invented, because
    // orFallback serves the fixture rather than showing nothing.
    await expect(page.getByText(/nothing below is your fleet/)).toBeVisible()
  })
})

/**
 * A screen states a number, then links to a list filtered to what it counted. Getting that pairing wrong is silent: the page renders, the link
 * works, it just lands on a different set of hosts than the one advertised.
 *
 * It shipped twice. An alert headed "5 hosts would not record a bypass" linked
 * to `?agent=absent` and landed on 2, because the count is
 * `bypassPosture === 'open'` while the filter asked about the agent — caught
 * only by pointing the console at a real control plane. The rule it violates
 * was already written down after the same mistake was avoided on the Overview:
 * **a counter links to a filter only when the two mean the same set.**
 *
 * authstub serves a deliberately mixed fleet, so no two of these numbers agree
 * by accident. (The Overview and Coverage tiles these once covered went with
 * those pages; what they counted is now on Servers and Approvals.)
 */
test.describe('counts land on exactly what they counted', () => {
  // These navigate more than once, so a service worker claims the page on the
  // second load and re-issues its requests -- invisibly to page.route. Scoped to this block so
  // the sign-in and reload tests above still exercise the worker.
  test.use({ serviceWorkers: 'block' })

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
   * The console must never present fixture data as real, and the replay player
   * was doing exactly that.
   *
   * `buildCast` writes a scripted session -- `systemctl status
   * payments-worker` and invented output -- which is the right thing to show in
   * the demo build and a fabrication anywhere else. The fetch helper answered
   * `null` for every failure, so the page could not tell "there is no control
   * plane to ask" from "one answered and said the artefact is not there", and
   * fell back to the fiction in both cases. 18 sessions in the dev control
   * plane are sealed, carry real recorded bytes, and have an artefact still on
   * the gateway that produced them: every one of them replayed as a story.
   */
  test('a recording the control plane cannot serve is not replaced by a fiction', async ({ page }) => {
    await page.goto('/')
    await signIn(page)
    await page.goto('/sessions')

    await page.locator('tbody tr[role="link"]').first().click()
    await expect(page).toHaveURL(/\/sessions\/[^/]+$/)

    await expect(
      page.getByText('This recording is not available to replay'),
    ).toBeVisible()
    // The control plane's reason, carried through rather than flattened into a
    // generic failure: it decides whether the operator waits for a retry or
    // goes and does something.
    await expect(page.getByText(/exists only on the host that produced it/)).toBeVisible()

    await expect(
      page.getByText(/systemctl status payments-worker/),
      'the generated demo recording was shown in place of the real one',
    ).toHaveCount(0)
    await expect(page.getByRole('button', { name: /export \.cast/i })).toBeDisabled()
  })

  /**
   * The list has to say which recordings can actually be opened.
   *
   * A sealed recording with no object-storage key is still on the gateway that
   * produced it, queued for retry, and cannot be fetched from here until that
   * lands. The list showed the fidelity badge, the byte count and a Replay
   * button for all 44 such sessions in dev -- every one of which opened a
   * player with nothing to play.
   */
  test('the sessions list says which recordings are not in storage', async ({ page }) => {
    await page.goto('/')
    await signIn(page)
    await page.goto('/sessions')

    const marker = page.getByLabel('Recording not on the control plane yet')
    // Exactly the one stranded session in the stub, not every row: the marker
    // is only useful if it distinguishes.
    await expect(marker).toHaveCount(1)

    const stranded = page.locator('tbody tr', { has: marker })
    await expect(stranded).toContainText('closed')
    // And it is not shown on the live rows, whose recordings are still being
    // written and have no business being in storage yet.
    const live = page.locator('tbody tr', { hasText: 'live' })
    await expect(live.getByLabel('Recording not on the control plane yet')).toHaveCount(0)
  })

  /**
   * "Chain intact" has to cover the log, or say that it does not.
   *
   * The console fetched the most recent 500 entries, verified them, reported
   * intact, and offered an evidence pack its own doc comment called "the whole
   * chain". The dev log holds 4,546. A hash chain checked from an arbitrary
   * starting point proves that fragment is internally consistent and nothing
   * whatever about what came before it.
   *
   * It now pages backwards until the log runs out. The stub serves all 4,546
   * with real `before` paging, so this exercises the loop against a server:
   * three requests, the last one short, which is how it learns to stop.
   */
  test('the audit page fetches the whole log, not the newest page of it', async ({ page }) => {
    await page.goto('/')
    await signIn(page)
    await page.goto('/audit')

    // Every entry, across three requests of 2000, 2000 and 546. The table
    // itself shows a page of them; the denominator is the whole log.
    await expect(page.getByText(/of 4,546/)).toBeVisible()
    await expect(
      page.getByText(/says nothing about the/),
      'the log was fetched in full, so nothing should be disclaimed',
    ).toHaveCount(0)

    await page.getByRole('button', { name: /verify chain/i }).click()
    await expect(page.getByText(/4,5\d\d links checked/)).toBeVisible()
    // No denominator badge: there is nothing left over to name.
    await expect(page.getByText(/in the log/)).toHaveCount(0)
  })

  /**
   * And when it cannot hold the whole log, it says so rather than implying the
   * verdict covers it. The header is inflated on the first response only --
   * which is the one the console reads the total from, by design, so a write
   * landing mid-fetch cannot move the denominator.
   */
  test('the audit page admits when it is holding only part of the log', async ({ page }) => {
    let first = true
    await page.route('**/api/v1/audit*', async (route) => {
      const res = await route.fetch()
      const headers = { ...res.headers() }
      if (first) {
        headers['x-argus-audit-total'] = '99999'
        first = false
      }
      await route.fulfill({ response: res, headers })
    })

    await page.goto('/')
    await signIn(page)
    await page.goto('/audit')

    await expect(page.getByText(/most recent 4,546 of 99,999 entries/)).toBeVisible()
    await expect(page.getByText(/says nothing about the 95,453 older entries/)).toBeVisible()

    await page.getByRole('button', { name: /verify chain/i }).click()
    await expect(page.getByText(/of 99,999 in the log/)).toBeVisible()
  })

  /**
   * A session Argus closed on a dead gateway's behalf must not read as a
   * clean logout.
   *
   * Its end time is the last moment it was seen alive, not a reported end, so
   * both the state and the duration are claims about a lower bound. A plain
   * "closed" with a plain figure would say the session ran exactly that long
   * and stopped tidily, neither of which was observed.
   */
  test('an inferred end is not shown as an ordinary close', async ({ page }) => {
    await page.goto('/')
    await signIn(page)
    await page.goto('/sessions')

    const row = page.locator('tbody tr', { hasText: 'ended (inferred)' })
    await expect(row).toHaveCount(1)
    // The duration on that row reads as a floor, not a figure.
    await expect(row).toContainText(/\d+h \d+m\+/)

    // And an ordinary close still reads plainly, so the distinction carries.
    await expect(page.locator('tbody tr', { hasText: /^closed/ }).first()).toBeVisible()
  })
})
