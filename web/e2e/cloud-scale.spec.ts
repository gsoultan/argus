import { test, expect, type Page } from '@playwright/test'

/**
 * Scale groups, against e2e/authstub.mjs with the inventory and groups
 * served by the test: making one from a server through the wizard, a
 * group's page with its chart, members and activity, changing its rules,
 * and Argus's own question in Approvals.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS
const now = Date.now()
const ago = (min: number) => new Date(now - min * 60_000).toISOString()
const bucket = (min: number) => new Date(Math.floor((now - min * 60_000) / 300_000) * 300_000).toISOString()

const item = (kind: string, id: string, name: string, extra: object) => ({
  accountId: 'ca1', accountName: 'payments-prod', environment: 'production', firstSeen: ago(90 * 1440), seenAt: ago(0), madeByArgus: false,
  resource: { kind, id, name, region: 'ap-southeast-3', ...extra },
})

const TG_WEB = 'arn:aws:elasticloadbalancing:ap-southeast-3:412788931204:targetgroup/web/9f8e'
const WEB01 = item('instance', 'i-0web01', 'web-01', { state: 'running', size: 'm6i.large', network: 'vpc-1', zone: 'ap-southeast-3a',
  links: [{ kind: 'subnet', id: 'subnet-a' }, { kind: 'firewall', id: 'sg-web', name: 'web' }] })

const INVENTORY: Record<string, object[]> = {
  instance: [WEB01, item('instance', 'i-0batch', 'batch-02', { state: 'running', size: 'c6i.xlarge' })],
  'target-group': [item('target-group', TG_WEB, 'web-tg', { network: 'vpc-1', state: 'healthy', links: [{ kind: 'load-balancer', id: 'lb-web', name: 'web-lb' }],
    details: { protocol: 'HTTP', port: '80', targetType: 'instance' } })],
  subnet: [item('subnet', 'subnet-a', 'private-a', { zone: 'ap-southeast-3a', network: 'vpc-1' }),
    item('subnet', 'subnet-b', 'private-b', { zone: 'ap-southeast-3b', network: 'vpc-1' })],
  database: [
    item('database', 'arn:aws:rds:ap-southeast-3:412788931204:db:orders-db', 'orders-db', { state: 'available', size: 'db.r6i.large', zone: 'ap-southeast-3a',
      details: { engine: 'postgres', version: '16.4', backup_days: '7', storage_gb: '500', replicas: 'orders-db-r1' } }),
    { ...item('database', 'arn:aws:rds:ap-southeast-3:412788931204:db:orders-db-r1', 'orders-db-r1', { state: 'available', size: 'db.r6i.large',
      zone: 'ap-southeast-3b', tags: { 'argus:replica-of': 'orders-db' }, details: { engine: 'postgres', version: '16.4', replica_of: 'orders-db', backup_days: '0' } }), madeByArgus: true },
    item('database', 'arn:aws:rds:ap-southeast-3:412788931204:db:ledger', 'ledger', { state: 'available', size: 'db.m6i.large', zone: 'ap-southeast-3a',
      details: { engine: 'mysql', version: '8.0.39', backup_days: '0', storage_gb: '100' } }),
  ],
}

const RULES = {
  add: [{ signal: 'cpu', over: 'average', op: 'above', value: 75, minutes: 10 }, { signal: 'memory', over: 'any', op: 'below', value: 15, minutes: 10 }],
  remove: [{ signal: 'cpu', over: 'average', op: 'below', value: 25, minutes: 30 }, { signal: 'memory', over: 'average', op: 'above', value: 30, minutes: 30 }],
}

const DRAFT = {
  from: WEB01, name: 'web', size: 'm6i.large', diskGb: 40, each: 70.08, environment: 'production', problem: '', targetGroup: TG_WEB,
  subnets: [{ value: 'subnet-a', label: 'private-a', zone: 'ap-southeast-3a' }, { value: 'subnet-b', label: 'private-b', zone: 'ap-southeast-3b' }],
  targetGroups: [{ value: TG_WEB, label: 'web-tg', note: 'web-lb' }], images: [],
  rules: RULES, limits: { min: 2, max: 4, costCap: 300, addWait: 10, removeWait: 30 },
}

const member = (host: string, id: string, zone: string, joined: number, state = 'member', inAws = false) => ({
  instanceId: id, hostname: host, zone: `ap-southeast-3${zone}`, subnet: `subnet-${zone}`, state, addedBy: inAws ? 'aws' : 'argus-scale',
  joinedAt: ago(joined), registeredAt: ago(joined - 2), drainUntil: null, leftAt: null, inAws,
})

const series = (base: number, wobble: number, from = 480) =>
  Array.from({ length: from / 5 }, (_, i) => ({ at: bucket(from - i * 5), value: Math.max(2, Math.min(98, base + wobble * Math.sin(i / 4) + (i > 70 ? 25 : 0))) }))

const GROUP = {
  id: 'sg1', accountId: 'ca1', accountName: 'payments-prod', region: 'ap-southeast-3', name: 'pay-api', environment: 'production',
  targetGroup: TG_WEB, targetName: 'pay-api-tg', port: 8080, balancer: 'pay-api-nlb',
  template: { image: 'ami-0c3e91', imageLabel: 'image of pay-api-01', size: 'm6i.large', diskGb: 40, subnets: ['subnet-a', 'subnet-b'], firewalls: ['sg-web'], from: 'i-01', fromName: 'pay-api-01' },
  limits: { min: 2, max: 4, costCap: 450, addWait: 10, removeWait: 30 }, rules: RULES, mode: 'auto', autoBy: 'dev@northwind.id', autoAt: ago(14 * 1440),
  state: 'active', pausedReason: '', note: 'Normal: no rule met across 3 servers', failedStarts: 0, lastAddAt: ago(260), lastRemoveAt: null,
  checkedAt: ago(1), createdBy: 'dev@northwind.id', createdAt: ago(20 * 1440),
  members: [member('pay-api-01', 'i-01', 'a', 20 * 1440), member('pay-api-i-0a4c71e2', 'i-0a4c71e2', 'b', 20 * 1440, 'member', true), member('pay-api-03', 'i-03', 'a', 260)],
  now: 41, monthly: 210.24, each: 70.08, open: null,
  awsTrack: {}, awsGroup: 'argus-pay-api', awsTemplate: 'lt-0b7d2e94c1a35f608', awsNote: '',
  aws: { words: 'AWS keeps 1 copy besides pay-api-01', min: 1, max: 1, desired: 1 },
  last: { at: ago(260), kind: 'added', actor: 'argus-scale', detail: 'Launched pay-api-03 (i-03); automatic, turned on by dev@northwind.id', changeId: 'c1' },
  health: { 'i-01': 'healthy', 'i-0a4c71e2': 'healthy', 'i-03': 'healthy' },
  usage: { 'pay-api-01': { cpu: 46, mem: 58 }, 'pay-api-i-0a4c71e2': { cpu: 41, mem: 55 }, 'pay-api-03': { cpu: 38, mem: 40 } },
}

const DETAIL = {
  ...GROUP,
  series: { 'pay-api-01': series(45, 10), 'pay-api-i-0a4c71e2': series(40, 12), 'pay-api-03': series(30, 8, 255) },
  activity: [
    { at: ago(258), kind: 'healthy', actor: 'argus', detail: 'pay-api-03 is healthy in pay-api-tg after 2 min 40 s', changeId: '' },
    { at: ago(260), kind: 'added', actor: 'argus-scale', detail: 'Launched pay-api-03 (i-03); automatic, turned on by dev@northwind.id', changeId: 'c1' },
    { at: ago(270), kind: 'rule', actor: 'argus', detail: 'Rule met: group average processor above 75% for 10 min (81%): pay-api-01 88%, pay-api-02 84%', changeId: 'c1' },
    { at: ago(14 * 1440), kind: 'changed', actor: 'dev@northwind.id', detail: 'Argus adds and takes away by itself within the limits; approved by rina@northwind.id', changeId: 'c0' },
    { at: ago(19 * 1440), kind: 'added', actor: 'aws', detail: 'AWS started pay-api-i-0a4c71e2 in argus-pay-api to keep the minimum; it serves once its health check passes', changeId: '' },
    { at: ago(20 * 1440 - 5), kind: 'aws', actor: 'argus', detail: 'Made Auto Scaling group argus-pay-api from launch template lt-0b7d2e94c1a35f608: AWS keeps 1 copy besides pay-api-01 even when Argus is down', changeId: '' },
  ],
}

const ASKED = {
  id: 'c7', accountId: 'ca1', accountName: 'payments-prod', region: 'ap-southeast-3', action: 'scale-add', level: 'change', environment: 'production',
  resourceId: 'sg2', resourceName: 'web', spec: { hostname: 'web-03' }, planHash: '', state: 'awaiting_approval',
  summary: 'Add web-03 to web: launch it from image of web-01 (m6i.large, ap-southeast-3b), register it in web-tg when it runs, +$70.08 a month',
  reason: 'group average processor above 75% for 10 min (88%): web-01 90%, web-02 86%', requestedBy: 'argus-scale', requestedAt: ago(4),
  decidedBy: null, decidedAt: null, appliedAt: null, requestId: '', result: {}, error: '',
  plan: { why: 'group average processor above 75% for 10 min (88%)', signal: 'cpu', threshold: 75, series: series(55, 4, 120).map((p, i) => ({ ...p, value: 48 + i * 1.6 })),
    monthly: 70.08, after: 3, min: 2, max: 6, groupCost: 210.24, costCap: 600, automatic: false },
}

async function signIn(page: Page) {
  await page.route('**/api/v1/cloud/accounts', async (r) => {
    const res = await r.fetch()
    const body = await res.json()
    body.accounts = body.accounts.map((a: { id: string }) => (a.id === 'ca1'
      ? { ...a, access: 'manage', regions: ['ap-southeast-3'], manageRoleArn: 'arn:aws:iam::412788931204:role/ArgusManage' } : a))
    return r.fulfill({ response: res, json: body })
  })
  await page.route('**/api/v1/cloud/resources?*', (r) => {
    const kind = new URL(r.request().url()).searchParams.get('kind') ?? ''
    return r.fulfill({ json: { items: INVENTORY[kind] ?? [] } })
  })
  await page.route('**/api/v1/cloud/suggestions', (r) => r.fulfill({ json: { suggestions: [] } }))
  await page.route('**/api/v1/cloud/scale-groups', (r) => r.fulfill({ json: { groups: [GROUP] } }))
  await page.route('**/api/v1/cloud/scale-groups/draft?*', (r) => r.fulfill({ json: DRAFT }))
  await page.route('**/api/v1/cloud/scale-groups/sg1', (r) => r.fulfill({ json: DETAIL }))
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeHidden()
}

