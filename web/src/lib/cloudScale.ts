import { queryOptions } from '@tanstack/react-query'
import { get, isConfigured } from '~/lib/live'
import type { CloudChange, CloudEnvironment, CloudItem } from '~/lib/cloud'

/* ── Scale groups: servers behind one load balancer, with rules ─────────── */

export type ScaleSignal = 'cpu' | 'memory' | 'swap' | 'load' | 'steal'

/** "signal is above or below value for minutes", over the group's average or any member. */
export interface ScaleRule {
  signal: ScaleSignal
  over: 'average' | 'any'
  op: 'above' | 'below'
  value: number
  minutes: number
}

export interface ScaleRules { add: ScaleRule[]; remove: ScaleRule[] }

export interface ScaleLimits {
  min: number
  max: number
  /** Dollars a month; 0 is none. */
  costCap: number
  addWait: number
  removeWait: number
}

export type ScaleMode = 'ask' | 'auto'

export interface ScaleMember {
  instanceId: string
  hostname: string
  zone: string
  subnet: string
  state: 'joining' | 'member' | 'draining' | 'gone' | 'failed'
  addedBy: string
  joinedAt: string
  registeredAt: string | null
  drainUntil: string | null
  leftAt: string | null
}

export interface ScaleActivity { at: string; kind: string; actor: string; detail: string; changeId: string }

export interface ScaleGroup {
  id: string
  accountId: string
  accountName: string
  region: string
  name: string
  environment: CloudEnvironment
  targetGroup: string
  targetName: string
  port: number
  balancer: string
  template: { image: string; imageLabel: string; size: string; diskGb: number; subnets: string[]; firewalls: string[]; from: string; fromName: string }
  limits: ScaleLimits
  rules: ScaleRules
  mode: ScaleMode
  autoBy: string
  autoAt: string | null
  state: 'active' | 'paused'
  pausedReason: string
  /** What the rules said the last time Argus looked. */
  note: string
  failedStarts: number
  lastAddAt: string | null
  lastRemoveAt: string | null
  checkedAt: string | null
  createdBy: string
  createdAt: string
  members: ScaleMember[]
  /** The group's processor over the last 10 minutes. */
  now: number | null
  monthly: number | null
  each: number | null
  last: ScaleActivity | null
  open: CloudChange | null
  /** Target health by instance id. */
  health: Record<string, string>
  usage: Record<string, { cpu: number | null; mem: number | null }>
}

export interface ScalePoint { at: string; value: number }

export interface ScaleGroupDetail extends ScaleGroup {
  activity: ScaleActivity[]
  /** Each member's processor over the last 8 hours, by hostname. */
  series: Record<string, ScalePoint[]>
}

export interface ScaleOption { value: string; label: string; zone?: string; note?: string }

/** The wizard's answers, worked out from one server. */
export interface ScaleDraft {
  from: CloudItem
  name: string
  size: string
  diskGb: number
  each: number | null
  subnets: ScaleOption[] | null
  targetGroups: ScaleOption[] | null
  images: ScaleOption[] | null
  targetGroup: string
  rules: ScaleRules
  limits: ScaleLimits
  environment: CloudEnvironment
  problem: string
}

/** What an add or take-away was asked on. */
export interface ScalePlan {
  why: string
  signal: ScaleSignal
  threshold: number
  series: ScalePoint[] | null
  monthly: number | null
  after: number
  min: number
  max: number
  groupCost: number | null
  costCap: number
  automatic: boolean
  autoBy?: string
}

export const scaleGroupsQuery = (enabled = true) =>
  queryOptions({
    queryKey: ['cloud', 'scale-groups'] as const,
    queryFn: () => get<{ groups: ScaleGroup[] }>('/api/v1/cloud/scale-groups').then((r) => r.groups),
    enabled: enabled && isConfigured(),
    refetchInterval: 60_000,
  })

