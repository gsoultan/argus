import { beforeAll, describe, expect, it } from 'vitest'
import { loadPixels, type PixelsModule } from '~/lib/rdpPixels'
// Inlined as a data URL: the test runs without a server to fetch it from.
import wasmDataUrl from '~/workers/rdp-pixels.wasm?inline'

/**
 * Run against the committed module, not a mock: the .wasm is a build artefact,
 * and this is what notices it has drifted from the glue that drives it.
 */
let px: PixelsModule
beforeAll(async () => {
  const base64 = wasmDataUrl.slice(wasmDataUrl.indexOf(',') + 1)
  px = await loadPixels(Uint8Array.from(atob(base64), (c) => c.charCodeAt(0)))
})

const RLE = 0x80

describe('rdp pixel decoder', () => {
  it('flips raw rows to top-down and emits RGBA', () => {
    // 1x2 at 24bpp, bottom-up, BGR on the wire: bottom red, top blue.
    const data = new Uint8Array([0x00, 0x00, 0xff, 0xff, 0x00, 0x00])
    const out = px.decode(data, 1, 2, 24)
    expect(Array.from(out!)).toEqual([0, 0, 0xff, 0xff, 0xff, 0, 0, 0xff])
  })

  it('expands RLE', () => {
    // One row of four at 24bpp: a two-pixel colour image, then a two-pixel run.
    const data = new Uint8Array([
      (0x04 << 5) | 2, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66,
      (0x03 << 5) | 2, 0x77, 0x88, 0x99,
    ])
    const out = px.decode(data, 4, 1, 24 | RLE)
    expect(Array.from(out!)).toEqual([
      0x33, 0x22, 0x11, 0xff, 0x66, 0x55, 0x44, 0xff,
      0x99, 0x88, 0x77, 0xff, 0x99, 0x88, 0x77, 0xff,
    ])
  })

  it('refuses malformed data rather than reading past it', () => {
    // A run of 31 pixels into a 2x1 bitmap.
    expect(px.decode(new Uint8Array([(0x03 << 5) | 31, 1, 2, 3]), 2, 1, 24 | RLE)).toBeNull()
    // Too few bytes for the raw size claimed.
    expect(px.decode(new Uint8Array([1, 2]), 4, 4, 16)).toBeNull()
    // An area no screen has.
    expect(px.decode(new Uint8Array(4), 16384, 16384, 16 | RLE)).toBeNull()
  })

  it('returns an independent copy each time', () => {
    const a = px.decode(new Uint8Array([0xff, 0xff]), 1, 1, 16)!
    const b = px.decode(new Uint8Array([0x00, 0x00]), 1, 1, 16)!
    expect(Array.from(a)).toEqual([0xff, 0xff, 0xff, 0xff])
    expect(Array.from(b)).toEqual([0, 0, 0, 0xff])
  })
})
