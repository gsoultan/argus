import { test, expect, type Page } from '@playwright/test'

/**
 * A server's Firewall tab and Assets › Firewalls, against e2e/authstub.mjs:
 * the firewall its agent read, each rule with who made it, who can reach each
 * listening port, and the fleet's firewalls one line each.
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

test("a server's Firewall tab shows its rules and who can reach each port", async ({ page }) => {
  await signIn(page)
  await page.goto('/assets/a-closed-healthy?tab=firewall')
  await expect(page.getByText(/Firewall is on/)).toBeVisible()
  await expect(page.getByText('Read only for now')).toBeVisible()
  await expect(page.getByRole('link', { name: 'orders' })).toBeVisible()
  await expect(page.getByText('Everyone, the internet included')).toBeVisible()
  if (SHOTS) {
    await page.screenshot({ path: `${SHOTS}/firewall-tab.png`, fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await page.screenshot({ path: `${SHOTS}/firewall-tab-phone.png`, fullPage: true })
  }
})

test('Assets › Firewalls lists every firewall and what is open to the internet', async ({ page }) => {
  await signIn(page)
  await page.goto('/assets/firewalls')
  await expect(page.getByRole('heading', { name: 'Firewalls' })).toBeVisible()
  await expect(page.getByText('Web (443), Redis (6379)')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/firewalls-fleet.png`, fullPage: true })
  await page.getByText(/^Open to the internet 2$/).click()
  await expect(page.getByText('SSH (22), MySQL (3306)')).toBeVisible()
  await expect(page.getByText('Web (443), Redis (6379)')).toBeVisible()
})
