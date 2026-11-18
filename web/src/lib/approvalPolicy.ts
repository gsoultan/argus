import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, isConfigured, post, send } from '~/lib/live'

/*
 * Each organization's approval policy (the control plane's
 * approval_policy.go): who may act directly where Argus would otherwise wait
 * for a second person, by kind of action and environment. The control plane
 * decides; this only shows and edits it.
 */

export type ApprovalChoice = 'second' | 'owners' | 'admins' | 'anyone'
export type ApprovalEnv = 'production' | 'staging' | 'other' | 'all'

export interface ApprovalKindInfo {
  kind: string
  group: string
  title: string
  what: string
  envs: ApprovalEnv[]
  /** When Argus asks today, before the policy: always, sometimes or never. */
  today: Partial<Record<ApprovalEnv, 'always' | 'sometimes' | 'never'>>
  notes?: Partial<Record<ApprovalEnv, string>>
  /** Environments the platform locked, and why. */
  locked?: Partial<Record<ApprovalEnv, string>>
}

export interface ApprovalRule {
  kind: string
  env: ApprovalEnv
  choice: ApprovalChoice
  setBy?: string
  setAt?: string
}

export interface ApprovalPolicy {
  rules: ApprovalRule[]
  requireReason: boolean
  notifyProduction: boolean
  updatedBy?: string
  updatedAt?: string
}

export interface ApprovalPolicyChange {
  id: string
  requestedBy: string
  requestedAt: string
  reason: string
  proposal: ApprovalPolicy
  state: string
  changes: string[]
  loosens: string[]
}

export interface ApprovalPolicyView {
  kinds: ApprovalKindInfo[]
  policy: ApprovalPolicy
  pending: ApprovalPolicyChange | null
  keepers: number
  canChange: boolean
  phrase: string
  direct30: number
  /** Per kind, the environments where the reader may act directly. */
  mine?: Record<string, ApprovalEnv[]>
}

export interface ApprovalLock {
  kind: string
  env: ApprovalEnv
  reason: string
}

export const CHOICES: { value: ApprovalChoice; label: string; short: string; risk: string }[] = [
  {
    value: 'second', label: 'A second person approves', short: 'Second person',
    risk: '',
  },
  {
    value: 'owners', label: 'Owners act directly', short: 'Owners act directly',
    risk: 'One owner can do this alone. A stolen owner sign-in reaches it straight away.',
  },
  {
    value: 'admins', label: 'Admins and owners act directly', short: 'Admins act directly',
    risk: 'Any one admin or owner can do this alone, with nobody seeing it first. One mistyped choice, or one stolen sign-in, takes effect straight away.',
  },
  {
    value: 'anyone', label: 'Anyone who may do it', short: 'No approval',
    risk: 'The second-person check is gone for this. Only Activity shows what happened, after the fact.',
  },
]

export const ENV_LABEL: Record<ApprovalEnv, string> = {
  production: 'Production', staging: 'Staging', other: 'Development and other', all: 'Every environment',
}

const RANK: Record<ApprovalChoice, number> = { second: 0, owners: 1, admins: 2, anyone: 3 }

/** The choice a policy makes for one cell. */
export function choiceOf(p: ApprovalPolicy, kind: string, env: ApprovalEnv): ApprovalChoice {
  return p.rules.find((r) => r.kind === kind && r.env === env)?.choice ?? 'second'
}

/** p with one cell set. */
export function withChoice(p: ApprovalPolicy, kind: string, env: ApprovalEnv, choice: ApprovalChoice): ApprovalPolicy {
  const rules = p.rules.filter((r) => !(r.kind === kind && r.env === env))
  if (choice !== 'second') rules.push({ kind, env, choice })
  return { ...p, rules }
}

export interface CellChange {
  kind: ApprovalKindInfo
  env: ApprovalEnv
  was: ApprovalChoice
  now: ApprovalChoice
  looser: boolean
}

/** Every cell that differs, and whether it lets more people act directly. */
export function cellChanges(kinds: ApprovalKindInfo[], was: ApprovalPolicy, now: ApprovalPolicy): CellChange[] {
  const out: CellChange[] = []
  for (const k of kinds) {
    for (const env of k.envs) {
      const a = choiceOf(was, k.kind, env)
      const b = choiceOf(now, k.kind, env)
      if (a !== b) out.push({ kind: k, env, was: a, now: b, looser: RANK[b] > RANK[a] })
    }
  }
  return out
}

/** Whether going from was to now lets more people act directly. */
export function loosens(kinds: ApprovalKindInfo[], was: ApprovalPolicy, now: ApprovalPolicy): boolean {
  return cellChanges(kinds, was, now).some((c) => c.looser) ||
    (was.requireReason && !now.requireReason) || (was.notifyProduction && !now.notifyProduction)
}

const key = ['approval-policy'] as const

export const approvalPolicyQuery = () =>
  queryOptions({
    queryKey: key,
    queryFn: () => get<ApprovalPolicyView>('/api/v1/approval-policy'),
    enabled: isConfigured(),
  })

export function useSaveApprovalPolicy() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: ApprovalPolicy & { reason: string; confirmSelf?: string }) =>
      send<{ applied: boolean }>('PUT', '/api/v1/approval-policy', body),
    onSettled: () => void qc.invalidateQueries({ queryKey: key }),
  })
}

export function useDecidePolicyChange() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, what, note }: { id: string; what: 'approve' | 'deny' | 'cancel'; note?: string }) =>
      post<unknown>(`/api/v1/approval-policy/changes/${id}/${what}`, note ? { note } : {}),
    onSettled: () => void qc.invalidateQueries({ queryKey: key }),
  })
}

const locksKey = ['platform', 'approval-locks'] as const

export const approvalLocksQuery = (enabled: boolean) =>
  queryOptions({
    queryKey: locksKey,
    queryFn: () => get<{ kinds: ApprovalKindInfo[]; locks: ApprovalLock[] }>('/api/v1/platform/approval-locks'),
    enabled: isConfigured() && enabled,
  })

export function useSaveApprovalLocks() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (locks: ApprovalLock[]) => send<unknown>('PUT', '/api/v1/platform/approval-locks', { locks }),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: locksKey })
      void qc.invalidateQueries({ queryKey: key })
    },
  })
}
