/**
 * Decodes a captured desktop (deskdrill -capture) in a real browser, with the
 * console's own graphics pipeline code, and draws it onto a canvas.
 *
 * Bundled by run.mjs and loaded into a page; run.mjs calls decodeCapture and
 * screenshots the canvas.
 */

import { Avc444Gl, combineV2, rgb, splitV2 } from '../../src/lib/avc444'
import { FRAME_GRAPHICS, FRAME_GRAPHICS_PART, PartJoiner } from '../../src/lib/gfx'
import { GraphicsRenderer } from '../../src/lib/gfxRenderer'

const FRAME_BITMAP = 1
const FRAME_READY = 3
const FRAME_ENCODED = 5
const HEADER = 12

export interface CheckResult {
  width: number
  height: number
  messages: number
  commands: number
  pictures: number
  skipped: number
  failures: number
  lastError: string
  unknownCodecs: number[]
  /** Commands by id, in hex, and the first few as they came. */
  seen: Record<string, number>
  firstCommands: string[]
  regions: number
  outside: number
  posted: number
  unmapped: number
  /** Full-colour pictures put together on the GPU. */
  combined: number
  /** The pixel in the middle of the screen at the end. */
  middle: number[]
  /** Distinct colours on the canvas at the end, up to 4096. */
  colours: number
}

async function decodeCapture(url: string): Promise<CheckResult> {
  const buf = await (await fetch(url)).arrayBuffer()
  const canvas = document.getElementById('screen') as HTMLCanvasElement
  const ctx = canvas.getContext('2d')
  if (!ctx) throw new Error('no 2D canvas')

  const renderer = new GraphicsRenderer({
    rects: (rects) => {
      for (const r of rects) {
        ctx.drawImage(r.bitmap, r.x, r.y)
        r.bitmap.close()
      }
    },
    resize: (width, height) => {
      canvas.width = width
      canvas.height = height
    },
  })
  const parts = new PartJoiner()
  const result = { width: 0, height: 0, messages: 0, commands: 0 }
  const seen: Record<string, number> = {}
  const firstCommands: string[] = []

  const all = new DataView(buf)
  for (let m = 0; m + 4 <= buf.byteLength; ) {
    const length = all.getUint32(m, true)
    const start = m + 4
    m = start + length
    result.messages++
    const view = new DataView(buf, start, length)
    for (let at = 0; at + HEADER <= length; ) {
      const kind = view.getUint8(at)
      if (kind === FRAME_GRAPHICS || kind === FRAME_GRAPHICS_PART) {
        const n = view.getUint32(at + 4, true)
        let command: Uint8Array | null = new Uint8Array(buf, start + at + HEADER, n)
        if (kind === FRAME_GRAPHICS_PART) command = parts.add(command, view.getUint32(at + 8, true))
        at += HEADER + ((n + 3) & ~3)
        if (command) {
          result.commands++
          const id = command.byteLength >= 2 ? command[0]! | (command[1]! << 8) : -1
          const name = '0x' + id.toString(16).padStart(4, '0')
          seen[name] = (seen[name] ?? 0) + 1
          if (firstCommands.length < 12) firstCommands.push(describe(command))
          await renderer.command(command)
        }
        continue
      }
      if (kind === FRAME_READY) {
        result.width = view.getUint16(at + 6, true)
        result.height = view.getUint16(at + 8, true)
        if (canvas.width !== result.width) {
          canvas.width = result.width
          canvas.height = result.height
        }
      }
      if (kind === FRAME_BITMAP) {
        at += HEADER + view.getUint16(at + 6, true) * view.getUint16(at + 8, true) * 4
      } else if (kind === FRAME_ENCODED) {
        at += HEADER + ((view.getUint16(at + 10, true) + 3) & ~3)
      } else {
        at += HEADER
      }
    }
  }

  const colours = new Set<number>()
  // The middle of the screen, to say what a blank one was blank with.
  const middle = Array.from(ctx.getImageData(canvas.width >> 1, canvas.height >> 1, 1, 1).data)
  const px = ctx.getImageData(0, 0, canvas.width, canvas.height).data
  for (let i = 0; i < px.length && colours.size < 4096; i += 4 * 7) {
    colours.add(((px[i] ?? 0) << 16) | ((px[i + 1] ?? 0) << 8) | (px[i + 2] ?? 0))
  }
  renderer.close()
  return {
    ...result,
    ...renderer.stats,
    unknownCodecs: [...renderer.unknownCodecs],
    seen,
    firstCommands,
    colours: colours.size,
    middle,
  }
}

