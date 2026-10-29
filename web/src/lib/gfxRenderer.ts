/**
 * Draws the graphics pipeline's commands onto surfaces and hands the parts
 * that changed to the console, as the bitmap path does: ImageBitmaps of the
 * screen's dirty rectangles, once per frame.
 *
 * Commands are handled strictly in order, as Windows means them. H.264 is
 * the one asynchronous step: a picture is queued to the decoder and drawn when
 * it comes out. Anything that reads a surface (a copy, a cache store) and the
 * end of a frame wait for the pictures already queued, so nothing is read
 * before it has been drawn.
 *
 * Each surface is its own H.264 stream, as in Windows, so each has its own
 * decoder. A decoder that fails is replaced and waits for the next key frame;
 * until then that surface keeps its last picture.
 */

import {
  Cmd,
  Codec,
  H264_CONFIG,
  hasKeyFrame,
  parseAVC420,
  parseAVC444,
  parseCommand,
  parseWireToSurface1,
  rect16,
  type AVC420,
  type Rect,
} from './gfx'

export interface DirtyRect {
  x: number
  y: number
  width: number
  height: number
  bitmap: ImageBitmap
}

export interface RendererEvents {
  rects(rects: DirtyRect[]): void
  resize(width: number, height: number): void
}

/** How long the end of a frame waits for its pictures before painting anyway. */
const DECODE_WAIT_MS = 1000

/** Past this many dirty rectangles in a frame, one rectangle covering them all is cheaper. */
const MAX_DIRTY = 32

interface Surface {
  canvas: OffscreenCanvas
  ctx: OffscreenCanvasRenderingContext2D
  width: number
  height: number
  /** Where the surface sits on screen, once it is mapped. */
  output: { x: number; y: number } | null
  decoder: VideoDecoder | null
  /** The decoder needs a key frame before it can take anything else. */
  needKey: boolean
  dirty: Rect[]
}

interface Job {
  surface: Surface
  dest: Rect
  regions: Rect[]
  done: () => void
}

export class GraphicsRenderer {
  private surfaces = new Map<number, Surface>()
  private cache = new Map<number, OffscreenCanvas>()
  private jobs = new Map<number, Job>()
  private pending: Promise<void>[] = []
  private nextJob = 0
  /** Codecs seen that this console does not draw, reported once each. */
  readonly unknownCodecs = new Set<number>()

  private events: RendererEvents

  constructor(events: RendererEvents) {
    this.events = events
  }

