import type {
  AccessRequest,
  Asset,
  AuditEvent,
  Coverage,
  DiscoveredHost,
  FleetStats,
  GatewayPolicy,
  Session,
} from '~/types/domain'

/**
 * Live control-plane client.
 *
 * Argus can run without a control plane — the gateway still brokers and
 * records, the agent still catches bypasses. So the console degrades rather
 * than breaks: when no control plane is configured or reachable, the callers
 * fall back to the in-memory fixture in `api.ts`. `isLive()` is what the UI
 * uses to say which it is showing, because silently presenting demo data as
 * real is the one thing a security console must never do.
 */

// '/' means same-origin; anything else is an absolute base. Same-origin is the
// supported shape, because the session cookie depends on it.
const RAW_BASE = import.meta.env.VITE_CONTROL_URL ?? ''
const BASE = RAW_BASE === '/' ? '' : RAW_BASE.replace(/\/$/, '')
const TOKEN = import.meta.env.VITE_CONTROL_TOKEN ?? ''

/**
 * Where the gateway serves its browser endpoints.
 *
 * The terminal, the shadow stream and termination all go straight to the
 * gateway rather than through the control plane: the gateway is the only place
 * that holds the live connection, and proxying the byte stream through the
 * control plane would put a second service on the path of every keystroke.
 */
export const GATEWAY_URL: string =
  import.meta.env.VITE_GATEWAY_URL ?? 'http://127.0.0.1:8081'

export function isConfigured(): boolean {
  return RAW_BASE !== ''
}

export interface Identity {
  authenticated: boolean
  /**
   * Set when the control plane could not be reached at all.
   *
   * Distinct from "not signed in": the two look identical to a user and need
   * opposite responses. Reporting an outage as a configuration problem is how
   * someone spends an afternoon on their sign-in settings because a proxy line
   * pointed at the wrong scheme.
   */
  unreachable?: boolean
  email?: string
  displayName?: string
  role?: string
  /** True when this control plane holds accounts of its own. */
  passwordEnabled?: boolean
  /** False on a fresh install, where nobody can sign in yet. */
  accountsExist?: boolean
  /** Whether the signed-in account has a second factor. */
  mfaEnrolled?: boolean
}

/** What a password sign-in returns: a session, or a demand for a second factor. */
export interface PasswordLoginResult {
  mfaRequired?: boolean
  challenge?: string
  authenticated?: boolean
  role?: string
}

/**
 * Signs in with an email and password.
 *
 * The refusal is deliberately the same whatever went wrong, and it is shown
 * verbatim: the control plane is the authority on why, and inventing a friendlier
 * message here would either leak which addresses exist or mislead.
 */
export async function passwordLogin(
  email: string,
  password: string,
): Promise<PasswordLoginResult> {
  return post<PasswordLoginResult>('/auth/password', { email, password })
}

/** Completes a sign-in that owed a second factor. */
export async function verifyMFA(
  challenge: string,
  code: string,
  recovery = false,
): Promise<PasswordLoginResult> {
  return post<PasswordLoginResult>('/auth/mfa', { challenge, code, recovery })
}

/** Starts enrolment, returning the secret and the URI an app scans. */
export async function beginMFAEnrolment(): Promise<{ secret: string; uri: string }> {
  return post<{ secret: string; uri: string }>('/auth/mfa/enrol', {})
}

/** Confirms enrolment and returns the recovery codes, shown exactly once. */
export async function confirmMFAEnrolment(code: string): Promise<{ recoveryCodes: string[] }> {
  return post<{ recoveryCodes: string[] }>('/auth/mfa/confirm', { code })
}

/** Replaces the signed-in account's own password. */
export async function changePassword(current: string, next: string): Promise<void> {
  await post('/auth/password/change', { current, new: next })
}

/** Who am I, and if nobody, where do I go to sign in. */
export async function whoami(): Promise<Identity> {
  if (!isConfigured()) return { authenticated: false }
  try {
    const res = await fetch(`${BASE}/auth/me`, {
      credentials: 'include',
      headers: TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {},
    })
    // 401 is the expected answer for a browser that has not signed in yet, and
    // its body carries passwordEnabled and accountsExist -- everything the
    // sign-in screen needs. Treating it as an outage is what hid the sign-in
    // form entirely: LoginGate checks `unreachable` first, so a healthy control
    // plane correctly saying "you are not signed in" rendered as "could not be
    // reached", with nothing to click. Sign-in was impossible by construction,
    // which is exactly the confusion the flag exists to stop.
    if (res.ok || res.status === 401) {
      return (await res.json()) as Identity
    }
    // A gateway error means something in front of the control plane could not
    // reach it. Any other status means it answered, and answering is the thing
    // "unreachable" is about.
    if (res.status === 502 || res.status === 503 || res.status === 504) {
      return { authenticated: false, unreachable: true }
    }
    return { authenticated: false }
  } catch {
    // No response at all: wrong address, wrong scheme, or nothing listening.
    return { authenticated: false, unreachable: true }
  }
}

