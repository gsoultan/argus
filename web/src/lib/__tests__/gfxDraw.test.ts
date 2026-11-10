import { describe, expect, it } from 'vitest'
import { drawPicture, type PictureJob } from '~/lib/gfxRenderer'

/**
 * A context that draws as Safari does: a VideoFrame's source rectangle is
 * ignored (WebKit bug 260459), so the nine-argument drawImage puts the whole
 * picture into the destination rectangle. It records, for each draw, where
 * the picture's corners landed and the clip in force.
 */
function safari() {
  const draws: Array<{ x: number; y: number; w: number; h: number; clip: number[][] }> = []
  let path: number[][] = []
  let clipped: number[][] = []
  const saved: number[][][] = []
  const ctx = {
    save: () => void saved.push(clipped),
    restore: () => void (clipped = saved.pop() ?? []),
    beginPath: () => void (path = []),
    rect: (x: number, y: number, w: number, h: number) => void path.push([x, y, w, h]),
    clip: () => void (clipped = path),
    drawImage: (_image: unknown, ...a: number[]) => {
      // Whatever the arguments, the whole picture goes to the destination.
      const [x, y, w, h] = a.length === 8 ? a.slice(4) : a.length === 4 ? a : [a[0], a[1], 1920, 1080]
      draws.push({ x: x ?? 0, y: y ?? 0, w: w ?? 0, h: h ?? 0, clip: clipped })
    },
  }
  return { ctx: ctx as unknown as PictureJob['surface']['ctx'], draws }
}

const frame = { displayWidth: 1920, displayHeight: 1080 } as VideoFrame
const whole = { left: 0, top: 0, right: 1920, bottom: 1080 }

describe('drawPicture', () => {
  it('never shrinks the picture into a region, even where source rectangles are ignored', () => {
    const { ctx, draws } = safari()
    const dirty: PictureJob['surface']['dirty'] = []
    const strip = { left: 128, top: 320, right: 1792, bottom: 448 }
    const corner = { left: 0, top: 0, right: 64, bottom: 64 }
    const job = { surface: { width: 1920, height: 1080, ctx, dirty }, dest: whole, regions: [strip, corner] }

    expect(drawPicture(job, frame)).toEqual([2, 0])
    // One draw, of the picture at its own size and place.
    expect(draws).toHaveLength(1)
    expect(draws[0]).toMatchObject({ x: 0, y: 0, w: 1920, h: 1080 })
    // Only the regions take its pixels.
    expect(draws[0]?.clip).toEqual([
      [128, 320, 1664, 128],
      [0, 0, 64, 64],
    ])
    expect(dirty).toEqual([strip, corner])
  })

  it('leaves out regions that lie outside the picture', () => {
    const { ctx, draws } = safari()
    const dirty: PictureJob['surface']['dirty'] = []
    const small = { displayWidth: 640, displayHeight: 480 } as VideoFrame
    const job = {
      surface: { width: 1920, height: 1080, ctx, dirty },
      dest: whole,
      regions: [{ left: 600, top: 0, right: 700, bottom: 64 }],
    }
    expect(drawPicture(job, small)).toEqual([0, 1])
    expect(draws).toHaveLength(0)
    expect(dirty).toEqual([])
  })

  it('keeps a late picture on the surface without showing it', () => {
    const { ctx, draws } = safari()
    const dirty: PictureJob['surface']['dirty'] = []
    const job = { surface: { width: 1920, height: 1080, ctx, dirty }, dest: whole, regions: [] }
    expect(drawPicture(job, frame, false)).toEqual([1, 0])
    expect(draws).toHaveLength(1)
    expect(dirty).toEqual([])
  })
})
