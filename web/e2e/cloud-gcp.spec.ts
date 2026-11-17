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
      ? { ...a, access: 'manage', manageRoleArn: 'argus-manage@analytics-prod-4182.iam.gserviceaccount.com', actions: ['start', 'stop', 'reboot', 'resize', 'tags', 'protect', 'unprotect', 'volume-grow',
        'volume-create', 'volume-attach', 'volume-detach', 'volume-delete', 'snapshot-create', 'snapshot-delete',
        'address-allocate', 'address-associate', 'address-disassociate', 'address-release', 'remove'], tagKinds: ['instance', 'volume'] }
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
        : kind === 'snapshot'
          ? [{ ...base, resource: { kind: 'snapshot', id: '5501', name: 'etl-data-nightly', region: 'global', state: 'ready', size: '200 GiB' } }]
          : kind === 'address'
            ? [{ ...base, resource: { kind: 'address', id: '7701', name: 'etl-ip', region: 'us-central1', state: 'unattached', publicIp: '35.1.1.1', addresses: ['35.1.1.1'] } },
              { ...base, resource: { kind: 'address', id: '7702', name: 'etl-2-ip', region: 'us-central1', state: 'attached', publicIp: '35.2.2.2', addresses: ['35.2.2.2'],
                links: [{ kind: 'instance', id: '3302', name: 'etl-2' }] } }]
            : []
    await route.fulfill({ json: { items } })
  })
}

