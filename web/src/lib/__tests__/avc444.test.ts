import { describe, expect, it } from 'vitest'
import { chromaV2, combineV2, rgb, splitV2 } from '~/lib/avc444'

/** A picture with hard one-pixel colour edges, the case AVC444 exists for. */
function picture(width: number, height: number) {
  let seed = 7
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) % 256
  const y = new Uint8Array(width * height).map(() => rnd())
  const u = new Uint8Array(width * height).map(() => rnd())
  const v = new Uint8Array(width * height).map(() => rnd())
  return { y, u, v }
}

describe('AVC444v2', () => {
  it('gives every pixel back its own colour', () => {
    // Odd heights, so the last row's cut-short blocks are exercised. Widths
    // are whole multiples of four, as H.264 pictures are: the auxiliary
    // chroma planes pack four columns' worth into each half.
    for (const [w, h] of [[16, 8], [12, 7], [8, 5]] as const) {
      const p = picture(w, h)
      const { main, aux } = splitV2(w, h, p.y, p.u, p.v)
      for (let y = 0; y < h; y++) {
        for (let x = 0; x < w; x++) {
          const [u, v] = chromaV2(main, aux, x, y)
          const i = y * w + x
          for (const [got, want, average] of [
            [u, p.u[i]!, main.u[(y >> 1) * main.chromaStride + (x >> 1)]!],
            [v, p.v[i]!, main.v[(y >> 1) * main.chromaStride + (x >> 1)]!],
          ] as const) {
            if (x % 2 === 0 && y % 2 === 0 && x + 1 < w && y + 1 < h) {
              // The one sample worked out from the average: off by the
              // average's rounding, or the average itself when close to it.
              if (got === average && Math.abs(want - average) < 32) continue
              expect(Math.abs(got - want), `(${x},${y}) ${w}x${h}`).toBeLessThanOrEqual(3)
            } else if (x % 2 === 0 && y % 2 === 0) {
              // On the picture's last row or column the block is cut short
              // and the average stands.
              expect(got).toBe(average)
            } else {
              expect(got, `(${x},${y}) ${w}x${h}`).toBe(want)
            }
          }
        }
      }
    }
  })

  it('converts as BT.601, full range', () => {
    expect(rgb(128, 128, 128)).toEqual([128, 128, 128])
    expect(rgb(76, 85, 255)).toEqual([254, 0, 0])
    expect(rgb(0, 128, 128)).toEqual([0, 0, 0])
    expect(rgb(255, 128, 128)).toEqual([255, 255, 255])
  })

  it('follows a stream that says it is limited range, or BT.709', () => {
    const limited = { bt709: false, fullRange: false }
    expect(rgb(16, 128, 128, limited)).toEqual([0, 0, 0])
    expect(rgb(235, 128, 128, limited)).toEqual([255, 255, 255])
    // Pure red, as BT.709 limited range encodes it, to within rounding.
    const [r, g, b] = rgb(63, 102, 240, { bt709: true, fullRange: false })
    expect(r).toBe(255)
    expect(g).toBeLessThanOrEqual(1)
    expect(b).toBeLessThanOrEqual(1)
  })

  it('combines into RGBA', () => {
    const w = 4
    const h = 2
    const grey = new Uint8Array(w * h).fill(128)
    const { main, aux } = splitV2(w, h, new Uint8Array(w * h).fill(200), grey, grey)
    const out = combineV2(main, aux)
    expect(out.length).toBe(w * h * 4)
    expect(Array.from(out.subarray(0, 4))).toEqual([200, 200, 200, 255])
  })
})
