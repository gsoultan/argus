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
  changeApproval as liveChangeApproval,
  databasePoolerSetup as liveDatabasePoolerSetup,
  dbAssignments as liveDBAssignments,
  createDatabase as liveCreateDatabase,
  updateDatabase as liveUpdateDatabase,
  deleteDatabase as liveDeleteDatabase,
  setDBAssignment as liveSetDBAssignment,
  removeDBAssignment as liveRemoveDBAssignment,
  requestDatabaseAccess as liveRequestDatabaseAccess,
  agentUpdate as liveAgentUpdate,
  requestAgentUpdate as liveRequestAgentUpdate,
  cancelAgentUpdate as liveCancelAgentUpdate,
  requestAgentRestart as liveRequestAgentRestart,
  agentFleet as liveAgentFleet,
  updateAgentFleet as liveUpdateAgentFleet,
  setAgentAutoUpdate as liveSetAgentAutoUpdate,
  databaseBackups as liveDatabaseBackups,
  backupCatalog as liveBackupCatalog,
  startBackup as liveStartBackup,
  cancelBackup as liveCancelBackup,
  deleteBackup as liveDeleteBackup,
  pinBackup as livePinBackup,
  restoreBackup as liveRestoreBackup,
  cancelRestore as liveCancelRestore,
  undoRestore as liveUndoRestore,
  setBackupSchedule as liveSetBackupSchedule,
  testBackup as liveTestBackup,
  backupTest as liveBackupTest,
  cancelBackupTest as liveCancelBackupTest,
  tryBackupChecks as liveTryBackupChecks,
  savedQueries as liveSavedQueries,
  saveQuery as liveSaveQuery,
  updateSavedQuery as liveUpdateSavedQuery,
  deleteSavedQuery as liveDeleteSavedQuery,
  controlStatus,
} from '~/lib/live'
import type { AccessRequest, AssetInput, BackupsView, DirectSsh, PersonOwnSetting, Session, TeamAccess, User } from '~/types/domain'

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

/** Who can log in to a server directly; whoever sees the whole fleet. */
export const loginAccountsQuery = (id: string) =>
  queryOptions({ queryKey: ['asset', id, 'login-accounts'] as const, queryFn: () => api.loginAccounts(id) })

/** Login accounts an admin made on a server from the console. */
export const serverLoginsQuery = (id: string) =>
  queryOptions({ queryKey: ['asset', id, 'server-logins'] as const, queryFn: () => api.serverLogins(id) })

/** Makes, locks or deletes a console-made login; the server's lists follow. */
export function useServerLoginAction() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; name: string; action: 'create' | 'lock' | 'delete' }) =>
      v.action === 'create'
        ? api.createServerLogin(v.id, v.name)
        : api.removeServerLogin(v.id, v.name, v.action === 'delete'),
    onSuccess: (list, v) => {
      qc.setQueryData(['asset', v.id, 'server-logins'], list)
      void qc.invalidateQueries({ queryKey: qk.asset(v.id) })
      void qc.invalidateQueries({ queryKey: ['assets'] })
      void qc.invalidateQueries({ queryKey: ['asset', v.id, 'login-accounts'] })
    },
  })
}

/** The accounts a person can ask for on the servers they picked. */
export const requestAccountsQuery = (hostnames: string[]) =>
  queryOptions({
    queryKey: ['request-accounts', [...hostnames].sort()] as const,
    queryFn: () => api.requestAccounts(hostnames),
    enabled: hostnames.length > 0,
  })

/** What runs on a server, from its agent's last report. */
export const applicationsQuery = (id: string) =>
  queryOptions({ queryKey: ['asset', id, 'applications'] as const, queryFn: () => api.applications(id) })

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

/** The newest audit entries, for Home. Polled: it is the page's "what just happened". */
/** argus-control's own health (Settings › Status, and Home's summary of it). */
export const statusKey = ['platform', 'status'] as const
export const controlStatusQuery = () =>
  queryOptions({ queryKey: statusKey, queryFn: controlStatus, refetchInterval: 60_000 })

export const recentAuditQuery = (limit = 6) =>
  queryOptions({ queryKey: ['audit', 'recent', limit] as const, queryFn: () => api.recentAudit(limit), refetchInterval: 30_000 })

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

export function useSetAssetDirectSshAccount() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; account: string; mode: DirectSsh | null }) =>
      api.setAssetDirectSshAccount(v.id, v.account, v.mode),
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

export function useResetMFA() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (email: string) => api.resetMFA(email),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.users }),
  })
}

export function useSignOutAccount() {
  return useMutation({ mutationFn: (email: string) => api.signOutAccount(email) })
}

