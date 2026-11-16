import { test, expect, type Page } from '@playwright/test'

/**
 * Cloud › Reservations, against e2e/authstub.mjs with the reservations view
 * served by the test: coverage per account and region, what each account
 * holds with its use, what is paid for and not used, and a read role that
 * predates Savings Plans.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS
const now = Date.now()
const inDays = (d: number) => new Date(now + d * 86_400_000).toISOString()

const item = (kind: string, id: string, name: string, extra: object, account = 'ca1', accountName = 'payments-prod') => ({
  accountId: account, accountName, environment: 'production', firstSeen: inDays(-200), seenAt: inDays(0), madeByArgus: false,
  resource: { kind, id, name, region: 'ap-southeast-3', ...extra },
})

const VIEW = {
  accounts: [
    { id: 'ca1', name: 'payments-prod', environment: 'production', read: true, reservedInstances: true, capacityReservations: true, savingsPlans: true, savingsPlansNeedRole: false },
    { id: 'ca2', name: 'payments-staging', environment: 'staging', read: true, reservedInstances: true, capacityReservations: true, savingsPlans: false, savingsPlansNeedRole: true },
  ],
  reservedInstances: [
    { ...item('reserved-instance', 'ri-1', '4 × m6i.large', { size: 'm6i.large', state: 'active',
      details: { count: '4', scope: 'region', termMonths: '12', payment: 'No Upfront', end: inDays(116) } }),
    use: { units: 16, used: 16, use: 1, monthly: 280.32, flexible: true, covers: ['web-01', 'web-02', 'pay-api-01'] } },
    { ...item('reserved-instance', 'ri-2', '2 × m6i.large', { size: 'm6i.large', state: 'active',
      details: { count: '2', scope: 'region', termMonths: '12', payment: 'Partial Upfront', end: inDays(18) } }),
    use: { units: 8, used: 0, use: 0, monthly: 122, flexible: true, covers: [] } },
  ],
  savingsPlans: [
    { ...item('savings-plan', 'sp-1', 'Compute Savings Plan', { region: 'global', state: 'active',
      details: { type: 'Compute', commitment: '1.2', termMonths: '36', payment: 'No Upfront', end: inDays(420) } }),
    use: { commitment: 1.2, eligible: 2.4, alsoOther: true } },
  ],
  capacityReservations: [
    item('capacity-reservation', 'cr-1', 'checkout-burst', { size: 'c7i.xlarge', zone: 'ap-southeast-3a', state: 'active',
      details: { total: '3', available: '1', match: 'targeted' } }),
  ],
  coverage: [
    { accountId: 'ca1', accountName: 'payments-prod', region: 'ap-southeast-3', instances: 9, priced: 9, hourly: 2.6, byRi: 1.61, rest: 0.99 },
    { accountId: 'ca1', accountName: 'payments-prod', region: 'ap-southeast-1', instances: 3, priced: 3, hourly: 0.55, byRi: 0, rest: 0.55 },
    { accountId: 'ca2', accountName: 'payments-staging', region: 'ap-southeast-3', instances: 4, priced: 4, hourly: 0.42, byRi: 0, rest: 0.42 },
  ],
  unused: { 'ca1/ri-2': 122, 'ca1/cr-1': 62.05 },
  unusedMonthly: 184.05,
  checkedAt: new Date(now).toISOString(),
}

async function signIn(page: Page) {
  await page.route('**/api/v1/cloud/reservations', (r) => r.fulfill({ json: VIEW }))
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeHidden()
}

test('reservations: coverage, use, unused and ending soon', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await signIn(page)
  await page.goto('/cloud?tab=reservations')
  const panel = page.getByTestId('reservations')
  await expect(panel.getByText('$184.05')).toBeVisible()
  await expect(panel.getByText('a month · 2 reservations')).toBeVisible()
  // 1.61 of 3.57 an hour is covered.
  await expect(panel.getByText('45%')).toBeVisible()
  await expect(panel.getByText('2 × m6i.large ·', { exact: false })).toBeVisible()
  await expect(page.getByTestId('held-ri-2')).toContainText('0% used')
  await expect(page.getByTestId('held-ri-2')).toContainText('18 days')
  await expect(page.getByTestId('held-ri-2')).toContainText('$122.00 a month unused')
  await expect(page.getByTestId('held-ri-1')).toContainText('1 yr · no upfront')
  await expect(page.getByTestId('held-cr-1')).toContainText('1 empty, billed as if running')
  await expect(page.getByTestId('held-cr-1')).toContainText('67% used')
  await expect(page.getByTestId('held-sp-1')).toContainText('$1.20/hr compute')
  await expect(page.getByTestId('reservations-need-role')).toContainText('payments-staging')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/11-reservations.png`, fullPage: true })
})

test('reservations on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await signIn(page)
  await page.goto('/cloud?tab=reservations')
  await expect(page.getByTestId('reservations').getByText('$184.05')).toBeVisible()
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(0)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/12-reservations-phone.png`, fullPage: true })
})