  /** Handles one whole command, as Windows sent it. */
  async command(raw: Uint8Array): Promise<void> {
    const c = parseCommand(raw)
    if (!c) return
    const b = c.body
    switch (c.cmd) {
      case Cmd.ResetGraphics: {
        if (b.byteLength < 8) return
        for (const s of this.surfaces.values()) s.decoder?.close()
        this.surfaces.clear()
        this.events.resize(b.getUint32(0, true), b.getUint32(4, true))
        return
      }
      case Cmd.CreateSurface: {
        if (b.byteLength < 7) return
        const id = b.getUint16(0, true)
        const width = b.getUint16(2, true)
        const height = b.getUint16(4, true)
        this.surfaces.get(id)?.decoder?.close()
        const canvas = new OffscreenCanvas(Math.max(width, 1), Math.max(height, 1))
        const ctx = canvas.getContext('2d')
        if (!ctx) return
        this.surfaces.set(id, { canvas, ctx, width, height, output: null, decoder: null, needKey: true, dirty: [] })
        return
      }
      case Cmd.DeleteSurface: {
        if (b.byteLength < 2) return
        const id = b.getUint16(0, true)
        this.surfaces.get(id)?.decoder?.close()
        this.surfaces.delete(id)
        return
      }
      case Cmd.MapSurfaceToOutput: {
        if (b.byteLength < 12) return
        const s = this.surfaces.get(b.getUint16(0, true))
        if (!s) return
        s.output = { x: b.getUint32(4, true), y: b.getUint32(8, true) }
        s.dirty.push({ left: 0, top: 0, right: s.width, bottom: s.height })
        return
      }
      case Cmd.SolidFill: {
        if (b.byteLength < 8) return
        const s = this.surfaces.get(b.getUint16(0, true))
        if (!s) return
        // The fill colour is blue, green, red, then a byte to ignore.
        const [blue, green, red] = [b.getUint8(2), b.getUint8(3), b.getUint8(4)]
        const n = b.getUint16(6, true)
        if (8 + n * 8 > b.byteLength) return
        await this.settle()
        s.ctx.fillStyle = `rgb(${red},${green},${blue})`
        for (let i = 0; i < n; i++) {
          const r = clip(rect16(b, 8 + i * 8), s)
          if (!r) continue
          s.ctx.fillRect(r.left, r.top, r.right - r.left, r.bottom - r.top)
          s.dirty.push(r)
        }
        return
      }
      case Cmd.SurfaceToSurface: {
        if (b.byteLength < 14) return
        const src = this.surfaces.get(b.getUint16(0, true))
        const dst = this.surfaces.get(b.getUint16(2, true))
        const from = src && clip(rect16(b, 4), src)
        const n = b.getUint16(12, true)
        if (!src || !dst || !from || 14 + n * 4 > b.byteLength) return
        await this.settle()
        const w = from.right - from.left
        const h = from.bottom - from.top
        // Copied once first: source and destination may be the same surface
        // and overlap, which is what a scroll is.
        const copy = snapshot(src.canvas, from)
        for (let i = 0; i < n; i++) {
          const x = b.getUint16(14 + i * 4, true)
          const y = b.getUint16(16 + i * 4, true)
          dst.ctx.drawImage(copy, x, y)
          const r = clip({ left: x, top: y, right: x + w, bottom: y + h }, dst)
          if (r) dst.dirty.push(r)
        }
        return
      }
      case Cmd.SurfaceToCache: {
        if (b.byteLength < 20) return
        const s = this.surfaces.get(b.getUint16(0, true))
        const slot = b.getUint16(10, true)
        const from = s && clip(rect16(b, 12), s)
        if (!s || !from) return
        await this.settle()
        this.cache.set(slot, snapshot(s.canvas, from))
        return
      }
      case Cmd.CacheToSurface: {
        if (b.byteLength < 6) return
        const entry = this.cache.get(b.getUint16(0, true))
        const s = this.surfaces.get(b.getUint16(2, true))
        const n = b.getUint16(4, true)
        if (!entry || !s || 6 + n * 4 > b.byteLength) return
        await this.settle()
        for (let i = 0; i < n; i++) {
          const x = b.getUint16(6 + i * 4, true)
          const y = b.getUint16(8 + i * 4, true)
          s.ctx.drawImage(entry, x, y)
          const r = clip({ left: x, top: y, right: x + entry.width, bottom: y + entry.height }, s)
          if (r) s.dirty.push(r)
        }
        return
      }
      case Cmd.EvictCacheEntry: {
        if (b.byteLength >= 2) this.cache.delete(b.getUint16(0, true))
        return
      }
      case Cmd.WireToSurface1: {
        const w = parseWireToSurface1(b)
        const s = w && this.surfaces.get(w.surface)
        if (!w || !s) return
        switch (w.codec) {
          case Codec.AVC420:
            this.decode(s, w.dest, parseAVC420(w.bitmap))
            return
          case Codec.AVC444:
          case Codec.AVC444v2: {
            // The main view only: a whole picture at 4:2:0. The extra chroma
            // is left for the GPU step.
            const avc = parseAVC444(w.bitmap)
            if (avc?.main) this.decode(s, w.dest, avc.main)
            return
          }
          case Codec.Uncompressed:
            await this.settle()
            this.uncompressed(s, w.dest, w.bitmap)
            return
        }
        this.unknownCodecs.add(w.codec)
        return
      }
      case Cmd.EndFrame:
        await this.endFrame()
        return
    }
  }

  /** Releases every decoder. */
  close() {
    for (const s of this.surfaces.values()) s.decoder?.close()
    this.surfaces.clear()
    this.cache.clear()
  }

