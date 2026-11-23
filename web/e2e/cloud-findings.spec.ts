import { test, expect, type Page } from '@playwright/test'

/**
 * Cloud › Risks & waste, against e2e/authstub.mjs with the findings and the
 * inventory they point at served by the test: findings are grouped and say
 * how Argus knows; a firewall fix opens the usual rules change with the rule
 * already struck; keeping a High risk cannot be open-ended.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS
const now = Date.now()
const ago = (d: number) => new Date(now - d * 86_400_000).toISOString()

const base = { accountId: 'ca1', accountName: 'payments-prod', environment: 'production', region: 'us-east-1', basis: 'b' }
const SSH = { inbound: true, protocol: 'tcp', fromPort: 22, toPort: 22, source: '0.0.0.0/0' }

const FINDINGS = [
  { ...base, id: 'open:sg-web:tcp/22-22/0.0.0.0/0', check: 'rule-open', group: 'risk', severity: 'high', source: 'cloud', kind: 'firewall',
    resourceId: 'sg-web', name: 'sg-web-public', title: 'SSH open to the whole internet',
    why: 'sg-web-public lets 0.0.0.0/0 reach SSH (22). Used by web-01 and web-02.',
    matters: 'Anyone on the internet can try to reach it. Every instance behind it has the Argus agent, so nobody needs this rule to sign in through Argus.',
    fix: 'firewall-rules', rule: SSH, related: [{ kind: 'instance', id: 'i-1', name: 'web-01' }, { kind: 'instance', id: 'i-2', name: 'web-02' }] },
  { ...base, region: 'global', id: 'root-no-mfa:412788931204', check: 'root-no-mfa', group: 'risk', severity: 'high', source: 'cloud', kind: 'account',
    resourceId: '412788931204', name: 'Root user', title: 'Root user has no MFA', why: "The account's root user signs in with a password alone.",
    matters: 'Root can do anything in the account, including close it. A leaked password is enough.',
    howToFix: 'Sign in as root and add an MFA device under Security credentials.' },
  { ...base, id: 'imds-v1:i-2', check: 'imds-v1', group: 'risk', severity: 'medium', source: 'cloud', kind: 'instance', resourceId: 'i-2', name: 'web-02',
    title: 'Old instance metadata allowed', why: 'web-02 still answers metadata requests without a token (IMDSv1).',
    howToFix: 'Require tokens (IMDSv2).', fix: 'settings' },
  { ...base, region: 'global', id: 'user-no-mfa:arn:aws:iam::412788931204:user/ana', check: 'user-no-mfa', group: 'risk', severity: 'medium', source: 'cloud',
    kind: 'user', resourceId: 'arn:aws:iam::412788931204:user/ana', name: 'ana', title: 'Person signs in without MFA',
    why: 'IAM user ana has a console password and no MFA device.', howToFix: 'Add an MFA device for the user.' },
  { ...base, environment: 'staging', accountName: 'analytics', accountId: 'ca2', id: 'instance-idle:i-9', check: 'instance-idle', group: 'waste', severity: 'medium',
    source: 'judgement', kind: 'instance', resourceId: 'i-9', name: 'staging-worker-3', title: 'Running, but barely used',
    why: "Processor at most 1.2% for 95% of the time, no Argus session for 34 days, over the agent's last 30 days of readings.", monthly: 60.74, fix: 'stop' },
  { ...base, id: 'idle-snapshot:snap-1', check: 'idle-snapshot', group: 'waste', severity: 'low', source: 'cloud', kind: 'snapshot', resourceId: 'snap-0a14f1', name: 'db-primary nightly',
    title: 'Snapshot of a disk that is gone', why: '412 days old, of a disk that is gone, and no image uses it.', monthly: 4, atMost: true, fix: 'snapshot-delete', since: ago(412) },
  { ...base, id: 'idle-snapshot:snap-2', check: 'idle-snapshot', group: 'waste', severity: 'low', source: 'cloud', kind: 'snapshot', resourceId: 'snap-03caa1', name: 'before resize',
    title: 'Snapshot of a disk that is gone', why: '211 days old, of a disk that is gone, and no image uses it.', monthly: 10, atMost: true, fix: 'snapshot-delete', since: ago(211) },
  { ...base, id: 'quiet:sg-api:tcp/8080-8080/10.20.0.0/16', check: 'rule-nothing-listens', group: 'rules', severity: 'low', source: 'agent', kind: 'firewall',
    resourceId: 'sg-api', name: 'sg-pay-api', title: 'Port open but nothing listens',
    why: 'sg-pay-api lets 10.20.0.0/16 reach 8080. The agent on all 3 instances sees nothing listening there.', fix: 'firewall-rules',
    rule: { inbound: true, protocol: 'tcp', fromPort: 8080, toPort: 8080, source: '10.20.0.0/16' } },
  { ...base, id: 'group-unused:sg-old', check: 'group-unused', group: 'rules', severity: 'low', source: 'cloud', kind: 'firewall', resourceId: 'sg-old', name: 'sg-old-bastion',
    title: 'Security group used by nothing', why: 'No instance, load balancer, database or other network interface uses sg-old-bastion, and no other group\'s rule names it.',
    fix: 'firewall-delete' },
]

const VIEW = {
  items: FINDINGS, kept: [], monthly: 74.74, checkedAt: new Date(now - 60_000).toISOString(),
  accounts: [{ id: 'ca1', name: 'payments-prod', accountChecks: true }, { id: 'ca2', name: 'analytics', accountChecks: false }],
}

const item = (kind: string, id: string, name: string, extra: object = {}) => ({
  accountId: 'ca1', accountName: 'payments-prod', environment: 'production', firstSeen: ago(30), seenAt: ago(0), madeByArgus: false,
  resource: { kind, id, name, region: 'us-east-1', ...extra },
})
const RESOURCES: Record<string, object[]> = {
  firewall: [item('firewall', 'sg-web', 'sg-web-public', { network: 'vpc-1', rules: [SSH, { inbound: true, protocol: 'tcp', fromPort: 443, toPort: 443, source: '0.0.0.0/0' },
    { inbound: false, protocol: 'all', fromPort: 0, toPort: 65535, source: '0.0.0.0/0' }] })],
  instance: [item('instance', 'i-2', 'web-02', { state: 'running', size: 't3.medium', network: 'vpc-1',
    links: [{ kind: 'subnet', id: 'subnet-b' }, { kind: 'firewall', id: 'sg-web', name: 'sg-web-public' }], details: { image: 'ami-ubuntu24', metadataTokens: 'optional', hibernation: 'yes', systemStatus: 'ok', instanceStatus: 'impaired',
    eventCode: 'system-reboot', eventDescription: 'Scheduled reboot for host maintenance', eventNotBefore: new Date(now + 4 * 86_400_000).toISOString() } })],
  snapshot: [item('snapshot', 'snap-0a14f1', 'db-primary nightly', { size: '80 GiB' }), item('snapshot', 'snap-03caa1', 'before resize', { size: '200 GiB' })],
}

async function stub(page: Page) {
  await page.route('**/api/v1/cloud/findings', (r) => r.fulfill({ json: VIEW }))
  await page.route('**/api/v1/cloud/resources?*', (r) => {
    const kind = new URL(r.request().url()).searchParams.get('kind') ?? ''
    return r.fulfill({ json: { items: RESOURCES[kind] ?? [] } })
  })
  await page.route('**/api/v1/cloud/accounts', async (r) => {
    const res = await r.fetch()
    const body = await res.json()
    body.accounts = body.accounts.map((a: { access: string }) => ({ ...a, access: 'manage', manageRoleArn: 'arn:aws:iam::412788931204:role/ArgusManage' }))
    return r.fulfill({ response: res, json: body })
  })
}

