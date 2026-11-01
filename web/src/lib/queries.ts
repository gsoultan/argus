import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type AssetQuery } from '~/lib/api'
import {
  coverage as fetchCoverage,
  createEnrollToken,
  discoveredHosts,
  enrolHost,
  type EnrolGrant,
  enrolledAgents,
  revokeAgent,
  ignoreHost,
  isConfigured,
  databases as liveDatabases,
  databasesOnAsset as liveDatabasesOnAsset,
  database as liveDatabase,
  dbAssignments as liveDBAssignments,
  createDatabase as liveCreateDatabase,
  updateDatabase as liveUpdateDatabase,
  deleteDatabase as liveDeleteDatabase,
  setDBAssignment as liveSetDBAssignment,
  removeDBAssignment as liveRemoveDBAssignment,
  requestDatabaseAccess as liveRequestDatabaseAccess,
} from '~/lib/live'
import type { AccessRequest, AssetInput, DirectSsh, PersonOwnSetting, Session, TeamAccess, User } from '~/types/domain'

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

/** Direct SSH logins: polled, since someone is waiting at a terminal. */
export const directLoginsQuery = () =>
  queryOptions({ queryKey: ['direct-logins'], queryFn: api.directLogins, refetchInterval: 3_000 })

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
    mutationFn: ({ hostname, grant }: { hostname: string; grant?: EnrolGrant }) =>
      enrolHost(hostname, grant),
    onSuccess: () => invalidateCoverage(qc),
  })
}

export function useCreateEnrollToken() {
  return useMutation({ mutationFn: (hostname: string) => createEnrollToken(hostname) })
}

export function enrolledAgentsQuery() {
  return queryOptions({
    queryKey: ['enrolled-agents'],
    queryFn: enrolledAgents,
    enabled: isConfigured(),
  })
}

export function useRevokeAgent() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => revokeAgent(id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['enrolled-agents'] }),
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

export function useDecideDirectLogin() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; approve: boolean; note: string }) =>
      api.decideDirectLogin(v.id, v.approve, v.note),
    onSettled: () => void qc.invalidateQueries({ queryKey: ['direct-logins'] }),
  })
}

export function useSetAssetDirectSsh() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; mode: DirectSsh | null }) => api.setAssetDirectSsh(v.id, v.mode),
    onSuccess: (asset) => {
      qc.setQueryData(qk.asset(asset.id), asset)
      void qc.invalidateQueries({ queryKey: ['assets'] })
    },
  })
}

/* ── The inventory ───────────────────────────────────────────────────────── */

export const assignmentsQuery = (assetId: string) =>
  queryOptions({
    queryKey: ['assignments', assetId],
    queryFn: () => api.assignments(assetId),
  })

/**
 * Everything the inventory touches, invalidated together.
 *
 * Assignment changes what each person sees in `assets`, and an edit changes the
 * fleet counts on the Overview. Refreshing one and not the others is how a
 * console ends up showing two different answers to the same question on two
 * pages.
 */
function invalidateInventory(qc: ReturnType<typeof useQueryClient>) {
  void qc.invalidateQueries({ queryKey: ['assets'] })
  void qc.invalidateQueries({ queryKey: ['asset'] })
  void qc.invalidateQueries({ queryKey: ['assignments'] })
  void qc.invalidateQueries({ queryKey: qk.stats })
}

export function useCreateAsset() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (input: AssetInput) => api.createAsset(input),
    onSuccess: () => invalidateInventory(qc),
  })
}

export function useUpdateAsset() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; input: AssetInput }) => api.updateAsset(v.id, v.input),
    onSuccess: () => invalidateInventory(qc),
  })
}

export function useArchiveAsset() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.archiveAsset(id),
    onSuccess: () => invalidateInventory(qc),
  })
}

export function useSetAssignment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { assetId: string; email: string; principals: string[]; own?: PersonOwnSetting | null }) =>
      api.setAssignment(v.assetId, v.email, v.principals, v.own),
    onSuccess: () => {
      invalidateInventory(qc)
      void qc.invalidateQueries({ queryKey: ['person-servers'] })
    },
  })
}

export function useRemoveAssignment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { assetId: string; email: string }) =>
      api.removeAssignment(v.assetId, v.email),
    onSuccess: () => invalidateInventory(qc),
  })
}

/* ── Teams ───────────────────────────────────────────────────────────────── */

export const personServersQuery = (email: string) =>
  queryOptions({ queryKey: ['person-servers', email], queryFn: () => api.personServers(email) })

export const teamsQuery = () => queryOptions({ queryKey: ['teams'], queryFn: api.teams })

export const teamAssignmentsQuery = (assetId: string) =>
  queryOptions({
    queryKey: ['team-assignments', assetId],
    queryFn: () => api.teamAssignments(assetId),
  })

