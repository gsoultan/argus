import { queryOptions, useQuery } from '@tanstack/react-query'
import { get, isConfigured } from '~/lib/live'

/*
 * Approving your own request, when nobody else could (the control plane's
 * self_approval.go). Every approval means a second person agreed, so the
 * person who asked never decides their own, except where nobody else holds
 * a role that decides that kind: then they may, typing the phrase, and
 * Activity records it as a warning. The control plane counts the accounts
 * each time, so this is only what the console offers; it decides again.
 */

/** Whether the reader is the only one who decides each kind. */
export interface SelfApproval {
  /** Access requests, database changes and restores, cloud changes, schedules and access. */
  requests: boolean
  /** Update and software runs, disk grows and log cleanups. */
  runs: boolean
  /** What they type to approve their own. */
  phrase: string
}

export const SELF_APPROVAL_PHRASE = 'approve my own request'

const none: SelfApproval = { requests: false, runs: false, phrase: SELF_APPROVAL_PHRASE }

export const selfApprovalQuery = () =>
  queryOptions({
    queryKey: ['approvals', 'self'] as const,
    queryFn: () => get<SelfApproval>('/api/v1/approvals/self'),
    enabled: isConfigured(),
    // A new account changes the answer; a minute is soon enough, and the
    // control plane refuses a self-approval that is no longer allowed.
    staleTime: 60_000,
  })

/** Whether the reader decides kind alone. False until known, and in the fixture. */
export function useDecidesAlone(kind: 'requests' | 'runs'): boolean {
  const { data } = useQuery(selfApprovalQuery())
  return (data ?? none)[kind]
}

/** The phrase the control plane expects. */
export function useSelfApprovalPhrase(): string {
  const { data } = useQuery(selfApprovalQuery())
  return data?.phrase || SELF_APPROVAL_PHRASE
}
