import { describe, expect, it } from 'vitest'
import {
  Cmd,
  Codec,
  PartJoiner,
  hasKeyFrame,
  parseAVC420,
  parseAVC444,
  parseCommand,
  parseWireToSurface1,
} from '~/lib/gfx'
import { clip, codecString, merge } from '~/lib/gfxRenderer'

/** Little-endian bytes, built a field at a time. */
class Bytes {
  private out: number[] = []
  u8(v: number) {
    this.out.push(v & 0xff)
    return this
  }
  u16(v: number) {
    return this.u8(v).u8(v >> 8)
  }
  u32(v: number) {
    return this.u16(v & 0xffff).u16(v >>> 16)
  }
  bytes(b: ArrayLike<number>) {
    for (let i = 0; i < b.length; i++) this.out.push(b[i] ?? 0)
    return this
  }
  get(): Uint8Array {
    return new Uint8Array(this.out)
  }
}

function command(cmd: number, body: Uint8Array): Uint8Array {
  return new Bytes().u16(cmd).u16(0).u32(8 + body.length).bytes(body).get()
}

/** An AVC420 stream with the given regions around some H.264 bytes. */
function avc420(regions: number[][], h264: number[]): Uint8Array {
  const b = new Bytes().u32(regions.length)
  for (const r of regions) for (const v of r) b.u16(v)
  for (let i = 0; i < regions.length; i++) b.u8(22).u8(100)
  return b.bytes(h264).get()
}

const IDR = [0, 0, 0, 1, 0x67, 0x64, 0x00, 0x28, 0, 0, 1, 0x65, 0x88]
const SLICE = [0, 0, 0, 1, 0x41, 0x9a]

describe('graphics pipeline parsing', () => {
  it('reads a WireToSurface1 command and its AVC420 stream', () => {
    const bitmap = avc420([[10, 20, 74, 84]], IDR)
    const body = new Bytes().u16(3).u16(Codec.AVC420).u8(0x20)
    for (const v of [0, 0, 1920, 1080]) body.u16(v)
    body.u32(bitmap.length).bytes(bitmap)
    const c = parseCommand(command(Cmd.WireToSurface1, body.get()))
    expect(c?.cmd).toBe(Cmd.WireToSurface1)

    const w = parseWireToSurface1(c!.body)
    expect(w).toMatchObject({ surface: 3, codec: Codec.AVC420, dest: { left: 0, right: 1920, bottom: 1080 } })
    const avc = parseAVC420(w!.bitmap)
    expect(avc?.regions).toEqual([{ left: 10, top: 20, right: 74, bottom: 84 }])
    expect(Array.from(avc!.h264)).toEqual(IDR)
  })

  it('refuses lengths that run past the data', () => {
    const c = command(Cmd.EndFrame, new Uint8Array(4))
    expect(parseCommand(c.subarray(0, c.length - 1))).toBeNull()
    const body = new Bytes().u16(1).u16(Codec.AVC420).u8(0x20)
    for (const v of [0, 0, 8, 8]) body.u16(v)
    body.u32(100).bytes([1, 2, 3])
    expect(parseWireToSurface1(parseCommand(command(Cmd.WireToSurface1, body.get()))!.body)).toBeNull()
    expect(parseAVC420(new Bytes().u32(5).get())).toBeNull()
  })

  it('finds the main picture in AVC444, as LC says', () => {
    const main = avc420([[0, 0, 16, 16]], IDR)
    const aux = avc420([[0, 0, 16, 16]], SLICE)

    const both = new Bytes().u32(main.length).bytes(main).bytes(aux).get()
    const p = parseAVC444(both)
    expect(Array.from(p!.main!.h264)).toEqual(IDR)
    expect(Array.from(p!.aux!.h264)).toEqual(SLICE)

    const lumaOnly = new Bytes().u32((1 << 30) | 0).bytes(main).get()
    expect(parseAVC444(lumaOnly)).toMatchObject({ aux: null })
    expect(Array.from(parseAVC444(lumaOnly)!.main!.h264)).toEqual(IDR)

    const chromaOnly = new Bytes().u32((2 << 30) >>> 0).bytes(aux).get()
    expect(parseAVC444(chromaOnly)).toMatchObject({ main: null })

    const short = new Bytes().u32(main.length + 50).bytes(main).get()
    expect(parseAVC444(short)).toBeNull()
  })

  it('tells key frames from the rest', () => {
    expect(hasKeyFrame(new Uint8Array(IDR))).toBe(true)
    expect(hasKeyFrame(new Uint8Array(SLICE))).toBe(false)
    expect(hasKeyFrame(new Uint8Array([0, 0, 1]))).toBe(false)
  })

  it('reads the codec string from the parameter set', () => {
    expect(codecString(new Uint8Array(IDR))).toBe('avc1.640028')
    expect(codecString(new Uint8Array(SLICE))).toBeNull()
  })
})

describe('PartJoiner', () => {
  it('joins parts into the whole command', () => {
    const j = new PartJoiner()
    expect(j.add(new Uint8Array([1, 2, 3]), 5)).toBeNull()
    expect(Array.from(j.add(new Uint8Array([4, 5]), 5)!)).toEqual([1, 2, 3, 4, 5])
    // And starts afresh after.
    expect(Array.from(j.add(new Uint8Array([9]), 1)!)).toEqual([9])
  })

  it('drops a command whose parts disagree rather than splice two', () => {
    const j = new PartJoiner()
    j.add(new Uint8Array([1, 2]), 4)
    expect(j.add(new Uint8Array([3, 4]), 6)).toBeNull()
    expect(j.add(new Uint8Array([1, 2, 3]), 2)).toBeNull()
    expect(Array.from(j.add(new Uint8Array([7, 8]), 2)!)).toEqual([7, 8])
  })
})

describe('dirty rectangles', () => {
  it('clips to the surface', () => {
    expect(clip({ left: -5, top: 10, right: 50, bottom: 300 }, { width: 40, height: 200 })).toEqual({
      left: 0,
      top: 10,
      right: 40,
      bottom: 200,
    })
    expect(clip({ left: 50, top: 0, right: 60, bottom: 10 }, { width: 40, height: 40 })).toBeNull()
  })

  it('covers many with one', () => {
    const few = [{ left: 0, top: 0, right: 1, bottom: 1 }]
    expect(merge(few)).toBe(few)
    const many = Array.from({ length: 40 }, (_, i) => ({ left: i, top: i, right: i + 2, bottom: i + 3 }))
    expect(merge(many)).toEqual([{ left: 0, top: 0, right: 41, bottom: 42 }])
  })
})
