import {
  EPOCH, accessRequests, assets, buildAuditSkeleton, currentUser, groups,
  sessions, users,
} from '~/lib/seed'
import {
  archiveAsset as archiveOnControlPlane,
  assetAssignments,
  createAsset as createOnControlPlane,
  accessRequest,
  cancelRequest,
  createRequest,
  decideDirectLogin,
  decideRequest,
  gatewayPolicy,
  isConfigured,
  live,
  removeAssetAssignment,
  setAssetAssignment,
  setAssetDirectSsh,
  setAssetDirectSshAccount,
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
  myServerAccount,
  myServers as liveMyServers,
  setMySshKey,
  setLoginName,
  setAccountRole,
  setAccountDisabled,
  createAccount,
  sendTemporaryPassword,
  createSharedAccount,
  setAccountShareable,
  personServers as livePersonServers,
  resetAccountMFA, signOutAccount,
} from '~/lib/live'
import type { AuditPage } from '~/lib/live'
import { isElevatedAccount } from '~/lib/roles'
import type {
  AuditEvent,
  AccessRequest, Asset, AssetAssignment, AssetGroup, AssetInput, DirectLogin, DirectSsh, MyServer,
  ServerLoginAccount, ServerLoginAccounts, ServerApplications, RequestableHost, ServerLogin,
  FleetStats, GatewayPolicy, PersonOwnSetting, PersonServer, ServerAccount, Session, Team, TeamAccess,
  TeamAssignment, TemporaryPasswordOutcome, User,
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
  /** The fixture's signed-in person's own server account. */
  serverAccount: { loginName: currentUser.email.split('@')[0]!, sshKey: '', sshFingerprint: '', directSsh: true } as ServerAccount,
}

/** assetId -> login accounts made from the console, in the fixture. */
const demoServerLogins = new Map<string, ServerLogin[]>()

/**
 * The fixture's console-made logins: a ready one and a locked one on every
 * server with an agent, so the section has something to show.
 */
