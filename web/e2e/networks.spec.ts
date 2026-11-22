import { test, expect, type Page } from '@playwright/test'

/**
 * Networks, against e2e/authstub.mjs with the links served by the test: a
 * WireGuard link between two Argus servers shown once, a peer down, an idle
 * laptop, Cloudflare Tunnels connected and not, VPN software found, cloud VPNs
 * with one tunnel of two down, and a Cloudflare account whose tunnels are
 * matched to the servers that run them. What
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
    { assetId: 'a3', hostname: 'gw-jkt-01', metrics: '127.0.0.1:20241', state: 'up', readyConnections: 4, connectorId: '0b6a1c2e-1111-2222-3333-444455556666', version: '2025.11.1', tunnelId: 't1', tunnelName: 'web-public' },
    { assetId: 'a2', hostname: 'db-prod-02', metrics: '127.0.0.1:20241', state: 'down', readyConnections: 0, error: 'metrics did not answer' },
  ],
  problems: [
    { severity: 'down', kind: 'cloud-vpn-down', key: 'cloud-vpn::c1/ap-southeast-3/vpn-0a1', assetId: '', hostname: '', title: 'Cloud VPN jakarta-office is down (AWS prod in ap-southeast-3)', detail: 'No tunnel is up, so nothing crosses it. The cloud says 108.136.1.10: IPSEC IS DOWN; 108.136.1.11: IPSEC IS DOWN.' },
    { severity: 'down', kind: 'peer-down', key: 'peer-down:a1:wg0/K3', assetId: 'a1', hostname: 'db-prod-01', title: 'wg0 on db-prod-01 cannot reach gw-jkt-01', detail: 'Last handshake 13 min before the agent checked, although this side keeps the link alive.' },
    { severity: 'down', kind: 'tunnel-down', key: 'tunnel-down:a2:127.0.0.1:20241', assetId: 'a2', hostname: 'db-prod-02', title: 'Cloudflare Tunnel on db-prod-02 is not connected', detail: "cloudflared's health check: metrics did not answer." },
    { severity: 'warn', kind: 'cloud-vpn-partial', key: 'cloud-vpn::c3/asia-southeast2/ha-gw-to-dc/partial', assetId: '', hostname: '', title: 'Cloud VPN ha-gw to dc-jkt has 1 of 2 tunnels up (Google Cloud data in asia-southeast2)', detail: 'Traffic still flows, but there is no spare: if the tunnel that is up fails, the link is down.' },
    { severity: 'warn', kind: 'cf-unknown-machine', key: 'cf-unknown-machine::acct/t2/c9', assetId: '', hostname: '', title: 'Cloudflare Tunnel intranet has a connector no Argus server reports, from 114.4.22.7', detail: "It may be a machine without an Argus agent, such as someone's laptop publishing a company hostname, or a server whose cloudflared metrics Argus cannot read. Check who runs it." },
    { severity: 'warn', kind: 'stopped', key: 'stopped:a4:openvpn', assetId: 'a4', hostname: 'legacy-01', title: 'OpenVPN is installed but not running on legacy-01', detail: 'Installed, with no process or service running. If it should be connected, start it; if not, remove it.' },
  ],
  cloudVpns: [
    { accountId: 'c1', accountName: 'prod', provider: 'aws', region: 'ap-southeast-3', id: 'vpn-0a1', name: 'jakarta-office', state: 'down', tunnelsUp: 0, tunnelsTotal: 2,
      tunnels: [{ Address: '108.136.1.10', Up: false, Why: 'IPSEC IS DOWN' }, { Address: '108.136.1.11', Up: false, Why: 'IPSEC IS DOWN' }],
      remoteAddress: '36.72.1.9', remoteNetworks: ['192.168.10.0/24'], gateway: 'vgw-1', type: 'ipsec.1', routing: 'static', seenAt: ago(3) },
    { accountId: 'c2', accountName: 'corp', provider: 'azure', region: 'southeastasia', id: 'conn-office', name: 'office-sg', state: 'up', tunnelsUp: 1, tunnelsTotal: 1,
      tunnels: [{ Address: '203.0.113.9', Up: true, Why: 'Connected' }], remoteAddress: '203.0.113.9', remoteNetworks: ['10.30.0.0/16'], type: 'IPsec', routing: 'bgp', seenAt: ago(3) },
    { accountId: 'c3', accountName: 'data', provider: 'gcp', region: 'asia-southeast2', id: 'ha-gw-to-dc', name: 'ha-gw to dc-jkt', state: 'partial', tunnelsUp: 1, tunnelsTotal: 2,
      tunnels: [{ Address: 't0', Up: true, Why: 'ESTABLISHED' }, { Address: 't1', Up: false, Why: 'NO_INCOMING_PACKETS' }], remoteAddress: '36.72.1.9', remoteNetworks: [], type: 'HA VPN', routing: 'bgp', seenAt: ago(3) },
  ],
  cloudflare: [
    { id: 'cf1', name: 'Main account', accountId: '0123456789abcdef0123456789abcdef', syncedAt: ago(2), stale: false,
      read: ['tunnels', 'warpConnectors', 'routes', 'devices'], missing: { magicWan: 'Cloudflare answered 403' },
      routes: [], devices: [], devicesTotal: 37, magicWan: [],
      tunnels: [
        { id: 't1', name: 'web-public', kind: 'tunnel', status: 'healthy', routes: [], connectors: [
          { clientId: '0b6a1c2e-1111-2222-3333-444455556666', version: '2025.11.1', locations: ['cgk01', 'sin02'], connections: 4, assetId: 'a3', hostname: 'gw-jkt-01', old: false }] },
        { id: 't2', name: 'intranet', kind: 'tunnel', status: 'degraded', routes: ['10.20.0.0/16'], connectors: [
          { clientId: 'c9', version: '2024.2.1', originIp: '114.4.22.7', locations: ['cgk01'], connections: 1, old: true }] },
        { id: 't3', name: 'office-warp', kind: 'warp-connector', status: 'inactive', routes: [], connsInactiveAt: ago(60 * 24 * 40), connectors: [] },
      ] },
  ],
}

const WG = {
  networks: [
    { id: 'w1', name: 'Database mesh', shape: 'mesh', cidr: '10.88.0.0/24', port: 51820, iface: 'argus1a2b3c', state: 'active', generation: 2,
      requestedBy: 'rina@northwind.id', approvedBy: 'dev@northwind.id', createdAt: ago(60 * 24), approvedAt: ago(60),
      members: [
        { assetId: 'a1', hostname: 'db-prod-01', address: '10.88.0.1', endpoint: '203.0.113.7', routes: ['192.168.10.0/24'], state: 'applied', at: ago(58) },
        { assetId: 'a2', hostname: 'db-prod-02', address: '10.88.0.2', endpoint: '20.1.2.3', state: 'applied', at: ago(57) },
        { assetId: 'a3', hostname: 'gw-jkt-01', address: '10.88.0.3', state: 'rolled-back', at: ago(55), message: 'the server did not reach Argus within 2 minutes of the change, so it was undone' },
      ],
      pending: { name: 'Database mesh', shape: 'mesh', cidr: '10.88.0.0/24', port: 51820, members: [
        { assetId: 'a1', hostname: 'db-prod-01', address: '10.88.0.1', endpoint: '203.0.113.7' },
        { assetId: 'a2', hostname: 'db-prod-02', address: '10.88.0.2', endpoint: '20.1.2.3' }] },
      pendingBy: 'rina@northwind.id', pendingAt: ago(5) },
    { id: 'w2', name: 'Branch hub', shape: 'hub', cidr: '10.89.0.0/24', port: 51821, iface: 'argus9f8e7d', state: 'waiting', generation: 0,
      requestedBy: 'dev@northwind.id', createdAt: ago(3), members: [], approveOwn: false,
      pending: { name: 'Branch hub', shape: 'hub', cidr: '10.89.0.0/24', port: 51821, members: [
        { assetId: 'a3', hostname: 'gw-jkt-01', address: '10.89.0.1', hub: true, endpoint: 'gw.northwind.id' },
        { assetId: 'a4', hostname: 'legacy-01', address: '10.89.0.2' }] },
      pendingBy: 'dev@northwind.id', pendingAt: ago(3) },
  ],
}

const IPSEC = {
  links: [
    { id: 'i1', name: 'Jakarta office to AWS', conn: 'argus-0a1b2c3d', assetId: 'a3', hostname: 'gw-jkt-01', peer: '108.136.1.10',
      localRanges: ['192.168.10.0/24'], remoteRanges: ['10.30.0.0/16'], proposals: 'strong', state: 'active', requestedBy: 'rina@northwind.id',
      approvedBy: 'dev@northwind.id', createdAt: ago(600), server: 'applied', serverAt: ago(50), link: 'down',
      cloud: { provider: 'aws', accountName: 'prod', region: 'ap-southeast-3', name: 'jakarta-office', up: false, why: 'IPSEC IS DOWN' } },
    { id: 'i2', name: 'Branch firewall', conn: 'argus-9f8e7d6c', assetId: 'a1', hostname: 'db-prod-01', peer: '36.72.1.9',
      localRanges: ['10.88.0.0/24'], remoteRanges: ['192.168.20.0/24'], proposals: 'compatible', state: 'waiting', requestedBy: 'rina@northwind.id',
      createdAt: ago(4), pendingBy: 'rina@northwind.id', pendingAt: ago(4),
      pending: { name: 'Branch firewall', assetId: 'a1', hostname: 'db-prod-01', peer: '36.72.1.9', localRanges: ['10.88.0.0/24'], remoteRanges: ['192.168.20.0/24'], proposals: 'compatible' } },
  ],
}

async function signIn(page: Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeHidden()
  await page.route('**/api/v1/networks', (r) => r.fulfill({ json: VIEW }))
  await page.route('**/api/v1/networks/wireguard', (r) => r.request().method() === 'GET'
    ? r.fulfill({ json: WG })
    : r.fulfill({ status: 400, json: { error: '10.88.0.0/24 overlaps the Argus network Database mesh (10.88.0.0/24): pick a range nothing else uses' } }))
  await page.route('**/api/v1/networks/wireguard/*/approve', (r) => r.fulfill({ status: 204 }))
  await page.route('**/api/v1/networks/ipsec', (r) => r.fulfill({ json: IPSEC }))
  await page.route('**/api/v1/networks/ipsec/*/key', (r) => r.fulfill({ json: { psk: 'Kq3vX9tLmP2wR8sYzN4bH7cJ1dF6gA0eUoIiQxWv' } }))
  await page.route('**/api/v1/networks/cloudflare', (r) => r.request().method() === 'GET'
    ? r.fulfill({ json: { accounts: [], canSave: true } })
    : r.fulfill({ status: 400, json: { error: 'Cloudflare refused the token: Cloudflare answered 403: Unauthorized. It needs Cloudflare Tunnel Read on this account' } }))
}

