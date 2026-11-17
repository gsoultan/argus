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
/** A cloud an account can be connected in. A Google Cloud project is an account here. */
export type CloudProvider = 'aws' | 'gcp'

export const PROVIDER_LABEL: Record<CloudProvider, string> = { aws: 'AWS', gcp: 'Google Cloud' }

/** What one account is called in a provider. */
export const ACCOUNT_WORD: Record<CloudProvider, string> = { aws: 'account', gcp: 'project' }

/** An account's id as people read it: AWS's twelve digits in groups of four, a project id as it is. */
export function accountIdText(provider: CloudProvider, id: string): string {
  return provider === 'aws' ? id.replace(/(\d{4})(\d{4})(\d{4})/, '$1 $2 $3') : id
}

/** A Google Cloud project id: 6 to 30 lower case letters, digits and dashes, starting with a letter. */
export const GCP_PROJECT = /^[a-z][a-z0-9-]{4,28}[a-z0-9]$/
/** A Compute Engine name (disk, snapshot): 1 to 63 lower case letters, digits and dashes, starting with a letter. */
export const GCP_NAME = /^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$/
/** The persistent disk types Argus makes in Google Cloud. */
export const GCP_DISK_TYPES = [
  { value: 'pd-balanced', label: 'Balanced (pd-balanced)' },
  { value: 'pd-ssd', label: 'SSD (pd-ssd)' },
  { value: 'pd-standard', label: 'Standard, HDD (pd-standard)' },
]
/** Where a Google Cloud disk shows up on a Linux VM: Argus names its device after the disk. */
export function gcpDevicePath(disk: string): string {
  return `/dev/disk/by-id/google-${disk}`
}
export type CloudKind = 'instance' | 'volume' | 'firewall' | 'network' | 'subnet' | 'address' | 'snapshot' | 'image'
  | 'load-balancer' | 'target-group' | 'scaling-group' | 'database'

export interface CloudAccount {
  id: string
  provider: CloudProvider
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
  /** Permissions the manage role lacks, read from its policy at each sync. */
  manageMissing: string[]
  manageCheckedAt: string | null
  /** The id of the identity argus-control connects as. */
  identity: string
  /** That identity is gone, or no longer available here: Argus does not reach the account until it is moved. */
  identityMissing?: boolean
  /** When set, the only changes Argus makes in this account so far. */
  actions?: string[]
  /** When set, the only kinds Argus tags in this account so far. */
  tagKinds?: string[]
}

/** One of argus-control's identities this organization may use, and what the cloud says about it. */
export interface CloudBase {
  provider: string
  identity: string
  name: string
  /** New connections start with it. */
  default: boolean
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
  /** The cloud, and its own id for the account (twelve digits on AWS). */
  provider?: string
  providerAccount?: string
  accountName: string
  environment: CloudEnvironment
  firstSeen: string
  seenAt: string
  resource: CloudResource
  /**
   * The Argus server this instance is. "agent": the server's agent read this
   * instance's id from the cloud's metadata service. "address" and "name" are
   * matches, not something the agent reported.
   */
  server?: { id: string; hostname: string; by: 'agent' | 'address' | 'name' }
  /** Argus launched or created it, by its own record. Only these can be removed or deleted from Argus. */
  madeByArgus?: boolean
}

export interface CloudPrepared {
  provider: CloudProvider
  accountId: string
  externalId: string
  principal: string
  source: string
  /** The identity the roles trust, and its name. */
  identity: string
  identityName: string
  /** During a move, the old identity's principal the roles go on trusting too. */
  kept?: string
  roleName: string
  /** Set when the template makes the manage role too. */
  manageRoleName?: string
  /** Where the roles will be: their names are fixed, so nobody copies them back. */
  readRoleArn: string
  manageRoleArn?: string
  template: string
  command: string
  /** argus-control's own identity is in this account, so Argus can try to make the stack itself. */
  canDeploy: boolean
  /** The policy that identity needs to make the stack, for an administrator to attach. */
  deployRights?: string
}

/** Where the argus-access stack stands, when Argus can see it. */
export interface CloudStackState {
  status?: string
  reason?: string
  done: boolean
  failed: boolean
}

export interface CloudCheck {
  ready: boolean
  detail?: string
  stack?: CloudStackState
}

export interface CloudAccountInput {
  provider: CloudProvider
  name: string
  accountId: string
  /** Empty for the role the template makes. */
  roleArn?: string
  manageRoleArn?: string
  /** Read and change, through the template's manage role unless one is named. */
  manage?: boolean
  environment: CloudEnvironment
  /** Who Argus connects as; empty for the default. */
  identity?: string
}

const accountsKey = ['cloud', 'accounts'] as const
const itemsKey = (kind: CloudKind) => ['cloud', 'items', kind] as const

/** The instance a server's agent says it is, from the cloud's metadata service. */
export interface ReportedInstance {
  provider: string
  account: string
  region: string
  zone?: string
  id: string
  type?: string
}

/** A server's cloud side: what its agent reported, and the instance in a connected account. */
export interface AssetCloud {
  reported: ReportedInstance | null
  instance: CloudItem | null
}

/**
 * Only with a control plane: cloud accounts are never shown from sample
 * data, so without one the card is not drawn at all.
 */