async function asks(page: Page) {
  const got: { body: Record<string, unknown> | null } = { body: null }
  await page.route('**/api/v1/cloud/changes', async (r) => {
    if (r.request().method() !== 'POST') return r.fallback()
    got.body = r.request().postDataJSON()
    return r.fulfill({ status: 202, json: { id: 'c9', state: 'awaiting_approval', summary: 'Scale change', result: {} } })
  })
  return got
}

test('make a scale group from a server, step by step', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud?tab=scaling')
  await expect(page.getByRole('link', { name: /pay-api/ })).toContainText('Argus decides · automatic')
  await expect(page.getByRole('link', { name: /pay-api/ })).toContainText('Normal 41%')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/20-scaling.png` })
  await page.getByRole('button', { name: 'New scale group' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('combobox', { name: 'Server' }).click()
  await page.getByRole('option', { name: /web-01/ }).click()
  await dialog.getByRole('button', { name: 'Next' }).click()
  await expect(page).toHaveURL(/\/cloud\/scale\/new\?.*from=i-0web01/)
  await expect(page.getByRole('heading', { name: 'Put web-01 in a scale group' })).toBeVisible()
  await expect(page.getByRole('textbox', { name: 'Group name' })).toHaveValue('web')
  await page.getByRole('button', { name: 'Next: load balancer' }).click()
  await expect(page.getByText('web-01 is already in it.')).toBeVisible()
  await page.getByRole('button', { name: 'Next: size' }).click()
  await page.getByRole('textbox', { name: 'Most servers' }).fill('6')
  await page.getByRole('combobox', { name: 'Subnets' }).click({ force: true })
  await page.getByRole('option', { name: /private-b/ }).click()
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: 'Next: when and limits' }).click()
  await expect(page.getByText('$140 → $420 a month')).toBeVisible()
  await page.getByRole('textbox', { name: 'Cost cap' }).fill('600')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/21-new-scale-group.png` })
  await page.getByRole('button', { name: /Automatic within limits/ }).click()
  await page.getByRole('button', { name: /Ask each time/ }).click()
  await page.getByRole('button', { name: 'Next: review' }).click()
  await expect(page.getByText('Keep 2 to 6 servers like it (m6i.large) in web-tg, behind web-lb, across 2 zones.')).toBeVisible()
  await expect(page.getByText(/Make an AWS Auto Scaling group, argus-web, once the image is ready, that keeps 1 copy besides web-01 even when Argus is down/)).toBeVisible()
  await page.getByRole('textbox', { name: 'Reason' }).fill('web is busy every afternoon')
  await page.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ accountId: 'ca1', region: 'ap-southeast-3', action: 'scale-group-create', reason: 'web is busy every afternoon',
    spec: { name: 'web', from: 'i-0web01', image: '', reboot: false, targetGroup: TG_WEB, subnets: ['subnet-a', 'subnet-b'], mode: 'ask',
      limits: { min: 2, max: 6, costCap: 600, addWait: 10, removeWait: 30 }, rules: RULES } })
  await expect(page).toHaveURL(/\/cloud\?tab=scaling/)
})