export function useAddSharedAccount() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.addSharedAccount,
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.users }),
  })
}

export function useSetShareable() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { email: string; shareable: boolean }) => api.setShareable(v.email, v.shareable),
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

export function useSendTemporaryPassword() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.sendTemporaryPassword,
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.users }),
  })
}

/* ── Databases ──────────────────────────────────────────────────────────── */

export const dbqk = {
  databases: ['databases'] as const,
  database: (id: string) => ['database', id] as const,
  dbAssignments: (id: string) => ['database', id, 'assignments'] as const,
}

export const savedQueriesQuery = (dbId: string) =>
  queryOptions({ queryKey: [...dbqk.database(dbId), 'saved-queries'] as const, queryFn: () => liveSavedQueries(dbId), enabled: isConfigured() })

export function useSaveQuery(dbId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (q: { id?: string; name: string; sql: string; shared: boolean }) =>
      q.id ? liveUpdateSavedQuery(dbId, q.id, { name: q.name, sql: q.sql, shared: q.shared })
        : liveSaveQuery(dbId, { name: q.name, sql: q.sql, shared: q.shared }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: [...dbqk.database(dbId), 'saved-queries'] }),
  })
}

export function useDeleteSavedQuery(dbId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => liveDeleteSavedQuery(dbId, id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: [...dbqk.database(dbId), 'saved-queries'] }),
  })
}

export const databasesQuery = () =>
  queryOptions({ queryKey: dbqk.databases, queryFn: liveDatabases, enabled: isConfigured() })

export const databasesOnAssetQuery = (assetId: string) =>
  queryOptions({ queryKey: [...dbqk.databases, 'asset', assetId] as const, queryFn: () => liveDatabasesOnAsset(assetId), enabled: isConfigured() })

export const databaseQuery = (id: string) =>
  queryOptions({ queryKey: dbqk.database(id), queryFn: () => liveDatabase(id), enabled: isConfigured() })
export const databasePoolerSetupQuery = (id: string) =>
  queryOptions({ queryKey: [...dbqk.database(id), 'pooler-setup'] as const, queryFn: () => liveDatabasePoolerSetup(id), enabled: isConfigured() })

/** Whether a database's changes wait for approval, and who decides them. */
export const changeApprovalQuery = (id: string) =>
  queryOptions({ queryKey: [...dbqk.database(id), 'approval'] as const, queryFn: () => liveChangeApproval(id), enabled: isConfigured() })

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
    // The id goes in the path only; the control plane refuses unknown fields.
    mutationFn: ({ id, ...patch }: { id: string } & Parameters<typeof liveUpdateDatabase>[1]) =>
      liveUpdateDatabase(id, patch),
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
    mutationFn: ({ id, ...input }: { id: string } & Parameters<typeof liveSetDBAssignment>[1]) =>
      liveSetDBAssignment(id, input),
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
    mutationFn: ({ id, ...input }: { id: string } & Parameters<typeof liveRequestDatabaseAccess>[1]) =>
      liveRequestDatabaseAccess(id, input),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['requests'] }),
  })
}

/* ── Updating a server's agent ─────────────────────────────────────────── */

const agentUpdateKey = (assetId: string) => ['agent-update', assetId] as const

/** Polled fast while an update is under way, so the steps show as they happen. */
export const agentUpdateQuery = (assetId: string) =>
  queryOptions({
    queryKey: agentUpdateKey(assetId),
    queryFn: () => liveAgentUpdate(assetId),
    enabled: isConfigured(),
    refetchInterval: (q) => {
      const v = q.state.data
      if (!v) return false
      const s = v.status?.state
      const moving = v.request !== null || s === 'downloading' || s === 'waiting' || s === 'restarting'
      return moving ? 2_000 : 30_000
    },
  })

export function useRequestAgentUpdate(assetId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { version?: string; now?: boolean }) => liveRequestAgentUpdate(assetId, body),
    onSettled: () => void qc.invalidateQueries({ queryKey: agentUpdateKey(assetId) }),
  })
}

export function useRequestAgentRestart(assetId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { now?: boolean }) => liveRequestAgentRestart(assetId, body),
    onSettled: () => void qc.invalidateQueries({ queryKey: agentUpdateKey(assetId) }),
  })
}

export function useCancelAgentUpdate(assetId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => liveCancelAgentUpdate(assetId),
    onSettled: () => void qc.invalidateQueries({ queryKey: agentUpdateKey(assetId) }),
  })
}

/** Every server's agent. Polled fast while any is updating. */
export const agentFleetQuery = () =>
  queryOptions({
    queryKey: ['agent-fleet'] as const,
    queryFn: () => liveAgentFleet(),
    enabled: isConfigured(),
    refetchInterval: (q) => ((q.state.data?.updating ?? 0) > 0 ? 3_000 : 60_000),
  })

