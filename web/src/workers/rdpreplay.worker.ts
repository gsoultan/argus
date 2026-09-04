/**
 * Decodes a slice of a recorded Remote Desktop session.
 *
 * The whole recording stays here as one ArrayBuffer — compact, because it is
 * the protocol stream rather than pixels — and only the rectangles inside the
 * requested time window are turned into ImageBitmaps. Decoding the whole
 * session up front would be simpler and would hold a session's worth of
 * decoded pixels in memory, which for a long recording is gigabytes.
 *
 * Seeking backwards therefore asks for [0, t] and replays; seeking forwards
 * asks for [now, t]. Rectangles are incremental, so there is no way to jump
 * into the middle of one without having drawn what came before.
 */

const FRAME_BITMAP = 1
const REPLAY_HEADER = 4
const HEADER = 12

export interface ReplayRect {
  ms: number
  x: number
  y: number
  bitmap: ImageBitmap
}

export type ReplayRequest =
  | { type: 'load'; buffer: ArrayBuffer }
  | { type: 'window'; id: number; fromMs: number; toMs: number }

export type ReplayResponse =
  | { type: 'loaded'; frames: number; durationMs: number }
  | { type: 'rects'; id: number; rects: ReplayRect[]; toMs: number }
  | { type: 'error'; message: string }

/**
 * First index whose timestamp is >= `ms`.
 *
 * The window handler used to walk the whole index on every request, which the
 * player issued once per animation frame — so scrubbing a long recording cost
 * O(frames) sixty times a second, and the comment above claiming this was "a
 * slice not a scan" described an intention rather than the code.
 */
function lowerBound(entries: Array<{ at: number; ms: number }>, ms: number): number {
  let lo = 0
  let hi = entries.length
  while (lo < hi) {
    const mid = (lo + hi) >> 1
    if (entries[mid]!.ms < ms) lo = mid + 1
    else hi = mid
  }
  return lo
}

let buffer: ArrayBuffer | null = null
/** Byte offset and timestamp of every frame, so a window is a slice not a scan. */
let index: Array<{ at: number; ms: number }> = []

self.onmessage = async (ev: MessageEvent<ReplayRequest>) => {
  const msg = ev.data
  if (msg.type === 'load') {
    buffer = msg.buffer
    index = []
    const view = new DataView(buffer)
    let at = 0
    let durationMs = 0
    // Indexed once. Without it every seek would walk the whole buffer parsing
    // headers, which is the difference between a scrub that tracks the pointer
    // and one that stutters.
    while (at + REPLAY_HEADER + HEADER <= buffer.byteLength) {
      const ms = view.getUint32(at, true)
      const kind = view.getUint8(at + REPLAY_HEADER)
      const w = view.getUint16(at + REPLAY_HEADER + 6, true)
      const h = view.getUint16(at + REPLAY_HEADER + 8, true)
      if (kind !== FRAME_BITMAP) break
      const size = REPLAY_HEADER + HEADER + w * h * 4
      if (at + size > buffer.byteLength) break
      index.push({ at, ms })
      durationMs = Math.max(durationMs, ms)
      at += size
    }
    post({ type: 'loaded', frames: index.length, durationMs })
    return
  }

  if (msg.type === 'window') {
    if (!buffer) {
      post({ type: 'error', message: 'no recording loaded' })
      return
    }
    const view = new DataView(buffer)
    const rects: ReplayRect[] = []
    const transfer: Transferable[] = []

    try {
      for (let i = lowerBound(index, msg.fromMs); i < index.length; i++) {
        const entry = index[i]!
        if (entry.ms > msg.toMs) break
        const base = entry.at + REPLAY_HEADER
        const x = view.getUint16(base + 2, true)
        const y = view.getUint16(base + 4, true)
        const w = view.getUint16(base + 6, true)
        const h = view.getUint16(base + 8, true)
        const pixels = new Uint8ClampedArray(buffer, base + HEADER, w * h * 4)
        const bitmap = await createImageBitmap(new ImageData(pixels, w, h))
        rects.push({ ms: entry.ms, x, y, bitmap })
        transfer.push(bitmap)
      }
    } catch (err) {
      post({ type: 'error', message: err instanceof Error ? err.message : String(err) })
      return
    }
    post({ type: 'rects', id: msg.id, rects, toMs: msg.toMs }, transfer)
  }
}

function post(msg: ReplayResponse, transfer: Transferable[] = []) {
  ;(self as unknown as Worker).postMessage(msg, transfer)
}