async function signIn(page: Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
}

test('findings are grouped, say how Argus knows, and fix through the usual change', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await stub(page)
  await signIn(page)
  await page.goto('/cloud?tab=risks')

  await expect(page.getByRole('tab', { name: /Risks & waste/ })).toContainText('2 high')
  await expect(page.getByText('SSH open to the whole internet')).toBeVisible()
  await expect(page.getByRole('cell').filter({ hasText: 'Not in use · $74.74 a month' })).toBeVisible()
  await expect(page.getByText("Argus's judgement")).toBeVisible()
  await expect(page.getByText(/have not been read yet/)).toContainText('analytics')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/01-list.png`, fullPage: true })

  await page.getByText('SSH open to the whole internet').click()
  const drawer = page.getByRole('dialog')
  await expect(drawer.getByText('Why it matters')).toBeVisible()
  if (SHOTS) {
    await page.waitForTimeout(400) // the drawer slides in
    await page.screenshot({ path: `${SHOTS}/02-risk.png` })
  }
  await drawer.getByRole('button', { name: 'Ask to remove this rule' }).click()

  const modal = page.getByRole('dialog', { name: /Rules of sg-web-public/ })
  await expect(modal.getByText('− tcp 22 from anywhere')).toBeVisible()
  await expect(modal.getByRole('button', { name: 'Ask for approval' })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/03-remove-rule.png` })
  await modal.getByRole('button', { name: 'Cancel' }).click()
})