test("a group's page: chart, members, activity, and changing its rules", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud/scale/sg1')
  await expect(page.getByRole('heading', { name: 'pay-api' })).toBeVisible()
  await expect(page.getByText('3 healthy')).toBeVisible()
  await expect(page.getByRole('img', { name: /Processor across the group/ })).toBeVisible()
  await expect(page.getByText('Launched pay-api-03 (i-03); automatic, turned on by dev@northwind.id')).toBeVisible()
  await expect(page.getByRole('cell', { name: /pay-api-03/ })).toBeVisible()
  await expect(page.getByRole('cell', { name: /pay-api-i-0a4c71e2/ })).toContainText('AWS')
  await expect(page.getByTestId('scale-aws')).toContainText('pay-api never falls below 2: AWS keeps 1 copy besides pay-api-01, replacing a failed one by itself. Growing above 2 waits for Argus.')
  await expect(page.getByTestId('scale-aws')).toContainText('argus-pay-api · wants 1 (1 to 1)')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/22-scale-group.png`, fullPage: true })
  await page.getByRole('button', { name: 'Change rules' }).click()
  const dialog = page.getByRole('dialog', { name: 'Change pay-api' })
  await dialog.getByRole('textbox', { name: 'Most' }).fill('5')
  await dialog.getByRole('textbox', { name: 'Reason' }).fill('more headroom')
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'scale-group-change', resourceId: 'sg1', reason: 'more headroom',
    spec: { mode: 'auto', limits: { min: 2, max: 5, costCap: 450 }, rules: RULES } })
  await page.getByRole('button', { name: 'Pause' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'scale-group-change', resourceId: 'sg1', spec: { pause: true } })
})

test("Argus's question in Approvals, on a phone", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 })
  await page.route('**/api/v1/cloud/changes?pending=1', (r) => r.fulfill({ json: { changes: [ASKED] } }))
  await signIn(page)
  await page.goto('/approvals')
  const card = page.getByText(ASKED.summary).locator('xpath=ancestor::div[contains(@class, "mantine-Card-root")]')
  await expect(card).toContainText('Asked by Argus')
  await expect(card).toContainText('Group after: 3 of 2–6 · $210 of $600 a month')
  await expect(card).toContainText('Lapses at')
  await expect(card.getByRole('img', { name: /Processor across the group/ })).toBeVisible()
  await expect(card.getByRole('button', { name: 'Approve' })).toBeVisible()
  if (SHOTS) await card.screenshot({ path: `${SHOTS}/23-approve-scale-phone.png` })
})

test('AWS decides: the wizard keeps the maximum under the cost cap, and the group page says what AWS keeps', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud/scale/new?account=ca1&region=ap-southeast-3&from=i-0web01')
  await page.getByRole('button', { name: 'Next: load balancer' }).click()
  await page.getByRole('button', { name: 'Next: size' }).click()
  await page.getByRole('textbox', { name: 'Most servers' }).fill('6')
  await page.getByRole('button', { name: 'Next: when and limits' }).click()
  await page.getByRole('button', { name: /AWS decides/ }).click()
  await expect(page.getByText('Add a server when')).toBeHidden()
  // $70.08 each: 6 is over a $300 cap, which AWS would not know.
  await expect(page.getByText('AWS does not know the cost cap, so the maximum keeps under it: 4 servers at most.').first()).toBeVisible()
  await page.getByRole('textbox', { name: 'Cost cap' }).fill('450')
  await page.getByRole('textbox', { name: 'Near' }).fill('55')
  await expect(page.getByText('Nothing changes: AWS Auto Scaling keeps adding and taking away servers by itself.')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/24-aws-decides.png` })
  await page.getByRole('button', { name: 'Next: review' }).click()
  await expect(page.getByText(/that adds and takes away copies between 1 and 5, keeping the group's processor near 55%/)).toBeVisible()
  await page.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'scale-group-create', spec: { mode: 'aws', track: { metric: 'cpu', target: 55 },
    limits: { min: 2, max: 6, costCap: 450 } } })

  // A group AWS decides for: no adding by hand, and what AWS keeps.
  await page.route('**/api/v1/cloud/scale-groups/sg1', (r) => r.fulfill({ json: { ...DETAIL, mode: 'aws', awsTrack: { metric: 'cpu', target: 55 },
    aws: { words: "AWS decides, keeping the group's processor near 55%", min: 1, max: 3, desired: 2 } } }))
  await page.goto('/cloud/scale/sg1')
  await expect(page.getByRole('heading', { name: 'pay-api' })).toBeVisible()
  await expect(page.getByText('AWS decides', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add a server now' })).toBeDisabled()
  await expect(page.getByTestId('scale-aws')).toContainText('Nothing changes: AWS Auto Scaling keeps adding and taking away servers by itself.')
  await expect(page.getByTestId('scale-aws')).toContainText('wants 2 (1 to 3)')
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Change pay-api' })
  await dialog.getByText('Ask each time', { exact: true }).click()
  await expect(dialog.getByText('Argus decides again; AWS ends the servers it added above the minimum.')).toBeVisible()
})