export const assetCloudQuery = (assetId: string, enabled = true) =>
  queryOptions({
    queryKey: ['asset', assetId, 'cloud'] as const,
    queryFn: () => get<AssetCloud>(`/api/v1/assets/${encodeURIComponent(assetId)}/cloud`),
    enabled: enabled && isConfigured(),
    refetchInterval: 60_000,
    retry: false,
  })

export const cloudAccountsQuery = () =>
  queryOptions({
    queryKey: accountsKey,
    queryFn: () => get<{ accounts: CloudAccount[]; bases: CloudBase[] }>('/api/v1/cloud/accounts'),
    enabled: isConfigured(),
    refetchInterval: 60_000,
  })

/**
 * How often a list of resources is asked for again: quickly while anything
 * in it is between two states, since the control plane re-reads those
 * regions sooner too, and once a minute otherwise.
 */
export function itemsRefetchEvery(items: CloudItem[] | undefined): number {
  return items?.some((it) => settling(it.resource.state)) ? ITEMS_SETTLING_EVERY : ITEMS_EVERY
}

export const ITEMS_EVERY = 60_000
export const ITEMS_SETTLING_EVERY = 10_000

/**
 * Whether a state is on its way to another one: a snapshot being taken, an
 * instance starting or stopping, a disk being made. Mirrors Settling in
 * internal/control/cloud.
 */
export function settling(state: string | undefined): boolean {
  return state !== undefined && SETTLING.has(state)
}

const SETTLING = new Set(['pending', 'stopping', 'shutting-down', 'creating', 'deleting', 'provisioning', 'modifying', 'optimizing', 'uploading'])

/**
 * The latest read of an opened item, found by account, kind and id in the
 * lists as they are now. A panel opened on an item shows this, not the copy
 * that was clicked, so it moves when the inventory does. When the item is
 * no longer listed, the copy that was clicked is kept.
 */
export function latestRead(open: CloudItem | null, lists: (CloudItem[] | undefined)[]): CloudItem | null {
  if (!open) return null
  for (const list of lists) {
    const now = list?.find((it) => it.accountId === open.accountId && it.resource.kind === open.resource.kind && it.resource.id === open.resource.id)
    if (now) return now
  }
  return open
}

export const cloudItemsQuery = (kind: CloudKind) =>
  queryOptions({
    queryKey: itemsKey(kind),
    queryFn: () => get<{ items: CloudItem[] }>(`/api/v1/cloud/resources?kind=${kind}`).then((r) => r.items),
    enabled: isConfigured(),
    refetchInterval: (q) => itemsRefetchEvery(q.state.data),
  })

/**
 * The roles an account needs. With no identity, an account already
 * connected gets its own identity's and a new one the default's; keep is an
 * identity the roles go on trusting while the account moves off it.
 */
export function prepareCloudAccount(accountId: string, manage = false, identity = '', keep = '', provider: CloudProvider = 'aws'): Promise<CloudPrepared> {
  return send('POST', '/api/v1/cloud/accounts/prepare', { provider, accountId, manage, identity, keep })
}

/** Whether the template's roles are there and let Argus in yet. Nothing is kept or recorded. */
export function checkCloudAccount(accountId: string, manage: boolean, identity = '', provider: CloudProvider = 'aws'): Promise<CloudCheck> {
  return send('POST', '/api/v1/cloud/accounts/check', { provider, accountId, manage, identity })
}

/** Asks Argus to make the stack with its own identity, in the account that identity is in. */
export function deployCloudAccess(accountId: string, manage: boolean, name: string, identity = ''): Promise<CloudStackState> {
  return send('POST', '/api/v1/cloud/accounts/deploy', { provider: 'aws', accountId, manage, name, identity })
}

export function templateURL(accountId: string, identity = '', keep = ''): string {
  const q = new URLSearchParams({ accountId })
  if (identity) q.set('identity', identity)
  if (keep) q.set('keep', keep)
  return `/api/v1/cloud/accounts/template?${q.toString()}`
}

/** Whether an account's roles let another identity in yet. Nothing is moved or recorded. */
export function checkCloudMove(id: string, identity: string): Promise<CloudCheck> {
  return send('POST', `/api/v1/cloud/accounts/${encodeURIComponent(id)}/identity/check`, { identity })
}

/** Moves an account to another identity; refused unless Argus gets in with it. */
export function useMoveCloudAccount() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: ({ id, identity }: { id: string; identity: string }) =>
      send<CloudAccount>('PUT', `/api/v1/cloud/accounts/${encodeURIComponent(id)}/identity`, { identity }),
    onSettled: done,
  })
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

export type CloudStorageAction =
  | 'volume-create' | 'volume-grow' | 'volume-attach' | 'volume-detach' | 'volume-delete'
  | 'snapshot-create' | 'snapshot-delete' | 'image-create' | 'image-delete'
  | 'address-allocate' | 'address-associate' | 'address-disassociate' | 'address-release'
export type CloudNetworkAction =
  | 'firewall-create' | 'firewall-rules' | 'firewall-delete' | 'instance-firewalls' | 'target-register' | 'target-deregister'