/** Teams change who reaches which server, so the inventory refreshes with them. */
function invalidateTeams(qc: ReturnType<typeof useQueryClient>) {
  void qc.invalidateQueries({ queryKey: ['teams'] })
  void qc.invalidateQueries({ queryKey: ['team-assignments'] })
  void qc.invalidateQueries({ queryKey: ['person-servers'] })
  invalidateInventory(qc)
}

export function useCreateTeam() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { name: string; members: string[] }) => api.createTeam(v.name, v.members),
    onSuccess: () => invalidateTeams(qc),
  })
}

export function useRenameTeam() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; name: string }) => api.renameTeam(v.id, v.name),
    onSuccess: () => invalidateTeams(qc),
  })
}

export function useDeleteTeam() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.deleteTeam(id),
    onSuccess: () => invalidateTeams(qc),
  })
}

export function useAddTeamMember() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; email: string }) => api.addTeamMember(v.id, v.email),
    onSuccess: () => invalidateTeams(qc),
  })
}

export function useRemoveTeamMember() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; email: string }) => api.removeTeamMember(v.id, v.email),
    onSuccess: () => invalidateTeams(qc),
  })
}

export function useSetTeamAssignment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { assetId: string; teamId: string; access: TeamAccess }) =>
      api.setTeamAssignment(v.assetId, v.teamId, v.access),
    onSuccess: () => invalidateTeams(qc),
  })
}

export function useRemoveTeamAssignment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { assetId: string; teamId: string }) =>
      api.removeTeamAssignment(v.assetId, v.teamId),
    onSuccess: () => invalidateTeams(qc),
  })
}

/* ── People ──────────────────────────────────────────────────────────────── */

export function useSetRole() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { email: string; role: User['role'] }) => api.setRole(v.email, v.role),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.users }),
  })
}

export const myServerAccountQuery = () =>
  queryOptions({ queryKey: ['me', 'server-account'] as const, queryFn: () => api.myServerAccount() })

export function useSetMySshKey() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (key: string) => api.setMySshKey(key),
    onSuccess: (acct) => {
      qc.setQueryData(['me', 'server-account'], acct)
      void qc.invalidateQueries({ queryKey: qk.users })
    },
  })
}

export function useSetLoginName() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { email: string; name: string }) => api.setLoginName(v.email, v.name),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.users }),
  })
}

export function useSetDisabled() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { email: string; disabled: boolean }) => api.setDisabled(v.email, v.disabled),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.users }),
  })
}

export function useAddAccount() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.addAccount,
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.users }),
  })
}

/* ── Databases ──────────────────────────────────────────────────────────── */

export const dbqk = {
  databases: ['databases'] as const,
  database: (id: string) => ['database', id] as const,
  dbAssignments: (id: string) => ['database', id, 'assignments'] as const,
}

export const databasesQuery = () =>
  queryOptions({ queryKey: dbqk.databases, queryFn: liveDatabases, enabled: isConfigured() })

export const databasesOnAssetQuery = (assetId: string) =>
  queryOptions({ queryKey: [...dbqk.databases, 'asset', assetId] as const, queryFn: () => liveDatabasesOnAsset(assetId), enabled: isConfigured() })

export const databaseQuery = (id: string) =>
  queryOptions({ queryKey: dbqk.database(id), queryFn: () => liveDatabase(id), enabled: isConfigured() })

export const dbAssignmentsQuery = (id: string) =>
  queryOptions({ queryKey: dbqk.dbAssignments(id), queryFn: () => liveDBAssignments(id), enabled: isConfigured() })

export function useCreateDatabase() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: liveCreateDatabase,
    onSuccess: () => void qc.invalidateQueries({ queryKey: dbqk.databases }),
  })
}

export function useUpdateDatabase() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string } & Parameters<typeof liveUpdateDatabase>[1]) =>
      liveUpdateDatabase(v.id, v),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: dbqk.databases })
      void qc.invalidateQueries({ queryKey: dbqk.database(v.id) })
    },
  })
}

export function useDeleteDatabase() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: liveDeleteDatabase,
    onSuccess: () => void qc.invalidateQueries({ queryKey: dbqk.databases }),
  })
}

export function useSetDBAssignment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string } & Parameters<typeof liveSetDBAssignment>[1]) =>
      liveSetDBAssignment(v.id, v),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: dbqk.dbAssignments(v.id) })
      void qc.invalidateQueries({ queryKey: dbqk.databases })
    },
  })
}

export function useRemoveDBAssignment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; assignmentId: string }) => liveRemoveDBAssignment(v.id, v.assignmentId),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: dbqk.dbAssignments(v.id) })
      void qc.invalidateQueries({ queryKey: dbqk.databases })
    },
  })
}

export function useRequestDatabaseAccess() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string } & Parameters<typeof liveRequestDatabaseAccess>[1]) =>
      liveRequestDatabaseAccess(v.id, v),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['requests'] }),
  })
}
