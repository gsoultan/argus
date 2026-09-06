/// <reference lib="webworker" />
/**
 * Audit chain verification worker.
 *
 * Recomputes `hash = SHA-256(prevHash || canonicalJSON(payload))` over the full
 * event log and reports the first index where the recomputed value diverges
 * from the served one. This runs off the main thread because verifying tens of
 * thousands of links is genuinely CPU-bound, and because a janky progress bar
 * during an audit demo is not a good look.
 *
 * Canonicalisation must match the Go control plane byte-for-byte: keys sorted
 * ascending, no whitespace, UTF-8.
 *
 * ## Why WASM here, and only here
 *
 * This used to call `crypto.subtle.digest`, which is native and fast at hashing
 * — but its API is asynchronous, and the chain is inherently serial: link N
 * cannot be hashed until link N−1 is known. So verification was one `await`,
 * one Promise and one fresh ArrayBuffer *per link*, for inputs of roughly a
 * hundred bytes. At that size the scheduling costs an order of magnitude more
 * than the hashing.
 *
 * hash-wasm exposes a reusable hasher that runs synchronously, so the whole
 * chain is a tight loop with no allocation per link and no trip through the
 * microtask queue. Nothing else in this console is a better WASM candidate:
 * decoding recordings is dominated by `JSON.parse`, which is already native and
 * which WASM would only slow down.
 */

import { createSHA256 } from 'hash-wasm'

export interface ChainInput {
  seq: number
  id: string
  at: string
  action: string
  severity: string
  actorEmail: string
  target: string
  detail: string
  /**
   * What the control plane recorded, when it served a real log.
   *
   * Absent for the in-memory fixture, which has no server to distrust.
   */
  prevHash?: string
  hash?: string
}

export interface ChainLink extends ChainInput {
  prevHash: string
  hash: string
}

export type ChainRequest =
  | { type: 'build'; events: ChainInput[] }
  | { type: 'verify'; links: ChainLink[] }

export type ChainResponse =
  | { type: 'progress'; done: number; total: number }
  | {
      type: 'built'
      links: ChainLink[]
      head: string
      ms: number
      /**
       * True when the events carried the control plane's own hashes, so
       * verification checks the served record rather than this worker's
       * arithmetic. False for the fixture, and the console says which.
       */
      againstServer: boolean
    }
  | { type: 'verified'; ok: boolean; checked: number; brokenAt: number | null; ms: number }
  | { type: 'error'; message: string }

export const GENESIS = '0'.repeat(64)

/**
 * The exact bytes the Go control plane hashes.
 *
 * `chainHash` in internal/control/store.go marshals this array, in this order,
 * and hashes prevHash concatenated with it. Six fields: seq and id are
 * deliberately absent, because they are assigned by the database and are not
 * part of what the event asserts.
 *
 * This used to include seq and id, which meant the console could never
 * reproduce a digest the server had written -- so it silently recomputed the
 * whole chain from content instead of checking anything, and reported "intact"
 * for a log it had not actually verified. `chainCanonicalMatchesGo` in
 * lib/__tests__/chain.test.ts pins this against digests produced by Go.
 *
 * `at` is used verbatim. The control plane normalises it to UTC before
 * serialising precisely so the string here is the string it hashed.
 */
export function canonical(e: ChainInput): string {
  return JSON.stringify([
    e.action, e.actorEmail, e.at, e.detail, e.severity, e.target,
  ])
}

/**
 * One hasher, reused for every link.
 *
 * Instantiating the WASM module is the only asynchronous part; `init()` resets
 * the same instance, so hashing 50,000 links allocates nothing per link.
 */
const hasher = createSHA256()

/** Yield to the event loop periodically so progress messages actually flush. */
const CHUNK = 2_000

/** Digest of one link, given the hasher and its predecessor. */
export type Hasher = { init(): Hasher; update(s: string): Hasher; digest(t: 'hex'): string }

export function linkHash(sha: Hasher, prevHash: string, e: ChainInput): string {
  return sha.init().update(prevHash + canonical(e)).digest('hex')
}

/**
 * Walks a chain oldest-first and reports the first divergence.
 *
 * Exported and pure so it can be tested against a log that carries a control
 * plane's hashes -- the case the console exists for, and the one it silently
 * failed to check when its canonical form did not match the server's.
 *
 * Mirrors VerifyAuditChain in internal/control/audit_verify.go, including the
 * distinction between the two failures: a wrong prevHash means a record was
 * removed, inserted or reordered; a wrong hash means one was edited in place.
 */
export function verifyChain(
  sha: Hasher,
  links: ChainLink[],
): { ok: boolean; checked: number; brokenAt: number | null } {
  const ordered = [...links].sort((a, b) => a.seq - b.seq)
  let prev = GENESIS
  let checked = 0
  for (const l of ordered) {
    if (l.prevHash !== prev || l.hash !== linkHash(sha, l.prevHash, l)) {
      return { ok: false, checked, brokenAt: l.seq }
    }
    prev = l.hash
    checked++
  }
  return { ok: true, checked, brokenAt: null }
}

const post = (m: ChainResponse) => (self as unknown as Worker).postMessage(m)

/** Lets a queued progress message reach the main thread mid-loop. */
const breathe = () => new Promise<void>((r) => setTimeout(r, 0))

self.onmessage = async (ev: MessageEvent<ChainRequest>) => {
  const started = performance.now()
  const msg = ev.data

  let sha256: Awaited<typeof hasher>
  try {
    sha256 = await hasher
  } catch (err) {
    post({
      type: 'error',
      message: err instanceof Error ? err.message : 'SHA-256 could not be initialised',
    })
    return
  }

  const link = (prevHash: string, e: ChainInput): string =>
    sha256.init().update(prevHash + canonical(e)).digest('hex')

  if (msg.type === 'build') {
    // Events arrive newest-first; the chain is built oldest-first.
    const ordered = [...msg.events].sort((a, b) => a.seq - b.seq)
    const links: ChainLink[] = []
    // A served log carries the control plane's hashes; the fixture does not.
    const againstServer = ordered.some((e) => Boolean(e.hash))
    let prev = GENESIS

    for (let i = 0; i < ordered.length; i++) {
      const e = ordered[i]!
      const computed = link(e.prevHash ?? prev, e)
      // The server's values are kept when it supplied them, so `verify` checks
      // the record that was served rather than this worker's own arithmetic --
      // recomputing both sides of a comparison proves nothing about the log.
      // Keeping them also means one edited row is reported where it happened
      // instead of every row after it reading as broken.
      links.push({
        ...e,
        prevHash: e.prevHash ?? prev,
        hash: e.hash ?? computed,
      })
      prev = e.hash ?? computed
      if (i > 0 && i % CHUNK === 0) {
        post({ type: 'progress', done: i, total: ordered.length })
        await breathe()
      }
    }

    post({
      type: 'built',
      links: links.reverse(), // back to newest-first for display
      head: prev,
      againstServer,
      ms: Math.round(performance.now() - started),
    })
    return
  }

  if (msg.type === 'verify') {
    const r = verifyChain(sha256 as unknown as Hasher, msg.links)
    post({
      type: 'verified',
      ok: r.ok,
      checked: r.ok ? r.checked : msg.links.length,
      brokenAt: r.brokenAt,
      ms: Math.round(performance.now() - started),
    })
  }
}
