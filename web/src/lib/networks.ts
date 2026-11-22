import { queryOptions } from '@tanstack/react-query'
import { get } from '~/lib/live'

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
}

export interface NetworkProblem {
  severity: 'down' | 'warn'
  kind: string
  assetId: string
  hostname: string
  title: string
  detail: string
}

export interface NetworksView {
  servers: NetworkServer[]
  software: NetworkSoftware[]
  interfaces: NetworkInterface[]
  tunnels: NetworkTunnel[]
  problems: NetworkProblem[]
}

export const networksQuery = () =>
  queryOptions({ queryKey: ['networks'] as const, queryFn: () => get<NetworksView>('/api/v1/networks'), refetchInterval: 30_000 })

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
