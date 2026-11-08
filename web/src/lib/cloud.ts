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
    mutationFn: ({ id, verb }: { id: string; verb: 'approve' | 'decline' | 'pause' | 'resume' | 'delete' }) =>
      verb === 'delete'
        ? send('DELETE', `/api/v1/cloud/schedules/${encodeURIComponent(id)}`)
        : send('POST', `/api/v1/cloud/schedules/${encodeURIComponent(id)}/${verb}`),
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