test('a Google disk grows by size alone, and is offered what Google does to disks', async ({ page }) => {
  await manageableProject(page)
  await signIn(page)
  await page.goto('/cloud?tab=disks')
  await page.getByText('etl-data').first().click()
  const drawer = page.getByRole('dialog')
  await expect(drawer.getByRole('button', { name: 'Grow' })).toBeVisible()
  for (const shown of ['Snapshot', 'Detach', 'Labels']) await expect(drawer.getByRole('button', { name: shown })).toBeVisible()
  await expect(drawer.getByRole('button', { name: 'Tags' })).toHaveCount(0)
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
  await expect(page.getByRole('menuitem', { name: 'Remove VM…' })).toBeVisible()
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

test('a Google disk is made from a snapshot, snapshotted, detached and deleted as Google does it', async ({ page }) => {
  await manageableProject(page)
  const asked: Record<string, unknown>[] = []
  await page.route('**/api/v1/cloud/changes', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = route.request().postDataJSON() as Record<string, unknown>
    asked.push(body)
    await route.fulfill({ json: { id: 'c' + asked.length, state: 'applied', summary: 'done', action: body.action } })
  })
  await signIn(page)
  await page.goto('/cloud?tab=disks')

  // A snapshot belongs to the whole project: its disk needs a region, a zone and a Compute Engine name.
  await page.getByText(/^Snapshots/).click()
  await page.getByText('etl-data-nightly').first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'New disk from it' }).click()
  const make = page.getByRole('dialog', { name: /New disk from etl-data-nightly/ })
  await expect(make.getByLabel('Size, GB')).toHaveValue('200')
  await make.getByLabel('Region').click()
  await page.getByRole('option', { name: 'us-central1', exact: true }).click()
  await make.getByLabel('Zone').click()
  await page.getByRole('option', { name: 'us-central1-a' }).click()
  await make.getByLabel('Name').fill('ETL data')
  await expect(make.getByText(/Lowercase letters, digits and -/)).toBeVisible()
  await make.getByLabel('Name').fill('etl-data')
  await expect(make.getByText(/already a disk etl-data in us-central1-a/)).toBeVisible()
  await make.getByLabel('Name').fill('etl-restore')
  await make.getByLabel('Size, GB').fill('100')
  await expect(make.getByText(/At least 200 GB, the snapshot's size/)).toBeVisible()
  await make.getByLabel('Size, GB').fill('250')
  await make.getByLabel('Attach to').click()
  await page.getByRole('option', { name: /etl-2 · as \/dev\/disk\/by-id\/google-etl-restore/ }).click()
  await expect(make.getByText(/Google bills it by size and type/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/13-new-disk.png` })
  await make.getByLabel('Why').fill('Restore last night for the audit')
  await make.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked.length).toBe(1)
  expect(asked[0]).toMatchObject({ action: 'volume-create', region: 'us-central1',
    spec: { name: 'etl-restore', zone: 'us-central1-a', sizeGb: 250, type: 'pd-balanced', snapshot: '5501', attachTo: '3302' } })

  // A snapshot of a disk: a Compute Engine name, or one Argus picks.
  await page.goto('/cloud?tab=disks')
  await page.getByText('etl-data', { exact: true }).first().click()
  let drawer = page.getByRole('dialog')
  await drawer.getByRole('button', { name: 'Snapshot' }).click()
  const snap = page.getByRole('dialog', { name: /Snapshot etl-data/ })
  await expect(snap.getByText(/Google copies the disk as it is now/)).toBeVisible()
  await snap.getByLabel('Name').fill('Nightly 1')
  const take = snap.getByRole('button', { name: /Take snapshot|Ask for approval/ })
  await snap.getByLabel('Why').fill('Before the upgrade')
  await expect(take).toBeDisabled()
  await snap.getByLabel('Name').fill('')
  await take.click()
  await expect.poll(() => asked.length).toBe(2)
  expect(asked[1]).toMatchObject({ action: 'snapshot-create', resourceId: '4410', spec: { name: '' } })

  // Google detaches at once, so the dialog says so.
  await page.goto('/cloud?tab=disks')
  await page.getByText('etl-data', { exact: true }).first().click()
  drawer = page.getByRole('dialog')
  await drawer.getByRole('button', { name: 'Detach' }).click()
  await expect(page.getByText(/Google detaches the disk at once/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/14-detach.png` })
  await page.getByRole('button', { name: 'Cancel' }).click()

  // Deleting a snapshot: no final-snapshot box, which Google does as its own change.
  await page.goto('/cloud?tab=disks')
  await page.getByText(/^Snapshots/).click()
  await page.getByText('etl-data-nightly').first().click()
  drawer = page.getByRole('dialog')
  await drawer.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Delete snapshot…' }).click()
  const del = page.getByRole('dialog', { name: /Delete etl-data-nightly/ })
  await expect(del.getByText(/Google deletes the snapshot/)).toBeVisible()
  await del.getByLabel(/to confirm/).fill('etl-data-nightly')
  await del.getByLabel('Why').fill('Kept a newer one')
  await del.getByRole('button', { name: /Ask for approval/ }).click()
  await expect.poll(() => asked.length).toBe(3)
  expect(asked[2]).toMatchObject({ action: 'snapshot-delete', region: 'global', resourceId: '5501', confirm: 'etl-data-nightly' })
})

test('a Google static address is reserved, assigned, taken off and released', async ({ page }) => {
  await manageableProject(page)
  const asked: Record<string, unknown>[] = []
  await page.route('**/api/v1/cloud/changes', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = route.request().postDataJSON() as Record<string, unknown>
    asked.push(body)
    await route.fulfill({ json: { id: 'c' + asked.length, state: 'applied', summary: 'done', action: body.action } })
  })
  await signIn(page)
  await page.goto('/cloud?tab=network')

  // Reserving: a Compute Engine name, in a region, and no AWS price.
  await page.getByRole('button', { name: 'Reserve a static address' }).click()
  const make = page.getByRole('dialog', { name: /Reserve a static address/ })
  await make.getByLabel('Region').click()
  await page.getByRole('option', { name: 'us-central1', exact: true }).click()
  await make.getByLabel('Name').fill('ETL IP')
  await expect(make.getByText(/Lowercase letters, digits and -/)).toBeVisible()
  await make.getByLabel('Name').fill('etl-ip')
  await expect(make.getByText(/already an address etl-ip in us-central1/)).toBeVisible()
  await make.getByLabel('Name').fill('etl-3-ip')
  await expect(make.getByText(/Google bills a static address/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/15-reserve-address.png` })
  await make.getByLabel('Why').fill('A fixed address for the partner allow list')
  await make.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked.length).toBe(1)
  expect(asked[0]).toMatchObject({ action: 'address-allocate', region: 'us-central1', spec: { name: 'etl-3-ip' } })

  // Assigning one to a VM.
  await page.goto('/cloud?tab=network')
  await page.getByText(/^Elastic IPs/).click()
  await page.getByText('35.1.1.1').first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'Attach to an instance' }).click()
  const attach = page.getByRole('dialog', { name: /Attach 35.1.1.1/ })
  await attach.getByLabel('Instance').click()
  await page.getByRole('option', { name: /etl-1/ }).click()
  await attach.getByLabel('Why').fill('Partner allow list')
  await attach.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked.length).toBe(2)
  expect(asked[1]).toMatchObject({ action: 'address-associate', resourceId: '7701', spec: { instance: '3301' } })

  // Taking one off says the VM is left without a public address.
  await page.goto('/cloud?tab=network')
  await page.getByText(/^Elastic IPs/).click()
  await page.getByText('35.2.2.2').first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'Take it off' }).click()
  await expect(page.getByText(/has no public address until it is given one/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/16-unassign-address.png` })
  await page.getByRole('button', { name: 'Cancel' }).click()

  // Releasing goes back to Google.
  await page.goto('/cloud?tab=network')
  await page.getByText(/^Elastic IPs/).click()
  await page.getByText('35.1.1.1').first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Release address…' }).click()
  const rel = page.getByRole('dialog', { name: /Release 35.1.1.1/ })
  await expect(rel.getByText(/goes back to Google/)).toBeVisible()
  await rel.getByLabel(/to confirm/).fill('35.1.1.1')
  await rel.getByLabel('Why').fill('Not needed any more')
  await rel.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked.length).toBe(3)
  expect(asked[2]).toMatchObject({ action: 'address-release', resourceId: '7701', confirm: '35.1.1.1' })
})

