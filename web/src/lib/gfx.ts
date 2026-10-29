/**
 * RDP's graphics pipeline (MS-RDPEGFX), drawn in the browser.
 *
 * The agent passes Windows' graphics commands through untouched, after only
 * undoing the bulk compression (internal/rdp/gfx.go). Windows draws onto
 * surfaces and says, per command, with what: H.264 for nearly everything when
 * it can, and fills and copies for the cheap cases. H.264 is decoded by the
 * browser's own decoder (WebCodecs), in hardware where there is some, which
 * is the point of the pipeline: a 1080p desktop at a few megabits a second and
 * no decoding on the CPU.
 *
 * AVC444 is two H.264 streams, the picture at 4:2:0 and the chroma it left
 * out. Only the first is decoded for now, so colour is 4:2:0: correct, a
 * little soft around coloured text. Recombining the second is a GPU shader,
 * and a later step.
 *
 * Parsing is kept apart from drawing so it can be tested without a decoder.
 */

/** Frame types carrying graphics commands, matching internal/rdp/display.go. */
export const FRAME_GRAPHICS = 6
export const FRAME_GRAPHICS_PART = 7

/** Commands (MS-RDPEGFX 2.2.1.5). */
export const Cmd = {
  WireToSurface1: 0x0001,
  WireToSurface2: 0x0002,
  SolidFill: 0x0004,
  SurfaceToSurface: 0x0005,
  SurfaceToCache: 0x0006,
  CacheToSurface: 0x0007,
  EvictCacheEntry: 0x0008,
  CreateSurface: 0x0009,
  DeleteSurface: 0x000a,
  StartFrame: 0x000b,
  EndFrame: 0x000c,
  ResetGraphics: 0x000e,
  MapSurfaceToOutput: 0x000f,
} as const

/** Codecs (MS-RDPEGFX 2.2.1.6). */
export const Codec = {
  Uncompressed: 0x0000,
  AVC420: 0x000b,
  AVC444: 0x000e,
  AVC444v2: 0x000f,
} as const

export interface Rect {
  left: number
  top: number
  right: number
  bottom: number
}

/** One command, its header read and its body left as a view. */
export interface Command {
  cmd: number
  body: DataView
}

export function parseCommand(raw: Uint8Array): Command | null {
  if (raw.byteLength < 8) return null
  const v = new DataView(raw.buffer, raw.byteOffset, raw.byteLength)
  const length = v.getUint32(4, true)
  if (length < 8 || length > raw.byteLength) return null
  return { cmd: v.getUint16(0, true), body: new DataView(raw.buffer, raw.byteOffset + 8, length - 8) }
}

export function rect16(v: DataView, at: number): Rect {
  return {
    left: v.getUint16(at, true),
    top: v.getUint16(at + 2, true),
    right: v.getUint16(at + 4, true),
    bottom: v.getUint16(at + 6, true),
  }
}

export interface WireToSurface {
  surface: number
  codec: number
  pixelFormat: number
  dest: Rect
  bitmap: Uint8Array
}

export function parseWireToSurface1(b: DataView): WireToSurface | null {
  if (b.byteLength < 17) return null
  const size = b.getUint32(13, true)
  if (size > b.byteLength - 17) return null
  return {
    surface: b.getUint16(0, true),
    codec: b.getUint16(2, true),
    pixelFormat: b.getUint8(4),
    dest: rect16(b, 5),
    bitmap: new Uint8Array(b.buffer, b.byteOffset + 17, size),
  }
}

/** One AVC420 stream: the regions it updates and its H.264 bytes. */
export interface AVC420 {
  regions: Rect[]
  h264: Uint8Array
}

/** Reads an RFX_AVC420_BITMAP_STREAM (MS-RDPEGFX 2.2.4.4). */
export function parseAVC420(p: Uint8Array): AVC420 | null {
  if (p.byteLength < 4) return null
  const v = new DataView(p.buffer, p.byteOffset, p.byteLength)
  const n = v.getUint32(0, true)
  // Eight bytes of rectangle and two of quality per region.
  const meta = 4 + n * 10
  if (n > 4096 || meta > p.byteLength) return null
  const regions: Rect[] = []
  for (let i = 0; i < n; i++) regions.push(rect16(v, 4 + i * 8))
  return { regions, h264: p.subarray(meta) }
}

/**
 * Reads an AVC444 or AVC444v2 bitmap stream (MS-RDPEGFX 2.2.4.5, 2.2.4.6):
 * the main picture, the extra chroma, or both, as LC says.
 */
export function parseAVC444(p: Uint8Array): { main: AVC420 | null; aux: AVC420 | null } | null {
  if (p.byteLength < 4) return null
  const info = new DataView(p.buffer, p.byteOffset, 4).getUint32(0, true)
  const size = info & 0x3fffffff
  const lc = info >>> 30
  const rest = p.subarray(4)
  switch (lc) {
    case 0:
      if (size > rest.byteLength) return null
      return { main: parseAVC420(rest.subarray(0, size)), aux: parseAVC420(rest.subarray(size)) }
    case 1:
      return { main: parseAVC420(rest), aux: null }
    case 2:
      return { main: null, aux: parseAVC420(rest) }
  }
  return null
}

/** Whether an Annex B stream holds an IDR slice: what a decoder can start on. */
export function hasKeyFrame(h264: Uint8Array): boolean {
  for (let i = 0; i + 3 < h264.byteLength; i++) {
    if (h264[i] === 0 && h264[i + 1] === 0) {
      let at = -1
      if (h264[i + 2] === 1) at = i + 3
      else if (h264[i + 2] === 0 && h264[i + 3] === 1) at = i + 4
      if (at >= 0 && at < h264.byteLength) {
        if (((h264[at] ?? 0) & 0x1f) === 5) return true
        i = at - 1
      }
    }
  }
  return false
}

/**
 * Joins FRAME_GRAPHICS_PART frames into whole commands. A key frame is often
 * larger than one display frame carries.
 */
export class PartJoiner {
  private parts: Uint8Array[] = []
  private have = 0
  private total = 0

  /** Adds a part; returns the whole command once its last part is in. */
  add(part: Uint8Array, total: number): Uint8Array | null {
    if (this.have === 0) this.total = total
    if (total !== this.total || this.have + part.byteLength > total) {
      // Out of step: drop what was gathered rather than splice two commands.
      this.parts = []
      this.have = 0
      return null
    }
    this.parts.push(part.slice())
    this.have += part.byteLength
    if (this.have < this.total) return null
    const out = new Uint8Array(this.total)
    let at = 0
    for (const p of this.parts) {
      out.set(p, at)
      at += p.byteLength
    }
    this.parts = []
    this.have = 0
    return out
  }
}

/** Whether this browser can decode H.264 in a worker, as the pipeline needs. */
export async function canDecodeH264(): Promise<boolean> {
  if (typeof VideoDecoder === 'undefined' || typeof OffscreenCanvas === 'undefined') return false
  try {
    const { supported } = await VideoDecoder.isConfigSupported(H264_CONFIG)
    return supported === true
  } catch {
    return false
  }
}

/**
 * The decoder configuration. Annex B with no description: Windows sends its
 * parameter sets in the stream. The profile in the string is High, which
 * covers what Windows encodes; the stream's own parameter sets decide.
 */
export const H264_CONFIG: VideoDecoderConfig = {
  codec: 'avc1.640034',
  optimizeForLatency: true,
  hardwareAcceleration: 'no-preference',
}
