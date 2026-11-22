import { queryOptions } from '@tanstack/react-query'
import { get, post, send } from '~/lib/live'

/*
 * Networks: the VPNs and tunnels on the servers Argus manages, as their agents
 * report them (internal/control/networks.go). Read-only.
 *
 * A link's state is as its agent read it. The agent reports within about a
 * minute when a link goes up or down, so that is current while the agent keeps
 * checking in; a quiet agent's links come back as "unknown".
 */

export type PeerState = 'up' | 'down' | 'idle' | 'never' | 'unknown'

export interface NetworkServer {
  assetId: string
  hostname: string
  reported: boolean
  checkedAt?: string
  quiet: boolean
  lastSeen?: string
  wgTool: boolean
}

export interface NetworkSoftware {
  assetId: string
  hostname: string
  kind: string
  name: string
  version?: string
  running: boolean
  unit?: string
  container?: string
}

export interface NetworkPeer {
  publicKey: string
  endpoint?: string
  allowedIps: string[]
  latestHandshake?: string
  keepalive?: number
  rxBytes: number
  txBytes: number
  state: PeerState
  peerAssetId?: string
  peerHostname?: string
  peerInterface?: string
}

export interface NetworkInterface {
  assetId: string
  hostname: string
  name: string
  publicKey?: string
  listenPort?: number
  addresses: string[]
  up: boolean
  rxBytes: number
  txBytes: number
  peers: NetworkPeer[]
  peersKnown: boolean
}

export interface NetworkTunnel {
  assetId: string
  hostname: string
  metrics: string
  state: 'up' | 'down' | 'unknown'
  readyConnections: number
  connectorId?: string
  version?: string
  error?: string
  /** The Cloudflare tunnel it serves, when a connected account lists it. */
  tunnelId?: string
  tunnelName?: string
}

export interface NetworkProblem {
  severity: 'down' | 'warn'
  kind: string
  key: string
  /** Empty for what is not on a server: a cloud VPN, a Cloudflare tunnel. */
  assetId: string
  hostname: string
  title: string
  detail: string
}

export interface CloudTunnel { Address: string; Up: boolean; Why: string }

/** A site-to-site VPN in a connected cloud account. */
export interface NetworkCloudVPN {
  accountId: string
  accountName: string
  provider: 'aws' | 'azure' | 'gcp' | string
  region: string
  id: string
  name: string
  /** up, partial, down, unknown, or the cloud's own word while it changes. */
  state: string
  tunnelsUp: number
  tunnelsTotal: number
  tunnels: CloudTunnel[]
  remoteAddress?: string
  remoteNetworks: string[]
  gateway?: string
  type?: string
  routing?: string
  seenAt: string
}

export interface CFConnector {
  clientId: string
  version?: string
  originIp?: string
  locations: string[]
  connections: number
  assetId?: string
  hostname?: string
  old: boolean
}

export interface CFTunnel {
  id: string
  name: string
  kind: 'tunnel' | 'warp-connector'
  status: 'healthy' | 'degraded' | 'down' | 'inactive' | string
  connsActiveAt?: string
  connsInactiveAt?: string
  connectors: CFConnector[]
  routes: string[]
}

export interface CFDevice { name: string; user?: string; os?: string; version?: string; last_seen?: string }
export interface CFMagicTunnel { kind: 'ipsec' | 'gre'; name: string; customerEndpoint: string; cloudflareEndpoint: string; interfaceAddress: string }
export interface CFRoute { network: string; tunnel_id: string; tunnel_name: string; comment: string }

export type CFList = 'tunnels' | 'warpConnectors' | 'routes' | 'devices' | 'magicWan'

/** A connected Cloudflare account as last read. */
export interface NetworkCFAccount {
  id: string
  name: string
  accountId: string
  syncedAt?: string
  stale: boolean
  lastError?: string
  read: CFList[]
  missing?: Partial<Record<CFList, string>>
  tunnels: CFTunnel[]
  routes: CFRoute[]
  devices: CFDevice[]
  devicesTotal: number
  magicWan: CFMagicTunnel[]
}

export interface NetworksView {
  servers: NetworkServer[]
  software: NetworkSoftware[]
  interfaces: NetworkInterface[]
  tunnels: NetworkTunnel[]
  problems: NetworkProblem[]
  cloudVpns: NetworkCloudVPN[]
  cloudflare: NetworkCFAccount[]
}