export type CloudCapacityAction = 'capacity-create' | 'capacity-cancel' | 'ri-buy' | 'sp-buy'
export type CloudBalancingAction = 'target-group-create' | 'health-check' | 'balancer-create' | 'group-size'
export type CloudScaleAction = 'scale-group-create' | 'scale-group-change' | 'scale-add' | 'scale-remove'
export type CloudReplicaAction = 'replica-create' | 'replica-remove'

/** Whether an action buys a one- or three-year commitment, which always needs someone else. */
export const isBuy = (a: CloudAction) => a === 'ri-buy' || a === 'sp-buy'
export type CloudAction = 'launch' | 'start' | 'stop' | 'hibernate' | 'reboot' | 'resize' | 'tags' | 'protect' | 'unprotect' | 'settings' | 'remove'
  | CloudStorageAction | CloudNetworkAction | CloudCapacityAction | CloudBalancingAction | CloudScaleAction | CloudReplicaAction
export type CloudChangeState = 'awaiting_approval' | 'applying' | 'applied' | 'failed' | 'declined' | 'cancelled' | 'lapsed'

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

export interface CloudImage { family: string; label: string; id: string; name: string; linux: boolean; minDiskGb?: number }

export interface CloudLaunchOptions {
  images: CloudImage[]
  sizes: string[]
  subnets: CloudItem[]
  firewalls: CloudItem[]
  /** A Google Cloud region's zones: its subnets span the region, so the zone is picked on its own. */
  zones?: string[]
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
  /** What the disk is, in words: in Google Cloud, its name. */
  description?: string
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
  /** What resourceId is, for tags on something other than an instance. */
  kind?: CloudKind
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

/** An instance's settings as AWS reports them now. */
export interface CloudInstanceConfig {
  terminationProtect: boolean
  stopProtect: boolean
  /** Whether the metadata service answers only requests with a token (IMDSv2). */
  requireTokens: boolean
  /** Whether it was launched able to hibernate. */
  hibernation: boolean
  /** "standard" or "unlimited"; "" for a size that is not burstable. */
  creditMode: '' | 'standard' | 'unlimited'
}

/** The sizes one instance can change to: Google's depend on its zone. */
export const cloudInstanceSizesQuery = (it: CloudItem | null) =>
  queryOptions({
    queryKey: ['cloud', 'instance-sizes', it?.accountId, it?.resource.region, it?.resource.id],
    queryFn: () => get<{ sizes: string[] }>(`/api/v1/cloud/instance-sizes?accountId=${encodeURIComponent(it!.accountId)}` +
      `&region=${encodeURIComponent(it!.resource.region)}&resourceId=${encodeURIComponent(it!.resource.id)}`),
    enabled: isConfigured() && Boolean(it),
    staleTime: 5 * 60_000,
  })

export const cloudInstanceSettingsQuery = (it: CloudItem | null) =>
  queryOptions({
    queryKey: ['cloud', 'instance-settings', it?.accountId, it?.resource.region, it?.resource.id],
    queryFn: () => get<CloudInstanceConfig>(`/api/v1/cloud/instance-settings?accountId=${encodeURIComponent(it!.accountId)}` +
      `&region=${encodeURIComponent(it!.resource.region)}&resourceId=${encodeURIComponent(it!.resource.id)}`),
    enabled: isConfigured() && Boolean(it),
    staleTime: 0,
    gcTime: 0,
  })

/** What an instance last wrote to its console, cleaned of terminal escapes. */
export interface CloudConsoleText { text: string; at: string | null }

/** Read on demand only: every read is audited. */
export const cloudConsoleQuery = (it: CloudItem | null, enabled: boolean) =>
  queryOptions({
    queryKey: ['cloud', 'console-output', it?.accountId, it?.resource.region, it?.resource.id],
    queryFn: () => get<CloudConsoleText>(`/api/v1/cloud/console-output?accountId=${encodeURIComponent(it!.accountId)}` +
      `&region=${encodeURIComponent(it!.resource.region)}&resourceId=${encodeURIComponent(it!.resource.id)}`),
    enabled: isConfigured() && Boolean(it) && enabled,
    staleTime: 0,
    gcTime: 0,
    retry: false,
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
    mutationFn: ({ id, verb, confirmSelf }: { id: string; verb: 'approve' | 'decline' | 'cancel'; confirmSelf?: string }) =>
      send<CloudChange>('POST', `/api/v1/cloud/changes/${encodeURIComponent(id)}/${verb}`, confirmSelf ? { confirmSelf } : undefined),
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
  lapsed: { label: 'Nobody answered', color: 'slate' },
}

/* ── Reading what came back ─────────────────────────────────────────────── */

/** Ports that should never be open to the whole internet. */
export const RISKY_PORTS: Record<number, string> = {
  21: 'FTP', 22: 'SSH', 23: 'Telnet', 445: 'SMB', 1433: 'SQL Server', 2375: 'Docker', 3306: 'MySQL',
  3389: 'Remote Desktop', 5432: 'PostgreSQL', 5900: 'VNC', 6379: 'Redis', 9200: 'Elasticsearch',
  11211: 'Memcached', 27017: 'MongoDB',
}

/** What a firewall lets the whole internet reach, by name. */
export function openToInternet(r: CloudResource): string[] {
  const out = new Set<string>()
  for (const rule of r.rules ?? []) {
    if (!rule.inbound || (rule.source !== '0.0.0.0/0' && rule.source !== '::/0')) continue
    if (rule.protocol === 'all') { out.add('every port'); continue }
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
  creating: 'sky', provisioning: 'sky', modifying: 'sky', optimizing: 'sky', deleting: 'amber',
  completed: 'teal', active: 'teal', failed: 'rose', 'active_impaired': 'amber',
}

export function stateColor(s: string | undefined): string {
  return (s && STATE_COLOR[s]) || 'slate'
}

export function stateLabel(s: string | undefined): string {
  if (!s) return '—'
  if (s === 'in-use') return 'In use'
  return s.charAt(0).toUpperCase() + s.slice(1).replace('-', ' ')
}

/* ── Schedules ──────────────────────────────────────────────────────────── */

/** One weekly window. Days are 1 (Monday) to 7 (Sunday). */
export interface CloudWindow { days: number[]; start: string; stop: string; stopDay?: number }

export interface CloudScheduleVersion {
  name: string
  timezone: string
  windows: CloudWindow[]
  skipDates: string[]
  accountIds: string[]
  regions: string[]
  mode: 'transitions' | 'enforce'
}

export interface CloudTransition { at: string; action: 'start' | 'stop' }

export interface CloudSaving { monthly: number; priced: number; unpriced: number }

export interface CloudRunTarget {
  accountId: string
  accountName: string
  region: string
  id: string
  name: string
  status: 'done' | 'failed' | 'skipped' | 'waiting'
  reason?: string
  requestId?: string
  waitUntil?: string
}

export interface CloudScheduleRun {
  id: string
  scheduleId: string
  dueAt: string
  action: 'start' | 'stop'
  state: 'running' | 'done' | 'partial' | 'failed' | 'skipped'
  targets: CloudRunTarget[]
  requestIds: string[]
  detail: string
  attempts: number
  nextAt?: string
  startedAt: string
  finishedAt?: string
}

export interface CloudSchedule extends CloudScheduleVersion {
  id: string
  owner: string
  state: 'awaiting_approval' | 'active' | 'paused'
  pausedReason: string
  pending?: CloudScheduleVersion
  pendingBy?: string
  pendingAt?: string
  reason: string
  createdBy: string
  createdAt: string
  updatedBy: string
  updatedAt: string
}

export interface CloudScheduleView extends CloudSchedule {
  next: CloudTransition[]
  matched: number
  running: number
  hoursPerWeek: number
  saving: CloudSaving
  needsApproval: boolean
  lastRun?: CloudScheduleRun
}

export interface CloudKeepRunning { accountId: string; instanceId: string; until: string; reason: string; setBy: string; setAt: string }

export interface CloudScheduleInstance {
  accountId: string; accountName: string; region: string; id: string; name: string
  state: string; size: string; environment: CloudEnvironment; note?: string
}

export interface CloudSchedulePreview {
  instances: CloudScheduleInstance[]
  transitions: CloudTransition[]
  hoursPerWeek: number
  saving: CloudSaving
  needsApproval: boolean
  runningNow: boolean
}

/** The tag that puts an instance on a schedule. */
export const SCHEDULE_TAG = 'ArgusSchedule'

/** The schedule an instance's tags name, or ''. */
export function scheduleOf(tags: Record<string, string> | undefined): string {
  for (const [k, v] of Object.entries(tags ?? {})) if (k.toLowerCase() === SCHEDULE_TAG.toLowerCase()) return v.trim()
  return ''
}

const schedulesKey = ['cloud', 'schedules'] as const

export const cloudSchedulesQuery = (opts: { enabled?: boolean } = {}) =>
  queryOptions({
    queryKey: schedulesKey,
    queryFn: () => get<{ schedules: CloudScheduleView[]; keepRunning: CloudKeepRunning[] }>('/api/v1/cloud/schedules'),
    enabled: isConfigured() && opts.enabled !== false,
    refetchInterval: 60_000,
  })

export const cloudScheduleRunsQuery = (id: string | null) =>
  queryOptions({
    queryKey: [...schedulesKey, 'runs', id],
    queryFn: () => get<{ runs: CloudScheduleRun[] }>(`/api/v1/cloud/schedules/${encodeURIComponent(id ?? '')}/runs`).then((r) => r.runs),
    enabled: isConfigured() && Boolean(id),
  })

export function previewSchedule(v: CloudScheduleVersion): Promise<CloudSchedulePreview> {
  return send('POST', '/api/v1/cloud/schedules/preview', v)
}

export function useSaveSchedule() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, version, reason }: { id?: string; version: CloudScheduleVersion; reason: string }) =>
      id
        ? send<CloudSchedule>('PUT', `/api/v1/cloud/schedules/${encodeURIComponent(id)}`, { ...version, reason })
        : send<CloudSchedule>('POST', '/api/v1/cloud/schedules', { ...version, reason }),
    onSettled: () => void qc.invalidateQueries({ queryKey: schedulesKey }),
  })
}

