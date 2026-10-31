import type { ChainInput } from '~/workers/chain.worker'
import type {
  AccessRequest,
  Asset,
  AssetAssignment,
  AssetInput,
  AuditEvent,
  DirectLogin,
  DirectSsh,
  Coverage,
  DiscoveredHost,
  FleetStats,
  GatewayPolicy,
  Session,
  Team,
  TeamAssignment,
  User,
} from '~/types/domain'

/**
 * Live control-plane client.
 *
 * The console can be run without a control plane, against a fixture, for
 * development and demonstration. So it degrades rather
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

interface RuntimeConfig { version?: string; fleetUrl?: string }

function runtimeConfig(): RuntimeConfig | undefined {
  return (globalThis as { __ARGUS__?: RuntimeConfig }).__ARGUS__
}

/**
 * Where servers reach the control plane, for the enrollment command.
 *
 * Usually the address this console was loaded from. Not behind a Cloudflare
 * Tunnel: there the console's hostname carries browsers only, agents cannot
 * enrol through it, and `console.fleet_url` names the address they can.
 */
export function fleetURL(): string {
  const configured = runtimeConfig()?.fleetUrl
  if (configured) return configured.replace(/\/$/, '')
  return isConfigured() ? `https://${controlPlaneHost()}` : ''
}

/**
 * The serving control plane's release, when it is one ("0.2.0", not "dev"),
 * so the agent it offers to install matches it.
 */
export function controlPlaneVersion(): string | undefined {
  const v = runtimeConfig()?.version?.replace(/^v/, '')
  return v && /^\d+\.\d+\.\d+/.test(v) ? v : undefined
}

export function isConfigured(): boolean {
  return RAW_BASE !== ''
}

/**
 * Where a browser terminal connects: the control plane, which relays to the
 * server's own agent, which opens and records the session on the server.
 */
export function terminalEndpoint(): string {
  return `${BASE}/api/v1/terminal`
}

/** The file browser's WebSocket endpoint, on the control plane. */
export function filesEndpoint(): string {
  return `${BASE}/api/v1/files`
}

/** Where a Remote Desktop opens, through the Windows server's agent. */
export function desktopEndpoint(): string {
  return `${BASE}/api/v1/desktop`
}

/** Where a session in flight is watched, read only. */
export function watchEndpoint(sessionId: string): string {
  return `${BASE}/api/v1/sessions/${encodeURIComponent(sessionId)}/watch`
}

/**
 * An http(s) endpoint as the WebSocket URL to open, with its query.
 *
 * Relative endpoints are the same-origin deployment, resolved against the
 * page the console was loaded from.
 */
export function socketURL(endpoint: string, params: Record<string, string>): string {
  const url = new URL(endpoint, window.location.href)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v)
  return url.toString()
}

/**
 * What this console is pointed at, for display in the header.
 *
 * Same-origin is the supported shape, so in the usual deployment this is the
 * host the operator already typed — which is the point: it says *which*
 * deployment they are looking at, verifiably, rather than a name someone
 * configured a UI to claim.
 */
