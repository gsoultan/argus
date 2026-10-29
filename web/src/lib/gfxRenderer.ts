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
 * decoder; with AVC444v2, two, one for each view. A decoder that fails is
 * replaced and waits for the next key frame; until then that surface keeps its
 * last picture.
 *
 * AVC444v2 is drawn in full colour on the GPU (avc444.ts) where there is
 * WebGL 2: each view's planes are copied out of its decoder, and the two are
 * put together when the pictures queued so far have all come out. Without it,
 * the main view is drawn on its own, at 4:2:0.
 */

import { Avc444Gl, framePlanes, type Planes } from './avc444'
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
  /** The surface's H.264 stream, and AVC444's second one. */
  main: Stream
  aux: Stream
  /** The latest planes of each view, while the surface is drawn in full colour. */
  mainPlanes: Planes | null
  auxPlanes: Planes | null
  /** Regions of full-colour pictures decoded but not yet drawn. */
  redraw: Array<{ dest: Rect; regions: Rect[] }>
  dirty: Rect[]
}

interface Stream {
  decoder: VideoDecoder | null
  /** The decoder needs a key frame before it can take anything else. */
  needKey: boolean
  /** The last copy of a picture's planes: copies go one at a time, in order. */
  copying: Promise<void>
}

interface Job {
  surface: Surface
  stream: 'main' | 'aux'
  /** Copy the picture's planes out for the GPU, rather than draw it. */
  planes: boolean
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
  /**
   * What has happened to the H.264 handed in: pictures drawn, pictures
   * skipped while a decoder waited for a key frame, and decoders lost.
   */
  readonly stats = {
    pictures: 0,
    skipped: 0,
    failures: 0,
    lastError: '',
    /** Picture regions drawn, and dropped for lying outside the picture. */
    regions: 0,
    outside: 0,
    /** Rectangles handed to the screen. */
    posted: 0,
    /** Frames that ended with changes on surfaces not on the screen. */
    unmapped: 0,
    /** Full-colour pictures put together on the GPU. */
    combined: 0,
    /** The colour space the first picture said it was in, as the browser read it. */
    colourSpace: '',
    /** The first picture's pixel format and coded size, as the decoder gave it. */
    format: '',
    /** The average luma of the last main view copied out, to tell an empty copy. */
    meanLuma: -1,
  }