export function useScheduleAction() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, verb, confirmSelf }: { id: string; verb: 'approve' | 'decline' | 'pause' | 'resume' | 'delete'; confirmSelf?: string }) =>
      verb === 'delete'
        ? send('DELETE', `/api/v1/cloud/schedules/${encodeURIComponent(id)}`)
        : send('POST', `/api/v1/cloud/schedules/${encodeURIComponent(id)}/${verb}`, confirmSelf ? { confirmSelf } : undefined),
    onSettled: () => void qc.invalidateQueries({ queryKey: schedulesKey }),
  })
}

function keepPath(it: CloudItem) {
  return `/api/v1/cloud/instances/${encodeURIComponent(it.accountId)}/${encodeURIComponent(it.resource.region)}/${encodeURIComponent(it.resource.id)}/keep-running`
}

export function useKeepRunning() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ it, until, reason }: { it: CloudItem; until?: string; reason?: string }) =>
      until ? send<CloudKeepRunning>('PUT', keepPath(it), { until, reason }) : send('DELETE', keepPath(it)),
    onSettled: () => void qc.invalidateQueries({ queryKey: schedulesKey }),
  })
}

export const SCHEDULE_STATE: Record<CloudSchedule['state'], { label: string; color: string }> = {
  active: { label: 'Active', color: 'teal' },
  awaiting_approval: { label: 'Waiting for approval', color: 'amber' },
  paused: { label: 'Paused', color: 'slate' },
}

