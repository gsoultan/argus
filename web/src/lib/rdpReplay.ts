/**
 * Plays a recorded Remote Desktop session back, a window of time at a time.
 *
 * The control plane turns a recording into display frames, each behind the
 * millisecond it arrived at: bitmap rectangles for a session drawn with bitmap
 * updates, and the graphics pipeline's commands, as Windows sent them, for one
 * drawn with H.264. The second kind is decoded here by the renderer the live
 * session used, so what a replay shows is produced the way the operator saw
 * it.
 *
 * Both kinds are deltas. A bitmap rectangle stands alone once drawn, so a
 * window is the rectangles in it. An H.264 picture depends on every one before
 * it, so a graphics session is drawn onto a screen kept here, every command
 * fed once and in order, and a window answers with the part of that screen it
 * changed. Going backwards starts again from the beginning.
 */

import { FRAME_GRAPHICS, FRAME_GRAPHICS_PART, PartJoiner } from './gfx'
import { GraphicsRenderer, type DirtyRect } from './gfxRenderer'

const FRAME_BITMAP = 1
/** The timestamp before each frame. */
export const REPLAY_HEADER = 4
/** A display frame's own header. */
const HEADER = 12

export interface ReplayRect {
  ms: number
  x: number
  y: number
  bitmap: ImageBitmap
}

export interface ReplayEntry {
  /** Where the frame starts, after its timestamp. */
  at: number
  ms: number
  kind: number
  /** The frame's whole size, header included. */
  size: number
}

/**
 * Indexes a replay once, so a window is a slice rather than a scan. Stops at
 * the first frame it does not know or that runs past the end, keeping what
 * came before.
 */
export function indexReplay(buffer: ArrayBuffer): { entries: ReplayEntry[]; graphics: boolean } {
  const view = new DataView(buffer)
  const entries: ReplayEntry[] = []
  let graphics = false
  let at = 0
  while (at + REPLAY_HEADER + HEADER <= buffer.byteLength) {
    const ms = view.getUint32(at, true)
    const frame = at + REPLAY_HEADER
    const kind = view.getUint8(frame)
    let size: number
    if (kind === FRAME_BITMAP) {
      size = HEADER + view.getUint16(frame + 6, true) * view.getUint16(frame + 8, true) * 4
    } else if (kind === FRAME_GRAPHICS || kind === FRAME_GRAPHICS_PART) {
      size = HEADER + ((view.getUint32(frame + 4, true) + 3) & ~3)
    } else {
      break
    }
    if (frame + size > buffer.byteLength) break
    if (kind !== FRAME_BITMAP) graphics = true
    entries.push({ at: frame, ms, kind, size })
    at = frame + size
  }
  return { entries, graphics }
}

/** First index whose timestamp is >= `ms`. */
export function lowerBound(entries: Array<{ ms: number }>, ms: number): number {
  let lo = 0
  let hi = entries.length
  while (lo < hi) {
    const mid = (lo + hi) >> 1
    if (entries[mid]!.ms < ms) lo = mid + 1
    else hi = mid
  }
  return lo
}

export class ReplayPlayer {
  readonly entries: ReplayEntry[]
  readonly graphics: boolean
  readonly durationMs: number
  private buffer: ArrayBuffer
  private width: number
  private height: number

  // A graphics session's state: the screen as drawn so far, and the next
  // frame to feed.
  private screen: OffscreenCanvas | null = null
  private ctx: OffscreenCanvasRenderingContext2D | null = null
  private renderer: GraphicsRenderer | null = null
  private parts = new PartJoiner()
  private next = 0
  private dirty: { left: number; top: number; right: number; bottom: number } | null = null

  constructor(buffer: ArrayBuffer, width: number, height: number) {
    this.buffer = buffer
    this.width = width
    this.height = height
    const { entries, graphics } = indexReplay(buffer)
    this.entries = entries
    this.graphics = graphics
    this.durationMs = entries.reduce((m, e) => Math.max(m, e.ms), 0)
  }

