import {
  EPOCH, accessRequests, assets, buildAuditSkeleton, currentUser, groups,
  sessions, users,
} from '~/lib/seed'
import {
  archiveAsset as archiveOnControlPlane,
  assetAssignments,
  createAsset as createOnControlPlane,
  createRequest,
  decideDirectLogin,
  decideRequest,
  gatewayPolicy,
  isConfigured,
  live,
  removeAssetAssignment,
  setAssetAssignment,
  setAssetDirectSsh,
  updateAsset as updateOnControlPlane,
  whoami,
  saveGatewayPolicy,
  terminateSession as terminateLive,
  teams as liveTeams,
  createTeam as liveCreateTeam,
  renameTeam as liveRenameTeam,
  deleteTeam as liveDeleteTeam,
  addTeamMember as liveAddTeamMember,
  removeTeamMember as liveRemoveTeamMember,
  assetTeamAssignments,
  setAssetTeamAssignment,
  removeAssetTeamAssignment,
  setAccountRole,
  setAccountDisabled,
  createAccount,
} from '~/lib/live'
import type { AuditPage } from '~/lib/live'
import type {
  AccessRequest, Asset, AssetAssignment, AssetGroup, AssetInput, DirectLogin, DirectSsh,
  FleetStats, GatewayPolicy, Session, Team, TeamAssignment, User,
} from '~/types/domain'

/**
 * In-memory stand-in for the Go control plane. Every function here maps 1:1 to
 * an endpoint on `argus-control`, so swapping this for `fetch` is mechanical.
 */

const latency = (ms = 120) => new Promise((r) => setTimeout(r, ms + Math.random() * 90))

/**
 * Gateway policy defaults.
 *
 * These are the safe positions, which is also what the Settings page claims:
 * every forwarding channel off, every recording guarantee on. The fixture holds
 * them so the page is honest without a control plane — it saves, it reads back
 * what it saved, and it says which store it is talking to.
 */
const DEFAULT_POLICY: GatewayPolicy = {
  allowLocalForward: false,
  allowRemoteForward: false,
  allowAgentForward: false,
  allowX11Forward: false,
  proxySftpSubsystem: true,
  failClosedOnRecordingLoss: true,
  requireEbpfForRoot: true,
  directSsh: 'refuse',
}

// Mutable copies — mutations in the UI need somewhere to land.
const db = {
  assets: [...assets],
  requests: [...accessRequests],
  sessions: [...sessions],
  policy: { ...DEFAULT_POLICY },
  /** assetId -> assignments, for the fixture. The control plane holds the real ones. */
  assignments: new Map<string, AssetAssignment[]>(),
  /** Nothing is held in the fixture: no agent is asking. */
  directLogins: [] as DirectLogin[],
  teams: [
    {
      id: 'team-ops', name: 'Ops', members: ['a.wijaya@northwind.id', 'siti.n@northwind.id'],
      servers: [], createdBy: 'dewi.p@northwind.id', createdAt: new Date(EPOCH - 30 * 86_400_000).toISOString(),
    },
    {
      id: 'team-dev', name: 'Developers', members: ['kevin.tan@northwind.id'],
      servers: [], createdBy: 'dewi.p@northwind.id', createdAt: new Date(EPOCH - 12 * 86_400_000).toISOString(),
    },
  ] as Team[],
  /** assetId -> team assignments. */
  teamAssignments: new Map<string, TeamAssignment[]>(),
}

/** The fixture's teams with their servers filled in from the assignment map. */
function fixtureTeams(): Team[] {
  const all = [...db.teamAssignments.values()].flat()
  return db.teams.map((t) => ({ ...t, servers: all.filter((g) => g.teamId === t.id) }))
}

function fixtureTeam(id: string): Team {
  const t = fixtureTeams().find((x) => x.id === id)
  if (!t) throw new Error('no such team')
  return t
}

/**
 * Builds a fixture asset from what an administrator typed.
 *
 * Everything observed rather than entered starts at its honest value: a host
 * nobody has verified is unpinned, and a host with no agent has none. A fixture
 * that invented a pinned key would make the console look better than the
 * product is.
 */