export const networksQuery = () =>
  queryOptions({ queryKey: ['networks'] as const, queryFn: () => get<NetworksView>('/api/v1/networks'), refetchInterval: 30_000 })

/* ── Cloudflare accounts ──────────────────────────────────────────────────── */

export interface CloudflareAccount {
  id: string
  name: string
  accountId: string
  tokenTail: string
  syncedAt?: string
  lastError?: string
  createdBy: string
  createdAt: string
}

export interface CloudflareView { accounts: CloudflareAccount[]; canSave: boolean; cannotSave?: string }

export const cloudflareQuery = () =>
  queryOptions({ queryKey: ['networks', 'cloudflare'] as const, queryFn: () => get<CloudflareView>('/api/v1/networks/cloudflare') })

export interface CloudflareInput { name: string; accountId?: string; token?: string }

export const addCloudflare = (input: CloudflareInput) => post<CloudflareAccount>('/api/v1/networks/cloudflare', input)
export const changeCloudflare = (id: string, input: CloudflareInput) =>
  send<CloudflareAccount>('PUT', `/api/v1/networks/cloudflare/${encodeURIComponent(id)}`, input)
export const removeCloudflare = (id: string) => send<void>('DELETE', `/api/v1/networks/cloudflare/${encodeURIComponent(id)}`)
export const syncCloudflare = (id: string) => post<CloudflareAccount>(`/api/v1/networks/cloudflare/${encodeURIComponent(id)}/sync`, {})

export const CF_STATUS: Record<string, { label: string; color: string; hint: string }> = {
  healthy: { label: 'Healthy', color: 'teal', hint: 'Cloudflare has connections from every connector.' },
  degraded: { label: 'Degraded', color: 'amber', hint: 'A connector has fewer connections than it should. It still works.' },
  down: { label: 'Down', color: 'rose', hint: 'No connector is connected: its hostnames and routes reach nothing.' },
  inactive: { label: 'Inactive', color: 'slate', hint: 'Has not run in a long time, or never has.' },
}

export const CLOUD_VPN_STATE: Record<string, { label: string; color: string }> = {
  up: { label: 'Up', color: 'teal' },
  partial: { label: 'Partly up', color: 'amber' },
  down: { label: 'Down', color: 'rose' },
  unknown: { label: 'Not known', color: 'slate' },
}

export const PROVIDER: Record<string, string> = { aws: 'AWS', azure: 'Azure', gcp: 'Google Cloud' }

/** What a Cloudflare list is called, for "the token cannot read …". */
export const CF_LIST: Record<CFList, string> = {
  tunnels: 'tunnels', warpConnectors: 'WARP Connectors', routes: 'private network routes', devices: 'WARP devices', magicWan: 'Magic WAN',
}

export const PEER_STATE: Record<PeerState, { label: string; color: string; hint: string }> = {
  up: { label: 'Up', color: 'teal', hint: 'A handshake in the 3 minutes before the agent checked.' },
  down: { label: 'Down', color: 'rose', hint: 'This side keeps the link alive, and has had no handshake for over 3 minutes.' },
  never: { label: 'Never connected', color: 'rose', hint: 'This side is set to connect and has never completed a handshake.' },
  idle: { label: 'Idle', color: 'slate', hint: 'No recent handshake, which is normal here: this side waits to be dialled, or the link only handshakes when there is traffic.' },
  unknown: { label: 'Not known', color: 'slate', hint: 'The agent has not checked in lately.' },
}

/** One WireGuard link counted once, whichever end it is seen from. */
export interface Link {
  key: string
  a: { assetId: string; hostname: string; iface: string }
  b: { assetId?: string; hostname?: string; iface?: string; endpoint?: string; publicKey: string }
  state: PeerState
  latestHandshake?: string
  allowedIps: string[]
  rxBytes: number
  txBytes: number
}

const RANK: Record<PeerState, number> = { down: 0, never: 1, up: 2, idle: 3, unknown: 4 }

/**
 * Every peer as a link. A link between two Argus servers appears on both, so
 * it is kept once, from the side whose view is the more serious (a down on
 * either end is down).
 */