test('a High risk is kept for at most 90 days', async ({ page }) => {
  await stub(page)
  await signIn(page)
  await page.goto('/cloud?tab=risks')
  const row = page.getByRole('row').filter({ hasText: 'Root user has no MFA' })
  await row.getByRole('button', { name: 'Keep it' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByRole('radio', { name: 'Until it changes' })).toBeDisabled()
  await expect(dialog.getByRole('button', { name: 'Keep it' })).toBeDisabled()
  await dialog.getByLabel('Why').fill('Root is locked in the safe; MFA device on order')
  await expect(dialog.getByRole('button', { name: 'Keep it' })).toBeEnabled()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/04-keep.png` })
})

test('old snapshots are deleted together, one change each', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await stub(page)
  const asked: string[] = []
  await page.route('**/api/v1/cloud/changes', async (r) => {
    if (r.request().method() !== 'POST') return r.fallback()
    const body = r.request().postDataJSON()
    asked.push(body.resourceId)
    return r.fulfill({ status: 201, json: { id: 'c' + asked.length, state: 'awaiting_approval', summary: 'Delete ' + body.resourceId } })
  })
  await signIn(page)
  await page.goto('/cloud?tab=risks')
  await page.getByLabel('Pick db-primary nightly').check()
  await page.getByLabel('Pick before resize').check()
  await page.getByRole('button', { name: 'Delete 2…' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Why').fill('Disks are long gone; nothing restores from these')
  await dialog.getByLabel(/to confirm/).fill('delete 2 snapshots')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/05-snapshots.png` })
  await dialog.getByRole('button', { name: 'Ask to delete 2 snapshots' }).click()
  await expect.poll(() => asked).toEqual(['snap-0a14f1', 'snap-03caa1'])
})

test('phone: one card per finding, and the Overview shows the High ones', async ({ page }) => {
  await stub(page)
  await signIn(page)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/cloud?tab=risks')
  await expect(page.getByText('SSH open to the whole internet')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/06-list-phone.png`, fullPage: true })
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.goto('/cloud')
  await expect(page.getByText('See all 9 in Risks & waste')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/07-overview.png`, fullPage: true })
})