function fixtureAsset(input: AssetInput): Asset {
  return {
    id: crypto.randomUUID(),
    hostname: input.hostname,
    address: input.address || input.hostname,
    port: input.port ?? (input.protocol === 'rdp' ? 3389 : 22),
    protocol: input.protocol ?? 'ssh',
    os: input.os ?? '',
    tags: input.tags ?? [],
    groupId: groups[0]?.id ?? 'ungrouped',
    source: 'console',
    credentialMode:
      input.credentialMode ?? (input.protocol === 'rdp' ? 'injected-password' : 'injected-key'),
    credentialRef: input.credentialRef ?? '',
    domain: input.domain ?? '',
    hostKeyState: 'unpinned',
    hostKeyFingerprint: null,
    hostKeyPinnedAt: null,
    health: 'reachable',
    lastCheckedAt: new Date().toISOString(),
    agentState: 'absent',
    agentLastSeenAt: null,
    bypassPosture: 'open',
    unmanagedKeyCount: 0,
    principals: input.principals,
    credentialRotatedAt: null,
    rotationIntervalDays: input.rotationIntervalDays ?? null,
  }
}

export interface AssetQuery {
  search?: string
  groupId?: string | null
  health?: Asset['health'] | null
  /**
   * `standing` means "any mode but ca-certificate" -- the set the Overview
   * counts and asks you to shrink. It is not a mode an asset can be in.
   */
  credentialMode?: Asset['credentialMode'] | 'standing' | null
  /**
   * `unverified` means "any state but pinned" -- the set the Overview's
   * "Unverified hosts" tile counts. It is not a state an asset can be in.
   */
  hostKeyState?: Asset['hostKeyState'] | 'unverified' | null
  /**
   * Agent liveness and whether the host can still be reached around Argus.
   *
   * Both exist so Coverage can hand a finding to the inventory: it counts hosts
   * with no agent and hosts whose agent went quiet, and until these existed
   * there was no way to ask "which ones?" — the two pages described the same
   * fleet and neither could take you from one to the other.
   */
  agentState?: Asset['agentState'] | null
  bypassPosture?: Asset['bypassPosture'] | null
}

/**
 * Derived from `db`, never from the pristine fixture — otherwise creating a
 * request updates the list but not the badge counting it.
 */
function computeStats(): FleetStats {
  const dayAgo = EPOCH - 24 * 60 * 60_000
  return {
    assetsTotal: db.assets.length,
    assetsUnreachable: db.assets.filter((a) => a.health === 'unreachable').length,
    hostKeysUnpinned: db.assets.filter((a) => a.hostKeyState !== 'pinned').length,
    // Live means a reporter still speaks for it. A session whose gateway went
    // away is counted apart rather than folded in -- see Session.silent.
    sessionsActive: db.sessions.filter((s) => s.state === 'active' && !s.silent).length,
    sessionsSilent: db.sessions.filter((s) => s.state === 'active' && s.silent).length,
    sessionsToday: db.sessions.filter((s) => Date.parse(s.startedAt) > dayAgo).length,
    requestsPending: db.requests.filter((r) => r.state === 'pending').length,
    credentialsOverdue: db.assets.filter((a) => {
      if (!a.credentialRotatedAt || !a.rotationIntervalDays) return false
      return EPOCH - Date.parse(a.credentialRotatedAt) > a.rotationIntervalDays * 86_400_000
    }).length,
    standingCredentialAssets: db.assets.filter((a) => a.credentialMode !== 'ca-certificate').length,
    sessionsDirectToday: db.sessions.filter(
      (s) => s.origin === 'direct' && Date.parse(s.startedAt) > dayAgo,
    ).length,
    // The coverage gap: a bypass here would leave no trace at all.
    assetsUnmonitored: db.assets.filter((a) => a.bypassPosture === 'open').length,
    agentsStale: db.assets.filter((a) => a.agentState === 'stale').length,
  }
}

