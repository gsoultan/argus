import { test, expect, type Page } from '@playwright/test'

/**
 * Load balancers, target groups and Auto Scaling groups, against
 * e2e/authstub.mjs with the inventory served by the test: making a target
 * group and a load balancer, changing a health check, and sizing a group,
 * each through the usual change.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS
const now = Date.now()
const ago = (d: number) => new Date(now - d * 86_400_000).toISOString()

const item = (kind: string, id: string, name: string, extra: object) => ({
  accountId: 'ca1', accountName: 'payments-prod', environment: 'production', firstSeen: ago(90), seenAt: ago(0), madeByArgus: false,
  resource: { kind, id, name, region: 'ap-southeast-3', ...extra },
})

const TG_API = 'arn:aws:elasticloadbalancing:ap-southeast-3:412788931204:targetgroup/pay-api/0a1b'
const TG_WEB = 'arn:aws:elasticloadbalancing:ap-southeast-3:412788931204:targetgroup/web/9f8e'

const INVENTORY: Record<string, object[]> = {
  network: [item('network', 'vpc-1', 'main', { size: '10.0.0.0/16' })],
  subnet: ['a', 'b', 'c'].map((z, i) => item('subnet', `subnet-${z}`, `private-${z}`, { network: 'vpc-1', size: `10.0.${i + 1}.0/24`, zone: `ap-southeast-3${z}` })),
  firewall: [item('firewall', 'sg-lb', 'web-lb', { network: 'vpc-1', rules: [] })],
  'target-group': [
    item('target-group', TG_API, 'pay-api', { network: 'vpc-1', size: 'HTTP:8080', state: 'unused',
      details: { protocol: 'HTTP', port: '8080', targetType: 'instance', healthCheckProtocol: 'HTTP', healthCheckPath: '/health', healthCheckPort: 'traffic-port',
        healthCheckInterval: '30', healthyThreshold: '5', unhealthyThreshold: '2', matcher: '200', targets: '0', healthy: '0' } }),
    item('target-group', TG_WEB, 'web', { network: 'vpc-1', size: 'HTTP:80', state: 'healthy', links: [{ kind: 'load-balancer', id: 'lb-web', name: 'web-lb' }],
      details: { protocol: 'HTTP', port: '80', targetType: 'instance', targets: '3', healthy: '3' } }),
  ],
  'scaling-group': [
    item('scaling-group', 'web-asg', 'web-asg', { state: 'active', size: '3', links: [{ kind: 'target-group', id: TG_WEB }],
      tags: { 'aws:cloudformation:stack-name': 'web-stack' },
      details: { min: '2', max: '6', desired: '3', running: '3', launchTemplate: 'web-lt', healthCheck: 'ELB' } }),
    item('scaling-group', 'batch-asg', 'batch-asg', { state: 'active', size: '1', details: { min: '0', max: '4', desired: '1', running: '0', launchTemplate: 'batch-lt', suspended: '1' } }),
  ],
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
    return r.fulfill({ status: 202, json: { id: 'c9', state: 'awaiting_approval', summary: 'Balancing change' } })
  })
  return got
}

async function pick(page: Page, dialog: ReturnType<Page['getByRole']>, label: string | RegExp, option: string | RegExp) {
  await dialog.getByRole('combobox', { name: label }).click({ force: true })
  await page.getByRole('option', { name: option }).click()
  // A multi-select stays open after a pick.
  await page.keyboard.press('Escape')
  await expect(page.getByRole('option', { name: option })).toBeHidden()
}

test('make a target group, then a load balancer in front of it', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud?tab=network')
  await page.locator('label', { hasText: /^Target groups/ }).click()
  await page.getByRole('button', { name: 'New target group' }).click()
  let dialog = page.getByRole('dialog')
  await dialog.getByRole('textbox', { name: 'Name' }).fill('web')
  await expect(dialog).toContainText('There is already a target group called web here')
  await dialog.getByRole('textbox', { name: 'Name' }).fill('checkout')
  await dialog.getByRole('textbox', { name: 'Port', exact: true }).fill('8080')
  await dialog.getByRole('textbox', { name: 'Path' }).fill('health')
  await expect(dialog).toContainText('A path starts with /')
  await dialog.getByRole('textbox', { name: 'Path' }).fill('/healthz')
  await dialog.getByRole('textbox', { name: 'Every (seconds)' }).fill('15')
  await expect(dialog).toContainText('Checks HTTP /healthz answering 200, every 15s, healthy after 5, unhealthy after 2.')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/13-new-target-group.png` })
  await dialog.getByRole('textbox', { name: 'Why' }).fill('checkout service')
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ accountId: 'ca1', region: 'ap-southeast-3', action: 'target-group-create',
    spec: { network: 'vpc-1', name: 'checkout', protocol: 'HTTP', port: 8080,
      health: { protocol: 'HTTP', path: '/healthz', port: 'traffic-port', intervalSeconds: 15, healthy: 5, unhealthy: 2, matcher: '200' } } })
  await expect(dialog).toBeHidden()

  await page.locator('label', { hasText: /^Load balancers/ }).click()
  await page.getByRole('button', { name: 'New load balancer' }).click()
  dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('about $16.43 a month before traffic')
  await dialog.getByRole('textbox', { name: 'Name' }).fill('pay-api-lb')
  // web is already behind web-lb, so only pay-api is offered.
  await dialog.getByRole('combobox', { name: 'Forwards to' }).click()
  await expect(page.getByRole('option', { name: /^web/ })).toHaveCount(0)
  await page.getByRole('option', { name: /pay-api/ }).click()
  await pick(page, dialog, 'Subnets', /private-a/)
  await pick(page, dialog, 'Subnets', /private-b/)
  await pick(page, dialog, 'Security groups', /web-lb/)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/14-new-load-balancer.png` })
  await dialog.getByRole('textbox', { name: 'Why' }).fill('front the pay api')
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'balancer-create', spec: {
    name: 'pay-api-lb', type: 'application', internal: false, subnets: ['subnet-a', 'subnet-b'], firewalls: ['sg-lb'], port: 80, targetGroup: TG_API } })
})

test('change a target group health check', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud?tab=network')
  await page.locator('label', { hasText: /^Target groups/ }).click()
  await page.getByRole('link', { name: /pay-api/ }).click()
  const drawer = page.getByRole('dialog', { name: 'pay-api' })
  await expect(drawer).toContainText('HTTP /health answering 200, every 30s, healthy after 5, unhealthy after 2')
  await drawer.getByRole('button', { name: 'Change' }).click()
  const dialog = page.getByRole('dialog', { name: 'Health check of pay-api' })
  await dialog.getByRole('textbox', { name: 'Path' }).fill('/ready')
  await dialog.getByRole('textbox', { name: 'Healthy codes' }).fill('200-299')
  await dialog.getByRole('textbox', { name: 'Why' }).fill('readiness endpoint')
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'health-check', resourceId: TG_API,
    spec: { protocol: 'HTTP', path: '/ready', matcher: '200-299', intervalSeconds: 30 } })
})

test('see Auto Scaling groups and change one size', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  const got = await asks(page)
  await signIn(page)
  await page.goto('/cloud?tab=scaling')
  const row = page.getByRole('link', { name: /web-asg/ })
  await expect(row).toContainText('3 of 2–6')
  await expect(row).toContainText('web-lt')
  await expect(row).toContainText('web')
  await expect(row).toContainText('CloudFormation · web-stack')
  await expect(page.getByRole('link', { name: /batch-asg/ })).toContainText('Partly suspended')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/15-scaling-groups.png` })
  await row.click()
  const drawer = page.getByRole('dialog', { name: 'web-asg' })
  await expect(drawer).toContainText('Wants 3, between 2 and 6; 3 running now.')
  await drawer.getByRole('button', { name: 'Change size' }).click()
  const dialog = page.getByRole('dialog', { name: 'Size of web-asg' })
  await expect(dialog).toContainText('CloudFormation stack web-stack')
  await dialog.getByRole('textbox', { name: 'Wants now' }).fill('8')
  await expect(dialog).toContainText('Between the minimum and the maximum')
  await dialog.getByRole('textbox', { name: 'Maximum' }).fill('8')
  await expect(dialog).toContainText('AWS starts 5 more')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/16-group-size.png` })
  await dialog.getByRole('textbox', { name: 'Why' }).fill('sale week')
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect.poll(() => got.body).toMatchObject({ action: 'group-size', resourceId: 'web-asg', spec: { min: 2, max: 8, desired: 8 } })
})

test('scaling on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await signIn(page)
  await page.goto('/cloud?tab=scaling')
  await expect(page.getByRole('link', { name: /web-asg/ })).toBeVisible()
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})