test('networks: links once each, down first, tunnels and software', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/networks')
  await expect(page.getByRole('heading', { name: 'Networks', exact: true })).toBeVisible()
  await expect(page.getByText('3 down')).toBeVisible()
  await expect(page.getByTestId('network-problem')).toHaveCount(6)
  // A problem off any server links to nothing.
  await expect(page.getByTestId('network-problem').first()).not.toContainText('Open ')
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
  await expect(page.getByTestId('cloud-vpn').filter({ hasText: 'jakarta-office' })).toContainText('0 of 2 up')
  await expect(page.getByTestId('cloud-vpn').filter({ hasText: 'ha-gw to dc-jkt' })).toContainText('Partly up')
  // The server's cloudflared names the tunnel it serves; the account's view names the server.
  await expect(page.getByTestId('cf-tunnel').filter({ hasText: 'gw-jkt-01' })).toContainText('web-public')
  await expect(page.getByTestId('cf-account-tunnel').filter({ hasText: 'web-public' })).toContainText('gw-jkt-01')
  await expect(page.getByTestId('cf-account-tunnel').filter({ hasText: 'intranet' })).toContainText('Unknown machine 114.4.22.7')
  await expect(page.getByTestId('cf-account')).toContainText('cannot read Magic WAN')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks.png`, fullPage: true })
})

test('networks: connecting a Cloudflare account says why a token is refused', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/networks')
  await page.getByRole('button', { name: 'Connect an account' }).click()
  const dialog = page.getByRole('dialog', { name: 'Connect a Cloudflare account' })
  await dialog.getByLabel('Name').fill('Main account')
  await dialog.getByLabel('Account id').fill('0123456789abcdef0123456789abcdef')
  await dialog.getByLabel('API token').fill('wrong-token')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks-connect-cloudflare.png` })
  await dialog.getByRole('button', { name: 'Connect' }).click()
  await expect(page.getByText(/needs Cloudflare Tunnel Read/)).toBeVisible()
  await expect(dialog).toBeVisible()
})