export const RUN_STATE: Record<CloudScheduleRun['state'], { label: string; color: string }> = {
  running: { label: 'In progress', color: 'sky' },
  done: { label: 'Done', color: 'teal' },
  partial: { label: 'Partly done', color: 'amber' },
  failed: { label: 'Failed', color: 'rose' },
  skipped: { label: 'Skipped', color: 'slate' },
}

export const SCHEDULE_PRESETS: { key: string; label: string; text: string; windows: CloudWindow[] }[] = [
  { key: 'office', label: 'Office hours', text: 'Monday to Friday, 08:00 to 19:00', windows: [{ days: [1, 2, 3, 4, 5], start: '08:00', stop: '19:00' }] },
  { key: 'weekdays', label: 'Weekdays only', text: 'Monday 07:00 to Friday 21:00', windows: [{ days: [1], start: '07:00', stopDay: 5, stop: '21:00' }] },
  { key: 'nights', label: 'Nights off', text: 'Every day, 07:00 to 22:00', windows: [{ days: [1, 2, 3, 4, 5, 6, 7], start: '07:00', stop: '22:00' }] },
]

const mins = (hhmm: string) => {
  const m = /^(\d{2}):(\d{2})$/.exec(hhmm)
  return m ? Number(m[1]) * 60 + Number(m[2]) : NaN
}

/**
 * When the windows run, per weekday (index 0 is Monday), as minute ranges
 * within the day. Wall-clock only: it draws the week, the control plane
 * does the arithmetic.
 */
export function weekRuns(windows: CloudWindow[]): [number, number][][] {
  const out: [number, number][][] = Array.from({ length: 7 }, () => [])
  for (const w of windows) {
    const s = mins(w.start)
    const e = mins(w.stop)
    if (Number.isNaN(s) || Number.isNaN(e)) continue
    for (const d of w.days) {
      if (d < 1 || d > 7) continue
      let ahead = 0
      if (w.stopDay) {
        ahead = (w.stopDay - d + 7) % 7
        if (ahead === 0 && e <= s) ahead = 7
      } else if (e <= s) ahead = 1
      let from = (d - 1) * 1440 + s
      const to = (d - 1 + ahead) * 1440 + e
      while (from < to) {
        const dayStart = Math.floor(from / 1440) * 1440
        const end = Math.min(to, dayStart + 1440)
        out[(dayStart / 1440) % 7]!.push([from - dayStart, end - dayStart])
        from = end
      }
    }
  }
  return out
}

export const DAY_SHORT = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']

/** "Mon–Fri 08:00–19:00", in words. */
export function windowWords(w: CloudWindow): string {
  const ds = [...w.days].sort((a, b) => a - b)
  const run = ds.length > 2 && ds.every((d, i) => i === 0 || d === ds[i - 1]! + 1)
  const days = ds.length === 7 ? 'Every day' : run ? `${DAY_SHORT[ds[0]! - 1]}–${DAY_SHORT[ds[ds.length - 1]! - 1]}` : ds.map((d) => DAY_SHORT[d - 1]).join(', ')
  if (w.stopDay) return `${days} ${w.start} to ${DAY_SHORT[w.stopDay - 1]} ${w.stop}`
  return `${days} ${w.start}–${w.stop}${mins(w.stop) <= mins(w.start) ? ' (next day)' : ''}`
}

/** A transition's time in the schedule's zone, as "Mon 9 Nov, 19:00". */
export function transitionWhen(at: string, zone: string): string {
  try {
    return new Intl.DateTimeFormat('en-GB', {
      timeZone: zone, weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
    }).format(new Date(at))
  } catch {
    return new Date(at).toUTCString()
  }
}

export function money(n: number): string {
  return n >= 100 ? `$${Math.round(n).toLocaleString('en-US')}` : `$${n.toFixed(2)}`
}

/* ── Disks, snapshots, images and Elastic IPs ───────────────────────────── */