export async function logout(): Promise<void> {
  if (!isConfigured()) return
  await fetch(`${BASE}/auth/logout`, { method: 'POST', credentials: 'include' })
}

/**
 * Requests a single-use ticket for one terminal session.
 *
 * The control plane decides whether this is allowed; the gateway only verifies
 * the signature. So a refusal here is the authoritative one, and its message is
 * worth showing verbatim rather than replacing with something generic.
 */
async function post<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    method: 'POST',
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {}),
    },
    body: JSON.stringify(body),
  })
  const parsed = (await res.json()) as T & { error?: string }
  if (!res.ok) throw new Error(parsed.error ?? `request failed (${res.status})`)
  return parsed
}

export async function createRequest(input: {
  assetHostnames: string[]
  principal: string
  justification: string
  durationMinutes: number
  breakGlass: boolean
}): Promise<AccessRequest> {
  return post<AccessRequest>('/api/v1/requests', input)
}

export async function decideRequest(
  id: string,
  decision: 'approved' | 'denied',
  note: string,
): Promise<AccessRequest> {
  return post<AccessRequest>(`/api/v1/requests/${id}/decision`, { decision, note })
}

export async function terminalTicket(
  target: string,
  principal: string,
): Promise<{ ticket: string } | { error: string }> {
  const res = await fetch(`${BASE}/api/v1/terminal/ticket`, {
    method: 'POST',
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {}),
    },
    body: JSON.stringify({ target, principal }),
  })
  const body = (await res.json()) as { ticket?: string; error?: string }
  if (!res.ok || !body.ticket) return { error: body.error ?? `request failed (${res.status})` }
  return { ticket: body.ticket }
}

/**
 * The control plane answered a request with a status it chose.
 *
 * Distinct from a transport failure: a refusal is a fact about one request,
 * not about whether the control plane exists. Conflating them is how a 403 on
 * a single endpoint used to mark the whole deployment unreachable.
 */
export class ControlPlaneError extends Error {
  // Declared and assigned rather than as constructor parameter properties:
  // the build runs with erasableSyntaxOnly, so type-only syntax that emits
  // runtime code is refused.
  readonly status: number
  readonly path: string

  constructor(status: number, path: string) {
    super(`${path} → ${status}`)
    this.name = 'ControlPlaneError'
    this.status = status
    this.path = path
  }

  /** True when the caller is signed in but not permitted to do this. */
  get refused(): boolean {
    return this.status === 401 || this.status === 403
  }
}

/** Set once the first request succeeds, so the UI can label its data source. */
let reachable: boolean | null = null

export function isLive(): boolean {
  return isConfigured() && reachable === true
}

/**
 * Requests a single-use ticket to watch a session already in flight.
 *
 * The refusal here is the authoritative one — the gateway only checks the
 * signature — so its message is shown verbatim. "You need the auditor role"
 * is useful; "request failed" is not.
 */
export async function shadowTicket(
  sessionId: string,
): Promise<{ ticket: string } | { error: string }> {
  return scopedTicket(`/api/v1/sessions/${sessionId}/shadow/ticket`, {})
}

/** Requests a single-use ticket to end a session, with the reason recorded. */
export async function terminateTicket(
  sessionId: string,
  reason: string,
): Promise<{ ticket: string } | { error: string }> {
  return scopedTicket(`/api/v1/sessions/${sessionId}/terminate/ticket`, { reason })
}

async function scopedTicket(
  path: string,
  body: Record<string, unknown>,
): Promise<{ ticket: string } | { error: string }> {
  if (!isConfigured()) return { error: 'the control plane is not configured' }
  const res = await fetch(`${BASE}${path}`, {
    method: 'POST',
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {}),
    },
    body: JSON.stringify(body),
  })
  const parsed = (await res.json()) as { ticket?: string; error?: string }
  if (!res.ok || !parsed.ticket) {
    return { error: parsed.error ?? `request failed (${res.status})` }
  }
  return { ticket: parsed.ticket }
}

