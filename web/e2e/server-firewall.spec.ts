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

test('letting something in is reviewed with its commands, then waits for the server', async ({ page }) => {
  await signIn(page)
  await page.goto('/assets/a-closed-healthy?tab=firewall')
  await expect(page.getByTestId('firewall-own')).toContainText('orders app')
  await expect(page.getByTestId('firewall-policies-here')).toContainText('Web servers')
  await page.getByTestId('firewall-add').click()
  await expect(page.getByTestId('firewall-line-form')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/firewall-add-line.png` })
  await page.getByTestId('firewall-review').click()
  await expect(page.getByTestId('firewall-commands')).toContainText("ufw allow in proto tcp from 10.0.0.0/8 to any port 22")
  await expect(page.getByText(/puts it back by itself if it cannot reach Argus/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/firewall-review.png` })
  await page.getByTestId('firewall-send').click()
  await expect(page.getByTestId('firewall-open-change')).toContainText('Waiting for the server')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/firewall-tab-change.png`, fullPage: true })
})

test('a rule made by hand can be taken over, and that waits for approval', async ({ page }) => {
  await signIn(page)
  await page.goto('/assets/a-closed-healthy?tab=firewall')
  await expect(page.getByTestId('firewall-own')).toBeVisible()
  // A change still waiting from another test is taken back first: nothing
  // is taken over while a change is on its way.
  const back = page.getByRole('button', { name: 'Take back' })
  if (await back.isVisible()) {
    await back.click()
    await expect(page.getByTestId('firewall-open-change')).toHaveCount(0)
  }
  await page.getByRole('button', { name: 'Take over' }).first().click()
  await expect(page.getByText('It waits for another admin to approve it.')).toBeVisible()
  await expect(page.getByTestId('firewall-commands')).toContainText('ufw --force delete')
})

test('firewall policies list where each is and how its rollout goes', async ({ page }) => {
  await signIn(page)
  await page.goto('/assets/firewalls')
  await page.getByTestId('firewall-policies-link').click()
  await expect(page.getByRole('heading', { name: 'Firewall policies' })).toBeVisible()
  await expect(page.getByText('Rolling out')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/firewall-policies.png`, fullPage: true })
  await page.getByRole('link', { name: 'Web servers' }).click()
  await expect(page.getByTestId('policy-rollout')).toContainText('4 of 6 done')
  await expect(page.getByTestId('policy-targets')).toContainText('Every server tagged web')
  await expect(page.getByTestId('policy-lines')).toContainText('Let in Web 80, 443 from Everyone')
  if (SHOTS) {
    await page.screenshot({ path: `${SHOTS}/firewall-policy.png`, fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await page.screenshot({ path: `${SHOTS}/firewall-policy-phone.png`, fullPage: true })
  }
})
