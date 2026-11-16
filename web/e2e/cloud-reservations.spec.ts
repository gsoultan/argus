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

const subnet = (id: string, zone: string) => item('subnet', id, `private-${zone.slice(-1)}`, { network: 'vpc-1', size: '10.0.1.0/24', zone })

/** The AWS account allows changes, in the region the reservations are in. */
async function manage(page: Page) {
  await page.route('**/api/v1/cloud/accounts', async (r) => {
    const res = await r.fetch()
    const body = await res.json()
    body.accounts = body.accounts.map((a: { id: string }) => (a.id === 'ca1'
      ? { ...a, access: 'manage', regions: ['ap-southeast-3'], manageRoleArn: 'arn:aws:iam::412788931204:role/ArgusManage' } : a))
    return r.fulfill({ response: res, json: body })
  })
  await page.route('**/api/v1/cloud/resources?*', (r) => {
    const kind = new URL(r.request().url()).searchParams.get('kind')
    return r.fulfill({ json: { items: kind === 'subnet' ? [subnet('subnet-a', 'ap-southeast-3a'), subnet('subnet-b', 'ap-southeast-3b')] : [] } })
  })
  await page.route('**/api/v1/cloud/launch-options?*', (r) => r.fulfill({ json: {
    images: [{ family: 'ubuntu-24', label: 'Ubuntu 24.04', id: 'ami-ubuntu24', name: 'u24', linux: true }],
    sizes: ['t3.small', 'c7i.xlarge', 'm6i.large'], subnets: [subnet('subnet-a', 'ap-southeast-3a'), subnet('subnet-b', 'ap-southeast-3b')],
    firewalls: [item('firewall', 'sg-web', 'sg-web', { network: 'vpc-1', rules: [] })], environment: 'production', needsApproval: true } }))
}

async function asks(page: Page) {
  const got: { body: Record<string, unknown> | null } = { body: null }
  await page.route('**/api/v1/cloud/changes', async (r) => {
    if (r.request().method() !== 'POST') return r.fallback()
    got.body = r.request().postDataJSON()
    return r.fulfill({ status: 202, json: { id: 'c9', state: 'awaiting_approval', summary: 'Capacity change' } })
  })
  return got
}