export function useUpdateAgentFleet() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { hostnames?: string[]; now?: boolean }) => liveUpdateAgentFleet(body),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ['agent-fleet'] })
      void qc.invalidateQueries({ queryKey: ['agent-update'] })
    },
  })
}

export function useSetAgentAutoUpdate() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (on: boolean) => liveSetAgentAutoUpdate(on),
    onSettled: () => void qc.invalidateQueries({ queryKey: ['agent-fleet'] }),
  })
}
/* ── Database backups ───────────────────────────────────────────────────── */

export const backupqk = {
  backups: (id: string) => ['database', id, 'backups'] as const,
  catalog: (id: string) => ['database', id, 'backups', 'catalog'] as const,
  test: (id: string, testId: string) => ['database', id, 'backups', 'test', testId] as const,
}

/** Polled every 2 s while a backup, restore or test restore runs, otherwise every 30 s. */
export const databaseBackupsQuery = (id: string) =>
  queryOptions({
    queryKey: backupqk.backups(id),
    queryFn: () => liveDatabaseBackups(id),
    enabled: isConfigured(),
    refetchInterval: (q) => {
      const v = q.state.data as BackupsView | undefined
      const busy = Boolean(v?.running) || Boolean(v?.testing) || (v?.restores ?? []).some((r) => r.state === 'running')
      return busy ? 2_000 : 30_000
    },
  })

/** One test restore with its report; polled while it runs. */
export const backupTestQuery = (id: string, testId: string) =>
  queryOptions({
    queryKey: backupqk.test(id, testId),
    queryFn: () => liveBackupTest(id, testId),
    enabled: isConfigured() && Boolean(testId),
    refetchInterval: (q) => ((q.state.data as { state?: string } | undefined)?.state === 'running' ? 2_000 : false),
  })

export const backupCatalogQuery = (id: string) =>
  queryOptions({ queryKey: backupqk.catalog(id), queryFn: () => liveBackupCatalog(id), enabled: isConfigured(), staleTime: 60_000 })

function useBackupMutation<V, R>(fn: (v: V) => Promise<R>, idOf: (v: V) => string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: fn,
    onSettled: (_d, _e, v) => {
      void qc.invalidateQueries({ queryKey: backupqk.backups(idOf(v)) })
      void qc.invalidateQueries({ queryKey: dbqk.databases })
      void qc.invalidateQueries({ queryKey: ['requests'] })
    },
  })
}

export const useStartBackup = () =>
  useBackupMutation((v: { id: string; reason?: string; destination?: BackupsView['schedule']['destination'] }) =>
    liveStartBackup(v.id, { reason: v.reason, destination: v.destination }), (v) => v.id)
export const useCancelBackup = () =>
  useBackupMutation((v: { id: string; backupId: string }) => liveCancelBackup(v.id, v.backupId), (v) => v.id)
export const useDeleteBackup = () =>
  useBackupMutation((v: { id: string; backupId: string }) => liveDeleteBackup(v.id, v.backupId), (v) => v.id)
export const usePinBackup = () =>
  useBackupMutation((v: { id: string; backupId: string; pinned: boolean }) => livePinBackup(v.id, v.backupId, v.pinned), (v) => v.id)
export const useRestoreBackup = () =>
  useBackupMutation((v: { id: string; backupId: string } & Parameters<typeof liveRestoreBackup>[2]) =>
    liveRestoreBackup(v.id, v.backupId, v), (v) => v.id)
export const useCancelRestore = () =>
  useBackupMutation((v: { id: string; restoreId: string }) => liveCancelRestore(v.id, v.restoreId), (v) => v.id)
export const useUndoRestore = () =>
  useBackupMutation((v: { id: string; restoreId: string; confirm: string }) => liveUndoRestore(v.id, v.restoreId, v.confirm), (v) => v.id)
export const useTestBackup = () =>
  useBackupMutation((v: { id: string; backupId: string }) => liveTestBackup(v.id, v.backupId), (v) => v.id)
export const useCancelBackupTest = () =>
  useBackupMutation((v: { id: string; testId: string }) => liveCancelBackupTest(v.id, v.testId), (v) => v.id)
export const useTryBackupChecks = () =>
  useMutation({ mutationFn: (v: { id: string; checks: Parameters<typeof liveTryBackupChecks>[1] }) => liveTryBackupChecks(v.id, v.checks) })
export const useSetBackupSchedule = () =>
  useBackupMutation((v: { id: string } & Parameters<typeof liveSetBackupSchedule>[1]) =>
    liveSetBackupSchedule(v.id, v), (v) => v.id)