export function controlPlaneHost(): string {
  if (!isConfigured()) return ''
  if (RAW_BASE === '/') return window.location.host
  try {
    return new URL(BASE).host
  } catch {
    return BASE
  }
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
  /** True when a fresh install is offering its /setup page. */
  setupAvailable?: boolean
  /** True when the control plane has no configuration yet and serves only /setup. */
  installing?: boolean
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
    // A 502, 503 or 504 means something in front of the control plane could not
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

/** Where first-run setup stands, as GET /auth/setup reports it. */
export interface SetupStatus {
  /** An account exists, so there is nothing to set up. */
  complete: boolean
  /** This control plane will accept the setup form. */
  available: boolean
  /** Where on the host the one-time token was written. */
  tokenFile?: string
  /** "install" when there is no configuration yet: the page sets up everything. */
  mode?: 'install'
  /** The port this control plane is listening on, the form's default. */
  port?: number
  /** The host name the browser used to reach it. */
  hostname?: string
}

/** Where the control plane's PostgreSQL is. */
export interface DatabaseInput {
  host: string
  port: number
  name: string
  user: string
  password?: string
  sslMode: string
}

export interface DatabaseCheck {
  version: string
  /** The database already belongs to a deployment; its accounts are kept. */
  accountsExist: boolean
}

export interface InstallInput {
  token: string
  database: DatabaseInput
  consoleUrl: string
  fleetUrl: string
  port: number
  quic: boolean
  certPem: string
  keyPem: string
  admin: { email: string; displayName: string; password: string }
}

export interface InstallResult {
  consoleUrl: string
  configFile: string
  email?: string
}

/** Tests a database before install, guarded by the setup token. */
export async function checkInstallDatabase(token: string, database: DatabaseInput): Promise<DatabaseCheck> {
  return post<DatabaseCheck>('/auth/setup/database', { token, database })
}

/**
 * Writes the configuration of a control plane that has none, and makes the
 * first administrator. The control plane restarts on it straight after.
 */
export async function install(input: InstallInput): Promise<InstallResult> {
  return post<InstallResult>('/auth/setup/install', input)
}

export async function setupStatus(): Promise<SetupStatus> {
  const res = await fetch(`${BASE}/auth/setup`, { credentials: 'include' })
  const parsed = (await res.json()) as SetupStatus & { error?: string }
  if (!res.ok) throw new Error(parsed.error ?? `request failed (${res.status})`)
  return parsed
}

/**
 * Creates the first account, which is an administrator. Refused without the
 * token the control plane wrote on its host, and refused for good once any
 * account exists.
 */
export async function completeSetup(input: {
  token: string
  email: string
  displayName: string
  password: string
}): Promise<{ email: string; role: string }> {
  return post<{ email: string; role: string }>('/auth/setup', input)
}

export async function logout(): Promise<void> {
  if (!isConfigured()) return
  await fetch(`${BASE}/auth/logout`, { method: 'POST', credentials: 'include' })
}

/**
 * Requests a single-use ticket for one terminal session.
 *
 * The control plane decides whether this is allowed; the agent only verifies
 * the grant it is turned into. So a refusal here is the authoritative one, and its message is
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
 * The refusal here is the authoritative one, so its message is shown verbatim. "You need the auditor role"
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
 * Ends a session in flight.
 *
 * Two steps on purpose: the control plane authorises and records the intent,
 * then the replica relaying the session asks the server's agent to end it.
 * An attempt that fails is still in the audit log, which is the version of
 * events an investigator needs -- "tried to stop it and could not" is more
 * urgent than "stopped it".
 */
export async function terminateSession(
  sessionId: string,
  reason: string,
): Promise<{ ok: true } | { error: string }> {
  const minted = await terminateTicket(sessionId, reason)
  if ('error' in minted) return minted

  try {
    const res = await fetch(`${BASE}/api/v1/sessions/${encodeURIComponent(sessionId)}/terminate`, {
      method: 'POST',
      credentials: 'include',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${minted.ticket}`,
      },
      body: JSON.stringify({ reason }),
    })
    if (!res.ok) {
      let msg = ''
      try {
        msg = ((await res.json()) as { error?: string }).error ?? ''
      } catch {
        // Not JSON; the status says enough.
      }
      return { error: msg || `the control plane refused (${res.status})` }
    }
    return { ok: true }
  } catch (err) {
    return { error: err instanceof Error ? err.message : 'the control plane is unreachable' }
  }
}

/**
 * What came back when the console asked for a session's recording.
 *
 * `unavailable` carries the control plane's own reason so the page can repeat
 * it. A recording that is sealed but still on the gateway that produced it is
 * a different situation from a storage outage, and the operator needs to know
 * which -- one is waiting on a retry, the other on somebody.
 */
/** Entries per request. The control plane refuses more than 2000. */
export const AUDIT_PAGE = 2000

/**
 * The most entries the console will hold.
 *
 * A bound, not a target. The page verifies the chain and exports it as
 * evidence, so it should hold the whole log wherever that is reasonable -- but
 * a long-lived deployment's log is unbounded and a browser's memory is not.
 * Past this the pack is honestly marked incomplete, which is what the coverage
 * statement and `complete: false` exist for.
 */
export const AUDIT_CEILING = 20_000

/**
 * A slice of the audit log, and how big the log actually is.
 *
 * `total` is null when nothing can say so -- a control plane too old to send
 * the header. Null must read as "unknown", never as "this window is the whole
 * log", because the difference is whether a verdict covers the record.
 */
export interface AuditPage {
  events: ChainInput[]
  total: number | null
}

export type RecordingResult =
  | { kind: 'ok'; cast: string; verified: string }
  | { kind: 'unavailable'; reason: string }

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

/* ── The inventory, as an administrator edits it ─────────────────────────── */

/**
 * Adds a host to the inventory.
 *
 * The control plane validates and is the authority on what it will accept, so
 * its refusal is shown verbatim rather than replaced with something friendlier
 * — "the credential is a name inside the gateway's vault, not a path" is the
 * useful part, and no client-side message could have said it.
 */
export async function createAsset(input: AssetInput): Promise<Asset> {
  return post<Asset>('/api/v1/assets', input)
}

export async function updateAsset(id: string, input: AssetInput): Promise<Asset> {
  return send<Asset>('PATCH', `/api/v1/assets/${encodeURIComponent(id)}`, input)
}

/** Retires an asset. Its sessions and audit history are kept. */
export async function archiveAsset(id: string): Promise<void> {
  await send('DELETE', `/api/v1/assets/${encodeURIComponent(id)}`)
}

export async function assetAssignments(id: string): Promise<AssetAssignment[]> {
  if (!isConfigured()) return []
  return get<AssetAssignment[]>(`/api/v1/assets/${encodeURIComponent(id)}/assignments`)
}

/** Assigns a host to a person. An empty principal list removes the assignment. */
export async function setAssetAssignment(
  id: string,
  email: string,
  principals: string[],
): Promise<AssetAssignment[]> {
  return send<AssetAssignment[]>('PUT', `/api/v1/assets/${encodeURIComponent(id)}/assignments`, {
    email,
    principals,
  })
}

export async function removeAssetAssignment(id: string, email: string): Promise<void> {
  await send(
    'DELETE',
    `/api/v1/assets/${encodeURIComponent(id)}/assignments/${encodeURIComponent(email)}`,
  )
}

/**
 * The verbs POST does not cover.
 *
 * Same contract as post(): the body carries `error` on a refusal and it is
 * surfaced unchanged. A DELETE answers with a small JSON object rather than
 * 204, so there is always something to read.
 */
async function send<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    method,
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {}),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  })
  const parsed = (await res.json()) as T & { error?: string }
  if (!res.ok) throw new Error(parsed.error ?? `request failed (${res.status})`)
  return parsed
}

/* ── Teams ──────────────────────────────────────────────────────────────── */

export async function teams(): Promise<Team[]> {
  return get<Team[]>('/api/v1/teams')
}

export async function createTeam(name: string, members: string[]): Promise<Team> {
  return post<Team>('/api/v1/teams', { name, members })
}

export async function renameTeam(id: string, name: string): Promise<Team> {
  return send<Team>('PATCH', `/api/v1/teams/${encodeURIComponent(id)}`, { name })
}

export async function deleteTeam(id: string): Promise<void> {
  await send('DELETE', `/api/v1/teams/${encodeURIComponent(id)}`)
}

export async function addTeamMember(id: string, email: string): Promise<Team> {
  return send<Team>('PUT', `/api/v1/teams/${encodeURIComponent(id)}/members`, { email })
}

export async function removeTeamMember(id: string, email: string): Promise<Team> {
  return send<Team>(
    'DELETE',
    `/api/v1/teams/${encodeURIComponent(id)}/members/${encodeURIComponent(email)}`,
  )
}

export async function assetTeamAssignments(id: string): Promise<TeamAssignment[]> {
  if (!isConfigured()) return []
  return get<TeamAssignment[]>(`/api/v1/assets/${encodeURIComponent(id)}/team-assignments`)
}

/** Gives a team a server. An empty principal list takes it away. */
export async function setAssetTeamAssignment(
  id: string,
  teamId: string,
  principals: string[],
): Promise<TeamAssignment[]> {
  return send<TeamAssignment[]>(
    'PUT',
    `/api/v1/assets/${encodeURIComponent(id)}/team-assignments`,
    { teamId, principals },
  )
}

export async function removeAssetTeamAssignment(id: string, teamId: string): Promise<void> {
  await send(
    'DELETE',
    `/api/v1/assets/${encodeURIComponent(id)}/team-assignments/${encodeURIComponent(teamId)}`,
  )
}

/**
 * The one line an administrator pastes on a new Linux server. The script it
 * fetches installs the agent, links it and hooks sshd, in that order.
 */
export function installOneLiner(t: EnrollToken, version: string): string {
  const base = fleetURL()
  const q = new URLSearchParams({ token: t.token, version, url: base })
  const pin = t.curlPin ? `-k --pinnedpubkey '${t.curlPin}' ` : ''
  return `curl -fsSL ${pin}'${base}/api/v1/install/agent.sh?${q.toString()}' | sudo sh`
}

/** Approves or denies a direct SSH login held for approval. */
export async function decideDirectLogin(id: string, approve: boolean, note: string): Promise<DirectLogin> {
  return post<DirectLogin>(`/api/v1/direct-logins/${encodeURIComponent(id)}/decision`, { approve, note })
}

/** Sets a server's own direct SSH choice; null follows Settings. */
export async function setAssetDirectSsh(id: string, mode: DirectSsh | null): Promise<Asset> {
  return send<Asset>('PUT', `/api/v1/assets/${encodeURIComponent(id)}/direct-ssh`, { mode: mode ?? '' })
}

/** What an approval grants: the accounts allowed, and who may use them. */
export interface EnrolGrant {
  principals: string[]
  people: string[]
  /** Team ids. Everyone in each team gets the same accounts. */
  teams?: string[]
}

/**
 * Promotes a discovered host into a managed asset, with the login accounts and
 * people the approving admin picked. Both may be empty, which grants nothing.
 */
export async function enrolHost(
  hostname: string,
  grant: EnrolGrant = { principals: [], people: [] },
): Promise<{ assetId: string; assetHostname: string }> {
  const res = await post<{ assetId: string; assetHostname?: string }>(
    `/api/v1/discovered/${encodeURIComponent(hostname)}/enrol`,
    grant,
  )
  return { assetId: res.assetId, assetHostname: res.assetHostname ?? hostname }
}

/** A single-use token for one agent install. The secret is shown once. */
export interface EnrollToken {
  id: string
  token: string
  hostname?: string
  expiresAt: string
  /** sha256 pin of the control plane's key; absent behind a TLS proxy. */
  pin?: string
  /** The same pin in curl's --pinnedpubkey form. */
  curlPin?: string
}

/** Makes an enrollment token. Admin or owner; audited by the control plane. */
export async function createEnrollToken(hostname: string): Promise<EnrollToken> {
  return post<EnrollToken>('/api/v1/agents/enroll-tokens', hostname ? { hostname } : {})
}

/** An agent's outbound connection to the control plane, as of its last ping. */
export interface AgentTunnel {
  connected: boolean
  transport?: 'quic' | 'tcp'
  replica?: string
  connectedAt?: string
  seenAt?: string
  /** Last measured round trip through the tunnel. */
  rttMillis?: number
}

/** An enrolled agent's credential, without the secret. */
export interface EnrolledAgent {
  id: string
  hostname: string
  enrolledAt: string
  enrolledBy: string
  revokedAt?: string
  revokedBy?: string
  /** Absent from a control plane older than tunnels. */
  tunnel?: AgentTunnel
}

export async function enrolledAgents(): Promise<EnrolledAgent[]> {
  return get<EnrolledAgent[]>('/api/v1/agents')
}

/** Withdraws one agent's credential. Its past reports are untouched. */
export async function revokeAgent(id: string): Promise<void> {
  await post(`/api/v1/agents/${encodeURIComponent(id)}/revoke`, {})
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

/** A certificate, as the Settings page describes it. */
export interface CertSummary {
  subject: string
  issuer: string
  names: string[]
  notAfter: string
  pin: string
}

/**
 * The control plane's configuration, as GET /api/v1/settings returns it.
 *
 * Secrets are write-only: the *Set flags say whether one is configured, and
 * the value fields are sent only to replace one.
 */
export interface ControlSettings {
  configFile: string
  applyError?: string
  consoleUrl: string
  fleetUrl: string
  listen: string
  consoleListen: string
  quicListen: string
  behindTlsProxy: boolean
  allowedOrigins: string[]
  trustedProxies: string[]
  loginPerMinute: number
  failuresPerHour: number
  ticketsPerMinute: number
  sessionTtl: string
  ticketTtl: string
  agentStaleAfter: string
  logLevel: string
  database: {
    host: string; port: number; name: string; user: string; sslMode: string
    passwordSet: boolean; password?: string; raw?: boolean
  }
  storage: {
    enabled: boolean; endpoint: string; bucket: string; region: string; useSsl: boolean
    accessKey: string; secretKeySet: boolean; secretKey?: string
  }
  email: {
    enabled: boolean; host: string; port: number; username: string
    passwordSet: boolean; password?: string; from: string; to: string[]
  }
  telegram: { enabled: boolean; botTokenSet: boolean; botToken?: string; chatIds: string[] }
  fcm: {
    enabled: boolean; credentialsSet: boolean; credentials?: string
    projectId: string; topic: string; tokens: string[]
  }
  tls: {
    enabled: boolean; certificate?: CertSummary; managed: boolean; canIssue: boolean
    caCert?: string; mutualTls: boolean; certPem?: string; keyPem?: string; reissue?: boolean
  }
}

export async function controlSettings(): Promise<ControlSettings> {
  return get<ControlSettings>('/api/v1/settings')
}

/** Writes the whole configuration. The control plane reloads itself on it. */
export async function saveControlSettings(
  s: ControlSettings,
): Promise<{ changed: string[]; restarting: boolean }> {
  return send('PUT', '/api/v1/settings', s)
}

export async function testDatabase(d: ControlSettings['database']): Promise<DatabaseCheck> {
  return post<DatabaseCheck>('/api/v1/settings/test-database', d)
}

/**
 * Waits for the control plane to answer again after a reload. Resolves false
 * if it has not within the time allowed.
 */
export async function waitForControlPlane(timeoutMs = 30_000): Promise<boolean> {
  const until = Date.now() + timeoutMs
  // A first pause, so the answer is not from the process that is going away.
  await new Promise((r) => setTimeout(r, 1500))
  while (Date.now() < until) {
    try {
      const res = await fetch(`${BASE}/healthz`, { cache: 'no-store' })
      if (res.ok) return true
    } catch {
      // Still restarting.
    }
    await new Promise((r) => setTimeout(r, 1000))
  }
  return false
}

export async function createAccount(input: {
  email: string
  displayName: string
  role: string
  password: string
}): Promise<{ email: string; role: string }> {
  return post('/api/v1/users', input)
}

/** Changes someone's role. Not your own; only an owner makes an owner. */
export async function setAccountRole(email: string, role: string): Promise<{ email: string; role: string }> {
  return send('PUT', `/api/v1/users/${encodeURIComponent(email)}/role`, { role })
}

export async function setAccountDisabled(email: string, disabled: boolean): Promise<{ email: string }> {
  return post(`/api/v1/users/${encodeURIComponent(email)}/${disabled ? 'disable' : 'enable'}`, {})
}

export const live = {
  orFallback,

  stats: () => get<FleetStats>('/api/v1/stats'),
  assets: () => get<Asset[]>('/api/v1/assets'),

  /**
   * The accounts that exist, for deciding who to assign a host to.
   *
   * The Users page showed the fixture against a live deployment until this
   * existed: real names, real roles, none of them the ones you had actually
   * created.
   */
  users: () => get<User[]>('/api/v1/users'),

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
  /**
   * The most recent slice of the log, and how big the log actually is.
   *
   * The console verifies what it receives and reports "chain intact". Without
   * the total it was describing a window while sounding like it described the
   * log: 500 of the dev control plane's 4,546 entries, with the other 4,046
   * absent from the page, from the verdict and from the exported evidence
   * pack -- which is documented as "the whole chain".
   *
   * A hash chain checked from an arbitrary starting point proves the fragment
   * is internally consistent and nothing at all about what came before it.
   */
  async audit(): Promise<AuditPage> {
    const events: AuditEvent[] = []
    let total: number | null = null
    let before = 0

    // Paged until the log runs out or the ceiling is reached. Verifying the
    // newest 500 and calling the verdict the log's was the defect; fetching
    // one page and stopping would be the same defect with better wording.
    for (;;) {
      const qs = `limit=${AUDIT_PAGE}${before > 0 ? `&before=${before}` : ''}`
      const res = await fetch(`${BASE}/api/v1/audit?${qs}`, {
        headers: { Authorization: `Bearer ${TOKEN}` },
      })
      if (!res.ok) throw new Error(`audit: ${res.status}`)

      // Only from the first response. It counts the whole log, and re-reading
      // it per page would let a mid-fetch write change the denominator.
      if (total === null) {
        const header = res.headers.get('X-Argus-Audit-Total')
        const n = header === null ? NaN : Number(header)
        // A control plane too old to send it, or a number that is not one, has
        // to read as "unknown" rather than as "what we have is everything".
        total = Number.isFinite(n) ? n : null
      }

      const batch = (await res.json()) as AuditEvent[]
      events.push(...batch)
      // Short page means the log ended. Asking again would return nothing and
      // cost a round trip to learn it.
      if (batch.length < AUDIT_PAGE) break
      if (events.length >= AUDIT_CEILING) break
      // The lowest seq we hold. Paging on seq rather than an offset because the
      // log grows at the head while this runs, and an offset would shift under
      // us -- which on a hash chain means skipping a link.
      before = batch[batch.length - 1]!.seq
    }

    return { events, total }
  },

  requests: (state?: string) =>
    get<AccessRequest[]>(`/api/v1/requests${state ? `?state=${state}` : ''}`),

  directLogins: () => get<DirectLogin[]>('/api/v1/direct-logins'),

  /**
   * Fetches a session's recording.
   *
   * The control plane re-verifies the hash chain before serving and reports the
   * verdict in X-Argus-Chain-Verified. The console shows that verdict rather
   * than assuming intact, because a recording that fails verification is the
   * single most important thing an auditor can be told about it.
   */
  /**
   * `null` means *no control plane is configured* and nothing else.
   *
   * That distinction is the whole point of the return type. This used to
   * answer `null` for every failure, and the caller could not tell "there is
   * no server to ask" from "the server answered and said the artefact is not
   * there" -- so it fell back to a generated cast in both cases and replayed a
   * scripted fiction as if it were the session. 18 sessions in the dev control
   * plane are in exactly that state: sealed, with real recorded bytes, and an
   * artefact still sitting on the gateway that produced it.
   *
   * The control plane's own wording is carried through rather than replaced.
   * It knows which of several things went wrong and says so precisely.
   */
  async recording(id: string): Promise<RecordingResult | null> {
    if (!isConfigured()) return null
    let res: Response
    try {
      res = await fetch(`${BASE}/api/v1/sessions/${id}/recording`, {
        headers: { Authorization: `Bearer ${TOKEN}` },
      })
    } catch {
      return { kind: 'unavailable', reason: 'The control plane could not be reached.' }
    }
    if (!res.ok) {
      const body = await res.text().catch(() => '')
      let reason = ''
      try {
        reason = (JSON.parse(body) as { error?: string }).error ?? ''
      } catch {
        reason = body.slice(0, 200)
      }
      return {
        kind: 'unavailable',
        reason: reason || `The control plane answered ${res.status}.`,
      }
    }
    return {
      kind: 'ok',
      cast: await res.text(),
      verified: res.headers.get('X-Argus-Chain-Verified') ?? 'unverified',
    }
  },
}