  private decode(s: Surface, dest: Rect, avc: AVC420 | null) {
    if (!avc || avc.h264.byteLength === 0) return
    const key = hasKeyFrame(avc.h264)
    if (!s.decoder || s.decoder.state === 'closed') {
      s.decoder = this.newDecoder()
      s.needKey = true
    }
    if (s.needKey && !key) return
    if (s.decoder.state === 'unconfigured') {
      s.decoder.configure({ ...H264_CONFIG, codec: codecString(avc.h264) ?? H264_CONFIG.codec })
    }
    s.needKey = false
    const id = this.nextJob++
    this.pending.push(
      new Promise<void>((done) => {
        this.jobs.set(id, { surface: s, dest, regions: avc.regions, done })
      }),
    )
    try {
      s.decoder.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: id, data: avc.h264 }))
    } catch {
      this.fail(s)
    }
  }

  private newDecoder(): VideoDecoder {
    const decoder: VideoDecoder = new VideoDecoder({
      output: (frame) => {
        const job = this.jobs.get(frame.timestamp)
        this.jobs.delete(frame.timestamp)
        if (job) {
          drawPicture(job, frame)
          job.done()
        }
        frame.close()
      },
      error: () => {
        for (const s of this.surfaces.values()) if (s.decoder === decoder) this.fail(s)
      },
    })
    return decoder
  }

  /** Drops a surface's decoder; the next key frame starts a fresh one. */
  private fail(s: Surface) {
    const decoder = s.decoder
    s.decoder = null
    s.needKey = true
    for (const [id, job] of this.jobs) {
      if (job.surface === s) {
        this.jobs.delete(id)
        job.done()
      }
    }
    if (decoder && decoder.state !== 'closed') decoder.close()
  }

  private uncompressed(s: Surface, dest: Rect, bitmap: Uint8Array) {
    const width = dest.right - dest.left
    const height = dest.bottom - dest.top
    if (width <= 0 || height <= 0 || bitmap.byteLength < width * height * 4) return
    // XRGB or ARGB, little-endian: blue, green, red, then alpha or nothing.
    const rgba = new Uint8ClampedArray(width * height * 4)
    for (let i = 0; i < rgba.length; i += 4) {
      rgba[i] = bitmap[i + 2] ?? 0
      rgba[i + 1] = bitmap[i + 1] ?? 0
      rgba[i + 2] = bitmap[i] ?? 0
      rgba[i + 3] = 255
    }
    s.ctx.putImageData(new ImageData(rgba, width, height), dest.left, dest.top)
    const r = clip(dest, s)
    if (r) s.dirty.push(r)
  }

  /** Waits for every picture queued so far, or gives up on them after a while. */
  private async settle() {
    if (this.pending.length === 0) return
    const waiting = this.pending.splice(0)
    let timer: ReturnType<typeof setTimeout> | undefined
    await Promise.race([
      Promise.all(waiting),
      new Promise<void>((done) => {
        timer = setTimeout(done, DECODE_WAIT_MS)
      }),
    ])
    clearTimeout(timer)
  }

  private async endFrame() {
    await this.settle()
    const out: Array<Promise<DirtyRect>> = []
    for (const s of this.surfaces.values()) {
      const dirty = s.dirty.splice(0)
      if (!s.output || dirty.length === 0) continue
      for (const r of merge(dirty)) {
        const width = r.right - r.left
        const height = r.bottom - r.top
        const at = s.output
        out.push(
          createImageBitmap(s.canvas, r.left, r.top, width, height).then((bitmap) => ({
            x: at.x + r.left,
            y: at.y + r.top,
            width,
            height,
            bitmap,
          })),
        )
      }
    }
    if (out.length > 0) this.events.rects(await Promise.all(out))
  }
}

/** Draws a decoded picture's updated regions onto its surface. */
function drawPicture(job: Job, frame: VideoFrame) {
  const { surface: s, dest } = job
  // Windows says which parts of the picture changed; an empty list means all
  // of it.
  const regions = job.regions.length > 0 ? job.regions : [dest]
  for (const region of regions) {
    const r = clip(region, s)
    if (!r) continue
    const w = r.right - r.left
    const h = r.bottom - r.top
    // Region rectangles are in surface coordinates, and the picture starts at
    // the destination's corner.
    const sx = r.left - dest.left
    const sy = r.top - dest.top
    if (sx < 0 || sy < 0 || sx + w > frame.displayWidth || sy + h > frame.displayHeight) continue
    s.ctx.drawImage(frame, sx, sy, w, h, r.left, r.top, w, h)
    s.dirty.push(r)
  }
}

/** A rectangle cut to its surface, or null if nothing of it is left. */
export function clip(r: Rect, s: { width: number; height: number }): Rect | null {
  const out = {
    left: Math.max(0, r.left),
    top: Math.max(0, r.top),
    right: Math.min(s.width, r.right),
    bottom: Math.min(s.height, r.bottom),
  }
  return out.right > out.left && out.bottom > out.top ? out : null
}

/**
 * Fewer, larger rectangles: past MAX_DIRTY, the one rectangle covering them
 * all. Repainting a little extra costs less than a bitmap per rectangle.
 */
export function merge(rects: Rect[]): Rect[] {
  if (rects.length <= MAX_DIRTY) return rects
  return [
    rects.reduce((a, r) => ({
      left: Math.min(a.left, r.left),
      top: Math.min(a.top, r.top),
      right: Math.max(a.right, r.right),
      bottom: Math.max(a.bottom, r.bottom),
    })),
  ]
}

function snapshot(src: OffscreenCanvas, r: Rect): OffscreenCanvas {
  const w = r.right - r.left
  const h = r.bottom - r.top
  const c = new OffscreenCanvas(w, h)
  c.getContext('2d')?.drawImage(src, r.left, r.top, w, h, 0, 0, w, h)
  return c
}

/**
 * The codec string for a stream, from its sequence parameter set: profile,
 * constraints and level, as WebCodecs wants them. Null if the stream has none.
 */
export function codecString(h264: Uint8Array): string | null {
  for (let i = 0; i + 7 < h264.byteLength; i++) {
    if (h264[i] !== 0 || h264[i + 1] !== 0 || h264[i + 2] !== 1) continue
    const at = i + 3
    if (((h264[at] ?? 0) & 0x1f) !== 7) continue
    const hex = (v: number | undefined) => (v ?? 0).toString(16).padStart(2, '0')
    return `avc1.${hex(h264[at + 1])}${hex(h264[at + 2])}${hex(h264[at + 3])}`
  }
  return null
}