function demoLogins(id: string): ServerLogin[] {
  const have = demoServerLogins.get(id)
  if (have) return have
  const a = db.assets.find((x) => x.id === id)
  if (!a || a.agentState === 'absent') return []
  const at = new Date(EPOCH - 9 * 86_400_000).toISOString()
  const seed: ServerLogin[] = [
    { name: 'releases', state: 'active', createdBy: 'dewi.p@northwind.id', createdAt: at, changedBy: 'dewi.p@northwind.id', changedAt: at, agent: 'ready' },
    { name: 'contractor', state: 'locked', createdBy: 'dewi.p@northwind.id', createdAt: at, changedBy: 'dewi.p@northwind.id', changedAt: new Date(EPOCH - 2 * 86_400_000).toISOString(), agent: 'locked' },
  ]
  if (!a.principals.includes('releases')) a.principals = [...a.principals, 'releases']
  demoServerLogins.set(id, seed)
  return seed
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
    desktopMode: input.desktopMode ?? 'rdp',
    kind: input.kind ?? 'server',
    controlMode: input.kind === 'android' ? (input.controlMode ?? 'attended') : undefined,
    controlNeedsApproval: input.kind === 'android' ? input.controlNeedsApproval === true : undefined,
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
          mfaEnrolled: !!id.mfaEnrolled,
          lastSeenAt: new Date().toISOString(),
          ...(id.org ? { org: id.org } : {}),
          ...(id.orgs ? { orgs: id.orgs } : {}),
          shared: id.shared === true,
          multiOrg: id.multiOrg === true,
          platformOwner: id.platformOwner === true,
          canEmailPeople: id.canEmailPeople === true,
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

  async myServerAccount(): Promise<ServerAccount> {
    if (isConfigured()) return myServerAccount()
    await latency(100)
    return { ...db.serverAccount }
  },

  async myServers(): Promise<MyServer[]> {
    if (isConfigured()) return liveMyServers()
    await latency(100)
    // The fixture: a personal account on every Linux server, let straight in
    // on most, held for approval on a few, and browser only on the databases.
    return db.assets.filter((a) => a.protocol !== 'rdp').map((a, i) => ({
      hostname: a.hostname,
      personalAccount: true,
      sudo: true,
      directSsh: /db|pg|sql/i.test(a.hostname) ? 'off' : i % 4 === 2 ? 'approve' : 'allow',
    }))
  },

  async setMySshKey(key: string): Promise<ServerAccount> {
    if (isConfigured()) return setMySshKey(key)
    await latency(200)
    const k = key.trim()
    if (k && !/^(ssh-(ed25519|rsa)|ecdsa-sha2-nistp\d+|sk-ssh-ed25519@openssh\.com) [A-Za-z0-9+/=]+( .*)?$/.test(k)) {
      throw new Error('paste one public SSH key, such as the contents of ~/.ssh/id_ed25519.pub')
    }
    db.serverAccount = { ...db.serverAccount, sshKey: k, sshFingerprint: k ? 'SHA256:sample-fingerprint' : '' }
    return { ...db.serverAccount }
  },

  async setLoginName(email: string, name: string): Promise<void> {
    if (isConfigured()) {
      await setLoginName(email, name)
      return
    }
    await latency(200)
    if (!/^[a-z_][a-z0-9._-]{0,31}$/.test(name)) {
      throw new Error('a server account name is 1 to 32 characters: lowercase letters, digits, dot, underscore and hyphen')
    }
    if (users.some((u) => u.email !== email && u.loginName === name)) {
      throw new Error('another person already has that server account name')
    }
    const u = users.find((x) => x.email === email)
    if (!u) throw new Error(`no account for ${email}`)
    u.loginName = name
  },

  /** Removes someone's second step, for a lost phone. Live only. */
  async resetMFA(email: string): Promise<void> {
    if (isConfigured()) {
      await resetAccountMFA(email)
      return
    }
    await latency(200)
    const u = users.find((x) => x.email === email)
    if (!u) throw new Error(`no account for ${email}`)
    u.mfaEnrolled = false
  },

  /** Signs someone out of every browser. Live only; the fixture has no sessions. */
  async signOutAccount(email: string): Promise<void> {
    if (isConfigured()) await signOutAccount(email)
    else await latency(200)
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
    u.disabledAt = disabled ? new Date(EPOCH).toISOString() : null
    u.disabledBy = disabled ? currentUser.email : ''
  },

  async addAccount(input: { email: string; displayName: string; role: User['role'] }): Promise<TemporaryPasswordOutcome> {
    if (isConfigured()) return createAccount(input)
    await latency(250)
    if (users.some((u) => u.email === input.email.toLowerCase())) throw new Error(`${input.email} already has an account`)
    const expires = new Date(Date.now() + 72 * 3600_000).toISOString()
    users.push({
      id: crypto.randomUUID(), email: input.email.toLowerCase(), displayName: input.displayName || input.email.split('@')[0]!,
      role: input.role, mfaEnrolled: false, lastSeenAt: null,
      temporaryPassword: true, temporaryPasswordExpiresAt: expires,
    })
    return { email: input.email.toLowerCase(), role: input.role, emailed: true, temporaryPasswordExpiresAt: expires }
  },

  async sendTemporaryPassword(email: string): Promise<TemporaryPasswordOutcome> {
    if (isConfigured()) return sendTemporaryPassword(email)
    await latency(250)
    const u = users.find((x) => x.email === email)
    if (!u) throw new Error(`no account for ${email}`)
    const expires = new Date(Date.now() + 72 * 3600_000).toISOString()
    u.temporaryPassword = true
    u.temporaryPasswordExpiresAt = expires
    return { email, emailed: true, temporaryPasswordExpiresAt: expires }
  },

  async addSharedAccount(input: { email: string; displayName: string; role: User['role']; homeOrg?: string }): Promise<void> {
    if (isConfigured()) {
      await createSharedAccount(input)
      return
    }
    await latency(250)
    throw new Error('no account that can be shared uses this address')
  },

  async setShareable(email: string, shareable: boolean): Promise<void> {
    if (isConfigured()) {
      await setAccountShareable(email, shareable)
      return
    }
    await latency(150)
    const u = users.find((x) => x.email === email)
    if (!u) throw new Error(`no account for ${email}`)
    u.shareable = shareable
  },

  async stats(): Promise<FleetStats> {
    return live.orFallback(live.stats, async () => {
      await latency(90)
      return computeStats()
    })
  },

  async recentAudit(limit: number): Promise<AuditEvent[]> {
    return live.orFallback(() => live.recentAudit(limit), async () => {
      await latency(80)
      return buildAuditSkeleton()
        .map((e) => ({ ...e, hash: '', prevHash: '' }))
        .sort((a, b) => b.seq - a.seq)
        .slice(0, limit)
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

  /** Who can log in to a server directly, from its agent's last check-in. */
  async loginAccounts(id: string): Promise<ServerLoginAccounts> {
    return live.orFallback(() => live.loginAccounts(id), async () => {
      await latency(90)
      const a = db.assets.find((x) => x.id === id)
      if (!a || a.agentState === 'absent') {
        return { reported: false, checkedAt: null, accounts: [], passwords: false, keys: false, exposeAuthInfo: false }
      }
      const sample: Record<string, Omit<ServerLoginAccount, 'user'>> = {
        deploy: { password: 'locked', keys: 2, canLogin: true },
        ops: { password: 'set', keys: 1, canLogin: true },
        root: { password: 'set', keys: 0, canLogin: false },
      }
      const accounts: ServerLoginAccount[] = [
        ...a.principals.map((user) => ({ user, ...(sample[user] ?? { password: 'set' as const, keys: 0, canLogin: true }) })),
        { user: 'sys-admin', password: 'set', keys: 0, canLogin: true },
        { user: 'staging', password: 'locked', keys: 1, canLogin: true },
      ]
      return {
        reported: true, checkedAt: a.agentLastSeenAt ?? null, accounts,
        passwords: true, keys: true, exposeAuthInfo: true,
      }
    })
  },

  /** Login accounts an admin made on this server from the console. */
  async serverLogins(id: string): Promise<ServerLogin[]> {
    return live.orFallback(() => live.serverLogins(id), async () => {
      await latency(90)
      return demoLogins(id)
    })
  },

  async createServerLogin(id: string, name: string): Promise<ServerLogin[]> {
    if (isConfigured()) return live.createServerLogin(id, name)
    await latency(300)
    const a = db.assets.find((x) => x.id === id)
    if (!a) throw new Error('asset not found')
    if (!/^[a-z_][a-z0-9._-]{0,31}$/.test(name)) throw new Error('a server account name is 1 to 32 characters: lowercase letters, digits, dot, underscore and hyphen, starting with a letter or underscore')
    const list = demoLogins(id)
    const had = list.find((l) => l.name === name)
    if (!had && a.principals.includes(name)) throw new Error(`${a.hostname} already lists "${name}" as a login account; Argus makes only new accounts`)
    const now = new Date().toISOString()
    const next: ServerLogin = { name, state: 'active', createdBy: had?.createdBy ?? currentUser.email, createdAt: had?.createdAt ?? now, changedBy: currentUser.email, changedAt: now, agent: '' }
    demoServerLogins.set(id, [...list.filter((l) => l.name !== name), next])
    if (!a.principals.includes(name)) a.principals = [...a.principals, name]
    return demoLogins(id)
  },

  async removeServerLogin(id: string, name: string, forGood: boolean): Promise<ServerLogin[]> {
    if (isConfigured()) return live.removeServerLogin(id, name, forGood)
    await latency(300)
    const a = db.assets.find((x) => x.id === id)
    if (!a) throw new Error('asset not found')
    const now = new Date().toISOString()
    demoServerLogins.set(id, demoLogins(id).map((l) => l.name !== name ? l : {
      ...l, state: forGood ? 'deleted' as const : 'locked' as const, agent: forGood ? 'deleted' as const : 'locked' as const,
      changedBy: currentUser.email, changedAt: now,
    }))
    a.principals = a.principals.filter((p) => p !== name)
    return demoLogins(id)
  },

  /**
   * The accounts a person can ask for on each server: its Login accounts list,
   * each checked against what its agent last reported.
   */
  async requestAccounts(hostnames: string[]): Promise<RequestableHost[]> {
    return live.orFallback(() => live.requestAccounts(hostnames), async () => {
      await latency(90)
      return hostnames.map((hostname) => {
        const a = db.assets.find((x) => x.hostname === hostname)
        if (!a) return { hostname, known: false, agentReported: false, accounts: [] }
        const reported = a.agentState !== 'absent'
        return {
          hostname, known: true, agentReported: reported,
          // The demo fleet has no deploy account wherever the agent reports,
          // so the form's "not on this server" path is visible without a backend.
          accounts: a.principals.map((name) => ({
            name, elevated: isElevatedAccount(name), onServer: !reported || name !== 'deploy',
          })),
        }
      })
    })
  },

  /** What runs on a server, from its agent's last report. */
  async applications(id: string): Promise<ServerApplications> {
    return live.orFallback(() => live.applications(id), async () => {
      await latency(90)
      const a = db.assets.find((x) => x.id === id)
      const none: ServerApplications = {
        reported: false, checkedAt: null, applications: [], listeners: [], containers: [], services: [], sources: [],
      }
      if (!a || a.agentState === 'absent' || a.protocol === 'rdp') return none
      const l = (address: string, port: number, extra: Partial<ServerApplications['listeners'][number]> = {}) =>
        ({ address, port, localOnly: address.startsWith('127.') || address === '::1', ...extra })
      return {
        reported: true,
        checkedAt: a.agentLastSeenAt ?? null,
        applications: [
          { kind: 'nginx', name: 'nginx', version: '1.24.0-2ubuntu7', running: true,
            listen: [l('0.0.0.0', 80), l('0.0.0.0', 443)], process: 'nginx', unit: 'nginx.service',
            package: 'nginx', container: '', image: '', evidence: ['package', 'unit', 'process'] },
          { kind: 'postgres', name: 'PostgreSQL', version: '16.4-1.pgdg24.04+1', running: true,
            listen: [l('127.0.0.1', 5432), l('10.20.0.14', 5432)], process: 'postgres',
            unit: 'postgresql@16-main.service', package: 'postgresql-16', container: '', image: '',
            evidence: ['package', 'unit', 'process'] },
          { kind: 'redis', name: 'Redis', version: '7.2', running: true, listen: [l('127.0.0.1', 6379)],
            process: '', unit: '', package: '', container: 'cache', image: 'redis:7.2', evidence: ['container'] },
          { kind: 'docker', name: 'Docker', version: '5:27.3.1-1~ubuntu.24.04~noble', running: true, listen: [],
            process: '', unit: 'docker.service', package: 'docker-ce', container: '', image: '',
            evidence: ['package', 'unit', 'containers'] },
        ],
        listeners: [
          l('0.0.0.0', 22, { process: 'sshd', unit: 'ssh.service' }),
          l('0.0.0.0', 80, { process: 'nginx', unit: 'nginx.service', kind: 'nginx' }),
          l('0.0.0.0', 443, { process: 'nginx', unit: 'nginx.service', kind: 'nginx' }),
          l('127.0.0.1', 5432, { process: 'postgres', unit: 'postgresql@16-main.service', kind: 'postgres' }),
          l('10.20.0.14', 5432, { process: 'postgres', unit: 'postgresql@16-main.service', kind: 'postgres' }),
          l('127.0.0.1', 6379, { process: 'docker-proxy', unit: 'docker.service', kind: 'redis' }),
          l('0.0.0.0', 9100, { process: 'node_exporter', unit: 'node-exporter.service' }),
        ],
        containers: [
          { name: 'cache', image: 'redis:7.2', state: 'running', runtime: 'docker', ports: [l('127.0.0.1', 6379)], kind: 'redis' },
          { name: 'billing-worker', image: 'ghcr.io/acme/billing:2.3.1', state: 'running', runtime: 'docker', ports: [], kind: '' },
        ],
        services: ['containerd.service', 'cron.service', 'docker.service', 'nginx.service', 'node-exporter.service',
          'postgresql@16-main.service', 'ssh.service', 'systemd-journald.service'],
        sources: ['sockets', 'systemd', 'dpkg', 'docker'],
      }
    })
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
    own?: PersonOwnSetting | null,
  ): Promise<AssetAssignment[]> {
    if (isConfigured()) return setAssetAssignment(id, email, principals, own)
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
      principals.length === 0 && !own
        ? rest
        : [
            ...rest,
            {
              assetId: id,
              hostname: asset.hostname,
              userEmail: email,
              principals,
              personalAccounts: own ? own.personalAccounts : null,
              sudo: own?.sudo ?? false,
              directSsh: own?.directSsh ?? 'off',
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

  /** What one person may do on each server: their own setting replaces their teams'. */
  async personServers(email: string): Promise<PersonServer[]> {
    if (isConfigured()) return livePersonServers(email)
    await latency(120)
    const rank = { off: 0, approve: 1, allow: 2 } as const
    const out: PersonServer[] = []
    for (const asset of db.assets) {
      const own = (db.assignments.get(asset.id) ?? []).find((g) => g.userEmail === email)
      const held = (db.teamAssignments.get(asset.id) ?? []).filter((g) =>
        db.teams.find((t) => t.id === g.teamId)?.members.includes(email))
      if (!own && held.length === 0) continue
      const ps: PersonServer = {
        assetId: asset.id, hostname: asset.hostname,
        principals: [...new Set([...(own?.principals ?? []), ...held.flatMap((g) => g.principals)])].sort(),
        personalAccount: false, sudo: false, directSsh: 'off', source: '',
        teams: held.map((g) => g.teamName), ownSetting: own?.personalAccounts != null,
      }
      if (own?.personalAccounts != null) {
        if (own.personalAccounts) {
          Object.assign(ps, { personalAccount: true, sudo: !!own.sudo, directSsh: own.directSsh ?? 'off', source: 'own' })
        }
      } else {
        for (const g of held.filter((g) => g.personalAccounts)) {
          ps.personalAccount = true
          ps.source = 'teams'
          ps.sudo ||= !!g.sudo
          if (rank[g.directSsh ?? 'off'] > rank[ps.directSsh]) ps.directSsh = g.directSsh ?? 'off'
        }
      }
      out.push(ps)
    }
    return out
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

  async setTeamAssignment(assetId: string, teamId: string, access: TeamAccess): Promise<TeamAssignment[]> {
    if (isConfigured()) return setAssetTeamAssignment(assetId, teamId, access)
    const { principals, personalAccounts, sudo, directSsh } = access
    await latency(250)
    const asset = db.assets.find((a) => a.id === assetId)
    const team = db.teams.find((t) => t.id === teamId)
    if (!asset) throw new Error('asset not found')
    if (!team) throw new Error('no such team')
    const unknown = principals.find((p) => !asset.principals.includes(p))
    if (unknown) {
      throw new Error(`${asset.hostname} does not permit the principal "${unknown}"; add it to the asset first`)
    }
    if ((sudo || directSsh !== 'off') && !personalAccounts) {
      throw new Error('sudo and direct SSH apply to personal accounts; turn those on first')
    }
    const rest = (db.teamAssignments.get(assetId) ?? []).filter((g) => g.teamId !== teamId)
    const next = principals.length === 0 && !personalAccounts ? rest : [...rest, {
      assetId, hostname: asset.hostname, teamId, teamName: team.name, principals,
      personalAccounts, sudo, directSsh,
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

  async setAssetDirectSshAccount(id: string, account: string, mode: DirectSsh | null): Promise<Asset> {
    if (isConfigured()) return setAssetDirectSshAccount(id, account, mode)
    await latency(300)
    const a = db.assets.find((x) => x.id === id)
    if (!a) throw new Error('asset not found')
    const accounts = { ...(a.directSshAccounts ?? {}) }
    if (mode) accounts[account] = mode
    else delete accounts[account]
    a.directSshAccounts = accounts
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

  async request(id: string): Promise<AccessRequest> {
    return live.orFallback(
      () => accessRequest(id),
      async () => {
        await latency(60)
        const r = db.requests.find((x) => x.id === id)
        if (!r) throw new Error('request not found')
        return r
      },
    )
  },

  async cancelRequest(id: string): Promise<AccessRequest> {
    if (isConfigured()) return cancelRequest(id)
    await latency(300)
    const r = db.requests.find((x) => x.id === id)
    if (!r) throw new Error('request not found')
    if (r.state !== 'pending') throw new Error(`this request was already ${r.state}`)
    r.state = 'cancelled'
    r.decidedAt = new Date().toISOString()
    r.decidedByEmail = currentUser.email
    return r
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
    if (r.state !== 'pending') throw new Error(`this request was already ${r.state}`)
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
