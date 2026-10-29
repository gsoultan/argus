/**
 * Decodes a captured desktop (deskdrill -capture) in a real browser, with the
 * console's own graphics pipeline code, and draws it onto a canvas.
 *
 * Bundled by run.mjs and loaded into a page; run.mjs calls decodeCapture and
 * screenshots the canvas.
 */

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

;(window as unknown as { decodeCapture: typeof decodeCapture }).decodeCapture = decodeCapture