export const api = {
  /**
   * Who is signed in.
   *
   * The session, when there is one. This used to return the fixture
   * unconditionally, so the console showed the demo operator's name and role
   * however you had actually authenticated -- an auditor signed in as an
   * auditor was shown as an admin, and the Settings page, which gates its
   * switches on this role, offered eight controls the control plane would
   * refuse. The server enforced correctly; the console simply advertised
   * something it could not do.
   */
  async me(): Promise<User> {
    if (isConfigured()) {
      const id = await whoami()
      if (id.authenticated) {
        return {
          id: id.email ?? 'session',
          email: id.email ?? '',
          displayName: id.displayName || (id.email ?? '').split('@')[0] || 'signed in',
          role: (id.role ?? 'auditor') as User['role'],
          // Identity lives in the provider, so the console reports what the
          // session carries rather than inventing the rest.
          mfaEnrolled: false,
          lastSeenAt: new Date().toISOString(),
        }
      }
    }
    await latency(60)
    return currentUser
  },

  async users(): Promise<User[]> {
    return live.orFallback(live.users, async () => {
      await latency()
      return users
    })
  },

  async setRole(email: string, role: User['role']): Promise<void> {
    if (isConfigured()) {
      await setAccountRole(email, role)
      return
    }
    await latency(200)
    const u = users.find((x) => x.email === email)
    if (!u) throw new Error(`no account for ${email}`)
    if (u.email === currentUser.email) throw new Error('you cannot change your own role; ask another administrator')
    u.role = role
  },

  async setDisabled(email: string, disabled: boolean): Promise<void> {
    if (isConfigured()) {
      await setAccountDisabled(email, disabled)
      return
    }
    await latency(200)
    const u = users.find((x) => x.email === email)
    if (!u) throw new Error(`no account for ${email}`)
    u.disabled = disabled
  },

  async addAccount(input: { email: string; displayName: string; role: User['role']; password: string }): Promise<void> {
    if (isConfigured()) {
      await createAccount(input)
      return
    }
    await latency(250)
    if (users.some((u) => u.email === input.email.toLowerCase())) throw new Error(`${input.email} already has an account`)
    users.push({
      id: crypto.randomUUID(), email: input.email.toLowerCase(), displayName: input.displayName || input.email.split('@')[0]!,
      role: input.role, mfaEnrolled: false, lastSeenAt: null,
    })
  },

  async stats(): Promise<FleetStats> {
    return live.orFallback(live.stats, async () => {
      await latency(90)
      return computeStats()
    })
  },

  async groups(): Promise<AssetGroup[]> {
    await latency()
    return groups
  },

  async assets(q: AssetQuery = {}): Promise<Asset[]> {
    const source = await live.orFallback(live.assets, async () => {
      await latency()
      return db.assets
    })
    // Filtering stays client-side so the same predicates apply to live and
    // fixture data, and the two never diverge in what a filter means.
    const needle = q.search?.trim().toLowerCase()
    return source.filter((a) => {
      if (needle) {
        const hay = `${a.hostname} ${a.address} ${a.os} ${a.tags.join(' ')}`.toLowerCase()
        if (!hay.includes(needle)) return false
      }
      if (q.groupId && a.groupId !== q.groupId) return false
      if (q.health && a.health !== q.health) return false
      if (q.credentialMode === 'standing') {
        if (a.credentialMode === 'ca-certificate') return false
      } else if (q.credentialMode && a.credentialMode !== q.credentialMode) return false
      if (q.hostKeyState === 'unverified') {
        if (a.hostKeyState === 'pinned') return false
      } else if (q.hostKeyState && a.hostKeyState !== q.hostKeyState) return false
      if (q.agentState && a.agentState !== q.agentState) return false
      if (q.bypassPosture && a.bypassPosture !== q.bypassPosture) return false
      return true
    })
  },

  /**
   * One asset.
   *
   * Found in the list rather than fetched by id, so it inherits the list's
   * filtering exactly: an operator who is not assigned a host cannot reach its
   * detail page by guessing the id, without that rule having to be written a
   * second time on a second endpoint.
   */
  async asset(id: string): Promise<Asset | undefined> {
    const source = await live.orFallback(live.assets, async () => {
      await latency(70)
      return db.assets
    })
    return source.find((a) => a.id === id)
  },

  /* ── The inventory, as an administrator edits it ───────────────────────── */

  async createAsset(input: AssetInput): Promise<Asset> {
    if (isConfigured()) return createOnControlPlane(input)
    await latency(400)
    const asset = fixtureAsset(input)
    db.assets = [asset, ...db.assets]
    return asset
  },

  async updateAsset(id: string, input: AssetInput): Promise<Asset> {
    if (isConfigured()) return updateOnControlPlane(id, input)
    await latency(400)
    const existing = db.assets.find((a) => a.id === id)
    if (!existing) throw new Error('asset not found')
    const next = { ...fixtureAsset(input), id, hostKeyState: existing.hostKeyState }
    db.assets = db.assets.map((a) => (a.id === id ? next : a))
    return next
  },

  /** Retires an asset. Sessions and audit history keep pointing at it. */
  async archiveAsset(id: string): Promise<void> {
    if (isConfigured()) {
      await archiveOnControlPlane(id)
      return
    }
    await latency(300)
    db.assets = db.assets.filter((a) => a.id !== id)
    db.assignments.delete(id)
  },

  async assignments(id: string): Promise<AssetAssignment[]> {
    if (isConfigured()) return assetAssignments(id)
    await latency(120)
    return db.assignments.get(id) ?? []
  },

  /** An empty principal list is a removal, not an assignment of nothing. */
  async setAssignment(
    id: string,
    email: string,
    principals: string[],
  ): Promise<AssetAssignment[]> {
    if (isConfigured()) return setAssetAssignment(id, email, principals)
    await latency(300)
    const asset = db.assets.find((a) => a.id === id)
    if (!asset) throw new Error('asset not found')
    const unknown = principals.find((p) => !asset.principals.includes(p))
    if (unknown) {
      throw new Error(
        `${asset.hostname} does not permit the principal "${unknown}"; add it to the asset first`,
      )
    }
    const rest = (db.assignments.get(id) ?? []).filter((g) => g.userEmail !== email)
    const next =
      principals.length === 0
        ? rest
        : [
            ...rest,
            {
              assetId: id,
              hostname: asset.hostname,
              userEmail: email,
              principals,
              grantedBy: currentUser.email,
              grantedAt: new Date().toISOString(),
            },
          ]
    db.assignments.set(id, next)
    return next
  },

  async removeAssignment(id: string, email: string): Promise<void> {
    if (isConfigured()) {
      await removeAssetAssignment(id, email)
      return
    }
    await latency(200)
    db.assignments.set(
      id,
      (db.assignments.get(id) ?? []).filter((g) => g.userEmail !== email),
    )
  },

  /* ── Teams ─────────────────────────────────────────────────────────── */

  async teams(): Promise<Team[]> {
    if (isConfigured()) return liveTeams()
    await latency(100)
    return fixtureTeams()
  },

  async createTeam(name: string, members: string[]): Promise<Team> {
    if (isConfigured()) return liveCreateTeam(name, members)
    await latency(250)
    const clean = name.trim().replace(/\s+/g, ' ')
    if (!clean) throw new Error('a team needs a name')
    if (db.teams.some((t) => t.name.toLowerCase() === clean.toLowerCase())) {
      throw new Error(`there is already a team called "${clean}"`)
    }
    const t: Team = {
      id: crypto.randomUUID(), name: clean, members: [...new Set(members.map((m) => m.toLowerCase()))],
      servers: [], createdBy: currentUser.email, createdAt: new Date().toISOString(),
    }
    db.teams.push(t)
    return fixtureTeam(t.id)
  },

  async renameTeam(id: string, name: string): Promise<Team> {
    if (isConfigured()) return liveRenameTeam(id, name)
    await latency(200)
    const t = db.teams.find((x) => x.id === id)
    if (!t) throw new Error('no such team')
    t.name = name.trim()
    return fixtureTeam(id)
  },

  async deleteTeam(id: string): Promise<void> {
    if (isConfigured()) return liveDeleteTeam(id)
    await latency(200)
    db.teams = db.teams.filter((t) => t.id !== id)
    for (const [k, v] of db.teamAssignments) db.teamAssignments.set(k, v.filter((g) => g.teamId !== id))
  },

  async addTeamMember(id: string, email: string): Promise<Team> {
    if (isConfigured()) return liveAddTeamMember(id, email)
    await latency(200)
    const t = db.teams.find((x) => x.id === id)
    if (!t) throw new Error('no such team')
    const e = email.trim().toLowerCase()
    if (!t.members.includes(e)) t.members = [...t.members, e].sort()
    return fixtureTeam(id)
  },

  async removeTeamMember(id: string, email: string): Promise<Team> {
    if (isConfigured()) return liveRemoveTeamMember(id, email)
    await latency(200)
    const t = db.teams.find((x) => x.id === id)
    if (!t) throw new Error('no such team')
    t.members = t.members.filter((m) => m !== email)
    return fixtureTeam(id)
  },

  async teamAssignments(assetId: string): Promise<TeamAssignment[]> {
    if (isConfigured()) return assetTeamAssignments(assetId)
    await latency(100)
    return db.teamAssignments.get(assetId) ?? []
  },

  async setTeamAssignment(assetId: string, teamId: string, principals: string[]): Promise<TeamAssignment[]> {
    if (isConfigured()) return setAssetTeamAssignment(assetId, teamId, principals)
    await latency(250)
    const asset = db.assets.find((a) => a.id === assetId)
    const team = db.teams.find((t) => t.id === teamId)
    if (!asset) throw new Error('asset not found')
    if (!team) throw new Error('no such team')
    const unknown = principals.find((p) => !asset.principals.includes(p))
    if (unknown) {
      throw new Error(`${asset.hostname} does not permit the principal "${unknown}"; add it to the asset first`)
    }
    const rest = (db.teamAssignments.get(assetId) ?? []).filter((g) => g.teamId !== teamId)
    const next = principals.length === 0 ? rest : [...rest, {
      assetId, hostname: asset.hostname, teamId, teamName: team.name, principals,
      grantedBy: currentUser.email, grantedAt: new Date().toISOString(),
    }]
    db.teamAssignments.set(assetId, next)
    return next
  },

  async removeTeamAssignment(assetId: string, teamId: string): Promise<void> {
    if (isConfigured()) return removeAssetTeamAssignment(assetId, teamId)
    await latency(200)
    db.teamAssignments.set(assetId, (db.teamAssignments.get(assetId) ?? []).filter((g) => g.teamId !== teamId))
  },

  /** Pin the currently-presented host key. The MITM's most important control. */
  async pinHostKey(id: string): Promise<Asset> {
    await latency(300)
    const asset = db.assets.find((a) => a.id === id)
    if (!asset) throw new Error('asset not found')
    asset.hostKeyState = 'pinned'
    asset.hostKeyPinnedAt = new Date().toISOString()
    if (!asset.hostKeyFingerprint) asset.hostKeyFingerprint = `SHA256:${crypto.randomUUID().replace(/-/g, '')}`
    return asset
  },

  async rotateCredential(id: string): Promise<Asset> {
    await latency(600)
    const asset = db.assets.find((a) => a.id === id)
    if (!asset) throw new Error('asset not found')
    if (asset.credentialMode === 'ca-certificate') {
      throw new Error('Asset uses certificate auth — there is no standing credential to rotate.')
    }
    asset.credentialRotatedAt = new Date().toISOString()
    return asset
  },

  async sessions(state?: Session['state']): Promise<Session[]> {
    return live.orFallback(
      () => live.sessions(state),
      async () => {
        await latency()
        return state ? db.sessions.filter((s) => s.state === state) : db.sessions
      },
    )
  },

  async session(id: string): Promise<Session | undefined> {
    return live.orFallback(
      () => live.session(id),
      async () => {
        await latency(70)
        return db.sessions.find((s) => s.id === id)
      },
    )
  },

  /**
   * Ends a live session.
   *
   * Against a configured deployment this asks the control plane relaying the
   * session to have the server's agent end it. It previously only marked the row
   * terminated, which meant an operator got a success message while the session
   * they were trying to stop carried on — the worst possible failure for this
   * particular button.
   */
  async terminateSession(id: string, reason: string): Promise<Session> {
    const s = db.sessions.find((x) => x.id === id)

    if (isConfigured()) {
      const res = await terminateLive(id, reason)
      if ('error' in res) throw new Error(res.error)
      // The agent reports the closure itself; refetching is what makes the
      // row authoritative rather than optimistic.
      return (await this.session(id)) ?? mustFind(s)
    }

    await latency(400)
    const found = mustFind(s)
    found.state = 'terminated'
    found.endedAt = new Date().toISOString()
    return found
  },

  async policy(): Promise<GatewayPolicy> {
    return live.orFallback(
      async () => (await gatewayPolicy()) ?? db.policy,
      async () => {
        await latency(80)
        return db.policy
      },
    )
  },

  async savePolicy(next: GatewayPolicy): Promise<GatewayPolicy> {
    if (isConfigured()) return saveGatewayPolicy(next)
    await latency(400)
    db.policy = { ...next }
    return db.policy
  },

  async directLogins(): Promise<DirectLogin[]> {
    return live.orFallback(
      () => live.directLogins(),
      async () => {
        await latency(60)
        return db.directLogins
      },
    )
  },

  async decideDirectLogin(id: string, approve: boolean, note: string): Promise<DirectLogin> {
    if (isConfigured()) return decideDirectLogin(id, approve, note)
    await latency(300)
    const d = db.directLogins.find((x) => x.id === id)
    if (!d) throw new Error('direct login not found')
    d.state = approve ? 'approved' : 'denied'
    d.decidedAt = new Date().toISOString()
    d.decidedByEmail = currentUser.email
    d.decisionNote = note || null
    return d
  },

  async setAssetDirectSsh(id: string, mode: DirectSsh | null): Promise<Asset> {
    if (isConfigured()) return setAssetDirectSsh(id, mode)
    await latency(300)
    const a = db.assets.find((x) => x.id === id)
    if (!a) throw new Error('asset not found')
    a.directSsh = mode
    return a
  },

  async requests(state?: AccessRequest['state']): Promise<AccessRequest[]> {
    return live.orFallback(
      () => live.requests(state),
      async () => {
        await latency()
        return state ? db.requests.filter((r) => r.state === state) : db.requests
      },
    )
  },

  async createRequest(input: {
    assetHostnames: string[]
    principal: string
    justification: string
    durationMinutes: number
    breakGlass: boolean
  }): Promise<AccessRequest> {
    if (isConfigured()) return createRequest(input)
    await latency(500)
    const chosen = db.assets.filter((a) => input.assetHostnames.includes(a.hostname))
    const req: AccessRequest = {
      id: crypto.randomUUID(),
      requesterId: currentUser.id,
      requesterEmail: currentUser.email,
      assetIds: chosen.map((a) => a.id),
      assetHostnames: input.assetHostnames,
      principal: input.principal,
      justification: input.justification,
      durationMinutes: input.durationMinutes,
      state: 'pending',
      createdAt: new Date().toISOString(),
      decidedAt: null,
      decidedByEmail: null,
      decisionNote: null,
      expiresAt: null,
      breakGlass: input.breakGlass,
    }
    db.requests = [req, ...db.requests]
    return req
  },

  async decideRequest(
    id: string,
    decision: 'approved' | 'denied',
    note: string,
  ): Promise<AccessRequest> {
    if (isConfigured()) return decideRequest(id, decision, note)
    await latency(450)
    const r = db.requests.find((x) => x.id === id)
    if (!r) throw new Error('request not found')
    r.state = decision
    r.decidedAt = new Date().toISOString()
    r.decidedByEmail = currentUser.email
    r.decisionNote = note || null
    r.expiresAt =
      decision === 'approved'
        ? new Date(Date.now() + r.durationMinutes * 60_000).toISOString()
        : null
    return r
  },

  /**
   * Returns the chain skeleton. Hashes are computed client-side in a worker —
   * the point of the chain is that you don't have to trust this response.
   */
  /**
   * The log the audit page verifies, and how much of it this is.
   *
   * `total` is null when nothing can say -- the fixture is the whole of itself,
   * and a control plane too old to send the header has not told us. Null must
   * read as "unknown", never as "the window is everything".
   */
  async auditSkeleton(): Promise<AuditPage> {
    return live.orFallback(live.audit, async () => {
      await latency(200)
      const events = buildAuditSkeleton()
      // The fixture holds exactly what it generates; there is no more of it,
      // so the window and the log are the same thing here.
      return { events, total: events.length }
    })
  },
}

function mustFind(s: Session | undefined): Session {
  if (!s) throw new Error('session not found')
  return s
}