/**
 * Ends a session on the gateway.
 *
 * Two steps on purpose: the control plane authorises and records the intent,
 * then the gateway acts. An attempt that the gateway refuses is still in the
 * audit log, which is the version of events an investigator needs — "tried to
 * stop it and could not" is more urgent than "stopped it".
 */
export async function terminateSession(
  gatewayUrl: string,
  sessionId: string,
  reason: string,
): Promise<{ ok: true } | { error: string }> {
  const minted = await terminateTicket(sessionId, reason)
  if ('error' in minted) return minted

  try {
    const res = await fetch(
      `${gatewayUrl.replace(/\/$/, '')}/api/v1/sessions/${sessionId}/terminate`,
      {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${minted.ticket}`,
        },
        body: JSON.stringify({ reason }),
      },
    )
    if (!res.ok) return { error: (await res.text()).trim() || `gateway refused (${res.status})` }
    return { ok: true }
  } catch (err) {
    return { error: err instanceof Error ? err.message : 'the gateway is unreachable' }
  }
}

/**
 * Ends a Remote Desktop session on the gateway.
 *
 * Separate from terminateSession because the two run on different endpoints:
 * an SSH session is proxied and an RDP one is driven by Argus as the client,
 * so there is no single connection to close for both.
 */
export async function terminateRDPSession(
  gatewayUrl: string,
  sessionId: string,
  reason: string,
): Promise<{ ok: true } | { error: string }> {
  const minted = await terminateTicket(sessionId, reason)
  if ('error' in minted) return minted
  try {
    const res = await fetch(
      `${gatewayUrl.replace(/\/$/, '')}/api/v1/rdp/${sessionId}/terminate`,
      {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${minted.ticket}`,
        },
        body: JSON.stringify({ reason }),
      },
    )
    if (!res.ok) return { error: (await res.text()).trim() || `gateway refused (${res.status})` }
    return { ok: true }
  } catch (err) {
    return { error: err instanceof Error ? err.message : 'the gateway is unreachable' }
  }
}

/** Sessions the gateway currently has open, as opposed to what it last reported. */
export interface LiveSession {
  id: string
  userEmail: string
  principal: string
  assetHostname: string
  clientIp: string
  startedAt: string
  viewers: number
  riskFlags: string[]
  certSerial?: number
}

export async function liveSessions(gatewayUrl: string): Promise<LiveSession[]> {
  try {
    const res = await fetch(`${gatewayUrl.replace(/\/$/, '')}/api/v1/sessions/live`, {
      headers: TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {},
    })
    if (!res.ok) return []
    return (await res.json()) as LiveSession[]
  } catch {
    // A gateway that cannot be reached has no live sessions to show. Surfacing
    // an error here would break the dashboard for a panel that is advisory.
    return []
  }
}

/** Hosts running an agent that the inventory has no entry for. */
export async function discoveredHosts(
  state: 'unreviewed' | 'enrolled' | 'ignored' | '' = 'unreviewed',
): Promise<DiscoveredHost[]> {
  if (!isConfigured()) return []
  const q = state ? `?state=${state}` : ''
  return get<DiscoveredHost[]>(`/api/v1/discovered${q}`)
}

export async function coverage(): Promise<Coverage | null> {
  if (!isConfigured()) return null
  return get<Coverage>('/api/v1/coverage')
}

/** Promotes a discovered host into a managed asset. Grants no principals. */
export async function enrolHost(hostname: string): Promise<{ assetId: string }> {
  return post<{ assetId: string }>(
    `/api/v1/discovered/${encodeURIComponent(hostname)}/enrol`,
    {},
  )
}

/** Records a deliberate decision not to manage a host. The note is required. */
export async function ignoreHost(hostname: string, note: string): Promise<void> {
  await post(`/api/v1/discovered/${encodeURIComponent(hostname)}/ignore`, { note })
}

/**
 * Fetches a Remote Desktop recording, decoded to display frames.
 *
 * The control plane decodes and verifies before returning anything, so the
 * verdict arrives with the frames rather than after them — a viewer must not
 * form an impression of a session and only then be told it was altered.
 */