test('reserve capacity and cancel it, through the usual change', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await manage(page)
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud?tab=reservations')
  await page.getByRole('button', { name: 'Reserve capacity' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('bills the room from the moment it has it')
  await dialog.getByRole('combobox', { name: 'Zone' }).click()
  await page.getByRole('option', { name: 'ap-southeast-3b' }).click()
  await dialog.getByRole('combobox', { name: 'Instance type' }).click()
  await page.getByRole('option', { name: 'c7i.xlarge' }).click()
  await dialog.getByRole('textbox', { name: 'Instances' }).fill('4')
  await dialog.getByRole('switch', { name: 'Only for launches that name it' }).click()
  await dialog.getByText('After a number of days').click()
  await dialog.getByRole('textbox', { name: 'Days' }).fill('14')
  await dialog.getByRole('textbox', { name: /^Name/ }).fill('sale-week')
  await dialog.getByLabel('Why').fill('Room for the year-end sale')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/13-reserve-capacity.png` })
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ accountId: 'ca1', region: 'ap-southeast-3', action: 'capacity-create',
    spec: { zone: 'ap-southeast-3b', size: 'c7i.xlarge', count: 4, targeted: true, name: 'sale-week' } })
  const end = Date.parse(String((got.body!.spec as Record<string, unknown>).endAt))
  expect(Math.abs(end - (Date.now() + 14 * 86_400_000))).toBeLessThan(120_000)

  await page.getByTestId('held-cr-1').getByRole('button', { name: 'Cancel reservation' }).click()
  await expect(page.getByRole('dialog')).toContainText('The 2 instances in it keep running')
  await page.getByRole('dialog').getByLabel('Why').fill('The burst is over')
  await page.getByRole('dialog').getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'capacity-cancel', resourceId: 'cr-1', region: 'ap-southeast-3' })
})

test('launch into a capacity reservation', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await page.route('**/api/v1/cloud/reservations', (r) => r.fulfill({ json: VIEW }))
  await manage(page)
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud/launch')
  await page.getByRole('textbox', { name: 'Name' }).fill('checkout-07')
  // Only a reservation in the subnet's zone, for the type, shows.
  await expect(page.getByTestId('launch-capacity')).toHaveCount(0)
  await page.getByRole('combobox', { name: 'Instance type' }).click()
  await page.getByRole('option', { name: 'c7i.xlarge' }).click()
  await page.getByTestId('launch-capacity').click()
  await page.getByRole('option', { name: /checkout-burst · 1 of 3 free · named launches only/ }).click()
  await page.getByRole('checkbox', { name: /sg-web/ }).click()
  await page.getByLabel('Why').fill('One more checkout node')
  await expect(page.getByText('Reserved, checkout-burst')).toBeVisible()
  await page.getByRole('textbox', { name: 'How many' }).fill('2')
  await expect(page.getByTestId('launch-capacity')).toHaveCount(0)
  await page.getByRole('textbox', { name: 'How many' }).fill('1')
  await page.getByTestId('launch-capacity').click()
  await page.getByRole('option', { name: /checkout-burst/ }).click()
  await page.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'launch', spec: { size: 'c7i.xlarge', subnet: 'subnet-a', capacityReservation: 'cr-1' } })
})

const QUOTE = {
  offer: { id: 'ri-offer-1', upfront: 0, hourly: 0.41, total: 3592, monthly: 299.3 }, totalText: '$3,592', onDemand: 4744, saves: 1152,
  paysOffMonth: 1, covers: ['pay-api-01', 'web-01', 'web-02'], alwaysApproval: true,
}

test('buy a Reserved Instance: priced, total typed, always a second person', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await page.route('**/api/v1/cloud/reservations', (r) => r.fulfill({ json: VIEW }))
  await manage(page)
  const quoted: Record<string, unknown>[] = []
  await page.route('**/api/v1/cloud/reservations/quote', (r) => {
    quoted.push(r.request().postDataJSON())
    return r.fulfill({ json: QUOTE })
  })
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud?tab=reservations')

  // Buying again what ends in 18 days starts from it.
  await page.getByTestId('held-ri-2').getByRole('button', { name: 'Buy again…' }).click()
  const dialog = page.getByTestId('buy-reservation')
  await expect(dialog.getByRole('combobox', { name: 'Instance type' })).toHaveValue('m6i.large')
  await expect(dialog.getByRole('textbox', { name: 'Instances' })).toHaveValue('2')
  await dialog.getByRole('textbox', { name: 'Instances' }).fill('3')
  await expect(dialog.getByTestId('buy-totals')).toContainText('$3,592')
  await expect(dialog.getByTestId('buy-totals')).toContainText('pays off from month 1')
  await expect(dialog).toContainText('pay-api-01, web-01, web-02')
  await expect.poll(() => quoted.at(-1)).toMatchObject({ accountId: 'ca1',
    query: { kind: 'ri', size: 'm6i.large', count: 3, termMonths: 12, payment: 'Partial Upfront', region: 'ap-southeast-3' } })
  await dialog.getByText('No upfront').click()
  await dialog.getByRole('textbox', { name: 'Type the total to confirm' }).fill('3500')
  await expect(dialog).toContainText('Type $3,592')
  await dialog.getByRole('textbox', { name: 'Type the total to confirm' }).fill('$3,592')
  await dialog.getByRole('textbox', { name: 'Why' }).fill('Web tier has been steady since July; replaces the RI ending soon')
  if (SHOTS) {
    await page.waitForTimeout(300)
    await page.screenshot({ path: `${SHOTS}/14-buy-reservation.png` })
  }
  await dialog.getByRole('button', { name: 'Ask to buy' }).click()
  await expect.poll(() => got.body).toMatchObject({ accountId: 'ca1', region: 'ap-southeast-3', action: 'ri-buy', confirm: '$3,592',
    spec: { kind: 'ri', size: 'm6i.large', count: 3, termMonths: 12, payment: 'No Upfront' } })
})
