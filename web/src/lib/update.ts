/**
 * Whether a new build can be put in place right now without costing anyone
 * anything.
 *
 * Waiting for a click was safe and, in practice, meant nobody upgraded: a
 * waiting service worker only takes over once every Argus tab is closed, so a
 * refresh served the old console again from its cache, and the notice offering
 * the new one was easy to close or never see. People upgraded the control plane
 * and kept looking at the previous release.
 *
 * So the new build goes in at once, unless something open would be lost: a
 * terminal or desktop (Connect), a session being watched, or a dialog
 * someone may be typing into. Then it waits for them to leave.
 */
export function busyWith(pathname: string, dialogOpen: boolean): boolean {
  if (dialogOpen) return true
  if (pathname === '/connect' || pathname.startsWith('/connect/')) return true
  return /^\/sessions\/[^/]+\/?$/.test(pathname)
}
