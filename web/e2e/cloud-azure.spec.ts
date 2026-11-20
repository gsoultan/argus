import { test, expect, type Page } from '@playwright/test'

/**
 * Connecting an Azure subscription, against e2e/authstub.mjs: the same two
 * steps as an AWS account or a Google Cloud project, with one command for
 * Cloud Shell and nothing copied back. Read only by default; allowing
 * changes makes the Argus manage role too, with the same command.
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
  await expect(page.getByText(/Nothing in the subscription can be changed from Argus/)).toBeVisible()
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

test('allowing changes in a subscription makes the Argus manage role with the same command', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud/connect?provider=azure')
  await page.getByLabel(/Name in Argus/).fill('billing-azure')
  await page.getByLabel(/Subscription id/).fill(SUB)
  await page.getByText('Read and change').click()
  await expect(page.getByText(/start, stop and restart VMs, change their size and their tags/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/07-read-and-change.png`, fullPage: true })
  await page.getByRole('button', { name: 'Next: give Argus access' }).click()
  await expect(page.getByText(`Argus needs two roles in ${SUB}`)).toBeVisible()
  await expect(page.getByText(`Waiting for the roles in ${SUB}`)).toBeVisible()
  await page.getByText('How it is locked down').click()
  await expect(page.getByText(/Azure gives Argus one identity, not one per role/)).toBeVisible()
  await page.getByText('Rather click through the portal?').click()
  await expect(page.getByText(/cannot be made in the portal/)).toBeVisible()
  if (SHOTS) {
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/08-command-with-changes.png`, fullPage: true })
  }
})

test('a subscription sits beside the other clouds and can be allowed changes', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud')
  const row = page.locator('div', { hasText: /^billing-azure/ }).first()
  await expect(page.getByText('billing-azure')).toBeVisible()
  await expect(page.getByText('6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90 · 2 regions')).toBeVisible()
  await expect(row).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/05-overview.png`, fullPage: true })
  await expect(page.getByRole('button', { name: 'Allow changes' })).toHaveCount(3)
  await page.getByRole('button', { name: 'Allow changes' }).nth(2).click()
  await expect(page.getByText(/a custom role that can start, stop,\s+restart, resize and tag VMs, grow, make, attach, detach, snapshot and delete disks, and make, attach and delete public\s+addresses, and nothing else/)).toBeVisible()
  await expect(page.getByRole('button', { name: /Copy the command/ })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/09-allow-changes.png` })
  await page.getByRole('button', { name: 'Cancel' }).click()

  await page.getByRole('button', { name: 'Disconnect' }).nth(2).click()
  await expect(page.getByText('Disconnecting makes no call to Azure. Argus only forgets the subscription.')).toBeVisible()
  await expect(page.getByText(/until someone removes them under Access control \(IAM\)/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/06-disconnect.png` })
})
