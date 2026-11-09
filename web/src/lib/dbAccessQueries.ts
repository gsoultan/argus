/*
 * Giving many databases at once: the queries and mutations. Apart from
 * queries.ts so the pages that never give access do not load them.
 */
import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { assetDBAccess, giveDBAccess, planDBAccess, removeDBAccess, removeDBRule } from '~/lib/dbAccessApi'
import { isConfigured } from '~/lib/live'
import { dbqk } from '~/lib/queries'

/** Every grant and rule on one server's databases, for Who has what. */
export const assetDBAccessQuery = (assetId: string) =>
  queryOptions({ queryKey: ['database-access', assetId] as const, queryFn: () => assetDBAccess(assetId), enabled: isConfigured() })

/** What a grant on many databases would change; nothing is written. */
export const planDBAccessQuery = (input: Parameters<typeof planDBAccess>[0], enabled: boolean) =>
  queryOptions({
    queryKey: ['database-access', 'plan', input] as const,
    queryFn: () => planDBAccess(input),
    enabled: enabled && isConfigured(),
    staleTime: 0,
    gcTime: 0,
    retry: false,
  })

/** After a change to many databases' access, every view of it is stale. */
function invalidateDBAccess(qc: ReturnType<typeof useQueryClient>) {
  void qc.invalidateQueries({ queryKey: ['database'] })
  void qc.invalidateQueries({ queryKey: ['database-access'] })
  void qc.invalidateQueries({ queryKey: dbqk.databases })
}

export function useGiveDBAccess() {
  const qc = useQueryClient()
  return useMutation({ mutationFn: giveDBAccess, onSuccess: () => invalidateDBAccess(qc) })
}

export function useRemoveDBAccess() {
  const qc = useQueryClient()
  return useMutation({ mutationFn: removeDBAccess, onSuccess: () => invalidateDBAccess(qc) })
}

export function useRemoveDBRule() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { assetId: string; ruleId: string; takeAway: boolean }) => removeDBRule(v.assetId, v.ruleId, v.takeAway),
    onSuccess: () => invalidateDBAccess(qc),
  })
}
