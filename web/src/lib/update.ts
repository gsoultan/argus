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

/*
 * Noticing that the control plane now serves a different console.
 *
 * The service worker only looks for a new build when a page is loaded, and
 * this console is a single page: once open it is never loaded again. A tab left
 * open across an update kept the old build for as long as it stayed open, and
 * with the worker's cached index.html a refresh brought the old build back too.
 * Its route chunks were gone from the server by then, so the first page it had
 * not opened before failed to load.
 *
 * Every build's index.html names its own entry script, assets/index-<hash>.js,
 * so comparing the one this page runs with the one the server would hand out
 * now says whether there is a new build, without the release having to stamp
 * a version into the bundle.
 */

const ENTRY = /\/assets\/index-[\w-]+\.js/

/** The entry script this page was built with, or '' under the dev server. */
export function runningEntry(doc: Document = document): string {
  for (const s of Array.from(doc.querySelectorAll<HTMLScriptElement>('script[type="module"][src]'))) {
    const m = ENTRY.exec(s.getAttribute('src') ?? '')
    if (m) return m[0]
  }
  return ''
}

/** The entry script an index.html names, or '' when it names none. */
export function entryOf(html: string): string {
  return ENTRY.exec(html)?.[0] ?? ''
}

/**
 * Whether the server's console is a different build from this one. False when
 * either side is unknown (the dev server, a failed request): a guess here
 * reloads someone's page for nothing.
 */
export function isNewBuild(running: string, served: string): boolean {
  return running !== '' && served !== '' && running !== served
}

/**
 * The entry script the server hands out now. The query string keeps the
 * service worker's precache from answering with the index.html it holds, which
 * is the very copy being checked; no-store keeps the HTTP cache out of it too.
 */
export async function servedEntry(fetchImpl: typeof fetch = fetch): Promise<string> {
  try {
    const res = await fetchImpl(`/index.html?build-check=${Date.now()}`, {
      cache: 'no-store', credentials: 'same-origin',
    })
    if (!res.ok) return ''
    return entryOf(await res.text())
  } catch {
    // Offline, or the control plane is restarting: ask again next time.
    return ''
  }
}

/** Fired to ask the console to look for a new build now, not at the next minute. */
export const CHECK_BUILD_EVENT = 'argus:check-build'

export function checkForNewBuild(): void {
  window.dispatchEvent(new Event(CHECK_BUILD_EVENT))
}
