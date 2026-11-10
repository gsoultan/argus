import { describe, expect, it } from 'vitest'
import { rewriteSPS, withoutReordering } from '~/lib/spsRewrite'

/** Bits, written as Exp-Golomb where asked, into a NAL unit with emulation prevention. */
function nal(header: number, fields: Array<[number, number] | ['ue', number]>): Uint8Array {
  const bits: number[] = []
  for (const [n, v] of fields) {
    if (n === 'ue') {
      const len = Math.floor(Math.log2(v + 1))
      for (let i = 0; i < len; i++) bits.push(0)
      for (let i = len; i >= 0; i--) bits.push(Math.floor((v + 1) / 2 ** i) % 2)
    } else for (let i = n - 1; i >= 0; i--) bits.push(Math.floor(v / 2 ** i) % 2)
  }
  bits.push(1)
  while (bits.length % 8) bits.push(0)
  const raw: number[] = []
  for (let i = 0; i < bits.length; i += 8) raw.push(bits.slice(i, i + 8).reduce((a, b) => a * 2 + b, 0))
  const out = [header]
  let zeros = 0
  for (const v of raw) {
    if (zeros >= 2 && v <= 3) {
      out.push(3)
      zeros = 0
    }
    out.push(v)
    zeros = v === 0 ? zeros + 1 : 0
  }
  return new Uint8Array(out)
}

/** A 1920x1080 Baseline parameter set with no VUI, as an encoder may send it. */
const baselineNoVUI = nal(0x67, [
  [8, 66],
  [8, 0xc0],
  [8, 40],
  ['ue', 0], // profile, constraints, level, id
  ['ue', 0],
  ['ue', 2], // frame_num bits, picture order type 2
  ['ue', 3],
  [1, 0],
  ['ue', 119],
  ['ue', 67], // 3 references, 120x68 macroblocks
  [1, 1],
  [1, 1],
  [1, 1],
  ['ue', 0],
  ['ue', 0],
  ['ue', 0],
  ['ue', 4], // cropped to 1080
  [1, 0], // no VUI
])

describe('withoutReordering', () => {
  // A decoder told nothing about reordering holds pictures back, so the
  // screen showed late pictures, drawn over newer pixels as old frames.
  it('says no picture is reordered where the stream said nothing', () => {
    const sps = rewriteSPS(baselineNoVUI)
    expect(sps).not.toBeNull()
    // Rewritten, it already says so: a second rewrite leaves it alone.
    expect(rewriteSPS(sps!)).toBeNull()
    // Everything before the VUI flag is as it was.
    expect(sps!.subarray(0, 8)).toEqual(baselineNoVUI.subarray(0, 8))
  })

  it('rewrites only the parameter sets in a stream, and leaves the rest', () => {
    const slice = new Uint8Array([0x65, 0x88, 0x84, 0x00, 0x33])
    const stream = new Uint8Array([0, 0, 0, 1, ...baselineNoVUI, 0, 0, 0, 1, ...slice])
    const out = withoutReordering(stream)
    expect(out).not.toBe(stream)
    expect(Array.from(out.subarray(out.length - slice.length))).toEqual(Array.from(slice))
    expect(withoutReordering(out)).toBe(out)
  })

  it('leaves a stream without parameter sets as it is', () => {
    const p = new Uint8Array([0, 0, 0, 1, 0x41, 0x9a, 0x02])
    expect(withoutReordering(p)).toBe(p)
  })

  it('leaves a parameter set it cannot read alone', () => {
    expect(rewriteSPS(new Uint8Array([0x67, 0x64]))).toBeNull()
  })
})
