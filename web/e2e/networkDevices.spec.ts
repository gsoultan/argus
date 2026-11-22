import { test, expect, type Page } from '@playwright/test'

/**
 * Network devices, against e2e/authstub.mjs with the devices served by the
 * test: the list with each device's state, uptime and interfaces, one
 * device's interfaces and outages, and the form with SNMPv3 first, a test
 * that finds the interfaces, and v2c's warning. What counts as down is the
 * control plane's to decide (internal/control/network_devices.go).
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS
const now = Date.now()
const ago = (m: number) => new Date(now - m * 60_000).toISOString()

const ifs = (uplinkUp: boolean) => [
  { index: 1, name: 'Gi1/0/1', alias: 'uplink to core', up: uplinkUp, adminUp: true, speedMbps: 10000, inOctets: 0, outOctets: 0, inErrors: 0, outErrors: 0,
    inBps: uplinkUp ? 412_000_000 : undefined, outBps: uplinkUp ? 96_500_000 : undefined, watched: true },
  ...Array.from({ length: 23 }, (_, i) => ({
    index: i + 2, name: `Gi1/0/${i + 2}`, up: i % 4 !== 3, adminUp: i !== 22, speedMbps: 1000, inOctets: 0, outOctets: 0, inErrors: 0, outErrors: 0,
    inBps: i % 4 !== 3 ? 2_000_000 + i * 310_000 : undefined, outBps: i % 4 !== 3 ? 800_000 + i * 120_000 : undefined, errorsUp: i === 5 ? 14 : undefined,
  })),
]

const base = {
  port: 161, version: 'v3', user: 'argus-ro', authProto: 'SHA256', privProto: 'AES', hasSecret: true, everySeconds: 60, timeoutSeconds: 10,
  checkFrom: ['a1'], checkHosts: ['edge-jkt-01'], paused: false, groupId: '', createdBy: 'dev@northwind.id', createdAt: ago(60 * 24 * 40),
  failingRounds: 0, lastOutcome: 'up', canOperate: true, watch: [{ index: 1, name: 'Gi1/0/1 (uplink to core)' }],
}

const LIST = [
  { ...base, id: 'd1', name: 'core-sw1', host: '10.20.0.2', state: 'up', stateSince: ago(60 * 24 * 12), lastAt: ago(0.3), lastMs: 210,
    lastDetail: 'core-sw1 answered; the 1 watched interface is up (read from edge-jkt-01)',
    device: { name: 'core-sw1', description: 'Cisco IOS Software, C9300 Software, Version 17.9.4', location: 'Jakarta DC, rack 4',
      uptimeSec: 86_400 * 12 + 3600 * 4, at: ago(0.3), interfaces: ifs(true) },
    uptime: { day: 100, week: 100, month: 99.98 } },
  { ...base, id: 'd2', name: 'branch-fw', host: '10.40.0.1', state: 'down', stateSince: ago(9), lastAt: ago(0.2), lastMs: 180,
    lastDetail: 'branch-fw answered, but Gi1/0/1 (uplink to core) is down (from edge-sby-01)', checkHosts: ['edge-sby-01'],
    device: { name: 'branch-fw', description: 'FortiGate-60F v7.2.8', uptimeSec: 86_400 * 40, at: ago(0.2), interfaces: ifs(false) },
    openOutage: { id: 4, checkId: 'd2', startedAt: ago(9), cause: 'branch-fw answered, but Gi1/0/1 (uplink to core) is down', confirmed: 'edge-sby-01' },
    uptime: { day: 99.37, week: 99.91, month: 99.97 } },
  { ...base, id: 'd3', name: 'floor2-printer', host: 'printer-2f.lan', version: 'v2c', user: '', authProto: '', privProto: '', watch: [],
    state: 'up', stateSince: ago(30), lastAt: ago(0.5), lastMs: 90, lastDetail: 'HP LaserJet answered (read from edge-jkt-01)',
    device: { name: 'NPI3B1C2F', description: 'HP LaserJet MFP M428fdw', uptimeSec: 1500, at: ago(0.5), restartedAt: ago(25),
      interfaces: [{ index: 1, name: 'lo', up: true, adminUp: true, inOctets: 0, outOctets: 0, inErrors: 0, outErrors: 0 },
        { index: 2, name: 'eth0', up: true, adminUp: true, speedMbps: 100, inOctets: 0, outOctets: 0, inErrors: 0, outErrors: 0, inBps: 12_400, outBps: 3_100 }] },
    uptime: { day: 100, week: 100, month: 100 } },
]

const rounds = Array.from({ length: 1440 }, (_, i) => {
  const m = 1440 - i
  const down = m <= 9
  return { at: ago(m), secs: 60, outcome: down ? 'down' : 'up', ms: 150 + (i % 40), detail: down ? 'Gi1/0/1 is down' : 'answered' }
})

async function signIn(page: Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeHidden()
  await page.route('**/api/v1/network-devices', (r) => r.fulfill({ json: LIST }))
  await page.route('**/api/v1/network-devices/d2', (r) => r.fulfill({ json: {
    device: LIST[1], rounds,
    outages: [{ id: 4, checkId: 'd2', startedAt: ago(9), cause: 'branch-fw answered, but Gi1/0/1 (uplink to core) is down', confirmed: 'edge-sby-01' }],
  } }))
  await page.route('**/api/v1/check-groups', (r) => r.fulfill({ json: [] }))
}

