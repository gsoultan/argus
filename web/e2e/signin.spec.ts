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
