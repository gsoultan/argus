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
  await expect(page.getByText(/a custom role that can launch, start, stop,\s+restart, resize, tag and remove VMs, grow, make, attach, detach, snapshot and delete disks, make, attach and delete public\s+addresses, and make, change and delete network security groups, and nothing else/)).toBeVisible()
  await expect(page.getByRole('button', { name: /Copy the command/ })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/09-allow-changes.png` })
  await page.getByRole('button', { name: 'Cancel' }).click()

  await page.getByRole('button', { name: 'Disconnect' }).nth(2).click()
  await expect(page.getByText('Disconnecting makes no call to Azure. Argus only forgets the subscription.')).toBeVisible()
  await expect(page.getByText(/until someone removes them under Access control \(IAM\)/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/06-disconnect.png` })
})

test('an Azure VM is launched into a resource group, with one security group and no lock', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await page.route('**/api/v1/cloud/accounts', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback()
    const res = await route.fetch()
    const body = await res.json()
    body.accounts = body.accounts.map((a: { provider: string }) => a.provider === 'azure'
      ? { ...a, access: 'manage', actions: ['start', 'stop', 'reboot', 'resize', 'tags', 'remove', 'launch'], tagKinds: ['instance'], manageMissing: [] }
      : a)
    await route.fulfill({ response: res, json: body })
  })
  const base = { accountId: 'ca3', provider: 'azure', providerAccount: '6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90', accountName: 'billing-azure', environment: 'production',
    firstSeen: '2026-11-14T08:00:00Z', seenAt: new Date().toISOString(), madeByArgus: false }
  let asks = 0
  await page.route('**/api/v1/cloud/launch-options?*', (r) => {
    asks++
    return r.fulfill({ json: {
      images: [{ family: 'ubuntu-24.04', label: 'Ubuntu 24.04 LTS', id: 'Canonical:ubuntu-24_04-lts:server', name: 'ubuntu-24_04-lts server', linux: true, minDiskGb: 30 },
        { family: 'own', label: 'win-base (this subscription)', id: 'image/Web-RG/win-base', name: 'win-base', linux: false, minDiskGb: 127 }],
      sizes: ['Standard_B1s', 'Standard_B2s', 'Standard_D2s_v5'],
      subnets: [{ ...base, resource: { kind: 'subnet', id: 'web-rg/main/web', name: 'web', region: 'southeastasia', size: '10.0.1.0/24', network: 'n1' } }],
      firewalls: [{ ...base, resource: { kind: 'firewall', id: 'nsg-1', name: 'web-nsg', region: 'southeastasia' } },
        { ...base, resource: { kind: 'firewall', id: 'nsg-2', name: 'db-nsg', region: 'southeastasia' } }],
      zones: ['southeastasia-1', 'southeastasia-2', 'southeastasia-3'], groups: ['Data-RG', 'Web-RG'], environment: 'production', needsApproval: true } })
  })
  const asked: Record<string, unknown>[] = []
  await page.route('**/api/v1/cloud/changes', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = route.request().postDataJSON() as Record<string, unknown>
    asked.push(body)
    await route.fulfill({ status: 202, json: { id: 'c1', state: 'awaiting_approval', summary: 'Launch api-1', action: body.action } })
  })
  await signIn(page)
  await page.goto('/cloud/launch')
  await page.getByRole('combobox', { name: 'Account' }).click()
  await page.getByRole('option', { name: 'billing-azure' }).click()
  await expect(page.getByText(/A new Azure Linux VM with no managed identity and no password/)).toBeVisible()
  await expect.poll(() => asks).toBeGreaterThan(0)
  await page.getByRole('textbox', { name: 'Name' }).fill('api-1')
  await page.getByRole('combobox', { name: 'Resource group' }).click()
  await page.getByRole('option', { name: 'Web-RG' }).click()
  await expect(page.getByRole('combobox', { name: 'VM size' })).toHaveValue('Standard_B2s')
  // Azure's own words, and nothing of the other clouds'.
  await expect(page.getByText('Network security group', { exact: true })).toBeVisible()
  await expect(page.getByText(/a resource lock, which Argus cannot set/)).toBeVisible()
  await expect(page.getByText('Use Spot capacity')).toHaveCount(0)
  await expect(page.getByText('Termination protection')).toHaveCount(0)
  // One security group: picking a second replaces the first.
  await page.getByRole('checkbox', { name: /db-nsg/ }).click()
  await page.getByRole('checkbox', { name: /web-nsg/ }).click()
  await page.getByRole('combobox', { name: 'Zone' }).click()
  await page.getByRole('option', { name: 'southeastasia-2' }).click()
  // A Windows image is not launched from here.
  await page.getByText('win-base (this subscription)').click()
  await expect(page.getByText(/Argus launches Linux VMs in Azure for now/)).toBeVisible()
  await page.getByLabel('Why').fill('A new API node')
  await expect(page.getByRole('button', { name: 'Ask for approval' })).toBeDisabled()
  await page.getByText('Ubuntu 24.04 LTS').first().click()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/10-launch-vm.png`, fullPage: true })
  await page.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked.length).toBe(1)
  expect(asked[0]).toMatchObject({ accountId: 'ca3', region: 'southeastasia', action: 'launch',
    spec: { name: 'api-1', group: 'Web-RG', size: 'Standard_B2s', zone: 'southeastasia-2', subnet: 'web-rg/main/web', firewalls: ['nsg-1'],
      image: 'Canonical:ubuntu-24_04-lts:server', protect: false, agent: true } })
})