test('network devices: state, uptime, restarts and interfaces', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/network-devices')
  await expect(page.getByRole('heading', { name: 'Network devices' })).toBeVisible()
  await expect(page.getByText('1 down')).toBeVisible()
  // Down first; the printer's restart is shown, not counted.
  await expect(page.getByRole('row').nth(1)).toContainText('branch-fw')
  await expect(page.getByRole('row').nth(1)).toContainText('Gi1/0/1 (uplink to core) down')
  await expect(page.getByRole('row', { name: /floor2-printer/ }).getByText('Restarted')).toBeVisible()
  await expect(page.getByRole('row', { name: /core-sw1/ })).toContainText('12 d 4 h')
  await expect(page.getByRole('row', { name: /core-sw1/ })).toContainText('19 of 24 up')
  await expect(page.getByRole('link', { name: 'Network devices' })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/11-network-devices.png`, fullPage: true })

  await page.getByRole('button', { name: 'Open branch-fw' }).click()
  const drawer = page.getByRole('dialog')
  await expect(drawer.getByText(/Down since/)).toBeVisible()
  const table = drawer.getByTestId('interfaces')
  await expect(table.getByRole('row').nth(1)).toContainText('Gi1/0/1 (uplink to core)')
  await expect(table.getByRole('row').nth(1)).toContainText('Down')
  await expect(drawer.getByText(/SNMPv3, SHA256 and AES encryption, as argus-ro/)).toBeVisible()
  await expect(drawer.getByRole('button', { name: 'Show all 24 interfaces' })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/12-network-device-down.png` })
})

test('network devices: adding one, v3 first, with a test that finds the interfaces', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  let added: Record<string, unknown> | null = null
  let tested: Record<string, unknown> | null = null
  await page.route('**/api/v1/network-devices/test', (r) => {
    tested = r.request().postDataJSON()
    return r.fulfill({ json: { kind: 'snmp', ok: true, detail: 'core-sw2 answered', ms: 220, from: 'edge-jkt-01',
      snmp: { name: 'core-sw2', description: 'Cisco IOS', uptimeSec: 86_400 * 3, interfaces: ifs(true).slice(0, 6) } } })
  })
  await page.route('**/api/v1/network-devices', (r) => {
    if (r.request().method() === 'POST') {
      added = r.request().postDataJSON()
      return r.fulfill({ status: 201, json: { ...LIST[0], id: 'd9', name: 'core-sw2' } })
    }
    return r.fulfill({ json: LIST })
  })
  await page.goto('/network-devices')
  await page.getByRole('button', { name: 'Add device' }).first().click()
  const form = page.getByRole('dialog', { name: 'Add a network device' })
  await form.getByLabel('Address').fill('10.20.0.3')
  await form.getByPlaceholder('Choose one to three').click()
  await page.getByRole('option').first().click()
  await page.keyboard.press('Escape')
  await form.getByLabel('User').fill('argus-ro')
  await form.getByLabel('Authentication password').fill('auth-pass-1')
  await form.getByLabel('Encryption password').fill('priv-pass-1')
  await form.getByRole('button', { name: 'Test and find interfaces' }).click()
  await expect(form.getByTestId('device-test')).toContainText('core-sw2 answered from edge-jkt-01, up for 3 d, with 6 interfaces')
  await form.getByRole('checkbox', { name: /Gi1\/0\/1 \(uplink to core\)/ }).check()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/13-add-network-device.png` })
  expect(tested).toMatchObject({ host: '10.20.0.3', version: 'v3', user: 'argus-ro', authProto: 'SHA256', privProto: 'AES' })
  await form.getByRole('button', { name: 'Add device' }).click()
  await expect.poll(() => added).toMatchObject({ host: '10.20.0.3', watch: [{ index: 1, name: 'Gi1/0/1 (uplink to core)' }], authPass: 'auth-pass-1' })
})

test('network devices: v2c says the community travels in the clear', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/network-devices')
  await page.getByRole('button', { name: 'Add device' }).first().click()
  const again = page.getByRole('dialog', { name: 'Add a network device' })
  await again.getByText('SNMP v2c', { exact: true }).click()
  await expect(again.getByText(/sends the community string in the clear/)).toBeVisible()
  await expect(again.getByLabel('Community string')).toBeVisible()
})

test('network devices: on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await signIn(page)
  await page.goto('/network-devices')
  await expect(page.getByText('1 down')).toBeVisible()
  const width = await page.evaluate(() => document.documentElement.scrollWidth)
  expect(width).toBeLessThanOrEqual(390)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/14-network-devices-phone.png`, fullPage: true })
})
