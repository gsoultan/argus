import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type AssetQuery } from '~/lib/api'
import {
  coverage as fetchCoverage,
  discoveredHosts,
  enrolHost,
  ignoreHost,
} from '~/lib/live'
import type { AccessRequest, Session } from '~/types/domain'

export const qk = {
  me: ['me'] as const,
  users: ['users'] as const,
  stats: ['stats'] as const,
  groups: ['groups'] as const,
  assets: (q: AssetQuery) => ['assets', q] as const,
  asset: (id: string) => ['asset', id] as const,
  sessions: (state?: Session['state']) => ['sessions', state ?? 'all'] as const,
  session: (id: string) => ['session', id] as const,
  requests: (state?: AccessRequest['state']) => ['requests', state ?? 'all'] as const,
  audit: ['audit'] as const,
}

export const meQuery = () => queryOptions({ queryKey: qk.me, queryFn: api.me })
export const usersQuery = () => queryOptions({ queryKey: qk.users, queryFn: api.users })
export const groupsQuery = () => queryOptions({ queryKey: qk.groups, queryFn: api.groups })

export const statsQuery = () =>
  queryOptions({ queryKey: qk.stats, queryFn: api.stats, refetchInterval: 15_000 })

export const assetsQuery = (q: AssetQuery = {}) =>
  queryOptions({ queryKey: qk.assets(q), queryFn: () => api.assets(q) })

export const assetQuery = (id: string) =>
  queryOptions({ queryKey: qk.asset(id), queryFn: () => api.asset(id) })

export const sessionsQuery = (state?: Session['state']) =>
  queryOptions({
    queryKey: qk.sessions(state),
    queryFn: () => api.sessions(state),
    // Live sessions need to feel live; history does not.
    refetchInterval: state === 'active' ? 5_000 : false,
  })

export const sessionQuery = (id: string) =>
  queryOptions({ queryKey: qk.session(id), queryFn: () => api.session(id) })

export const requestsQuery = (state?: AccessRequest['state']) =>
  queryOptions({ queryKey: qk.requests(state), queryFn: () => api.requests(state) })

export const policyQuery = () =>
  queryOptions({ queryKey: ['policy'], queryFn: api.policy })

export const auditQuery = () =>
  queryOptions({ queryKey: qk.audit, queryFn: api.auditSkeleton, staleTime: 5 * 60_000 })

/* ── Mutations ───────────────────────────────────────────────────────────── */

export function usePinHostKey() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.pinHostKey,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['assets'] })
      void qc.invalidateQueries({ queryKey: ['asset'] })
      void qc.invalidateQueries({ queryKey: qk.stats })
    },
  })
}

export function useRotateCredential() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.rotateCredential,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['assets'] })
      void qc.invalidateQueries({ queryKey: ['asset'] })
      void qc.invalidateQueries({ queryKey: qk.stats })
    },
  })
}

export function useTerminateSession() {
  const qc = useQueryClient()
  return useMutation({
    // The reason is part of the call, not an afterthought: the gateway refuses
    // a termination without one, because a session cut short and unexplained is
    // a gap in the record rather than an entry in it.
    mutationFn: (v: { id: string; reason: string }) => api.terminateSession(v.id, v.reason),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sessions'] })
      void qc.invalidateQueries({ queryKey: ['session'] })
      void qc.invalidateQueries({ queryKey: qk.stats })
    },
  })
}

export function coverageQuery() {
  return queryOptions({
    queryKey: ['coverage'],
    queryFn: () => fetchCoverage(),
    // Coverage is what someone checks when they want to know whether the fleet
    // is watched, so a stale answer is worse than a slow one.
    refetchInterval: 30_000,
  })
}

export function discoveredQuery(state: 'unreviewed' | 'enrolled' | 'ignored' | '') {
  return queryOptions({
    queryKey: ['discovered', state],
    queryFn: () => discoveredHosts(state),
    refetchInterval: 30_000,
  })
}

function invalidateCoverage(qc: ReturnType<typeof useQueryClient>) {
  void qc.invalidateQueries({ queryKey: ['coverage'] })
  void qc.invalidateQueries({ queryKey: ['discovered'] })
  void qc.invalidateQueries({ queryKey: ['assets'] })
  void qc.invalidateQueries({ queryKey: qk.stats })
}

export function useEnrolHost() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (hostname: string) => enrolHost(hostname),
    onSuccess: () => invalidateCoverage(qc),
  })
}

export function useIgnoreHost() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { hostname: string; note: string }) => ignoreHost(v.hostname, v.note),
    onSuccess: () => invalidateCoverage(qc),
  })
}

export function useSavePolicy() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.savePolicy,
    // Replaced with the server's answer rather than the draft, so a field the
    // control plane refused to change is visible immediately instead of looking
    // saved until the next reload.
    onSuccess: (saved) => qc.setQueryData(['policy'], saved),
  })
}

export function useCreateRequest() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.createRequest,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['requests'] })
      void qc.invalidateQueries({ queryKey: qk.stats })
    },
  })
}

export function useDecideRequest() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; decision: 'approved' | 'denied'; note: string }) =>
      api.decideRequest(v.id, v.decision, v.note),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['requests'] })
      void qc.invalidateQueries({ queryKey: qk.stats })
    },
  })
}