test('networks: server networks wait for a second admin and show what each server did', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/networks')
  const mesh = page.getByTestId('wg-network').filter({ hasText: 'Database mesh' })
  await expect(mesh.getByTestId('wg-member')).toHaveCount(3)
  await expect(mesh.getByTestId('wg-member').filter({ hasText: 'gw-jkt-01' })).toContainText('Undone')
  await expect(mesh.getByTestId('wg-member').filter({ hasText: 'gw-jkt-01' })).toContainText('Dials out only')
  await expect(mesh.getByTestId('wg-member').filter({ hasText: 'db-prod-01' })).toContainText('routes 192.168.10.0/24')
  // Someone else's change: approve it here.
  await expect(mesh.getByTestId('wg-pending')).toContainText('rina@northwind.id asks to take out gw-jkt-01')
  await expect(mesh.getByRole('button', { name: 'Approve' })).toBeVisible()
  // Your own, never approved: on no server yet, and you cannot approve it while someone else can.
  const hub = page.getByTestId('wg-network').filter({ hasText: 'Branch hub' })
  await expect(hub).toContainText('Waiting for approval')
  await expect(hub.getByTestId('wg-member')).toHaveCount(0)
  await expect(hub.getByTestId('wg-pending')).toContainText('gw-jkt-01 10.89.0.1 (hub)')
  await expect(hub.getByRole('button', { name: 'Approve' })).toBeDisabled()
  await expect(hub.getByRole('button', { name: 'Take back' })).toBeVisible()
  if (SHOTS) await page.getByTestId('wg-network').first().locator('xpath=ancestor::*[contains(@class,"mantine-Paper")][1]').screenshot({ path: `${SHOTS}/networks-server-networks.png` })

  await page.getByRole('button', { name: 'New network' }).click()
  const dialog = page.getByRole('dialog', { name: 'New server network' })
  await expect(dialog.getByLabel('Range')).toHaveValue('10.90.0.0/24')
  await dialog.getByLabel('Name').fill('Office')
  await dialog.getByLabel('Range').fill('10.88.0.0/24')
  await dialog.getByRole('combobox', { name: 'Servers' }).click()
  await page.getByRole('option').nth(0).click()
  await page.getByRole('option').nth(1).click()
  await dialog.getByText('Shape').click()
  await expect(dialog.getByTestId('wg-site-note')).toHaveCount(0)
  await dialog.getByRole('textbox', { name: /^Ranges .* routes$/ }).first().fill('192.168.10.0/24')
  await expect(dialog.getByTestId('wg-site-note')).toContainText('route to 10.88.0.0/24 through that server')
  await expect(dialog.getByRole('button', { name: 'Ask for approval' })).toBeDisabled()
  await dialog.getByLabel(/2-minute trial/).check()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks-new-server-network.png` })
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect(page.getByText(/overlaps the Argus network Database mesh/)).toBeVisible()
  await expect(dialog).toBeVisible()
})

test('networks: IPsec links show both ends and the key only on request', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/networks')
  const aws = page.getByTestId('ipsec-link').filter({ hasText: 'Jakarta office to AWS' })
  await expect(aws).toContainText('Down')
  await expect(aws.getByTestId('ipsec-cloud')).toContainText('jakarta-office in AWS prod (ap-southeast-3): the cloud says this tunnel is down (IPSEC IS DOWN)')
  await expect(page.getByTestId('ipsec-link').filter({ hasText: 'Branch firewall' }).getByTestId('ipsec-pending')).toContainText('rina@northwind.id asks to set it up')
  if (SHOTS) await aws.locator('xpath=ancestor::*[contains(@class,"mantine-Paper")][1]').screenshot({ path: `${SHOTS}/networks-ipsec-links.png` })
  await aws.getByRole('button', { name: 'Show key' }).click()
  await expect(page.getByTestId('ipsec-key')).toHaveText('Kq3vX9tLmP2wR8sYzN4bH7cJ1dF6gA0eUoIiQxWv')
  await page.keyboard.press('Escape')

  await page.getByRole('button', { name: 'New link' }).click()
  const dialog = page.getByRole('dialog', { name: 'New IPsec link' })
  await dialog.getByLabel('Name').fill('Office to Azure')
  await dialog.getByLabel('Other end').fill('20.1.2.3')
  await dialog.getByLabel('Ranges on this side').fill('192.168.10.0/24')
  await dialog.getByLabel('Ranges on the other side').fill('10.40.0.0/16')
  await dialog.getByText('Compatible', { exact: true }).click()
  await expect(dialog).toContainText('Azure gateway on its default policy')
  await dialog.getByText('Use the key the cloud made').click()
  await expect(dialog.getByLabel('Key', { exact: true })).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Ask for approval' })).toBeDisabled()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks-new-ipsec-link.png` })
})

