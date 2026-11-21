import type { Asset } from '~/types/domain'

/**
 * A server's status and names, in a module of their own: Home's fleet map
 * needs them, and importing them from myservers pulled the whole server-card
 * module into the first visit.
 */

/**
 * Whether a server is worth trying, in words someone without the vocabulary can
 * act on.
 *
 * Built from what the console was last told, never from a probe, so every label
 * is a statement about the last check rather than a promise about the next
 * connect -- the card says when that check was. Only `blocked` stops the
 * Connect button, because it is the one case Argus itself refuses: SSH to a
 * server whose host key no longer matches the pin.
 *
 * Online needs a live signal from the host: its agent's tunnel, or a heartbeat
 * the control plane has not yet marked stale. `health` alone is not one: it is
 * written as "reachable" when an asset is added and nothing probes it after,
 * so trusting it showed every server online, including ones switched off. A
 * host with no agent has nothing that reports on it, so it is `unknown`
 * rather than a guess either way.
 */
export type Reach = 'online' | 'slow' | 'offline' | 'blocked' | 'unknown'

export function reachOf(a: Asset): Reach {
  if (a.protocol !== 'rdp' && a.hostKeyState === 'changed') return 'blocked'
  // A connected agent tunnel is how every session opens, terminal or desktop,
  // so it counts as reachable on its own.
  if (a.tunnelConnected === true) return 'online'
  if (a.health === 'unreachable') return 'offline'
  if (a.agentState === 'healthy') return a.health === 'degraded' ? 'slow' : 'online'
  if (a.agentState === 'stale') return 'offline'
  return 'unknown'
}

/** Whether a reach counts toward "Online" in the filters and counts. */
export function isUp(r: Reach): boolean {
  return r === 'online' || r === 'slow'
}

/**
 * The one set of words for whether a server answers, the same on Home, the
 * Assets list and a server's Overview: Online (online or slow), Offline (its
 * agent went silent, it was found unreachable, or no agent ever reported
 * from it) and Blocked (its identity changed).
 *
 * There used to be a fourth, "No agent", for a server no agent ever reported
 * from. Every asset added from the console installs its agent first, so the
 * word only ever met people as a puzzle: it is Offline now, and the reason is
 * said beside it (see offlineReason). Home's counts come from the control
 * plane by the same rules; see internal/control/home_snapshot.go.
 */
export type Answer = 'online' | 'down' | 'blocked'

export const ANSWERS: readonly Answer[] = ['online', 'down', 'blocked']

export function answerOf(a: Asset): Answer {
  return ({ online: 'online', slow: 'online', offline: 'down', unknown: 'down', blocked: 'blocked' } as const)[reachOf(a)]
}

/** ?status= as a link writes it; the retired "noagent" is Offline now. */
export function parseAnswer(v: unknown): Answer | undefined {
  if (v === 'noagent') return 'down'
  return typeof v === 'string' && (ANSWERS as readonly string[]).includes(v) ? (v as Answer) : undefined
}

export const ANSWER_LABEL: Record<Answer, string> = {
  online: 'Online',
  down: 'Offline',
  blocked: 'Blocked',
}

/**
 * Why an asset is offline, in a few words: how long since its agent was last
 * heard from, or that no agent ever reported from it. Undefined when it is
 * not offline.
 */
export function offlineReason(a: Asset, rel: (iso: string) => string): string | undefined {
  const r = reachOf(a)
  if (r === 'unknown') return 'Agent never reported'
  if (r !== 'offline') return undefined
  return a.agentLastSeenAt ? `Last seen ${rel(a.agentLastSeenAt)}` : 'Not reachable'
}

/**
 * "Android", "Windows" or "Linux", which is the distinction a person picking
 * a server makes. A device says so in its kind; a server is told apart by
 * how it opens and what it reports.
 */
export function kindOf(a: Asset): 'windows' | 'linux' | 'android' {
  if (a.kind === 'android') return 'android'
  return a.protocol === 'rdp' || /windows/i.test(a.os) ? 'windows' : 'linux'
}

/**
 * What a host that checked in and is not yet added is, from the OS its agent
 * reports: the Argus app says "Android …", so finishing adding it offers a
 * device's choices rather than a server's.
 */
export function kindOfHost(h: { os?: string }): 'android' | undefined {
  return /android/i.test(h.os ?? '') ? 'android' : undefined
}

/** Whether this is an Android device driven through the Argus app on it. */
export function isDevice(a: Asset | undefined): boolean {
  return a?.kind === 'android'
}

/** The part of the hostname people say out loud. */
export function shortName(hostname: string): string {
  return hostname.split('.')[0] ?? hostname
}

