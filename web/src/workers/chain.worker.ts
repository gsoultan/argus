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
 */

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

const GENESIS = '0'.repeat(64)

function canonical(e: ChainInput): string {
  // Explicit key order — do not rely on object literal insertion order.
  return JSON.stringify([
    e.action, e.actorEmail, e.at, e.detail, e.id, e.seq, e.severity, e.target,
  ])
}

const enc = new TextEncoder()

function toHex(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf)
  let s = ''
  for (let i = 0; i < bytes.length; i++) s += bytes[i]!.toString(16).padStart(2, '0')
  return s
}

async function link(prevHash: string, e: ChainInput): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', enc.encode(prevHash + canonical(e)))
  return toHex(digest)
}

/** Yield to the event loop periodically so progress messages actually flush. */
const CHUNK = 250

self.onmessage = async (ev: MessageEvent<ChainRequest>) => {
  const started = performance.now()
  const msg = ev.data

  if (msg.type === 'build') {
    // Events arrive newest-first; the chain is built oldest-first.
    const ordered = [...msg.events].sort((a, b) => a.seq - b.seq)
    const links: ChainLink[] = []
    let prev = GENESIS

    for (let i = 0; i < ordered.length; i++) {
      const e = ordered[i]!
      const hash = await link(prev, e)
      links.push({ ...e, prevHash: prev, hash })
      prev = hash
      if (i % CHUNK === 0) {
        ;(self as unknown as Worker).postMessage({
          type: 'progress', done: i, total: ordered.length,
        } satisfies ChainResponse)
      }
    }

    ;(self as unknown as Worker).postMessage({
      type: 'built',
      links: links.reverse(), // back to newest-first for display
      head: prev,
      ms: Math.round(performance.now() - started),
    } satisfies ChainResponse)
    return
  }

  if (msg.type === 'verify') {
    const ordered = [...msg.links].sort((a, b) => a.seq - b.seq)
    let prev = GENESIS
    let brokenAt: number | null = null

    for (let i = 0; i < ordered.length; i++) {
      const l = ordered[i]!
      const expected = await link(prev, l)
      if (l.prevHash !== prev || l.hash !== expected) {
        brokenAt = l.seq
        break
      }
      prev = l.hash
      if (i % CHUNK === 0) {
        ;(self as unknown as Worker).postMessage({
          type: 'progress', done: i, total: ordered.length,
        } satisfies ChainResponse)
      }
    }

    ;(self as unknown as Worker).postMessage({
      type: 'verified',
      ok: brokenAt === null,
      checked: ordered.length,
      brokenAt,
      ms: Math.round(performance.now() - started),
    } satisfies ChainResponse)
  }
}
