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
 */
export type Reach = 'online' | 'slow' | 'offline' | 'blocked'

export function reachOf(a: Asset): Reach {
  if (a.protocol !== 'rdp' && a.hostKeyState === 'changed') return 'blocked'
  // A connected agent tunnel is how every session opens, terminal or desktop,
  // so it counts as reachable on its own.
  if (a.tunnelConnected === true) return 'online'
  if (a.health === 'reachable') return 'online'
  if (a.health === 'degraded') return 'slow'
  return 'offline'
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

/** Whether this is an Android device driven through the Argus app on it. */
export function isDevice(a: Asset | undefined): boolean {
  return a?.kind === 'android'
}

/** The part of the hostname people say out loud. */
export function shortName(hostname: string): string {
  return hostname.split('.')[0] ?? hostname
}

