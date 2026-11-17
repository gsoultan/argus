import { test, expect, type Page } from '@playwright/test'

/**
 * Connecting a Google Cloud project, against e2e/authstub.mjs: the same two
 * steps as an AWS account, with one command for Cloud Shell and nothing
 * copied back. Read only by default; allowing changes makes argus-manage
 * too, and the console offers only the changes built for Google so far.
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

test('allowing changes in a project makes argus-manage with the same command', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud/connect')
  await page.locator('label', { hasText: 'Google Cloud' }).click()
  await page.getByLabel(/Name in Argus/).fill('payments-gcp')
  await page.getByLabel(/Project id/).fill('payments-prod-418203')
  await page.getByText('Read and change').click()
  await expect(page.getByText(/start, stop and reset VMs, change their machine type, labels and deletion protection/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/06-read-and-change.png`, fullPage: true })
  await page.getByRole('button', { name: 'Next: give Argus access' }).click()
  await expect(page.getByText('Argus needs two service accounts in payments-prod-418203')).toBeVisible()
  await expect(page.getByText(/argus-read and argus-manage work/)).toBeVisible()
  if (SHOTS) {
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/07-two-accounts.png`, fullPage: true })
  }
})

test('a project sits beside AWS accounts and can be allowed changes', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud')
  const row = page.getByText('analytics-prod', { exact: true }).locator('xpath=ancestor::div[contains(@style,"border-top")][1]')
  await expect(row.getByText('Google Cloud')).toBeVisible()
  await expect(row.getByText('analytics-prod-4182 · 2 regions')).toBeVisible()
  await expect(row.getByRole('button', { name: 'Change identity' })).toHaveCount(0)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/05-accounts.png`, fullPage: true })
  await row.getByRole('button', { name: 'Allow changes' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByText(/second service account, argus-manage/)).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Copy the command' })).toBeVisible()
  await expect(dialog.getByText(/Once the command says Done/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/08-allow-changes.png` })
})

/** A project with changes allowed, and one of its disks, answered in this test only. */
async function manageableProject(page: Page) {
  await page.route('**/api/v1/cloud/accounts', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback()
    const res = await route.fetch()
    const body = await res.json()
    body.accounts = body.accounts.map((a: { provider: string }) => a.provider === 'gcp'
      ? { ...a, access: 'manage', manageRoleArn: 'argus-manage@analytics-prod-4182.iam.gserviceaccount.com', actions: ['start', 'stop', 'reboot', 'resize', 'tags', 'protect', 'unprotect', 'volume-grow'], tagKinds: ['instance', 'volume'] }
      : a)
    await route.fulfill({ response: res, json: body })
  })
  await page.route('**/api/v1/cloud/resources?kind=*', async (route) => {
    const kind = new URL(route.request().url()).searchParams.get('kind')
    const base = { accountId: 'ca2', provider: 'gcp', providerAccount: 'analytics-prod-4182', accountName: 'analytics-prod', environment: 'production',
      firstSeen: '2026-11-14T08:00:00Z', seenAt: new Date().toISOString(), madeByArgus: false }
    const items = kind === 'volume'
      ? [{ ...base, resource: { kind: 'volume', id: '4410', name: 'etl-data', region: 'us-central1', zone: 'us-central1-a', state: 'in-use',
          size: '200 GiB', details: { type: 'pd-balanced' }, links: [{ kind: 'instance', id: '3301', name: 'etl-1' }] } }]
      : kind === 'instance'
        ? [{ ...base, resource: { kind: 'instance', id: '3301', name: 'etl-1', region: 'us-central1', zone: 'us-central1-a', state: 'running',
            size: 'e2-standard-4', addresses: ['10.128.0.7'], tags: { team: 'data' } } },
          { ...base, resource: { kind: 'instance', id: '3302', name: 'etl-2', region: 'us-central1', zone: 'us-central1-a', state: 'stopped',
            size: 'e2-standard-4', addresses: ['10.128.0.8'], details: { deletionProtection: 'on' } } }]
        : []
    await route.fulfill({ json: { items } })
  })
}