/** How much each change can do; the control plane decides, this only says so up front. */
export const ACTION_LEVEL: Record<CloudAction, CloudChange['level']> = {
  launch: 'change', start: 'operate', stop: 'operate', hibernate: 'operate', reboot: 'operate', tags: 'operate',
  resize: 'change', protect: 'change', unprotect: 'change', settings: 'change', remove: 'remove',
  'volume-create': 'change', 'volume-grow': 'change', 'volume-attach': 'change', 'volume-detach': 'change', 'volume-delete': 'remove',
  'snapshot-create': 'operate', 'snapshot-delete': 'remove', 'image-create': 'change', 'image-delete': 'remove',
  'address-allocate': 'change', 'address-associate': 'change', 'address-disassociate': 'change', 'address-release': 'remove',
  'firewall-create': 'change', 'firewall-rules': 'change', 'firewall-delete': 'remove', 'instance-firewalls': 'change',
  'target-register': 'change', 'target-deregister': 'change',
  'capacity-create': 'change', 'capacity-cancel': 'change', 'ri-buy': 'remove', 'sp-buy': 'remove',
  'target-group-create': 'change', 'health-check': 'change', 'balancer-create': 'change', 'group-size': 'change',
  'scale-group-create': 'change', 'scale-group-change': 'change', 'scale-add': 'change', 'scale-remove': 'change',
  'replica-create': 'change', 'replica-remove': 'remove',
}

/** The permissions each storage change takes, as the manage role must grant them. */
export const ACTION_NEEDS: Partial<Record<CloudAction, string[]>> = {
  'volume-create': ['ec2:CreateVolume', 'ec2:AttachVolume'], 'volume-grow': ['ec2:ModifyVolume'],
  'volume-attach': ['ec2:AttachVolume'], 'volume-detach': ['ec2:DetachVolume'], 'volume-delete': ['ec2:DeleteVolume'],
  'snapshot-create': ['ec2:CreateSnapshot'], 'snapshot-delete': ['ec2:DeleteSnapshot'],
  'image-create': ['ec2:CreateImage'], 'image-delete': ['ec2:DeregisterImage', 'ec2:DeleteSnapshot'],
  'address-allocate': ['ec2:AllocateAddress'], 'address-associate': ['ec2:AssociateAddress'],
  'address-disassociate': ['ec2:DisassociateAddress'], 'address-release': ['ec2:ReleaseAddress'],
  'firewall-create': ['ec2:CreateSecurityGroup', 'ec2:AuthorizeSecurityGroupIngress', 'ec2:AuthorizeSecurityGroupEgress'],
  'firewall-rules': ['ec2:AuthorizeSecurityGroupIngress', 'ec2:AuthorizeSecurityGroupEgress', 'ec2:RevokeSecurityGroupIngress', 'ec2:RevokeSecurityGroupEgress'],
  'firewall-delete': ['ec2:DeleteSecurityGroup'], 'instance-firewalls': ['ec2:ModifyInstanceAttribute'],
  'target-register': ['elasticloadbalancing:RegisterTargets'], 'target-deregister': ['elasticloadbalancing:DeregisterTargets'],
  'capacity-create': ['ec2:CreateCapacityReservation', 'ec2:CreateTags'], 'capacity-cancel': ['ec2:CancelCapacityReservation'],
  'ri-buy': ['ec2:PurchaseReservedInstancesOffering'], 'sp-buy': ['savingsplans:DescribeSavingsPlansOfferings', 'savingsplans:CreateSavingsPlan'],
  'target-group-create': ['elasticloadbalancing:CreateTargetGroup', 'elasticloadbalancing:AddTags'],
  'health-check': ['elasticloadbalancing:ModifyTargetGroup'],
  'balancer-create': ['elasticloadbalancing:CreateLoadBalancer', 'elasticloadbalancing:CreateListener', 'elasticloadbalancing:AddTags'],
  'group-size': ['autoscaling:UpdateAutoScalingGroup'],
  'scale-group-create': ['ec2:CreateImage', 'elasticloadbalancing:RegisterTargets', 'ec2:CreateLaunchTemplate',
    'autoscaling:CreateAutoScalingGroup', 'autoscaling:PutScalingPolicy'],
  'scale-add': ['ec2:RunInstances', 'elasticloadbalancing:RegisterTargets'],
  'scale-remove': ['elasticloadbalancing:DeregisterTargets', 'ec2:TerminateInstances'],
  'replica-create': ['rds:CreateDBInstanceReadReplica', 'rds:AddTagsToResource'],
  'replica-remove': ['rds:ModifyDBInstance', 'rds:DeleteDBInstance'],
}

/** Whether an account's manage role lacks what an action takes. */
export function roleLacks(a: CloudAccount | undefined, action: CloudAction): boolean {
  return Boolean(a?.manageMissing?.length && ACTION_NEEDS[action]?.some((p) => a.manageMissing.includes(p)))
}

export const VOLUME_TYPES: { value: string; label: string; perGb: number }[] = [
  { value: 'gp3', label: 'gp3 · general purpose SSD', perGb: 0.08 },
  { value: 'gp2', label: 'gp2 · older general purpose SSD', perGb: 0.10 },
  { value: 'io2', label: 'io2 · provisioned IOPS SSD', perGb: 0.125 },
  { value: 'io1', label: 'io1 · older provisioned IOPS SSD', perGb: 0.125 },
  { value: 'st1', label: 'st1 · throughput HDD, 125 GiB and up', perGb: 0.045 },
  { value: 'sc1', label: 'sc1 · cold HDD, 125 GiB and up', perGb: 0.015 },
]

