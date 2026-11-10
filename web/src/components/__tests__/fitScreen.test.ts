import { describe, expect, it } from 'vitest'
import { fitScreen } from '~/components/RDPScreen'

describe('fitScreen', () => {
  // The page clips what overflows, so a screen sized by width alone lost its
  // bottom: 1920x1080 in a 1100-wide frame with 400 of height left was shown
  // 618 tall, and the last third never reached the viewer.
  it('keeps the whole screen inside the height left', () => {
    const { w, h } = fitScreen(1920, 1080, 1100, 400)
    expect(h).toBeLessThanOrEqual(400)
    expect(w).toBeLessThanOrEqual(1100)
    expect(w / h).toBeCloseTo(1920 / 1080, 1)
  })

  it('is limited by width when the frame is narrow', () => {
    expect(fitScreen(1920, 1080, 960, 2000)).toEqual({ w: 960, h: 540 })
  })

  it('never scales up past the screen itself', () => {
    expect(fitScreen(1024, 768, 3000, 3000)).toEqual({ w: 1024, h: 768 })
  })

  it('fits a phone by its height', () => {
    expect(fitScreen(1080, 2400, 1200, 600)).toEqual({ w: 270, h: 600 })
  })
})
