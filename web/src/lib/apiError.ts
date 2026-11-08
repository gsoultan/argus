/**
 * Plain-language errors for the console, and the one place a 401 is noticed.
 *
 * Two problems lived here before. A refused request surfaced as
 * "/api/v1/databases → 401" -- a path and a number, which tells the person in
 * front of the screen nothing about what happened or what to do. And a session
 * that expired while a page was open left that page drawing those errors,
 * because nothing told the sign-in gate the session was gone; reloading was the
 * only way back to the sign-in form.
 *
 * Kept free of imports so live.ts, the gate and the error screen can all use
 * it without a cycle.
 */

/** What a status means, said to the person rather than to the network. */
export function describeStatus(status: number): string {
  switch (status) {
    case 400:
    case 422:
      return 'The request was not accepted. Check what you entered and try again.'
    case 401:
      return 'Your session has ended. Sign in again to continue.'
    case 403:
      return 'You do not have permission to do this. Ask an administrator if you need access.'
    case 404:
      return 'This could not be found. It may have been removed or renamed.'
    case 408:
    case 504:
      return 'The server took too long to answer. Try again in a moment.'
    case 409:
      return 'This changed while you were working on it. Reload and try again.'
    case 413:
      return 'That is too large to send.'
    case 429:
      return 'Too many requests at once. Wait a moment and try again.'
    case 502:
    case 503:
      return 'The control plane is not available right now. Try again in a moment.'
    default:
      if (status >= 500) return 'Something went wrong on the server. Try again, and if it keeps happening, check the control plane logs.'
      return `The request could not be completed (error ${status}).`
  }
}

/**
 * Server replies that are a status name rather than an explanation. They add
 * nothing to the status, so the plain description is shown in their place.
 */
const TERSE = new Set([
  'unauthorized', 'unauthorised', 'forbidden', 'not found', 'bad request',
  'internal server error', 'internal error', 'method not allowed',
  'service unavailable', 'bad gateway', 'gateway timeout', 'too many requests',
  'conflict', 'request failed',
])

/**
 * The message to show for a refused request.
 *
 * The control plane's own words win when it gave real ones: "your current
 * password is not correct" or "you need the auditor role" is what the person
 * needs, and replacing it with something generic would lose it. A bare status
 * name, or nothing, gets the plain description of the status instead.
 */
export function humanizeError(status: number, serverMessage?: string | null): string {
  const said = (serverMessage ?? '').trim()
  if (said === '' || TERSE.has(said.toLowerCase().replace(/[.!]+$/, ''))) {
    return describeStatus(status)
  }
  const sentence = said.charAt(0).toUpperCase() + said.slice(1)
  return /[.!?]$/.test(sentence) ? sentence : `${sentence}.`
}

/**
 * Any thrown value as text for the screen.
 *
 * A fetch that never got an answer throws a TypeError whose message depends on
 * the browser -- "Failed to fetch", "Load failed", "NetworkError when
 * attempting to fetch resource" -- none of which says what to check.
 */
export function errorText(error: unknown, fallback = 'Something went wrong.'): string {
  if (error instanceof TypeError && /fetch|load failed|network/i.test(error.message)) {
    return 'The control plane could not be reached. Check your connection and try again.'
  }
  if (error instanceof Error) return error.message || fallback
  if (typeof error === 'string' && error.trim() !== '') return error
  return fallback
}

/** No answer came back at all: the control plane was not reached. */
export class NetworkError extends Error {
  constructor(cause?: unknown) {
    super('The control plane could not be reached. Check your connection and try again.', { cause })
    this.name = 'NetworkError'
  }
}

/* ── Session expiry ───────────────────────────────────────────────────────── */

type Listener = () => void
const listeners = new Set<Listener>()

/**
 * Subscribes to "a request was refused as not signed in". Returns the
 * unsubscribe function, so it drops straight into a useEffect.
 */
export function onSessionExpired(listener: Listener): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

/**
 * Called by the API client with every response it receives.
 *
 * Only a 401 counts, and not from /auth/*: there a 401 is an answer -- a wrong
 * password, a wrong code, "nobody is signed in yet" from /auth/me -- not news
 * that a session ended. Even outside /auth a 401 is only a hint; the listener
 * asks /auth/me before showing the sign-in form, so a 401 that means something
 * else cannot sign anyone out.
 */
export function noteResponse(url: string, status: number): void {
  if (status !== 401) return
  if (/\/auth\//.test(url)) return
  for (const listener of [...listeners]) listener()
}
