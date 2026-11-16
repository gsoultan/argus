/**
 * The pointer on a remote screen, from mouse, pen and touch alike.
 *
 * Mouse events alone left a phone or tablet watching from its browser with
 * little to do: a tap became a click only after the finger lifted (and after
 * the browser had waited to see whether it was a double tap), and a drag
 * scrolled the page instead of reaching the screen. Pointer events carry all
 * three, as they happen:
 *
 * - One finger is the left button: pressed where it lands, at once, moved
 *   with it, released where it lifts. On a device that is a tap or a swipe.
 * - A second finger lets the button go and turns the two into a scroll
 *   wheel, a notch for every WHEEL_STEP pixels they travel together.
 * - A mouse is a mouse, as before.
 *
 * Moves are sent at most once an animation frame: a pointer reports far more
 * often than a screen refreshes. A press or release first sends the move it
 * would otherwise wait for, so a quick move and click lands where the
 * pointer is, not where it was a frame ago.
 */

export type PointerWire =
  | { type: 'mouse'; x: number; y: number }
  | { type: 'button'; button: number; down: boolean; x: number; y: number }
  | { type: 'wheel'; delta: number; x: number; y: number }

/** What is read from a PointerEvent; tests pass plain objects. */
export interface PointerLike {
  pointerId: number
  pointerType: string
  button: number
  buttons: number
  clientX: number
  clientY: number
}

/** Pixels two fingers travel for one notch of the wheel. */
export const WHEEL_STEP = 40

/** The bit in PointerEvent.buttons for PointerEvent.button. */
function buttonBit(button: number): number {
  switch (button) {
    case 1:
      return 4
    case 2:
      return 2
    default:
      return 1 << button
  }
}

export class PointerInput {
  private queued: { x: number; y: number } | null = null
  private scheduled = false
  /** Fingers on the screen, by pointer id, where each is now. */
  private touches = new Map<number, { x: number; y: number }>()
  /** The finger holding the button down, or null. */
  private pressing: number | null = null
  /** Two fingers scrolling: where their middle was, and travel not yet sent. */
  private scrolling = false
  private midY = 0
  private travel = 0

  private readonly send: (w: PointerWire) => void
  private readonly toDesktop: (p: { clientX: number; clientY: number }) => { x: number; y: number }
  private readonly schedule: (fn: () => void) => void

  constructor(
    send: (w: PointerWire) => void,
    toDesktop: (p: { clientX: number; clientY: number }) => { x: number; y: number },
    schedule: (fn: () => void) => void = (fn) => requestAnimationFrame(fn),
  ) {
    this.send = send
    this.toDesktop = toDesktop
    this.schedule = schedule
  }

  down(e: PointerLike) {
    if (e.pointerType !== 'touch') {
      this.flush()
      this.send({ type: 'button', button: e.button, down: true, ...this.toDesktop(e) })
      return
    }
    this.touches.set(e.pointerId, { x: e.clientX, y: e.clientY })
    if (this.touches.size === 1 && !this.scrolling) {
      const at = this.toDesktop(e)
      this.flush()
      this.send({ type: 'mouse', ...at })
      this.send({ type: 'button', button: 0, down: true, ...at })
      this.pressing = e.pointerId
      return
    }
    if (this.touches.size === 2) {
      // A second finger: the first one's press ends where it is, and the two
      // scroll.
      this.release()
      this.scrolling = true
      this.midY = this.middleY()
      this.travel = 0
    }
  }

  move(e: PointerLike) {
    if (e.pointerType !== 'touch') {
      if (e.button >= 0) {
        // Another button pressed or let go while one is held: no pointerdown
        // or pointerup comes for it.
        this.flush()
        this.send({
          type: 'button',
          button: e.button,
          down: (e.buttons & buttonBit(e.button)) !== 0,
          ...this.toDesktop(e),
        })
        return
      }
      this.queue(this.toDesktop(e))
      return
    }
    if (!this.touches.has(e.pointerId)) return
    this.touches.set(e.pointerId, { x: e.clientX, y: e.clientY })
    if (this.scrolling) {
      if (this.touches.size < 2) return
      const mid = this.middleY()
      // Fingers moving up move the page up: scrolling down, a positive delta.
      this.travel += this.midY - mid
      this.midY = mid
      while (Math.abs(this.travel) >= WHEEL_STEP) {
        const sign = Math.sign(this.travel)
        this.travel -= sign * WHEEL_STEP
        this.send({ type: 'wheel', delta: sign * WHEEL_STEP, ...this.toDesktop(this.middle()) })
      }
      return
    }
    if (e.pointerId === this.pressing) this.queue(this.toDesktop(e))
  }

  up(e: PointerLike) {
    if (e.pointerType !== 'touch') {
      this.flush()
      this.send({ type: 'button', button: e.button, down: false, ...this.toDesktop(e) })
      return
    }
    if (e.pointerId === this.pressing) {
      this.touches.set(e.pointerId, { x: e.clientX, y: e.clientY })
      this.release()
    }
    this.touches.delete(e.pointerId)
    if (this.touches.size === 0) this.scrolling = false
  }

  /** A pointer the browser took back (a system gesture): its press ends. */
  cancel(e: PointerLike) {
    if (e.pointerType !== 'touch') return
    this.up(e)
  }

  /** Lets go of a finger's press, where that finger is. */
  private release() {
    if (this.pressing === null) return
    const p = this.touches.get(this.pressing)
    this.pressing = null
    if (!p) return
    const at = this.toDesktop({ clientX: p.x, clientY: p.y })
    this.flush()
    this.send({ type: 'mouse', ...at })
    this.send({ type: 'button', button: 0, down: false, ...at })
  }

  private middle() {
    let x = 0
    let y = 0
    for (const p of this.touches.values()) {
      x += p.x
      y += p.y
    }
    const n = Math.max(1, this.touches.size)
    return { clientX: x / n, clientY: y / n }
  }

  private middleY() {
    return this.middle().clientY
  }

  private queue(at: { x: number; y: number }) {
    this.queued = at
    if (this.scheduled) return
    this.scheduled = true
    this.schedule(() => {
      this.scheduled = false
      this.flush()
    })
  }

  /** Sends the move waiting for the next frame, now. */
  flush() {
    const q = this.queued
    if (!q) return
    this.queued = null
    this.send({ type: 'mouse', ...q })
  }
}
