import { describe, expect, it } from 'vitest'
import { PointerInput, type PointerLike, type PointerWire, WHEEL_STEP } from '../pointerInput'

/** A PointerInput whose frames run only when the test says so. */
function setup() {
  const sent: PointerWire[] = []
  let frame: (() => void) | null = null
  const input = new PointerInput(
    (w) => sent.push(w),
    (p) => ({ x: Math.round(p.clientX), y: Math.round(p.clientY) }),
    (fn) => {
      frame = fn
    },
  )
  const runFrame = () => {
    const f = frame
    frame = null
    f?.()
  }
  return { sent, input, runFrame }
}

const touch = (id: number, x: number, y: number): PointerLike => ({
  pointerId: id, pointerType: 'touch', button: 0, buttons: 1, clientX: x, clientY: y,
})
const mouse = (x: number, y: number, button = -1, buttons = 0): PointerLike => ({
  pointerId: 1, pointerType: 'mouse', button, buttons, clientX: x, clientY: y,
})

describe('PointerInput', () => {
  it('presses where a finger lands, at once, and lets go where it lifts', () => {
    const { sent, input, runFrame } = setup()
    input.down(touch(7, 10, 20))
    expect(sent).toEqual([
      { type: 'mouse', x: 10, y: 20 },
      { type: 'button', button: 0, down: true, x: 10, y: 20 },
    ])
    sent.length = 0
    input.move(touch(7, 30, 60))
    runFrame()
    expect(sent).toEqual([{ type: 'mouse', x: 30, y: 60 }])
    sent.length = 0
    input.up(touch(7, 35, 65))
    expect(sent).toEqual([
      { type: 'mouse', x: 35, y: 65 },
      { type: 'button', button: 0, down: false, x: 35, y: 65 },
    ])
  })

  it('sends the waiting move before a click, so the click lands where the pointer is', () => {
    const { sent, input, runFrame } = setup()
    input.move(mouse(5, 5))
    input.move(mouse(50, 40))
    input.down(mouse(50, 40, 0, 1))
    expect(sent).toEqual([
      { type: 'mouse', x: 50, y: 40 },
      { type: 'button', button: 0, down: true, x: 50, y: 40 },
    ])
    // The frame that was waiting has nothing left to send.
    runFrame()
    expect(sent).toHaveLength(2)
  })

  it('sends a mouse move at most once a frame', () => {
    const { sent, input, runFrame } = setup()
    for (let i = 0; i < 10; i++) input.move(mouse(i, i))
    expect(sent).toHaveLength(0)
    runFrame()
    expect(sent).toEqual([{ type: 'mouse', x: 9, y: 9 }])
  })

  it('takes a second button pressed while one is held', () => {
    const { sent, input } = setup()
    input.down(mouse(1, 1, 0, 1))
    input.move(mouse(1, 1, 2, 3))
    input.move(mouse(1, 1, 2, 1))
    expect(sent.slice(1)).toEqual([
      { type: 'button', button: 2, down: true, x: 1, y: 1 },
      { type: 'button', button: 2, down: false, x: 1, y: 1 },
    ])
  })

  it('turns two fingers into the wheel, after letting go of the first one', () => {
    const { sent, input } = setup()
    input.down(touch(1, 100, 300))
    input.down(touch(2, 140, 300))
    expect(sent.slice(2)).toEqual([
      { type: 'mouse', x: 100, y: 300 },
      { type: 'button', button: 0, down: false, x: 100, y: 300 },
    ])
    sent.length = 0
    // Both fingers up by two and a half steps: two notches down.
    input.move(touch(1, 100, 300 - 2.5 * WHEEL_STEP))
    input.move(touch(2, 140, 300 - 2.5 * WHEEL_STEP))
    const wheels = sent.filter((w) => w.type === 'wheel')
    expect(wheels.length).toBe(2)
    expect(wheels.every((w) => w.type === 'wheel' && w.delta > 0)).toBe(true)
    // Lifting both sends no click.
    sent.length = 0
    input.up(touch(1, 100, 200))
    input.up(touch(2, 140, 200))
    expect(sent).toEqual([])
    // And the next single finger presses again.
    input.down(touch(3, 10, 10))
    expect(sent[1]).toEqual({ type: 'button', button: 0, down: true, x: 10, y: 10 })
  })

  it('lets go when the browser takes a finger back', () => {
    const { sent, input } = setup()
    input.down(touch(4, 10, 10))
    input.cancel(touch(4, 12, 12))
    expect(sent.at(-1)).toEqual({ type: 'button', button: 0, down: false, x: 12, y: 12 })
  })
})