/** A few fields of a command, enough to tell what Windows asked for. */
function describe(c: Uint8Array): string {
  const v = new DataView(c.buffer, c.byteOffset, c.byteLength)
  const id = v.getUint16(0, true)
  const u16s: number[] = []
  for (let at = 8; at + 2 <= Math.min(c.byteLength, 40); at += 2) u16s.push(v.getUint16(at, true))
  return `0x${id.toString(16)} ${c.byteLength} bytes: ${u16s.join(' ')}`
}

/**
 * The GPU's AVC444v2 reconstruction against the CPU's, on a picture with hard
 * one-pixel colour edges: the largest difference in any channel, or -1 where
 * there is no WebGL 2.
 */
function checkCombiner(): number {
  const gl = Avc444Gl.create()
  if (!gl) return -1
  const w = 64
  const h = 34
  let seed = 3
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) % 256
  const plane = () => new Uint8Array(w * h).map(() => rnd())
  const { main, aux } = splitV2(w, h, plane(), plane(), plane())
  let worst = 0
  // As Windows' client assumes, and as a stream might say instead.
  for (const colour of [{ bt709: false, fullRange: true }, { bt709: true, fullRange: false }]) {
    main.colour = colour
    const want = combineV2(main, aux)
    const got = gl.read(main, aux)
    for (let i = 0; i < want.length; i++) worst = Math.max(worst, Math.abs((want[i] ?? 0) - (got[i] ?? 0)))
  }
  return worst
}

/**
 * How far the screen is from a 4:4:4 picture (8-bit Y, U, V planes, limited
 * range BT.601, as ffmpeg makes them): the mean difference per channel, and
 * the same for that picture at 4:2:0, which is what a console without the
 * second view would at best show.
 */
async function compareTruth(url: string): Promise<{ full: number; at420: number }> {
  const canvas = document.getElementById('screen') as HTMLCanvasElement
  const { width: w, height: h } = canvas
  const truth = new Uint8Array(await (await fetch(url)).arrayBuffer())
  const shown = canvas.getContext('2d')!.getImageData(0, 0, w, h).data
  const colour = { bt709: false, fullRange: false }
  const at = (plane: number, x: number, y: number) => truth[plane * w * h + y * w + x] ?? 0
  const block = (plane: number, x: number, y: number) => {
    const bx = x & ~1
    const by = y & ~1
    return Math.round((at(plane, bx, by) + at(plane, bx + 1, by) + at(plane, bx, by + 1) + at(plane, bx + 1, by + 1)) / 4)
  }
  let full = 0
  let at420 = 0
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const want = rgb(at(0, x, y), at(1, x, y), at(2, x, y), colour)
      const blurred = rgb(at(0, x, y), block(1, x, y), block(2, x, y), colour)
      for (let c = 0; c < 3; c++) {
        full += Math.abs((shown[(y * w + x) * 4 + c] ?? 0) - want[c]!)
        at420 += Math.abs(blurred[c]! - want[c]!)
      }
    }
  }
  return { full: full / (w * h * 3), at420: at420 / (w * h * 3) }
}

const w = window as unknown as {
  decodeCapture: typeof decodeCapture
  checkCombiner: typeof checkCombiner
  compareTruth: typeof compareTruth
}
w.decodeCapture = decodeCapture
w.checkCombiner = checkCombiner
w.compareTruth = compareTruth