test('networks: Argus makes the AWS end of a link, with its price, as a cloud change', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.route('**/api/v1/cloud/accounts', (r) => r.fulfill({ json: { bases: [], accounts: [{
    id: 'ca1', provider: 'aws', accountId: '412788931204', name: 'payments-prod', access: 'manage', environment: 'production',
    regions: ['ap-southeast-3'], manageMissing: [], counts: {}, syncErrors: {},
  }] } }))
  await page.route(/\/api\/v1\/cloud\/resources\?kind=network/, (r) => r.fulfill({ json: { items: [{
    accountId: 'ca1', provider: 'aws', accountName: 'payments-prod', environment: 'production', seenAt: ago(2),
    resource: { kind: 'network', id: 'vpc-0abc1234', name: 'prod-vpc', region: 'ap-southeast-3', state: 'available', size: '10.30.0.0/16' },
  }] } }))
  let asked: Record<string, unknown> | null = null
  await page.route('**/api/v1/cloud/changes', async (r) => {
    if (r.request().method() !== 'POST') return r.fulfill({ json: { changes: [] } })
    asked = r.request().postDataJSON()
    await r.fulfill({ status: 202, json: { id: 'c1', state: 'awaiting_approval', action: 'vpn-create', summary: 'Make the AWS Site-to-Site VPN "Jakarta office"' } })
  })
  await page.goto('/networks')
  await page.getByRole('button', { name: 'Make one in AWS' }).click()
  const dialog = page.getByRole('dialog', { name: 'Make the AWS end of a link' })
  await expect(dialog.getByTestId('vpn-cost')).toContainText('US$0.05 an hour')
  await dialog.getByLabel('Name', { exact: true }).fill('Jakarta office')
  await dialog.getByRole('combobox', { name: 'VPC', exact: true }).click()
  await page.getByRole('option', { name: /prod-vpc/ }).click()
  await dialog.getByRole('combobox', { name: 'Your server' }).click()
  await page.getByRole('option').first().click()
  await dialog.getByLabel('Its public address').fill('10.0.0.9')
  await expect(dialog.getByText('Its public IPv4 address, as AWS sees it')).toBeVisible()
  await dialog.getByLabel('Its public address').fill('36.72.1.9')
  await dialog.getByLabel('Range behind your server').fill('192.168.10.0/24')
  await expect(dialog.getByText('Empty for all of 10.30.0.0/16')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks-make-aws-vpn.png` })
  await dialog.getByRole('textbox', { name: 'Why' }).fill('The Jakarta office needs the payments VPC')
  await dialog.getByRole('button', { name: 'Ask for approval' }).click()
  await expect(dialog).toBeHidden()
  expect(asked).toMatchObject({ accountId: 'ca1', region: 'ap-southeast-3', action: 'vpn-create',
    spec: { name: 'Jakarta office', network: 'vpc-0abc1234', serverAddress: '36.72.1.9', serverRange: '192.168.10.0/24', ciphers: 'strong', propagate: true } })
})

const STAFF_NET = { id: 's1', name: 'Staff VPN', shape: 'staff', cidr: '10.66.0.0/22', port: 51900, iface: 'argus5e6f7a', state: 'active', generation: 4,
  requestedBy: 'rina@northwind.id', approvedBy: 'dev@northwind.id', createdAt: ago(60 * 24 * 3), approvedAt: ago(60 * 24 * 3),
  staff: { default: 'company', days: 90, mfa: true },
  members: [{ assetId: 'a3', hostname: 'gw-jkt-01', address: '10.66.0.1', endpoint: 'vpn.northwind.id', hub: true, state: 'applied', at: ago(20) }] }
const STAFF = { vpns: [{
  network: STAFF_NET, ready: true,
  devices: [
    { id: 'd1', networkId: 's1', userEmail: 'dev@northwind.id', name: 'Work laptop', address: '10.66.0.2', createdAt: ago(60 * 24 * 10), expiresAt: new Date(now + 80 * 86_400_000).toISOString() },
    { id: 'd2', networkId: 's1', userEmail: 'rina@northwind.id', name: 'Phone', address: '10.66.0.3', createdAt: ago(60 * 24 * 95), expiresAt: ago(60 * 24 * 5), expired: true },
  ],
  grants: [
    { teamId: 'tm1', teamName: 'Platform', ranges: ['10.88.0.0/24', '192.168.10.0/24'], logins: false, updatedAt: ago(600) },
    { teamId: 'tm2', teamName: 'Support', ranges: ['10.30.0.0/16'], logins: true, updatedAt: ago(300) },
  ],
  modes: { 'rina@northwind.id': 'full' },
  mine: { reach: ['10.88.0.0/24', '192.168.10.0/24', '10.30.0.0/16'], logins: ['10.30.0.0/16'], mode: 'company', needsCode: true, hasMfa: true },
}] }
const CONF = `[Interface]
PrivateKey = yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=
Address = 10.66.0.4/32

[Peer]
PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=
Endpoint = vpn.northwind.id:51900
AllowedIPs = 10.66.0.0/22, 10.88.0.0/24, 192.168.10.0/24, 10.30.0.0/16
PersistentKeepalive = 25
`

async function staffRoutes(page: Page) {
  await page.route('**/api/v1/networks/staff', (r) => r.fulfill({ json: STAFF }))
  await page.route('**/api/v1/teams', (r) => r.fulfill({ json: [
    { id: 'tm1', name: 'Platform', members: [], servers: [], createdBy: 'dev@northwind.id', createdAt: ago(9000) },
    { id: 'tm2', name: 'Support', members: [], servers: [], createdBy: 'dev@northwind.id', createdAt: ago(9000) },
    { id: 'tm3', name: 'Finance', members: [], servers: [], createdBy: 'dev@northwind.id', createdAt: ago(9000) },
  ] }))
}

test('networks: the staff VPN, what each team reaches, and that direct logins are not recorded', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await staffRoutes(page)
  let grant: unknown
  await page.route('**/api/v1/networks/staff/s1/grants/*', async (r) => { grant = r.request().postDataJSON(); await r.fulfill({ status: 204 }) })
  await page.goto('/networks')
  const vpn = page.getByTestId('staff-vpn')
  await expect(vpn.getByTestId('wg-member')).toContainText('gw-jkt-01')
  await expect(vpn.getByTestId('staff-grant')).toHaveCount(2)
  await expect(vpn.getByTestId('staff-grant').filter({ hasText: 'Support' })).toContainText('Allowed, not recorded')
  await expect(vpn.getByTestId('staff-grant').filter({ hasText: 'Platform' })).toContainText('Through Argus only')
  await expect(vpn.getByTestId('staff-mode')).toContainText('rina@northwind.id')
  await expect(vpn.getByTestId('staff-device').filter({ hasText: 'Phone' })).toContainText('Expired')
  // The staff VPN is not listed again among the server networks.
  await expect(page.getByTestId('wg-network').filter({ hasText: 'Staff VPN' })).toHaveCount(1)
  if (SHOTS) await vpn.locator('xpath=ancestor::*[contains(@class,"mantine-Paper")][1]').screenshot({ path: `${SHOTS}/networks-staff-vpn.png` })

  await vpn.getByRole('button', { name: 'Give a team access' }).click()
  const dialog = page.getByRole('dialog', { name: 'Give a team access' })
  await dialog.getByRole('combobox', { name: 'Team' }).click()
  await expect(page.getByRole('option')).toHaveCount(1)
  await page.getByRole('option', { name: 'Finance' }).click()
  await dialog.getByLabel('Ranges it reaches').fill('10.40.0.0/24, 10.41.0.0/24')
  await dialog.getByLabel('Allow SSH and RDP straight to these servers').check()
  await expect(dialog.getByTestId('staff-logins-note')).toContainText('not recorded')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks-staff-grant.png` })
  await dialog.getByRole('button', { name: 'Save' }).click()
  await expect(dialog).toBeHidden()
  expect(grant).toEqual({ ranges: ['10.40.0.0/24', '10.41.0.0/24'], logins: true })
})