export async function rdpReplay(
  sessionId: string,
): Promise<{ buffer: ArrayBuffer; width: number; height: number; verified: string } | null> {
  if (!isConfigured()) return null
  const res = await fetch(`${BASE}/api/v1/sessions/${sessionId}/rdp-replay`, {
    credentials: 'include',
    headers: TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {},
  })
  if (!res.ok) return null

  let info: { width?: number; height?: number } = {}
  try {
    info = JSON.parse(res.headers.get('X-Argus-Replay-Info') ?? '{}')
  } catch {
    // A missing or malformed header should not lose the frames; the canvas
    // falls back to a sensible size.
  }
  return {
    buffer: await res.arrayBuffer(),
    width: info.width ?? 1024,
    height: info.height ?? 768,
    verified: res.headers.get('X-Argus-Chain-Verified') ?? 'unverified',
  }
}

async function get<T>(path: string): Promise<T> {
  let res: Response
  try {
    res = await fetch(`${BASE}${path}`, {
      // Session cookie is the real credential; the bearer token is a development
      // fallback the control plane refuses once any account exists.
      credentials: 'include',
      headers: TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {},
    })
  } catch (err) {
    // Nothing answered. This is the only thing "unreachable" should mean.
    reachable = false
    throw err
  }

  // It answered. That it refused this particular request says nothing about
  // whether the control plane is there -- and marking it unreachable on any
  // non-OK status meant a single 403 flipped isLive() false everywhere, which
  // made orFallback serve fixture data in place of a real deployment's.
  if (res.status !== 502 && res.status !== 503 && res.status !== 504) {
    reachable = true
  } else {
    reachable = false
  }

  if (!res.ok) throw new ControlPlaneError(res.status, path)
  return (await res.json()) as T
}

/**
 * Wraps a live call so a control-plane outage falls back instead of erroring.
 *
 * An outage only. A request the control plane refused is propagated, because
 * quietly answering it with the demo fixture would put invented hosts and
 * sessions on screen in a real deployment and label them as real -- the one
 * thing this console must never do. An error the operator can see beats a
 * fiction they cannot.
 */
async function orFallback<T>(live: () => Promise<T>, fallback: () => Promise<T>): Promise<T> {
  if (!isConfigured()) return fallback()
  try {
    return await live()
  } catch (err) {
    if (err instanceof ControlPlaneError) throw err
    return fallback()
  }
}

/** Gateway policy as the control plane currently holds it. */
export async function gatewayPolicy(): Promise<GatewayPolicy | null> {
  if (!isConfigured()) return null
  return get<GatewayPolicy>('/api/v1/policy')
}

/**
 * Replaces the whole policy, not a field.
 *
 * Sending the complete document means two admins editing at once cannot
 * interleave into a combination neither of them chose — the second write loses
 * cleanly and visibly rather than silently merging.
 */
export async function saveGatewayPolicy(policy: GatewayPolicy): Promise<GatewayPolicy> {
  return post<GatewayPolicy>('/api/v1/policy', policy)
}

export const live = {
  orFallback,

  stats: () => get<FleetStats>('/api/v1/stats'),
  assets: () => get<Asset[]>('/api/v1/assets'),

  sessions: (state?: string, origin?: string) => {
    const q = new URLSearchParams()
    if (state) q.set('state', state)
    if (origin) q.set('origin', origin)
    const qs = q.toString()
    return get<Session[]>(`/api/v1/sessions${qs ? `?${qs}` : ''}`)
  },

  session: (id: string) => get<Session>(`/api/v1/sessions/${id}`),

  /**
   * The control plane returns the chain it computed. The console still
   * recomputes it in a worker — an "intact" verdict that depends on trusting
   * the server that served the log is not worth much.
   */
  audit: () => get<AuditEvent[]>('/api/v1/audit?limit=500'),

  requests: (state?: string) =>
    get<AccessRequest[]>(`/api/v1/requests${state ? `?state=${state}` : ''}`),

  /**
   * Fetches a session's recording.
   *
   * The control plane re-verifies the hash chain before serving and reports the
   * verdict in X-Argus-Chain-Verified. The console shows that verdict rather
   * than assuming intact, because a recording that fails verification is the
   * single most important thing an auditor can be told about it.
   */
  async recording(id: string): Promise<{ cast: string; verified: string } | null> {
    if (!isConfigured()) return null
    const res = await fetch(`${BASE}/api/v1/sessions/${id}/recording`, {
      headers: { Authorization: `Bearer ${TOKEN}` },
    })
    if (!res.ok) return null
    return {
      cast: await res.text(),
      verified: res.headers.get('X-Argus-Chain-Verified') ?? 'unverified',
    }
  },
}