test('read replicas of an RDS database: add one, and remove one Argus made', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud?tab=scaling')
  const card = page.getByText('Read replicas', { exact: true }).locator('xpath=ancestor::div[contains(@class, "mantine-Card-root")]')
  await expect(card.getByRole('cell', { name: /orders-db Postgres 16.4 · 1 replica/ })).toBeVisible()
  await expect(card.getByTestId('replica-row')).toContainText('Replica of orders-db · made by Argus')
  // A database with backups off says why before anyone asks.
  const ledger = card.getByRole('row', { name: /ledger/ })
  await expect(ledger.getByRole('button', { name: 'Add a read replica' })).toBeDisabled()
  await ledger.getByRole('button', { name: 'Add a read replica' }).hover({ force: true })
  await expect(page.getByText(/ledger's backups, which are off; turn on automated backups first/)).toBeVisible()
  if (SHOTS) await card.screenshot({ path: `${SHOTS}/24-read-replicas.png` })

  await card.getByRole('row', { name: /orders-db Postgres/ }).getByRole('button', { name: 'Add a read replica' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add a read replica of orders-db' })
  await expect(dialog.getByRole('textbox', { name: 'Replica name' })).toHaveValue('orders-db-r2')
  await expect(dialog.getByRole('textbox', { name: 'Class' })).toHaveValue('db.r6i.large')
  await expect(dialog.getByText('Same as the primary')).toBeVisible()
  await dialog.getByRole('textbox', { name: 'Replica name' }).fill('orders-db-r1')
  await expect(dialog.getByText('ap-southeast-3 already has a database named orders-db-r1')).toBeVisible()
  await dialog.getByRole('textbox', { name: 'Replica name' }).fill('orders-db-reads')
  await dialog.getByRole('combobox', { name: 'Zone' }).click()
  await page.getByRole('option', { name: 'ap-southeast-3b' }).click()
  await dialog.getByRole('textbox', { name: 'Why' }).fill('reads are 91% of statements')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/25-add-replica.png` })
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ accountId: 'ca1', region: 'ap-southeast-3', action: 'replica-create', reason: 'reads are 91% of statements',
    spec: { source: 'orders-db', name: 'orders-db-reads', size: 'db.r6i.large', zone: 'ap-southeast-3b' } })

  await card.getByTestId('replica-row').getByRole('button', { name: 'Remove' }).click()
  const rm = page.getByRole('dialog', { name: 'Remove orders-db-r1' })
  await expect(rm).toContainText('Turns off deletion protection on orders-db-r1 and deletes it. orders-db and its data are not touched.')
  await rm.getByRole('textbox', { name: 'Why' }).fill('sale is over')
  await rm.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'replica-remove', resourceId: 'arn:aws:rds:ap-southeast-3:412788931204:db:orders-db-r1' })
})
