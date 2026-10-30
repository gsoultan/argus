/**
 * Decodes a slice of a recorded Remote Desktop session.
 *
 * The whole recording stays here as one ArrayBuffer — compact, because it is
 * the protocol stream rather than pixels — and only what changes inside the
 * requested time window is turned into ImageBitmaps. Decoding the whole
 * session up front would be simpler and would hold a session's worth of
 * decoded pixels in memory, which for a long recording is gigabytes.
 *
 * Seeking backwards therefore asks for [0, t] and replays; seeking forwards
 * asks for [now, t]. Frames are incremental, so there is no way to jump into
 * the middle of one without having drawn what came before. lib/rdpReplay.ts
 * does the drawing.
 */

import { ReplayPlayer, type ReplayRect } from '~/lib/rdpReplay'

export type { ReplayRect }

export type ReplayRequest =
  | { type: 'load'; buffer: ArrayBuffer; width: number; height: number }
  | { type: 'window'; id: number; fromMs: number; toMs: number }

export type ReplayResponse =
  | { type: 'loaded'; frames: number; durationMs: number; graphics: boolean }
  | { type: 'rects'; id: number; rects: ReplayRect[]; toMs: number }
  | { type: 'error'; message: string }

let player: ReplayPlayer | null = null

/**
 * Requests are handled strictly in order. A graphics session's windows feed
 * one decoder, and an async onmessage would let a second window start before
 * the first had finished feeding it.
 */
let queue: Promise<void> = Promise.resolve()

self.onmessage = (ev: MessageEvent<ReplayRequest>) => {
  const msg = ev.data
  queue = queue.then(() => handle(msg))
}

async function handle(msg: ReplayRequest) {
  if (msg.type === 'load') {
    player?.close()
    // Indexed once. Without it every seek would walk the whole buffer parsing
    // headers, which is the difference between a scrub that tracks the pointer
    // and one that stutters.
    player = new ReplayPlayer(msg.buffer, msg.width, msg.height)
    post({ type: 'loaded', frames: player.entries.length, durationMs: player.durationMs, graphics: player.graphics })
    return
  }

  if (msg.type === 'window') {
    if (!player) {
      post({ type: 'error', message: 'no recording loaded' })
      return
    }
    let rects: ReplayRect[]
    try {
      rects = await player.window(msg.fromMs, msg.toMs)
    } catch (err) {
      post({ type: 'error', message: err instanceof Error ? err.message : String(err) })
      return
    }
    post({ type: 'rects', id: msg.id, rects, toMs: msg.toMs }, rects.map((r) => r.bitmap))
  }
}

function post(msg: ReplayResponse, transfer: Transferable[] = []) {
  ;(self as unknown as Worker).postMessage(msg, transfer)
}
