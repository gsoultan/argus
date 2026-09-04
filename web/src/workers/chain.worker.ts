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
  | { type: 'built'; links: ChainLink[]; head: string; ms: number }
  | { type: 'verified'; ok: boolean; checked: number; brokenAt: number | null; ms: number }
  | { type: 'error'; message: string }

export const GENESIS = '0'.repeat(64)

export function canonical(e: ChainInput): string {
  // Explicit key order — do not rely on object literal insertion order.
  return JSON.stringify([
    e.action, e.actorEmail, e.at, e.detail, e.id, e.seq, e.severity, e.target,
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
    let prev = GENESIS

    for (let i = 0; i < ordered.length; i++) {
      const e = ordered[i]!
      const hash = link(prev, e)
      links.push({ ...e, prevHash: prev, hash })
      prev = hash
      if (i > 0 && i % CHUNK === 0) {
        post({ type: 'progress', done: i, total: ordered.length })
        await breathe()
      }
    }

    post({
      type: 'built',
      links: links.reverse(), // back to newest-first for display
      head: prev,
      ms: Math.round(performance.now() - started),
    })
    return
  }

  if (msg.type === 'verify') {
    const ordered = [...msg.links].sort((a, b) => a.seq - b.seq)
    let prev = GENESIS
    let brokenAt: number | null = null

    for (let i = 0; i < ordered.length; i++) {
      const l = ordered[i]!
      const expected = link(prev, l)
      if (l.prevHash !== prev || l.hash !== expected) {
        brokenAt = l.seq
        break
      }
      prev = l.hash
      if (i > 0 && i % CHUNK === 0) {
        post({ type: 'progress', done: i, total: ordered.length })
        await breathe()
      }
    }

    post({
      type: 'verified',
      ok: brokenAt === null,
      checked: ordered.length,
      brokenAt,
      ms: Math.round(performance.now() - started),
    })
  }
}