  /** What the renderer has made of the H.264, for a graphics session. */
  get stats() {
    return this.renderer?.stats
  }

  /**
   * What changes between `fromMs` and `toMs`. A graphics session goes on from
   * where the last window ended, or starts again when `fromMs` is zero.
   */
  async window(fromMs: number, toMs: number): Promise<ReplayRect[]> {
    if (this.graphics) return this.compose(fromMs, toMs)
    const view = new DataView(this.buffer)
    const rects: ReplayRect[] = []
    for (let i = lowerBound(this.entries, fromMs); i < this.entries.length; i++) {
      const e = this.entries[i]!
      if (e.ms > toMs) break
      rects.push({ ms: e.ms, ...(await this.bitmap(view, e)) })
    }
    return rects
  }

  close() {
    this.renderer?.close()
    this.renderer = null
  }

  private async bitmap(view: DataView, e: ReplayEntry) {
    const x = view.getUint16(e.at + 2, true)
    const y = view.getUint16(e.at + 4, true)
    const w = view.getUint16(e.at + 6, true)
    const h = view.getUint16(e.at + 8, true)
    const pixels = new Uint8ClampedArray(this.buffer, e.at + HEADER, w * h * 4)
    return { x, y, bitmap: await createImageBitmap(new ImageData(pixels, w, h)) }
  }

  private async compose(fromMs: number, toMs: number): Promise<ReplayRect[]> {
    if (fromMs === 0 || !this.screen) this.restart()
    const view = new DataView(this.buffer)
    for (; this.next < this.entries.length; this.next++) {
      const e = this.entries[this.next]!
      if (e.ms > toMs) break
      if (e.kind === FRAME_BITMAP) {
        const r = await this.bitmap(view, e)
        this.draw({ x: r.x, y: r.y, width: r.bitmap.width, height: r.bitmap.height, bitmap: r.bitmap })
        continue
      }
      const length = view.getUint32(e.at + 4, true)
      let command: Uint8Array | null = new Uint8Array(this.buffer, e.at + HEADER, length)
      if (e.kind === FRAME_GRAPHICS_PART) command = this.parts.add(command, view.getUint32(e.at + 8, true))
      if (command) await this.renderer!.command(command)
    }
    const d = this.dirty
    this.dirty = null
    if (!d) return []
    const bitmap = await createImageBitmap(this.screen!, d.left, d.top, d.right - d.left, d.bottom - d.top)
    return [{ ms: toMs, x: d.left, y: d.top, bitmap }]
  }

  /** Back to an empty screen and the first frame. */
  private restart() {
    this.renderer?.close()
    this.screen = new OffscreenCanvas(this.width, this.height)
    this.ctx = this.screen.getContext('2d')
    if (!this.ctx) throw new Error('this browser has no 2D canvas in workers')
    this.ctx.fillStyle = '#05080c'
    this.ctx.fillRect(0, 0, this.width, this.height)
    this.renderer = new GraphicsRenderer({
      rects: (rects) => {
        for (const r of rects) this.draw(r)
      },
      // The recording's header has the size the session was opened at,
      // which is the one Windows draws at.
      resize: () => {},
    })
    this.parts = new PartJoiner()
    this.next = 0
    this.dirty = { left: 0, top: 0, right: this.width, bottom: this.height }
  }

  private draw(r: DirtyRect) {
    this.ctx!.drawImage(r.bitmap, r.x, r.y)
    r.bitmap.close()
    const left = Math.max(0, r.x)
    const top = Math.max(0, r.y)
    const right = Math.min(this.width, r.x + r.width)
    const bottom = Math.min(this.height, r.y + r.height)
    if (right <= left || bottom <= top) return
    const d = this.dirty
    this.dirty = d
      ? {
          left: Math.min(d.left, left),
          top: Math.min(d.top, top),
          right: Math.max(d.right, right),
          bottom: Math.max(d.bottom, bottom),
        }
      : { left, top, right, bottom }
  }
}