// The IMDSv1 finding fixes through the instance's settings, with tokens
// already turned on, and asks for only that one change.
test('old instance metadata is fixed from its finding', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await stub(page)
  await page.route('**/api/v1/cloud/instance-settings?*', (r) => r.fulfill({ json: {
    terminationProtect: true, stopProtect: false, requireTokens: false, hibernation: true, creditMode: 'standard' } }))
  let asked: Record<string, unknown> | null = null
  await page.route('**/api/v1/cloud/changes', async (r) => {
    if (r.request().method() !== 'POST') return r.fallback()
    asked = r.request().postDataJSON()
    return r.fulfill({ status: 202, json: { id: 'c1', state: 'awaiting_approval', summary: 'Require tokens for instance metadata (IMDSv2) on web-02' } })
  })
  await signIn(page)
  await page.goto('/cloud?tab=risks')
  await page.getByText('Old instance metadata allowed').click()
  await page.getByRole('dialog').getByRole('button', { name: 'Require tokens' }).click()
  const modal = page.getByRole('dialog', { name: /Settings of web-02/ })
  await expect(modal.getByRole('switch', { name: /Require tokens/ })).toBeChecked()
  await expect(modal.getByRole('switch', { name: 'Stop protection' })).not.toBeChecked()
  await expect(modal.getByText('t3.medium is burstable')).toBeVisible()
  await modal.getByLabel('Why').fill('Finding: IMDSv1 still allowed')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/08-imds-fix.png` })
  await modal.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked).toMatchObject({ action: 'settings', resourceId: 'i-2', spec: { requireTokens: true } })
  expect(Object.keys((asked as unknown as { spec: object }).spec)).toEqual(['requireTokens'])
})

// An instance AWS says is failing shows it, with planned maintenance, and
// its boot log is read only when asked for.
test('an instance failing its checks shows its boot log', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await stub(page)
  let reads = 0
  await page.route('**/api/v1/cloud/console-output?*', (r) => {
    reads++
    return r.fulfill({ json: { text: '[  OK  ] Mounted /boot.\n[FAILED] Failed to mount /data.\nYou are in emergency mode.\n', at: new Date(now - 120_000).toISOString() } })
  })
  await signIn(page)
  await page.goto('/cloud?tab=instances')
  await page.getByText('web-02').first().click()
  const health = page.getByTestId('instance-health')
  await expect(health.getByText('Failing')).toBeVisible()
  await expect(health.getByText(/Scheduled reboot for host maintenance/)).toBeVisible()
  expect(reads).toBe(0)
  await health.getByRole('button', { name: 'Boot log' }).click()
  await expect(page.getByTestId('boot-log')).toContainText('You are in emergency mode.')
  expect(reads).toBe(1)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/09-boot-log.png` })
})

// Launching another like an instance copies where and how it runs; three at
// once number on from its name, with extra disks and Spot.
test('launch three more like an instance', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await stub(page)
  const subnet = (id: string, name: string) => ({ ...item('subnet', id, name, { network: 'vpc-1', size: '10.0.1.0/24', zone: 'us-east-1a' }) })
  await page.route('**/api/v1/cloud/launch-options?*', (r) => r.fulfill({ json: {
    images: [{ family: 'ubuntu-22', label: 'Ubuntu 22.04', id: 'ami-ubuntu22', name: 'u22', linux: true },
      { family: 'ubuntu-24', label: 'Ubuntu 24.04', id: 'ami-ubuntu24', name: 'u24', linux: true }],
    sizes: ['t3.small', 't3.medium', 'm7i.large'], subnets: [subnet('subnet-a', 'private-a'), subnet('subnet-b', 'private-b')],
    firewalls: RESOURCES.firewall, environment: 'production', needsApproval: true } }))
  let asked: Record<string, unknown> | null = null
  await page.route('**/api/v1/cloud/changes', async (r) => {
    if (r.request().method() !== 'POST') return r.fallback()
    asked = r.request().postDataJSON()
    return r.fulfill({ status: 202, json: { id: 'c2', state: 'awaiting_approval', summary: 'Launch 3 instances, web-03 to web-05' } })
  })
  await signIn(page)
  await page.goto('/cloud?tab=instances')
  await page.getByText('web-02').first().click()
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: /Launch another like this/ }).click()
  await expect(page.getByTestId('launch-like')).toContainText('Like web-02')
  await expect(page.getByRole('textbox', { name: 'Name' })).toHaveValue('web-03')
  await page.getByRole('textbox', { name: 'How many' }).fill('3')
  await expect(page.getByTestId('launch-names')).toContainText('web-03, web-04, web-05')
  await page.getByRole('button', { name: 'Add a disk' }).click()
  await page.getByRole('switch', { name: 'Use Spot capacity' }).click()
  await expect(page.getByRole('switch', { name: 'Termination protection' })).toBeDisabled()
  await page.getByLabel('Why').fill('Three more web nodes for the sale')
  if (SHOTS) {
    await page.evaluate(() => window.scrollTo(0, 0))
    await page.screenshot({ path: `${SHOTS}/10-launch-like.png` })
  }
  await page.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => asked).toMatchObject({ action: 'launch', spec: {
    name: 'web-03', count: 3, extraDisks: [100], spot: true, protect: false, size: 't3.medium', image: 'ami-ubuntu24', subnet: 'subnet-b', firewalls: ['sg-web'] } })
})

