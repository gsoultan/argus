/**
 * Moves of the screen's own pixels: how screen sharing sends a scroll.
 *
 * The agent finds where part of the screen moved up or down and sends the
 * move (FRAME_COPY, internal/rdp EncodeCopy: twelve bytes) and only what
 * scrolled into view as rectangles, instead of every tile the scroll
 * changed. Drawn live by the worker or the page, and in a replay, alike.
 */

/** Frame type, matching internal/rdp/display.go. */
export const FRAME_COPY = 9

/** (x, y + dy), width by height, drawn at (x, y). */
export interface DisplayMove {
  x: number
  y: number
  width: number
  height: number
  dy: number
}

/** Reads a FRAME_COPY frame starting at `at`. */
export function readMove(view: DataView, at: number): DisplayMove {
  return {
    x: view.getUint16(at + 2, true),
    y: view.getUint16(at + 4, true),
    width: view.getUint16(at + 6, true),
    height: view.getUint16(at + 8, true),
    dy: view.getInt16(at + 10, true),
  }
}

/**
 * Draws a move onto the canvas it reads from. A canvas drawn onto itself is
 * read as it was before the draw, so a move that overlaps itself is safe.
 */
export function drawMove(
  ctx: CanvasRenderingContext2D | OffscreenCanvasRenderingContext2D,
  canvas: HTMLCanvasElement | OffscreenCanvas,
  m: DisplayMove,
) {
  ctx.drawImage(canvas, m.x, m.y + m.dy, m.width, m.height, m.x, m.y, m.width, m.height)
}