test('my vpn: a device profile is shown once, with a QR code, after an authenticator code', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await signIn(page)
  await staffRoutes(page)
  let asked: unknown
  await page.route('**/api/v1/networks/staff/s1/devices', async (r) => {
    asked = r.request().postDataJSON()
    await r.fulfill({ json: { mode: 'company', config: CONF, device: { id: 'd3', networkId: 's1', userEmail: 'dev@northwind.id', name: 'Home desktop',
      address: '10.66.0.4', createdAt: ago(0), expiresAt: new Date(now + 90 * 86_400_000).toISOString() } } })
  })
  await page.goto('/')
  await page.getByRole('link', { name: 'My VPN' }).first().click()
  await expect(page).toHaveURL(/\/vpn$/)
  const card = page.getByTestId('my-vpn')
  await expect(card).toContainText('Company only')
  await expect(card).toContainText('SSH and RDP straight to 10.30.0.0/16 is allowed, and is not recorded by Argus')
  await expect(card.getByTestId('my-device')).toHaveCount(1)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/my-vpn.png` })

  await card.getByRole('button', { name: 'Add a device' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add a device' })
  await dialog.getByLabel('Device').fill('Home desktop')
  await expect(dialog.getByRole('button', { name: 'Make the profile' })).toBeDisabled()
  await dialog.getByLabel('Code from your authenticator app').fill('123456')
  await dialog.getByRole('button', { name: 'Make the profile' }).click()
  const made = page.getByRole('dialog', { name: 'Home desktop is ready' })
  await expect(made.getByTestId('staff-profile')).toContainText('only time the profile is shown')
  await expect(made.getByRole('img', { name: 'QR code of the VPN profile' })).toBeVisible()
  await expect(made.getByRole('button', { name: 'Scan or download it first' })).toBeDisabled()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/my-vpn-profile.png` })
  const dl = page.waitForEvent('download')
  await made.getByRole('button', { name: /^Download/ }).click()
  expect((await dl).suggestedFilename()).toBe('Home-desktop.conf')
  await made.getByRole('button', { name: 'Done' }).click()
  await expect(made).toBeHidden()
  expect(asked).toEqual({ name: 'Home desktop', code: '123456' })
})

test('networks: on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await signIn(page)
  await page.goto('/networks')
  await expect(page.getByText('3 down')).toBeVisible()
  const width = await page.evaluate(() => document.documentElement.scrollWidth)
  expect(width).toBeLessThanOrEqual(390)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/networks-phone.png`, fullPage: true })
})
