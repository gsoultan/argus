import { describe, expect, it } from 'vitest'
import { FRAME_COPY, readMove } from '~/lib/displayMove'
import { FRAME_GRAPHICS, FRAME_GRAPHICS_PART } from '~/lib/gfx'
import { indexReplay, lowerBound } from '~/lib/rdpReplay'

/** One replay frame: its timestamp, then a display frame. */
function frame(ms: number, kind: number, body: number[], fields: { w?: number; h?: number; total?: number } = {}) {
  const graphics = kind === FRAME_GRAPHICS || kind === FRAME_GRAPHICS_PART
  const size = graphics ? (body.length + 3) & ~3 : body.length
  const out = new Uint8Array(4 + 12 + size)
  const v = new DataView(out.buffer)
  v.setUint32(0, ms, true)
  v.setUint8(4, kind)
  if (graphics) {
    v.setUint32(8, body.length, true)
    v.setUint32(12, fields.total ?? 0, true)
  } else {
    v.setUint16(10, fields.w ?? 0, true)
    v.setUint16(12, fields.h ?? 0, true)
  }
  out.set(body, 16)
  return out
}

function join(...parts: Uint8Array[]): ArrayBuffer {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  return out.buffer
}

describe('replay index', () => {
  it('walks bitmaps and graphics commands, padded to four bytes', () => {
    const buf = join(
      frame(0, 1, [1, 2, 3, 4, 5, 6, 7, 8], { w: 2, h: 1 }),
      frame(40, FRAME_GRAPHICS, [0xb, 0, 0, 0, 0x10, 0, 0, 0, 1]),
      frame(40, FRAME_GRAPHICS_PART, [9, 9], { total: 4 }),
      frame(80, FRAME_GRAPHICS_PART, [9, 9], { total: 4 }),
    )
    const { entries, graphics } = indexReplay(buf)
    expect(graphics).toBe(true)
    expect(entries.map((e) => [e.ms, e.kind, e.size])).toEqual([
      [0, 1, 20],
      [40, FRAME_GRAPHICS, 24],
      [40, FRAME_GRAPHICS_PART, 16],
      [80, FRAME_GRAPHICS_PART, 16],
    ])
    expect(lowerBound(entries, 40)).toBe(1)
    expect(lowerBound(entries, 41)).toBe(3)
  })

  it('keeps what came before a frame that runs past the end', () => {
    const whole = frame(0, 1, [1, 2, 3, 4], { w: 1, h: 1 })
    const cut = frame(10, FRAME_GRAPHICS, [1, 2, 3, 4, 5]).subarray(0, 18)
    const { entries, graphics } = indexReplay(join(whole, cut))
    expect(entries).toHaveLength(1)
    expect(graphics).toBe(false)
  })

  it('takes a move as its twelve bytes, and says the frames must be drawn in order', () => {
    // A move of 640x448 at (64, 0) from 40 rows below, as EncodeCopy writes it.
    const move = new Uint8Array(4 + 12)
    const v = new DataView(move.buffer)
    v.setUint32(0, 30, true)
    v.setUint8(4, FRAME_COPY)
    v.setUint16(6, 64, true)
    v.setUint16(8, 0, true)
    v.setUint16(10, 640, true)
    v.setUint16(12, 448, true)
    v.setInt16(14, -40, true)
    const buf = join(frame(0, 1, [1, 2, 3, 4], { w: 1, h: 1 }), move, frame(30, 1, [1, 2, 3, 4], { w: 1, h: 1 }))
    const { entries, graphics, moves } = indexReplay(buf)
    expect(graphics).toBe(false)
    expect(moves).toBe(true)
    expect(entries.map((e) => [e.ms, e.kind, e.size])).toEqual([
      [0, 1, 16],
      [30, FRAME_COPY, 12],
      [30, 1, 16],
    ])
    expect(readMove(new DataView(buf), entries[1]!.at)).toEqual({ x: 64, y: 0, width: 640, height: 448, dy: -40 })
  })
})