test('a Google disk grows by size alone, and only built changes are offered', async ({ page }) => {
  await manageableProject(page)
  await signIn(page)
  await page.goto('/cloud?tab=disks')
  await page.getByText('etl-data').first().click()
  const drawer = page.getByRole('dialog')
  await expect(drawer.getByRole('button', { name: 'Grow' })).toBeVisible()
  for (const absent of ['Snapshot', 'Detach', 'Tags']) await expect(drawer.getByRole('button', { name: absent })).toHaveCount(0)
  await expect(drawer.getByRole('button', { name: 'Labels' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'New disk' })).toHaveCount(0)
  await drawer.getByRole('button', { name: 'Grow' }).click()
  const grow = page.getByRole('dialog', { name: /Grow etl-data/ })
  await expect(grow.getByLabel('Size, GB')).toHaveValue('200')
  await expect(grow.getByLabel('Type', { exact: true })).toHaveCount(0)
  await grow.getByLabel('Size, GB').fill('500')
  await expect(grow.getByText(/keeps its old size until it is grown too/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/09-grow-disk.png` })

  await page.goto('/cloud?tab=instances')
  await page.getByText('etl-1').first().click()
  const vm = page.getByRole('dialog')
  await expect(vm.getByRole('button', { name: 'Reset' })).toBeVisible()
  await expect(vm.getByRole('button', { name: 'Stop' })).toBeVisible()
  for (const absent of ['Change size', 'Tags', 'Boot log']) await expect(vm.getByRole('button', { name: absent })).toHaveCount(0)
  await expect(vm.getByRole('button', { name: 'Change machine type' })).toBeVisible()
  await vm.getByRole('button', { name: 'Reset' }).click()
  await expect(page.getByText(/no gentle reboot/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/10-reset.png` })
})

test('a Google VM changes machine type, labels and deletion protection', async ({ page }) => {
  await manageableProject(page)
  const asked: Record<string, unknown>[] = []
  await page.route('**/api/v1/cloud/instance-sizes?*', (route) => route.fulfill({ json: { sizes: ['e2-medium', 'e2-standard-4', 'n2-standard-4'] } }))
  await page.route('**/api/v1/cloud/changes', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = route.request().postDataJSON() as Record<string, unknown>
    asked.push(body)
    await route.fulfill({ json: { id: 'c' + asked.length, state: 'applied', summary: 'done', action: body.action } })
  })
  await signIn(page)

  // A running VM: Google changes the machine type of a stopped one only.
  await page.goto('/cloud?tab=instances')
  await page.getByText('etl-1').first().click()
  let vm = page.getByRole('dialog')
  await vm.getByRole('button', { name: 'Change machine type' }).click()
  await expect(page.getByText(/Google changes the machine type of a stopped VM only/)).toBeVisible()
  await page.getByRole('button', { name: 'Cancel' }).click()

  // Labels are lowercase, said as they are typed.
  await vm.getByRole('button', { name: 'Labels' }).click()
  const labels = page.getByRole('dialog', { name: /Labels on etl-1/ })
  await labels.getByRole('button', { name: 'Add a label' }).click()
  await labels.getByLabel('Label key').nth(1).fill('Owner')
  await expect(labels.getByText(/starting with a letter/)).toBeVisible()
  await expect(labels.getByRole('button', { name: /^Save/ })).toBeDisabled()
  await labels.getByLabel('Label key').nth(1).fill('owner')
  await labels.getByLabel('Label value').nth(1).fill('dana')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/11-labels.png` })
  await labels.getByRole('button', { name: 'Save 1 change' }).click()
  await expect.poll(() => asked.length).toBe(1)
  expect(asked[0]).toMatchObject({ action: 'tags', resourceId: '3301', spec: { set: { owner: 'dana' }, remove: [] } })

  // A stopped one: the zone's machine types, and protection the way it can go.
  await page.goto('/cloud?tab=instances')
  await page.getByText('etl-2').first().click()
  vm = page.getByRole('dialog')
  await vm.getByRole('button', { name: 'More actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Turn off deletion protection' })).toBeVisible()
  for (const absent of ['Turn on deletion protection', 'Remove instance…', 'Make an image…', 'Instance settings…', 'Launch another like this…']) {
    await expect(page.getByRole('menuitem', { name: absent })).toHaveCount(0)
  }
  await page.keyboard.press('Escape')
  await vm.getByRole('button', { name: 'Change machine type' }).click()
  const resize = page.getByRole('dialog', { name: /Change the machine type of etl-2/ })
  await resize.getByLabel('New machine type').click()
  await page.getByRole('option', { name: 'n2-standard-4' }).click()
  // A production VM: someone else approves, and is told why.
  await resize.getByLabel('Why').fill('The nightly job runs out of memory')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/12-machine-type.png` })
  await resize.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked.length).toBe(2)
  expect(asked[1]).toMatchObject({ action: 'resize', resourceId: '3302', spec: { size: 'n2-standard-4', from: 'e2-standard-4' } })
})
