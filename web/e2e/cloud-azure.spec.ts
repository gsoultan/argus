import { test, expect, type Page } from '@playwright/test'

/**
 * Connecting an Azure subscription, against e2e/authstub.mjs: the same two
 * steps as an AWS account or a Google Cloud project, with one command for
 * Cloud Shell and nothing copied back. A subscription is read only so far,
 * so no page offers a change in one.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS
const SUB = '6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a91'

async function signIn(page: Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
}

test('an Azure subscription is connected in two steps, read only', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud/connect')
  await page.locator('label', { hasText: 'Azure' }).click()
  await expect(page.getByRole('heading', { name: 'Connect an Azure subscription' })).toBeVisible()
  // Read only is all there is: no choice that would let Argus change it.
  await expect(page.getByText('Read and change')).toHaveCount(0)
  await expect(page.getByText(/changes from Argus come later for Azure/)).toBeVisible()
  await page.getByLabel(/Name in Argus/).fill('billing-azure')
  await page.getByLabel(/Subscription id/).fill('billing-prod')
  await expect(page.getByText(/A subscription id is a GUID/)).toBeVisible()
  // Pasted as the portal sometimes shows it, in capitals.
  await page.getByLabel(/Subscription id/).fill(SUB.toUpperCase())
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/01-subscription.png`, fullPage: true })

  await page.getByRole('button', { name: 'Next: give Argus access' }).click()
  await expect(page.getByText(`Argus needs the Reader role in ${SUB}`)).toBeVisible()
  await expect(page.getByRole('link', { name: /Open Cloud Shell/ })).toHaveAttribute('href', /shell\.azure\.com/)
  await expect(page.getByText(`Waiting for the Reader role in ${SUB}`)).toBeVisible()
  await page.getByText('How it is locked down').click()
  await expect(page.getByText(/names that application, not a person/)).toBeVisible()
  if (SHOTS) {
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/02-command.png`, fullPage: true })
    await page.getByText('Rather click through the portal?').click()
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/03-by-hand.png`, fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/04-command-phone.png`, fullPage: true })
    await page.setViewportSize({ width: 1280, height: 720 })
  }
})

test('a subscription sits beside the other clouds and offers no changes', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud')
  const row = page.locator('div', { hasText: /^billing-azure/ }).first()
  await expect(page.getByText('billing-azure')).toBeVisible()
  await expect(page.getByText('6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90 · 2 regions')).toBeVisible()
  // The two other accounts can be allowed changes; the subscription cannot.
  await expect(page.getByRole('button', { name: 'Allow changes' })).toHaveCount(2)
  await expect(row).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/05-overview.png`, fullPage: true })

  await page.getByRole('button', { name: 'Disconnect' }).nth(2).click()
  await expect(page.getByText('Disconnecting makes no call to Azure. Argus only forgets the subscription.')).toBeVisible()
  await expect(page.getByText(/until someone removes it under Access control \(IAM\)/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/06-disconnect.png` })
})