export const scaleGroupQuery = (id: string) =>
  queryOptions({
    queryKey: ['cloud', 'scale-groups', id] as const,
    queryFn: () => get<ScaleGroupDetail>(`/api/v1/cloud/scale-groups/${encodeURIComponent(id)}`),
    enabled: isConfigured() && Boolean(id),
    refetchInterval: 30_000,
  })

export const scaleDraftQuery = (accountId: string, region: string, from: string) =>
  queryOptions({
    queryKey: ['cloud', 'scale-draft', accountId, region, from] as const,
    queryFn: () => get<ScaleDraft>(`/api/v1/cloud/scale-groups/draft?${new URLSearchParams({ accountId, region, from })}`),
    enabled: isConfigured() && Boolean(accountId && region && from),
    retry: false,
  })

export const SIGNALS: { value: ScaleSignal; label: string; unit: string; max: number }[] = [
  { value: 'cpu', label: 'processor', unit: '%', max: 100 },
  { value: 'memory', label: 'memory available', unit: '%', max: 100 },
  { value: 'swap', label: 'swap in use', unit: '%', max: 100 },
  { value: 'load', label: 'load per core', unit: '', max: 16 },
  { value: 'steal', label: 'processor steal', unit: '%', max: 100 },
]

export function signalLabel(s: ScaleSignal): string {
  return SIGNALS.find((x) => x.value === s)?.label ?? s
}

/** A rule in words: "group average processor above 75% for 10 min". */
export function ruleWords(r: ScaleRule): string {
  const unit = SIGNALS.find((x) => x.value === r.signal)?.unit ?? ''
  return `${r.over === 'any' ? "any member's" : 'group average'} ${signalLabel(r.signal)} ${r.op} ${r.value}${unit} for ${r.minutes} min`
}

/** What is wrong with a rule, or "". The control plane checks again. */
export function ruleProblem(r: ScaleRule): string {
  const max = SIGNALS.find((x) => x.value === r.signal)?.max ?? 100
  if (!(r.value > 0 && r.value < max)) return `The value is between 0 and ${max}.`
  if (r.minutes < 5 || r.minutes > 120 || r.minutes % 5 !== 0) return 'A rule holds for 5 to 120 minutes, in fives.'
  return ''
}

/** What is wrong with the limits, or "". */
export function limitsProblem(l: ScaleLimits, mode: ScaleMode): string {
  if (!(l.min >= 1)) return 'A group keeps at least 1 server.'
  if (!(l.max >= l.min && l.max <= 20)) return 'The maximum is from the minimum up to 20.'
  if (l.costCap < 0) return 'The cost cap is a monthly amount, or 0 for none.'
  if (mode === 'auto' && !l.costCap) return 'Automatic needs a cost cap, so the limits say what it may spend.'
  if (l.addWait < 5 || l.addWait > 240 || l.removeWait < 5 || l.removeWait > 240) return 'Wait 5 to 240 minutes after a change.'
  return ''
}

export const MODE_WORDS: Record<ScaleMode, string> = {
  ask: 'Argus decides · ask each time',
  auto: 'Argus decides · automatic',
}

/** The servers a group has now and that count towards its size. */
export function serving(g: Pick<ScaleGroup, 'members'>): ScaleMember[] {
  return g.members.filter((m) => m.state === 'joining' || m.state === 'member')
}

/** Who asked, as people read it: Argus's own scale changes say Argus. */
export function askerName(by: string): string {
  return by === 'argus-scale' ? 'Argus' : by
}

/** "Busy 78%" or "Normal 41%" for the group's processor now, against its first add rule. */
export function groupNow(g: ScaleGroup): { label: string; color: string } | null {
  if (g.now == null) return null
  const add = g.rules.add.find((r) => r.signal === 'cpu' && r.op === 'above')
  const remove = g.rules.remove.find((r) => r.signal === 'cpu' && r.op === 'below')
  const pct = `${Math.round(g.now)}%`
  if (add && g.now > add.value) return { label: `Busy ${pct}`, color: 'amber' }
  if (remove && g.now < remove.value) return { label: `Quiet ${pct}`, color: 'sky' }
  return { label: `Normal ${pct}`, color: 'teal' }
}