test('a Google VM is removed with the disks Google deletes with it', async ({ page }) => {
  await manageableProject(page)
  const asked: Record<string, unknown>[] = []
  await page.route('**/api/v1/cloud/remove-plan?*', (route) => route.fulfill({ json: {
    plan: { id: '3301', name: 'etl-1', state: 'running', size: 'e2-standard-4', protected: false, publicIp: '34.1.2.3', volumes: [
      { id: '4409', device: 'persistent-disk-0', sizeGb: 20, deleted: true, description: 'etl-1 (boot)' },
      { id: '4410', device: 'data', sizeGb: 200, deleted: false, description: 'etl-data' }] },
    hash: 'h1', environment: 'production', needsApproval: true } }))
  await page.route('**/api/v1/cloud/changes', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = route.request().postDataJSON() as Record<string, unknown>
    asked.push(body)
    await route.fulfill({ status: 202, json: { id: 'c1', state: 'awaiting_approval', summary: 'Remove etl-1', action: body.action } })
  })
  await signIn(page)
  await page.goto('/cloud?tab=instances')
  await page.getByText('etl-1').first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Remove VM…' }).click()
  const rm = page.getByRole('dialog', { name: /Remove etl-1/ })
  await expect(rm.getByText(/Google deletes the VM/)).toBeVisible()
  await expect(rm.getByText('etl-data')).toBeVisible()
  await expect(rm.getByText(/Argus waits for each snapshot to finish/)).toBeVisible()
  await rm.getByLabel(/to confirm/).fill('etl-1')
  await rm.getByLabel('Why').fill('Replaced by etl-3')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/17-remove-vm.png` })
  await rm.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked.length).toBe(1)
  expect(asked[0]).toMatchObject({ action: 'remove', resourceId: '3301', confirm: 'etl-1', spec: { finalSnapshots: true } })
})

test('a Google VM is launched in a zone, from an image family, with firewall rules by tag', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await manageableProject(page)
  const base = { accountId: 'ca2', provider: 'gcp', providerAccount: 'analytics-prod-4182', accountName: 'analytics-prod', environment: 'production',
    firstSeen: '2026-11-14T08:00:00Z', seenAt: new Date().toISOString(), madeByArgus: false }
  await page.route('**/api/v1/cloud/launch-options?*', (r) => r.fulfill({ json: {
    images: [{ family: 'ubuntu-24', label: 'Ubuntu 24.04 LTS', id: 'projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64', name: 'ubuntu-2404', linux: true }],
    sizes: ['e2-small', 'e2-medium', 'e2-standard-2', 'n2-standard-2'],
    subnets: [{ ...base, resource: { kind: 'subnet', id: '601', name: 'default', region: 'us-central1', details: { network: 'default', cidr: '10.128.0.0/20' } } }],
    firewalls: [{ ...base, resource: { kind: 'firewall', id: '901', name: 'allow-http', region: 'global', details: { network: 'default' } } }],
    zones: ['us-central1-a', 'us-central1-b'], environment: 'production', needsApproval: true } }))
  const asked: Record<string, unknown>[] = []
  await page.route('**/api/v1/cloud/changes', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = route.request().postDataJSON() as Record<string, unknown>
    asked.push(body)
    await route.fulfill({ status: 202, json: { id: 'c1', state: 'awaiting_approval', summary: 'Launch web-2', action: body.action } })
  })
  await signIn(page)
  await page.goto('/cloud/launch')
  await page.getByRole('combobox', { name: 'Account' }).click()
  await page.getByRole('option', { name: 'analytics-prod' }).click()
  await page.getByRole('combobox', { name: 'Region' }).click()
  await page.getByRole('option', { name: 'us-central1' }).click()
  await expect(page.getByText(/A new Compute Engine VM with no service account/)).toBeVisible()
  await expect(page.getByText('Firewall rules', { exact: true }).first()).toBeVisible()
  await page.getByRole('textbox', { name: 'Name' }).fill('web-2')
  await page.getByRole('combobox', { name: 'Zone' }).click()
  await page.getByRole('option', { name: 'us-central1-b' }).click()
  await page.getByRole('combobox', { name: 'Machine type' }).click()
  await page.getByRole('option', { name: 'e2-standard-2' }).click()
  await page.getByRole('checkbox', { name: /allow-http/ }).click()
  await page.getByLabel('Why').fill('A second web node')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/18-launch-vm.png` })
  await page.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked.length).toBe(1)
  expect(asked[0]).toMatchObject({ accountId: 'ca2', region: 'us-central1', action: 'launch',
    spec: { name: 'web-2', size: 'e2-standard-2', zone: 'us-central1-b', subnet: '601', firewalls: ['901'],
      image: 'projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64' } })
})
