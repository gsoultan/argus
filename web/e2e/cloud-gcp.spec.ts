import { test, expect, type Page } from '@playwright/test'

/**
 * Connecting a Google Cloud project, against e2e/authstub.mjs: the same two
 * steps as an AWS account, read only, with one command for Cloud Shell and
 * nothing copied back.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS

async function signIn(page: Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
}

test('a Google Cloud project is connected in two steps, read only', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud/connect')
  await page.locator('label', { hasText: 'Google Cloud' }).click()
  await expect(page.getByRole('heading', { name: 'Connect a Google Cloud project' })).toBeVisible()
  await expect(page.getByText('Read and change')).toHaveCount(0)
  await page.getByLabel(/Name in Argus/).fill('payments-gcp')
  await page.getByLabel(/Project id/).fill('pay_1')
  await expect(page.getByText(/A project id is 6 to 30/)).toBeVisible()
  await page.getByLabel(/Project id/).fill('payments-prod-418203')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/01-project.png`, fullPage: true })

  await page.getByRole('button', { name: 'Next: give Argus access' }).click()
  await expect(page.getByText('Argus needs a read-only service account in payments-prod-418203')).toBeVisible()
  await expect(page.getByRole('link', { name: /Open Cloud Shell/ })).toHaveAttribute('href', /project=payments-prod-418203/)
  await expect(page.getByText('Waiting for argus-read in payments-prod-418203')).toBeVisible()
  if (SHOTS) {
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/02-command.png`, fullPage: true })
    await page.getByText('Rather click through the console?').click()
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/03-by-hand.png`, fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/04-command-phone.png`, fullPage: true })
    await page.setViewportSize({ width: 1280, height: 720 })
  }
})

test('a project sits beside AWS accounts, without the AWS-only actions', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud')
  const row = page.getByText('analytics-prod', { exact: true }).locator('xpath=ancestor::div[contains(@style,"border-top")][1]')
  await expect(row.getByText('Google Cloud')).toBeVisible()
  await expect(row.getByText('analytics-prod-4182 · 2 regions')).toBeVisible()
  await expect(row.getByRole('button', { name: 'Allow changes' })).toHaveCount(0)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/05-accounts.png`, fullPage: true })
})