export function linksOf(ifs: NetworkInterface[]): Link[] {
  const out = new Map<string, Link>()
  for (const i of ifs) {
    for (const p of i.peers) {
      const pair = p.peerAssetId ? [`${i.assetId}/${i.name}`, `${p.peerAssetId}/${p.peerInterface}`].sort().join('|') : `${i.assetId}/${i.name}/${p.publicKey}`
      const l: Link = {
        key: pair,
        a: { assetId: i.assetId, hostname: i.hostname, iface: i.name },
        b: { assetId: p.peerAssetId, hostname: p.peerHostname, iface: p.peerInterface, endpoint: p.endpoint, publicKey: p.publicKey },
        state: p.state, latestHandshake: p.latestHandshake, allowedIps: p.allowedIps, rxBytes: p.rxBytes, txBytes: p.txBytes,
      }
      const seen = out.get(pair)
      if (!seen || RANK[l.state] < RANK[seen.state]) out.set(pair, l)
    }
  }
  return [...out.values()].sort((x, y) => RANK[x.state] - RANK[y.state] || x.a.hostname.localeCompare(y.a.hostname))
}

/** "1.2 GB", "340 kB", "0 B". */
export function formatBytes(n: number): string {
  const units = ['B', 'kB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1000 && i < units.length - 1) { v /= 1000; i++ }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

/* ── Server networks (WireGuard set up through the agents) ────────────────── */

export type WGShape = 'mesh' | 'hub'
export type WGMemberState = 'no-agent' | 'unsupported' | 'waiting-key' | 'applying' | 'trial' | 'applied' | 'failed' | 'rolled-back'

export interface WGMember {
  assetId: string
  hostname: string
  address: string
  hub?: boolean
  endpoint?: string
  /** Private ranges behind it that it routes for the others: a site link. */
  routes?: string[]
  publicKey?: string
  state: WGMemberState
  message?: string
  at?: string
}

export interface WGMemberDraft { assetId: string; hostname?: string; address?: string; hub?: boolean; endpoint?: string; routes?: string[] }

export interface WGNetworkDraft { name: string; shape: WGShape; cidr: string; port: number; members: WGMemberDraft[]; remove?: boolean }

export interface WGNetwork {
  id: string
  name: string
  shape: WGShape
  cidr: string
  port: number
  iface: string
  /** waiting: never approved, on no server yet. */
  state: 'waiting' | 'active'
  members: WGMember[]
  pending?: WGNetworkDraft
  pendingBy?: string
  pendingAt?: string
  requestedBy: string
  approvedBy?: string
  approvedAt?: string
  generation: number
  createdAt: string
  approveOwn?: boolean
}

export const wgNetworksQuery = () =>
  queryOptions({
    queryKey: ['networks', 'wireguard'] as const,
    queryFn: () => get<{ networks: WGNetwork[] }>('/api/v1/networks/wireguard').then((v) => v.networks),
    refetchInterval: 15_000,
  })

const wgPath = (id?: string, tail = '') => `/api/v1/networks/wireguard${id ? `/${encodeURIComponent(id)}` : ''}${tail}`
export const askWGNetwork = (d: WGNetworkDraft) => post<WGNetwork>(wgPath(), d)
export const askWGChange = (id: string, d: WGNetworkDraft) => send<WGNetwork>('PUT', wgPath(id), d)
export const askWGRemove = (id: string) => send<WGNetwork>('DELETE', wgPath(id))
export const approveWG = (id: string, confirmSelf?: string) => post<void>(wgPath(id, '/approve'), confirmSelf ? { confirmSelf } : {})
export const declineWG = (id: string) => post<void>(wgPath(id, '/decline'), {})

export const WG_MEMBER_STATE: Record<WGMemberState, { label: string; color: string; hint: string }> = {
  'no-agent': { label: 'Agent too old', color: 'slate', hint: 'Its agent has not reported on server networks. Update the agent.' },
  unsupported: { label: 'Not possible here', color: 'slate', hint: 'Networks are set up on Linux servers with systemd.' },
  'waiting-key': { label: 'Making its key', color: 'sky', hint: 'The server makes its key on its next heartbeat. The private key never leaves it.' },
  applying: { label: 'Setting up', color: 'sky', hint: 'The server is setting the network up.' },
  trial: { label: 'On trial', color: 'sky', hint: 'Applied. The server keeps it once it has reached Argus through the change, within 2 minutes.' },
  applied: { label: 'Applied', color: 'teal', hint: 'The server has the network as approved.' },
  failed: { label: 'Failed', color: 'rose', hint: 'The server could not apply it.' },
  'rolled-back': { label: 'Undone', color: 'rose', hint: 'The change was undone on the server, so it is as it was before.' },
}
