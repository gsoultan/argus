import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type AssetQuery } from '~/lib/api'
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
    mutationFn: api.terminateSession,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sessions'] })
      void qc.invalidateQueries({ queryKey: ['session'] })
      void qc.invalidateQueries({ queryKey: qk.stats })
    },
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
