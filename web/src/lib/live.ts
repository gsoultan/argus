import type { AccessRequest, Asset, AuditEvent, FleetStats, Session } from '~/types/domain'

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

export function isConfigured(): boolean {
  return RAW_BASE !== ''
}

export interface Identity {
  authenticated: boolean
  email?: string
  displayName?: string
  role?: string
  loginUrl?: string
  oidcEnabled?: boolean
}

/** Who am I, and if nobody, where do I go to sign in. */
export async function whoami(): Promise<Identity> {
  if (!isConfigured()) return { authenticated: false }
  try {
    const res = await fetch(`${BASE}/auth/me`, {
      credentials: 'include',
      headers: TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {},
    })
    return (await res.json()) as Identity
  } catch {
    return { authenticated: false }
  }
}

export function loginURL(returnTo = window.location.pathname): string {
  return `${BASE}/auth/login?return_to=${encodeURIComponent(returnTo)}`
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

/** Set once the first request succeeds, so the UI can label its data source. */
let reachable: boolean | null = null

export function isLive(): boolean {
  return isConfigured() && reachable === true
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    // Session cookie is the real credential; the bearer token is a development
    // fallback the control plane ignores once OIDC is configured.
    credentials: 'include',
    headers: TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {},
  })
  if (!res.ok) {
    reachable = false
    throw new Error(`${path} → ${res.status}`)
  }
  reachable = true
  return (await res.json()) as T
}

/** Wraps a live call so a control-plane outage falls back instead of erroring. */
async function orFallback<T>(live: () => Promise<T>, fallback: () => Promise<T>): Promise<T> {
  if (!isConfigured()) return fallback()
  try {
    return await live()
  } catch {
    return fallback()
  }
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