/** About what a month costs, at us-east-1 list prices. */
export const PRICE = { snapshotPerGb: 0.05, elasticIp: 3.65 }

export function volumeMonthly(type: string, gb: number): number {
  return (VOLUME_TYPES.find((t) => t.value === type)?.perGb ?? 0.08) * gb
}

/** A disk's size and type from its inventory size, "100 GiB gp3". */
export function volumeOf(r: CloudResource): { gb: number; type: string } {
  const m = /^(\d+) GiB\s*(\S*)/.exec(r.size ?? '')
  return { gb: m ? Number(m[1]) : 0, type: m?.[2] ?? '' }
}

/** The instance a disk or address is attached to, and as what. */
export function attachment(r: CloudResource): CloudRef | undefined {
  return r.links?.find((l) => l.kind === 'instance')
}

/** The device names Argus attaches disks as, and the first one an instance does not use. */
export function freeDevice(inst: CloudResource): string {
  const used = new Set((inst.links ?? []).filter((l) => l.kind === 'volume').flatMap((l) => {
    const d = l.name ?? ''
    return d.startsWith('/dev/xvd') ? [d, '/dev/sd' + d.slice(8)] : [d]
  }))
  for (const c of 'fghijklmnop') if (!used.has(`/dev/sd${c}`)) return `/dev/sd${c}`
  return ''
}

/* ── Security groups and load balancer targets ──────────────────────────── */

const RANK: Record<CloudEnvironment, number> = { development: 0, staging: 1, production: 2 }

/** The stricter of two environments. */
export function stricterEnv(a: CloudEnvironment, b: CloudEnvironment): CloudEnvironment {
  return RANK[b] > RANK[a] ? b : a
}

export function isOpenRule(r: CloudRule): boolean {
  return r.inbound && (r.source === '0.0.0.0/0' || r.source === '::/0')
}

/**
 * What an inbound rule lets the whole internet reach, in words, or '' when
 * it does not. Web ports alone are not an exposure. The control plane says
 * the same, and raises such a change so it needs a second person.
 */
export function ruleExposure(r: CloudRule): string {
  if (!isOpenRule(r) || r.protocol === 'icmp') return ''
  if (r.protocol === 'all') return 'every port'
  if (r.protocol === 'tcp' && (r.fromPort === 80 || r.fromPort === 443) && r.fromPort === r.toPort) return ''
  const names = Object.entries(RISKY_PORTS).filter(([p]) => r.fromPort <= Number(p) && Number(p) <= r.toPort)
    .map(([p, n]) => `${n} (${p})`)
  return names.length ? names.join(', ') : portText(r)
}

export function exposures(rules: CloudRule[]): string[] {
  return [...new Set(rules.map(ruleExposure).filter(Boolean))]
}

/** Whether two rules allow the same traffic, whatever their descriptions say. */
export function sameRule(a: CloudRule, b: CloudRule): boolean {
  return a.inbound === b.inbound && a.protocol === b.protocol && a.fromPort === b.fromPort && a.toPort === b.toPort &&
    a.source.toLowerCase() === b.source.toLowerCase()
}

/** A rule's source as people write it: an address becomes a /32 or /128. */
export function normalSource(s: string): string {
  const t = s.trim()
  if (/^(sg|pl)-[0-9a-f]+$/.test(t) || t.includes('/')) return t
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(t)) return `${t}/32`
  if (t.includes(':')) return `${t}/128`
  return t
}

/** Whether a source is a range, an address, a security group or a prefix list. */
export function validSource(s: string): boolean {
  const t = s.trim()
  return /^sg-[0-9a-f]{8,17}$/.test(t) || /^pl-[0-9a-f]{8,17}$/.test(t) ||
    /^\d{1,3}(\.\d{1,3}){3}(\/\d{1,2})?$/.test(t) || /^[0-9a-f:]+:[0-9a-f:]*(\/\d{1,3})?$/i.test(t)
}

/** Every live instance and load balancer that uses a security group. */
export function firewallUsers(g: CloudItem, instances: CloudItem[] = [], balancers: CloudItem[] = []): CloudItem[] {
  return [...instances, ...balancers].filter((x) => x.accountId === g.accountId && x.resource.region === g.resource.region &&
    x.resource.state !== 'terminated' && x.resource.state !== 'shutting-down' &&
    (x.resource.links ?? []).some((l) => l.kind === 'firewall' && l.id === g.resource.id))
}

/** A group's environment is the strictest of its own and of what uses it. */
export function firewallEnv(g: CloudItem, users: CloudItem[]): CloudEnvironment {
  return users.reduce((env, u) => stricterEnv(env, envOf(u.resource.tags, u.environment)), envOf(g.resource.tags, g.environment))
}

/** The instances behind a target group, with their health. */
export function targetsOf(tg: CloudResource): { id: string; health: string }[] {
  return (tg.links ?? []).filter((l) => l.kind === 'instance').map((l) => ({ id: l.id, health: l.name ?? 'unknown' }))
}

/** What the IAM user behind an access key needs: to assume Argus's roles, and nothing else. */
export const AWS_IDENTITY_POLICY = `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["sts:AssumeRole", "sts:SetSourceIdentity", "sts:TagSession"],
    "Resource": "arn:aws:iam::*:role/Argus*"
  }]
}`

