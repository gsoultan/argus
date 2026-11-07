/*
 * Cloud: connected accounts and what Argus last read in them.
 *
 * Live only. An invented AWS inventory shown as if it were real would invite
 * acting on it, so against no control plane these queries do not run and
 * the page says it needs one. Its own file, so only Cloud pays for it.
 */
import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, isConfigured, send } from '~/lib/live'

export type CloudEnvironment = 'production' | 'staging' | 'development'
export type CloudKind = 'instance' | 'volume' | 'firewall' | 'network' | 'subnet' | 'address'

export interface CloudAccount {
  id: string
  provider: 'aws'
  name: string
  accountId: string
  roleArn: string
  access: 'read' | 'manage'
  environment: CloudEnvironment
  identityArn: string
  regions: string[]
  connectedBy: string
  connectedAt: string
  syncedAt: string | null
  syncErrors: Record<string, string>
  counts: Partial<Record<CloudKind, number>>
}

export interface CloudBase {
  provider: string
  principal?: string
  source?: string
  error?: string
}

export interface CloudRef { kind: CloudKind; id: string; name?: string }

export interface CloudRule {
  inbound: boolean
  protocol: string
  fromPort: number
  toPort: number
  source: string
  description?: string
}

export interface CloudResource {
  kind: CloudKind
  id: string
  name: string
  region: string
  zone?: string
  state?: string
  size?: string
  createdAt?: string
  addresses?: string[]
  publicIp?: string
  network?: string
  links?: CloudRef[]
  rules?: CloudRule[]
  tags?: Record<string, string>
  details?: Record<string, string>
}

export interface CloudItem {
  accountId: string
  accountName: string
  environment: CloudEnvironment
  firstSeen: string
  seenAt: string
  resource: CloudResource
  /** A match by address or name, not something the agent reported. */
  server?: { id: string; hostname: string; by: 'address' | 'name' }
}

export interface CloudPrepared {
  provider: 'aws'
  accountId: string
  externalId: string
  principal: string
  source: string
  roleName: string
  template: string
  command: string
}

export interface CloudAccountInput {
  provider: 'aws'
  name: string
  accountId: string
  roleArn: string
  environment: CloudEnvironment
}

const accountsKey = ['cloud', 'accounts'] as const
const itemsKey = (kind: CloudKind) => ['cloud', 'items', kind] as const

export const cloudAccountsQuery = () =>
  queryOptions({
    queryKey: accountsKey,
    queryFn: () => get<{ accounts: CloudAccount[]; bases: CloudBase[] }>('/api/v1/cloud/accounts'),
    enabled: isConfigured(),
    refetchInterval: 60_000,
  })

export const cloudItemsQuery = (kind: CloudKind) =>
  queryOptions({
    queryKey: itemsKey(kind),
    queryFn: () => get<{ items: CloudItem[] }>(`/api/v1/cloud/resources?kind=${kind}`).then((r) => r.items),
    enabled: isConfigured(),
    refetchInterval: 60_000,
  })

export function prepareCloudAccount(accountId: string): Promise<CloudPrepared> {
  return send('POST', '/api/v1/cloud/accounts/prepare', { provider: 'aws', accountId })
}

export function templateURL(accountId: string): string {
  return `/api/v1/cloud/accounts/template?accountId=${encodeURIComponent(accountId)}`
}

function useInvalidate() {
  const qc = useQueryClient()
  return () => void qc.invalidateQueries({ queryKey: ['cloud'] })
}

export function useConnectCloudAccount() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (in_: CloudAccountInput) => send<CloudAccount>('POST', '/api/v1/cloud/accounts', in_),
    onSettled: done,
  })
}

export function useDisconnectCloudAccount() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (id: string) => send('DELETE', `/api/v1/cloud/accounts/${encodeURIComponent(id)}`),
    onSettled: done,
  })
}

export function useSyncCloudAccount() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => send('POST', `/api/v1/cloud/accounts/${encodeURIComponent(id)}/sync`),
    // The read runs in the background; look again shortly.
    onSettled: () => window.setTimeout(() => void qc.invalidateQueries({ queryKey: ['cloud'] }), 4_000),
  })
}

/* ── Reading what came back ─────────────────────────────────────────────── */

/** Ports that should never be open to the whole internet. */
export const RISKY_PORTS: Record<number, string> = {
  22: 'SSH', 3389: 'Remote Desktop', 5432: 'PostgreSQL', 3306: 'MySQL', 1433: 'SQL Server',
  27017: 'MongoDB', 6379: 'Redis', 9200: 'Elasticsearch',
}

/** What a firewall lets the whole internet reach, by name. */
export function openToInternet(r: CloudResource): string[] {
  const out = new Set<string>()
  for (const rule of r.rules ?? []) {
    if (!rule.inbound || (rule.source !== '0.0.0.0/0' && rule.source !== '::/0')) continue
    for (const [port, name] of Object.entries(RISKY_PORTS)) {
      const p = Number(port)
      if (rule.protocol === 'all' || (rule.protocol === 'tcp' && rule.fromPort <= p && p <= rule.toPort)) out.add(name)
    }
  }
  return [...out]
}

export function portText(r: CloudRule): string {
  if (r.protocol === 'all') return 'all traffic'
  if (r.protocol === 'icmp') return 'ping (ICMP)'
  const ports = r.fromPort === r.toPort ? String(r.fromPort) : `${r.fromPort}–${r.toPort}`
  return `${r.protocol} ${ports}`
}

/** How long ago an account was read, and whether that is too long. */
export function staleness(a: CloudAccount, now = Date.now()): 'fresh' | 'late' | 'never' {
  if (!a.syncedAt) return 'never'
  return now - Date.parse(a.syncedAt) > 3 * 5 * 60_000 ? 'late' : 'fresh'
}

export const ENV_COLOR: Record<CloudEnvironment, string> = {
  production: 'rose', staging: 'amber', development: 'teal',
}

export const ENV_LABEL: Record<CloudEnvironment, string> = {
  production: 'Production', staging: 'Staging', development: 'Development',
}

const STATE_COLOR: Record<string, string> = {
  running: 'teal', 'in-use': 'teal', available: 'slate', attached: 'teal', unattached: 'amber',
  pending: 'sky', stopping: 'amber', 'shutting-down': 'amber', stopped: 'slate', error: 'rose',
}

export function stateColor(s: string | undefined): string {
  return (s && STATE_COLOR[s]) || 'slate'
}

export function stateLabel(s: string | undefined): string {
  if (!s) return '—'
  if (s === 'in-use') return 'In use'
  return s.charAt(0).toUpperCase() + s.slice(1).replace('-', ' ')
}
