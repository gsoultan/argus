import {
  EPOCH, accessRequests, assets, buildAuditSkeleton, currentUser, groups,
  sessions, users,
} from '~/lib/seed'
import {
  createRequest,
  decideRequest,
  gatewayPolicy,
  GATEWAY_URL,
  isConfigured,
  live,
  saveGatewayPolicy,
  terminateRDPSession as terminateRDPOnGateway,
  terminateSession as terminateOnGateway,
} from '~/lib/live'
import type {
  AccessRequest, Asset, AssetGroup, AuditEvent, FleetStats, GatewayPolicy, Session, User,
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
  encryptRecordingsSeparateKey: true,
}

// Mutable copies — mutations in the UI need somewhere to land.
const db = {
  assets: [...assets],
  requests: [...accessRequests],
  sessions: [...sessions],
  policy: { ...DEFAULT_POLICY },
}

export interface AssetQuery {
  search?: string
  groupId?: string | null
  health?: Asset['health'] | null
  credentialMode?: Asset['credentialMode'] | null
  hostKeyState?: Asset['hostKeyState'] | null
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
    sessionsActive: db.sessions.filter((s) => s.state === 'active').length,
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
  async me(): Promise<User> {
    await latency(60)
    return currentUser
  },

  async users(): Promise<User[]> {
    await latency()
    return users
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
      if (q.credentialMode && a.credentialMode !== q.credentialMode) return false
      if (q.hostKeyState && a.hostKeyState !== q.hostKeyState) return false
      return true
    })
  },

  async asset(id: string): Promise<Asset | undefined> {
    await latency(70)
    return db.assets.find((a) => a.id === id)
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
   * Against a configured deployment this reaches the gateway, which is the only
   * process holding the connection. It previously only marked the row
   * terminated, which meant an operator got a success message while the session
   * they were trying to stop carried on — the worst possible failure for this
   * particular button.
   */
  async terminateSession(id: string, reason: string): Promise<Session> {
    const s = db.sessions.find((x) => x.id === id)

    if (isConfigured()) {
      // An SSH session is proxied and an RDP one is driven by Argus as the
      // client, so they end on different endpoints. Picking by protocol here
      // keeps that out of every caller.
      const res = s?.protocol === 'rdp'
        ? await terminateRDPOnGateway(GATEWAY_URL, id, reason)
        : await terminateOnGateway(GATEWAY_URL, id, reason)
      if ('error' in res) throw new Error(res.error)
      // The gateway reports the closure to the control plane itself; refetching
      // is what makes the row authoritative rather than optimistic.
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
  async auditSkeleton(): Promise<Omit<AuditEvent, 'hash' | 'prevHash'>[]> {
    return live.orFallback(live.audit, async () => {
      await latency(200)
      return buildAuditSkeleton()
    })
  },
}

function mustFind(s: Session | undefined): Session {
  if (!s) throw new Error('session not found')
  return s
}
