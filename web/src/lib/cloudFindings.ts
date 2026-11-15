import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, isConfigured, send } from '~/lib/live'
import type { CloudAction, CloudEnvironment, CloudRule } from '~/lib/cloud'

/*
 * Cloud › Risks & waste: security risks, what is paid for and not in use,
 * and firewall rules nothing needs, worked out by the control plane from the
 * last read of every connected account. Read only: a fix is the ordinary
 * change, opened already filled in.
 */

export type FindingGroup = 'risk' | 'waste' | 'rules'
export type FindingSeverity = 'high' | 'medium' | 'low'
/** How Argus knows: a fact AWS reported, something its agent measured, or a threshold it chose. */
export type FindingSource = 'cloud' | 'agent' | 'judgement'

/** Kinds a finding can be about: the inventory's, plus the account-wide ones. */
export type FindingKind = 'instance' | 'volume' | 'firewall' | 'address' | 'snapshot' | 'image' | 'load-balancer' | 'target-group'
  | 'account' | 'user' | 'bucket' | 'database'

export interface FindingRef { kind: string; id: string; name?: string }

export interface FindingKeep {
  accountId: string
  findingId: string
  reason: string
  keptBy: string
  keptAt: string
  expiresAt?: string
  untilChange: boolean
}

export interface CloudFinding {
  accountId: string
  accountName: string
  environment: CloudEnvironment
  id: string
  check: string
  group: FindingGroup
  severity: FindingSeverity
  source: FindingSource
  kind: FindingKind
  resourceId: string
  name: string
  region: string
  title: string
  why: string
  matters?: string
  howToFix?: string
  monthly?: number
  atMost?: boolean
  since?: string
  fix?: CloudAction
  rule?: CloudRule
  related?: FindingRef[]
  basis: string
  kept?: FindingKeep
}

export interface CloudFindingsView {
  items: CloudFinding[]
  kept: CloudFinding[]
  accounts: { id: string; name: string; accountChecks: boolean }[]
  /** What the listed waste bills a month. */
  monthly: number
  checkedAt: string
}

const key = ['cloud', 'findings'] as const

export const cloudFindingsQuery = (opts: { enabled?: boolean } = {}) =>
  queryOptions({
    queryKey: key,
    queryFn: () => get<CloudFindingsView>('/api/v1/cloud/findings'),
    enabled: isConfigured() && opts.enabled !== false,
    refetchInterval: 60_000,
  })

export interface KeepInput {
  accountId: string
  findingId: string
  reason: string
  /** 30 or 90; 0 with untilChange keeps it until it changes. */
  days: 0 | 30 | 90
  untilChange: boolean
}

export function useKeepFinding() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (in_: KeepInput) => send<FindingKeep>('POST', '/api/v1/cloud/findings/keep', in_),
    onSettled: () => void qc.invalidateQueries({ queryKey: key }),
  })
}

export function useUnkeepFinding() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (k: { accountId: string; findingId: string }) =>
      send('DELETE', `/api/v1/cloud/findings/keep?accountId=${encodeURIComponent(k.accountId)}&findingId=${encodeURIComponent(k.findingId)}`),
    onSettled: () => void qc.invalidateQueries({ queryKey: key }),
  })
}

export const SEVERITY_LABEL: Record<FindingSeverity, string> = { high: 'High', medium: 'Medium', low: 'Low' }
export const SEVERITY_COLOR: Record<FindingSeverity, string> = { high: 'rose', medium: 'amber', low: 'gray' }
export const GROUP_LABEL: Record<FindingGroup, string> = { risk: 'Security risks', waste: 'Not in use', rules: 'Firewall rules' }
export const SOURCE_LABEL: Record<FindingSource, string> = { cloud: 'AWS says', agent: 'Agent measured', judgement: "Argus's judgement" }
export const SOURCE_HELP: Record<FindingSource, string> = {
  cloud: 'A fact from the last read of the account.',
  agent: "Measured by Argus's own agent on the server.",
  judgement: 'A rule of thumb Argus applies to what it read or measured. Check it before acting.',
}

/** Counts for the tiles and the tab. */
export function findingCounts(items: CloudFinding[]) {
  const risks = items.filter((f) => f.group === 'risk')
  return {
    high: risks.filter((f) => f.severity === 'high').length,
    otherRisks: risks.filter((f) => f.severity !== 'high').length,
    risks: risks.length,
    waste: items.filter((f) => f.group === 'waste').length,
    rules: items.filter((f) => f.group === 'rules').length,
  }
}

/** Whether a High finding may be kept this way: never with no end. */
export function keepAllowed(f: CloudFinding, days: KeepInput['days']): boolean {
  return f.severity !== 'high' || days > 0
}