/* ── Load balancers, target groups and Auto Scaling groups ─────────────── */

/** How a target group tells a target is well. */
export interface CloudHealthCheck {
  protocol: 'HTTP' | 'HTTPS' | 'TCP'
  path?: string
  /** "traffic-port", the target's own, or a number. */
  port?: string
  intervalSeconds: number
  healthy: number
  unhealthy: number
  /** HTTP codes that mean well, like 200 or 200-399. */
  matcher?: string
}

/** A load balancer, before traffic, a month: AWS's hourly charge in the cheaper regions. */
export const BALANCER_MONTHLY = 0.0225 * 730

/** A target group or load balancer name: 1 to 32 letters, digits and hyphens, not starting or ending with one. */
export const LB_NAME = /^[A-Za-z0-9]([A-Za-z0-9-]{0,30}[A-Za-z0-9])?$/

/** AWS's defaults for a new target group of a protocol. */
export function defaultHealthCheck(protocol: CloudHealthCheck['protocol']): CloudHealthCheck {
  return protocol === 'TCP'
    ? { protocol, port: 'traffic-port', intervalSeconds: 30, healthy: 5, unhealthy: 2 }
    : { protocol, path: '/', port: 'traffic-port', intervalSeconds: 30, healthy: 5, unhealthy: 2, matcher: '200' }
}

/** A target group's health check as read, or its protocol's defaults. */
export function healthCheckOf(r: CloudResource): CloudHealthCheck {
  const d = r.details ?? {}
  const p = (d.healthCheckProtocol || d.protocol || 'HTTP') as CloudHealthCheck['protocol']
  const def = defaultHealthCheck(p === 'TCP' ? 'TCP' : p)
  const n = (s: string | undefined, or: number) => (s && Number.isFinite(Number(s)) ? Number(s) : or)
  return {
    protocol: p, path: p === 'TCP' ? undefined : d.healthCheckPath || def.path, port: d.healthCheckPort || 'traffic-port',
    intervalSeconds: n(d.healthCheckInterval, def.intervalSeconds), healthy: n(d.healthyThreshold, def.healthy),
    unhealthy: n(d.unhealthyThreshold, def.unhealthy), matcher: p === 'TCP' ? undefined : d.matcher || def.matcher,
  }
}

/** What is wrong with a health check, or null; the control plane checks the same. */
export function healthProblem(h: CloudHealthCheck): string | null {
  if (h.protocol !== 'TCP') {
    if (!/^\/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*$/.test(h.path ?? '')) return 'A path starts with /, like /health'
    if (h.matcher && !/^[1-5][0-9]{2}(-[1-5][0-9]{2})?(,[1-5][0-9]{2}(-[1-5][0-9]{2})?)*$/.test(h.matcher)) return 'Healthy codes are like 200, 200-299 or 200,301'
  }
  if (h.port && h.port !== 'traffic-port' && !(/^\d{1,5}$/.test(h.port) && Number(h.port) >= 1 && Number(h.port) <= 65535)) return 'A port is 1 to 65535'
  if (h.intervalSeconds < 5 || h.intervalSeconds > 300) return 'Check every 5 to 300 seconds'
  if ([h.healthy, h.unhealthy].some((x) => x < 2 || x > 10)) return '2 to 10 checks in a row'
  return null
}

/** The health check in a sentence, as the control plane says it. */
export function healthWords(h: CloudHealthCheck): string {
  let what = h.protocol
  if (h.protocol !== 'TCP' && h.path) what += ` ${h.path}`
  if (h.port && h.port !== 'traffic-port') what += ` on port ${h.port}`
  if (h.protocol !== 'TCP') what += ` answering ${h.matcher || '200'}`
  return `${what}, every ${h.intervalSeconds}s, healthy after ${h.healthy}, unhealthy after ${h.unhealthy}`
}

/** An Auto Scaling group's size as read. */
export function groupSizeOf(r: CloudResource): { min: number; max: number; desired: number; running: number } {
  const n = (s?: string) => Number(s ?? 0) || 0
  const d = r.details ?? {}
  return { min: n(d.min), max: n(d.max), desired: n(d.desired), running: n(d.running) }
}

/** Whether Argus makes this change in this account: a provider still being built lists the few it does. */
export function allows(account: Pick<CloudAccount, 'actions'> | undefined, action: string): boolean {
  return !account?.actions || account.actions.includes(action)
}

/** Whether Argus tags (labels, in Google Cloud) a resource of kind in account. */
export function allowsTags(account: Pick<CloudAccount, 'actions' | 'tagKinds'> | undefined, kind: string): boolean {
  return allows(account, 'tags') && (!account?.tagKinds || account.tagKinds.includes(kind))
}

/** Google keeps labels lowercase, and its own start with goog-. Says why k=v cannot be one, or undefined. */
export function labelProblem(k: string, v: string): string | undefined {
  if (!/^[a-z][a-z0-9_-]{0,62}$/.test(k)) return 'Lowercase letters, digits, _ and -, starting with a letter'
  if (k.startsWith('goog-')) return 'Google keeps goog- labels'
  if (!/^[a-z0-9_-]{0,63}$/.test(v)) return 'Lowercase letters, digits, _ and -'
  return undefined
}
