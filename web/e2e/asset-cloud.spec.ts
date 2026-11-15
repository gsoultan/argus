import { test, expect, type Page } from '@playwright/test'

/**
 * A server's Cloud card, against e2e/authstub.mjs: the instance its agent
 * reported, with the cloud and type in the header; and an instance in an
 * account Argus does not read, named rather than hidden.
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

test('a server shows the instance its agent reported', async ({ page }) => {
  await signIn(page)
  await page.goto('/assets/a-closed-healthy')
  const card = page.getByTestId('cloud-card')
  await expect(card.getByText('i-0a41c27b9e3f1d27d')).toBeVisible()
  await expect(page.getByText('reported by the agent')).toBeVisible()
  await expect(page.getByTestId('header-cloud')).toHaveText('AWS · payments-prod · m6i.large')
  if (SHOTS) {
    await card.scrollIntoViewIfNeeded()
    await page.screenshot({ path: `${SHOTS}/asset-cloud.png`, fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await page.screenshot({ path: `${SHOTS}/asset-cloud-phone.png`, fullPage: true })
  }
})

test('an instance in an account Argus does not read is named', async ({ page }) => {
  await signIn(page)
  await page.goto('/assets/a-monitored-stale')
  await expect(page.getByTestId('cloud-card-unread')).toContainText('999999999999')
  await expect(page.getByTestId('header-cloud')).toHaveText('AWS · t3.small')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/asset-cloud-unread.png`, fullPage: true })
})