/** Serves POSTed changes back on GET, as the control plane lists them. */
async function changes(page: Page) {
  const asked: Record<string, unknown>[] = []
  await page.route('**/api/v1/cloud/changes*', async (r) => {
    if (r.request().method() === 'POST') {
      const body = r.request().postDataJSON()
      const c = { id: 'c' + (asked.length + 1), accountId: body.accountId, accountName: 'payments-prod', region: body.region, action: body.action,
        level: 'change', environment: 'production', resourceId: body.resourceId, resourceName: body.resourceId, spec: body.spec ?? {}, plan: {},
        planHash: '', summary: `Change rules on ${body.resourceId}`, reason: body.reason ?? '', state: 'awaiting_approval', requestedBy: 'dev',
        requestedAt: new Date().toISOString(), decidedBy: null, decidedAt: null, appliedAt: null, requestId: '', result: {}, error: '', confirm: body.confirm }
      asked.push(c)
      return r.fulfill({ status: 202, json: c })
    }
    return r.fulfill({ json: { changes: asked } })
  })
  return asked
}

test('removing a rule from the list asks for it once, and the row says so', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await stub(page)
  const asked = await changes(page)
  await signIn(page)
  await page.goto('/cloud?tab=risks')
  const row = page.getByRole('row').filter({ hasText: 'SSH open to the whole internet' })
  await row.getByRole('button', { name: 'Remove the rule' }).click()

  const modal = page.getByRole('dialog', { name: /Rules of sg-web-public/ })
  await expect(modal.getByText('− tcp 22 from anywhere')).toBeVisible()
  await modal.getByLabel('Why').fill('Everyone signs in through Argus now')
  await modal.getByRole('button', { name: 'Ask for approval' }).click()
  await expect(modal).toBeHidden()

  expect(asked).toHaveLength(1)
  expect(asked[0]).toMatchObject({ action: 'firewall-rules', resourceId: 'sg-web', spec: { add: [], remove: [SSH] } })
  // The row waits for the approval instead of offering the same change again.
  await expect(row.getByText('Waiting for approval')).toBeVisible()
  await expect(row.getByRole('button', { name: 'Remove the rule' })).toHaveCount(0)
  await page.reload()
  await expect(page.getByRole('row').filter({ hasText: 'SSH open to the whole internet' }).getByText('Waiting for approval')).toBeVisible()
})

test("a Google firewall rule's only allow is removed by deleting the rule", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  const gcp = { accountId: 'ca2', accountName: 'web-gcp', environment: 'development' as const, region: 'global' }
  const finding = { ...FINDINGS[0], ...gcp, id: 'open:901:tcp/22-22/0.0.0.0/0', resourceId: '901', name: 'allow-ssh' }
  await page.route('**/api/v1/cloud/findings', (r) => r.fulfill({ json: { ...VIEW, items: [finding] } }))
  await page.route('**/api/v1/cloud/resources?*', (r) => {
    const kind = new URL(r.request().url()).searchParams.get('kind') ?? ''
    return r.fulfill({ json: { items: kind === 'firewall' ? [{ ...item('firewall', '901', 'allow-ssh', { region: 'global', network: '700', rules: [SSH],
      details: { direction: 'ingress', action: 'allow', targetTags: 'ssh' } }), ...gcp }] : [] } })
  })
  await page.route('**/api/v1/cloud/accounts', async (r) => {
    const res = await r.fetch()
    const body = await res.json()
    body.accounts = body.accounts.map((a: { access: string }) => ({ ...a, access: 'manage' }))
    return r.fulfill({ response: res, json: body })
  })
  const asked = await changes(page)
  await signIn(page)
  await page.goto('/cloud?tab=risks')
  await page.getByRole('button', { name: 'Remove the rule' }).click()

  const modal = page.getByRole('dialog', { name: /Rules of allow-ssh/ })
  await expect(modal.getByText(/a Google firewall rule cannot allow nothing, so Argus deletes the firewall rule/)).toBeVisible()
  await expect(modal.getByText('A Google firewall rule allows something; to allow nothing, delete it.')).toHaveCount(0)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/05-remove-gcp.png` })
  await modal.getByRole('button', { name: 'Delete firewall rule' }).click()
  await expect(modal).toBeHidden()
  expect(asked).toHaveLength(1)
  expect(asked[0]).toMatchObject({ action: 'firewall-delete', resourceId: '901', confirm: '901', region: 'global' })
})
