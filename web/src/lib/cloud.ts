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
  /** The role changes are made with; absent when the account is read only. */
  manageRoleArn?: string
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
  /** Set when the template makes the manage role too. */
  manageRoleName?: string
  template: string
  command: string
}

export interface CloudAccountInput {
  provider: 'aws'
  name: string
  accountId: string
  roleArn: string
  manageRoleArn?: string
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

export function prepareCloudAccount(accountId: string, manage = false): Promise<CloudPrepared> {
  return send('POST', '/api/v1/cloud/accounts/prepare', { provider: 'aws', accountId, manage })
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

export function useAllowCloudChanges() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: ({ id, roleArn }: { id: string; roleArn: string }) =>
      send<CloudAccount>('PUT', `/api/v1/cloud/accounts/${encodeURIComponent(id)}/manage`, { roleArn }),
    onSettled: done,
  })
}

export function useMakeCloudReadOnly() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (id: string) => send<CloudAccount>('DELETE', `/api/v1/cloud/accounts/${encodeURIComponent(id)}/manage`),
    onSettled: done,
  })
}

/* ── Changes ────────────────────────────────────────────────────────────── */

export type CloudAction = 'launch' | 'start' | 'stop' | 'reboot' | 'resize' | 'tags' | 'protect' | 'unprotect' | 'remove'
export type CloudChangeState = 'awaiting_approval' | 'applying' | 'applied' | 'failed' | 'declined' | 'cancelled'

export interface CloudChange {
  id: string
  accountId: string
  accountName: string
  region: string
  action: CloudAction
  level: 'operate' | 'change' | 'remove'
  environment: CloudEnvironment
  resourceId: string
  resourceName: string
  spec: Record<string, unknown>
  plan: Partial<CloudRemovePlanView>
  planHash: string
  summary: string
  reason: string
  state: CloudChangeState
  requestedBy: string
  requestedAt: string
  decidedBy: string | null
  decidedAt: string | null
  appliedAt: string | null
  requestId: string
  result: { resourceId?: string; requestId?: string; snapshots?: Record<string, string>; detail?: string }
  error: string
}

export interface CloudImage { family: string; label: string; id: string; name: string; linux: boolean }

export interface CloudLaunchOptions {
  images: CloudImage[]
  sizes: string[]
  subnets: CloudItem[]
  firewalls: CloudItem[]
  environment: CloudEnvironment
  needsApproval: boolean
  agentProblem?: string
}

export interface CloudVolume {
  id: string
  device: string
  sizeGb: number
  deleted: boolean
  lastSnapshotAt?: string
  lastSnapshotId?: string
}

export interface CloudRemovePlanView {
  plan: {
    id: string; name: string; state: string; size: string; protected: boolean; managedBy?: string
    volumes: CloudVolume[]; publicIp?: string; elasticIp?: string
  }
  hash: string
  environment: CloudEnvironment
  needsApproval: boolean
  server?: string
  refusal?: string
}

export interface CloudChangeInput {
  accountId: string
  region: string
  action: CloudAction
  resourceId?: string
  spec?: Record<string, unknown>
  reason?: string
  confirm?: string
}

const changesKey = ['cloud', 'changes'] as const

export const cloudChangesQuery = (opts: { pending?: boolean; enabled?: boolean } = {}) =>
  queryOptions({
    queryKey: [...changesKey, opts.pending ? 'pending' : 'all'],
    queryFn: () => get<{ changes: CloudChange[] }>(`/api/v1/cloud/changes${opts.pending ? '?pending=1' : ''}`).then((r) => r.changes),
    enabled: isConfigured() && opts.enabled !== false,
    refetchInterval: 30_000,
  })

export const cloudLaunchOptionsQuery = (accountId: string | null, region: string | null) =>
  queryOptions({
    queryKey: ['cloud', 'launch-options', accountId, region],
    queryFn: () => get<CloudLaunchOptions>(
      `/api/v1/cloud/launch-options?accountId=${encodeURIComponent(accountId ?? '')}&region=${encodeURIComponent(region ?? '')}`),
    enabled: isConfigured() && Boolean(accountId && region),
    staleTime: 5 * 60_000,
  })

export const cloudRemovePlanQuery = (it: CloudItem | null) =>
  queryOptions({
    queryKey: ['cloud', 'remove-plan', it?.accountId, it?.resource.region, it?.resource.id],
    queryFn: () => get<CloudRemovePlanView>(`/api/v1/cloud/remove-plan?accountId=${encodeURIComponent(it!.accountId)}` +
      `&region=${encodeURIComponent(it!.resource.region)}&resourceId=${encodeURIComponent(it!.resource.id)}`),
    enabled: isConfigured() && Boolean(it),
    staleTime: 0,
    gcTime: 0,
  })

export function useAskCloudChange() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (in_: CloudChangeInput) => send<CloudChange>('POST', '/api/v1/cloud/changes', in_),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: changesKey })
      // The change is read back by the background sync a moment later.
      window.setTimeout(() => void qc.invalidateQueries({ queryKey: ['cloud'] }), 5_000)
    },
  })
}

export function useDecideCloudChange() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, verb }: { id: string; verb: 'approve' | 'decline' | 'cancel' }) =>
      send<CloudChange>('POST', `/api/v1/cloud/changes/${encodeURIComponent(id)}/${verb}`),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: changesKey })
      window.setTimeout(() => void qc.invalidateQueries({ queryKey: ['cloud'] }), 5_000)
    },
  })
}

/** Which environments need a second person, for what: the control plane's rule. */
export function needsApproval(env: CloudEnvironment, level: CloudChange['level']): boolean {
  if (env === 'production') return level === 'change' || level === 'remove'
  if (env === 'staging') return level === 'remove'
  return false
}

/** The environment a resource is in: its Environment tag, else its account's. */
export function envOf(tags: Record<string, string> | undefined, account: CloudEnvironment): CloudEnvironment {
  for (const [k, v] of Object.entries(tags ?? {})) {
    if (k.toLowerCase() !== 'environment' && k.toLowerCase() !== 'env') continue
    const x = v.trim().toLowerCase()
    if (['production', 'prod', 'prd', 'live'].includes(x)) return 'production'
    if (['staging', 'stage', 'stg', 'uat', 'preprod'].includes(x)) return 'staging'
    if (['development', 'dev', 'test', 'testing', 'sandbox'].includes(x)) return 'development'
  }
  return account
}

export const CHANGE_STATE: Record<CloudChangeState, { label: string; color: string }> = {
  awaiting_approval: { label: 'Waiting for approval', color: 'amber' },
  applying: { label: 'Running', color: 'sky' },
  applied: { label: 'Done', color: 'teal' },
  failed: { label: 'Failed', color: 'rose' },
  declined: { label: 'Declined', color: 'slate' },
  cancelled: { label: 'Withdrawn', color: 'slate' },
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