  /** The GPU combiner, made on first use; null once it is known not to be had. */
  private gl: Avc444Gl | null | undefined

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
        for (const s of this.surfaces.values()) closeSurface(s)
        this.surfaces.clear()
        this.events.resize(b.getUint32(0, true), b.getUint32(4, true))
        return
      }
      case Cmd.CreateSurface: {
        if (b.byteLength < 7) return
        const id = b.getUint16(0, true)
        const width = b.getUint16(2, true)
        const height = b.getUint16(4, true)
        const old = this.surfaces.get(id)
        if (old) closeSurface(old)
        const canvas = new OffscreenCanvas(Math.max(width, 1), Math.max(height, 1))
        const ctx = canvas.getContext('2d')
        if (!ctx) return
        this.surfaces.set(id, {
          canvas,
          ctx,
          width,
          height,
          output: null,
          main: { decoder: null, needKey: true, copying: Promise.resolve() },
          aux: { decoder: null, needKey: true, copying: Promise.resolve() },
          mainPlanes: null,
          auxPlanes: null,
          redraw: [],
          dirty: [],
        })
        return
      }
      case Cmd.DeleteSurface: {
        if (b.byteLength < 2) return
        const id = b.getUint16(0, true)
        const old = this.surfaces.get(id)
        if (old) closeSurface(old)
        this.surfaces.delete(id)
        return
      }
      case Cmd.MapSurfaceToOutput:
      case Cmd.MapSurfaceToScaledOutput: {
        // The scaled form adds the size the surface is to be shown at, which
        // Windows sets to the surface's own size unless asked for scaling,
        // and which is not asked for.
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
            this.decode(s, 'main', false, w.dest, parseAVC420(w.bitmap))
            return
          case Codec.AVC444:
          case Codec.AVC444v2: {
            const avc = parseAVC444(w.bitmap)
            // Full colour needs the second view's layout, which is v2's, and
            // somewhere to put the views together.
            const full = w.codec === Codec.AVC444v2 && this.combiner() !== null
            if (avc?.main) this.decode(s, 'main', full, w.dest, avc.main)
            if (avc?.aux && full) this.decode(s, 'aux', true, w.dest, avc.aux)
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
    for (const s of this.surfaces.values()) closeSurface(s)
    this.surfaces.clear()
    this.cache.clear()
  }

  private combiner(): Avc444Gl | null {
    if (this.gl === undefined) this.gl = Avc444Gl.create()
    return this.gl
  }

  private decode(s: Surface, stream: 'main' | 'aux', planes: boolean, dest: Rect, avc: AVC420 | null) {
    if (!avc || avc.h264.byteLength === 0) return
    const st = s[stream]
    const key = hasKeyFrame(avc.h264)
    if (!st.decoder || st.decoder.state === 'closed') {
      st.decoder = this.newDecoder()
      st.needKey = true
    }
    if (st.needKey && !key) {
      this.stats.skipped++
      return
    }
    st.needKey = false
    if (!planes && stream === 'main') {
      // Drawn on its own from here: whatever full-colour planes were kept
      // would only hold the picture back.
      s.mainPlanes = null
      s.auxPlanes = null
    }
    const id = this.nextJob++
    this.pending.push(
      new Promise<void>((done) => {
        this.jobs.set(id, { surface: s, stream, planes, dest, regions: avc.regions, done })
      }),
    )
    try {
      if (st.decoder.state === 'unconfigured') {
        st.decoder.configure({ ...H264_CONFIG, codec: codecString(avc.h264) ?? H264_CONFIG.codec })
      }
      st.decoder.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: id, data: avc.h264 }))
    } catch (err) {
      this.stats.lastError = err instanceof Error ? err.message : String(err)
      this.fail(s, stream)
    }
  }

  private newDecoder(): VideoDecoder {
    const decoder: VideoDecoder = new VideoDecoder({
      output: (frame) => {
        const job = this.jobs.get(frame.timestamp)
        this.jobs.delete(frame.timestamp)
        if (!job) {
          frame.close()
          return
        }
        this.stats.pictures++
        this.stats.colourSpace ||= JSON.stringify(frame.colorSpace.toJSON())
        this.stats.format ||= `${frame.format ?? 'opaque'} ${frame.codedWidth}x${frame.codedHeight}`
        if (!job.planes) {
          this.drawn(drawPicture(job, frame))
          frame.close()
          job.done()
          return
        }
        const s = job.surface
        const key = job.stream === 'main' ? 'mainPlanes' : 'auxPlanes'
        const st = s[job.stream]
        st.copying = st.copying
          .then(() => framePlanes(frame, s[key] ?? undefined))
          .then((p) => {
            if (p && p.width % 4 === 0) {
              s[key] = p
              if (job.stream === 'main') this.stats.meanLuma = meanOf(p.y)
              s.redraw.push({ dest: job.dest, regions: job.regions })
            } else if (job.stream === 'main') {
              // A layout the planes cannot be read from: this picture is
              // drawn at 4:2:0, as the browser converts it.
              this.drawn(drawPicture(job, frame))
            }
          })
          .catch((err: unknown) => {
            this.stats.lastError = err instanceof Error ? err.message : String(err)
          })
          .finally(() => {
            frame.close()
            job.done()
          })
      },
      error: (err) => {
        this.stats.lastError = err.message
        for (const s of this.surfaces.values()) {
          if (s.main.decoder === decoder) this.fail(s, 'main')
          if (s.aux.decoder === decoder) this.fail(s, 'aux')
        }
      },
    })
    return decoder
  }

  private drawn([drawn, outside]: [number, number]) {
    this.stats.regions += drawn
    this.stats.outside += outside
  }

  /** Drops one of a surface's decoders; the next key frame starts a fresh one. */
  private fail(s: Surface, stream: 'main' | 'aux') {
    this.stats.failures++
    const st = s[stream]
    const decoder = st.decoder
    st.decoder = null
    st.needKey = true
    for (const [id, job] of this.jobs) {
      if (job.surface === s && job.stream === stream) {
        this.jobs.delete(id)
        job.done()
      }
    }
    if (decoder && decoder.state !== 'closed') decoder.close()
  }

  /** Puts full-colour pictures together and draws their regions. */
  private combine() {
    const gl = this.gl
    if (!gl) return
    for (const s of this.surfaces.values()) {
      const redraw = s.redraw.splice(0)
      const main = s.mainPlanes
      if (redraw.length === 0 || !main) continue
      // Until the second view's first picture, the main one alone, at 4:2:0.
      const aux = s.auxPlanes && s.auxPlanes.width === main.width && s.auxPlanes.height === main.height ? s.auxPlanes : null
      // Taken out as a bitmap rather than drawn from the GL canvas directly:
      // Chrome on Windows drew nothing from the canvas itself.
      const picture = gl.draw(main, aux)
      this.stats.combined++
      for (const { dest, regions } of redraw) {
        for (const region of regions.length > 0 ? regions : [dest]) {
          const r = clip(region, s)
          if (!r) continue
          const w = r.right - r.left
          const h = r.bottom - r.top
          const sx = r.left - dest.left
          const sy = r.top - dest.top
          if (sx < 0 || sy < 0 || sx + w > main.width || sy + h > main.height) {
            this.stats.outside++
            continue
          }
          s.ctx.drawImage(picture, sx, sy, w, h, r.left, r.top, w, h)
          s.dirty.push(r)
          this.stats.regions++
        }
      }
      picture.close()
    }
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
    this.combine()
  }

  private async endFrame() {
    await this.settle()
    const out: Array<Promise<DirtyRect>> = []
    for (const s of this.surfaces.values()) {
      const dirty = s.dirty.splice(0)
      if (dirty.length === 0) continue
      if (!s.output) {
        this.stats.unmapped++
        continue
      }
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
    this.stats.posted += out.length
    if (out.length > 0) this.events.rects(await Promise.all(out))
  }
}

/**
 * Draws a decoded picture's updated regions onto its surface. Returns how
 * many were drawn, and how many lay outside the picture.
 */
function drawPicture(job: Job, frame: VideoFrame): [number, number] {
  let drawn = 0
  let outside = 0
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
    if (sx < 0 || sy < 0 || sx + w > frame.displayWidth || sy + h > frame.displayHeight) {
      outside++
      continue
    }
    s.ctx.drawImage(frame, sx, sy, w, h, r.left, r.top, w, h)
    s.dirty.push(r)
    drawn++
  }
  return [drawn, outside]
}

function meanOf(b: Uint8Array): number {
  let sum = 0
  for (let i = 0; i < b.length; i += 97) sum += b[i] ?? 0
  return Math.round((sum / Math.ceil(b.length / 97)) * 10) / 10
}

function closeSurface(s: Surface) {
  for (const st of [s.main, s.aux]) if (st.decoder && st.decoder.state !== 'closed') st.decoder.close()
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
