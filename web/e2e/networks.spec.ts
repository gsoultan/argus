import { test, expect, type Page } from '@playwright/test'

/**
 * Networks, against e2e/authstub.mjs with the links served by the test: a
 * WireGuard link between two Argus servers shown once, a peer down, an idle
 * laptop, Cloudflare Tunnels connected and not, and VPN software found. What
 * counts as down is the control plane's to decide (internal/control/networks.go).
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS
const now = Date.now()
const ago = (m: number) => new Date(now - m * 60_000).toISOString()

const peer = (o: Record<string, unknown>) => ({ allowedIps: [], rxBytes: 0, txBytes: 0, ...o })

const VIEW = {
  servers: [
    { assetId: 'a1', hostname: 'db-prod-01', reported: true, checkedAt: ago(1), quiet: false, lastSeen: ago(0.2), wgTool: true },
    { assetId: 'a2', hostname: 'db-prod-02', reported: true, checkedAt: ago(1), quiet: false, lastSeen: ago(0.2), wgTool: true },
    { assetId: 'a3', hostname: 'gw-jkt-01', reported: true, checkedAt: ago(1), quiet: false, lastSeen: ago(0.2), wgTool: true },
    { assetId: 'a4', hostname: 'legacy-01', reported: false, quiet: false, lastSeen: ago(0.2), wgTool: false },
  ],
  software: [
    { assetId: 'a1', hostname: 'db-prod-01', kind: 'wireguard', name: 'WireGuard', running: false, unit: 'wg-quick@wg0.service' },
    { assetId: 'a3', hostname: 'gw-jkt-01', kind: 'cloudflared', name: 'Cloudflare Tunnel', version: '2025.11.1', running: true, unit: 'cloudflared.service' },
    { assetId: 'a4', hostname: 'legacy-01', kind: 'openvpn', name: 'OpenVPN', version: '2.6.9', running: false, unit: 'openvpn-server@office.service' },
  ],
  interfaces: [
    { assetId: 'a1', hostname: 'db-prod-01', name: 'wg0', publicKey: 'K1', listenPort: 51820, addresses: ['10.88.0.11/16'], up: true, rxBytes: 0, txBytes: 0, peersKnown: true,
      peers: [
        peer({ publicKey: 'K2', endpoint: '20.1.2.3:51820', allowedIps: ['10.88.0.12/32'], latestHandshake: ago(1), keepalive: 25, rxBytes: 86e9, txBytes: 88e9, state: 'up', peerAssetId: 'a2', peerHostname: 'db-prod-02', peerInterface: 'wg0' }),
        peer({ publicKey: 'K3', endpoint: '36.72.1.9:51820', allowedIps: ['10.88.0.2/32', '192.168.10.0/24'], latestHandshake: ago(14), keepalive: 25, rxBytes: 9e9, txBytes: 2e9, state: 'down', peerAssetId: 'a3', peerHostname: 'gw-jkt-01', peerInterface: 'wg0' }),
        peer({ publicKey: 'LAPTOPKEY123', allowedIps: ['10.88.5.4/32'], latestHandshake: ago(300), state: 'idle', rxBytes: 1e6, txBytes: 4e6 }),
      ] },
    { assetId: 'a2', hostname: 'db-prod-02', name: 'wg0', publicKey: 'K2', addresses: [], up: true, rxBytes: 0, txBytes: 0, peersKnown: true,
      peers: [peer({ publicKey: 'K1', latestHandshake: ago(1), state: 'up', peerAssetId: 'a1', peerHostname: 'db-prod-01', peerInterface: 'wg0' })] },
  ],
  tunnels: [
    { assetId: 'a3', hostname: 'gw-jkt-01', metrics: '127.0.0.1:20241', state: 'up', readyConnections: 4, connectorId: '0b6a1c2e-1111-2222-3333-444455556666', version: '2025.11.1' },
    { assetId: 'a2', hostname: 'db-prod-02', metrics: '127.0.0.1:20241', state: 'down', readyConnections: 0, error: 'metrics did not answer' },
  ],
  problems: [
    { severity: 'down', kind: 'peer-down', assetId: 'a1', hostname: 'db-prod-01', title: 'wg0 on db-prod-01 cannot reach gw-jkt-01', detail: 'Last handshake 13 min before the agent checked, although this side keeps the link alive.' },
    { severity: 'down', kind: 'tunnel-down', assetId: 'a2', hostname: 'db-prod-02', title: 'Cloudflare Tunnel on db-prod-02 is not connected', detail: "cloudflared's health check: metrics did not answer." },
    { severity: 'warn', kind: 'stopped', assetId: 'a4', hostname: 'legacy-01', title: 'OpenVPN is installed but not running on legacy-01', detail: 'Installed, with no process or service running. If it should be connected, start it; if not, remove it.' },
  ],
}

async function signIn(page: Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeHidden()
  await page.route('**/api/v1/networks', (r) => r.fulfill({ json: VIEW }))
}

test('networks: links once each, down first, tunnels and software', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/networks')
  await expect(page.getByRole('heading', { name: 'Networks' })).toBeVisible()
  await expect(page.getByText('2 down')).toBeVisible()
  await expect(page.getByTestId('network-problem')).toHaveCount(3)
  // db-prod-01 <-> db-prod-02 is seen from both ends and shown once.
  const links = page.getByTestId('wg-link')
  await expect(links).toHaveCount(3)
  await expect(links.first()).toContainText('gw-jkt-01')
  await expect(links.first()).toContainText('Down')
  await expect(page.getByTestId('wg-link').filter({ hasText: 'LAPTOPKEY1' })).toContainText('Idle')
  await expect(page.getByTestId('cf-tunnel').filter({ hasText: 'gw-jkt-01' })).toContainText('Connected')
  await expect(page.getByTestId('cf-tunnel').filter({ hasText: 'db-prod-02' })).toContainText('metrics did not answer')
  await expect(page.getByText(/1 server runs an agent too old/)).toBeVisible()
  await expect(page.getByRole('link', { name: 'VPNs and tunnels' })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks.png`, fullPage: true })
})

test('networks: on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await signIn(page)
  await page.goto('/networks')
  await expect(page.getByText('2 down')).toBeVisible()
  const width = await page.evaluate(() => document.documentElement.scrollWidth)
  expect(width).toBeLessThanOrEqual(390)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks-phone.png`, fullPage: true })
})
