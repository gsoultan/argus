/**
 * Turns the gateway's binary display frames into ImageBitmaps.
 *
 * All of it runs off the main thread, which is the point. Decoding a screen
 * update means allocating a buffer the size of the rectangle and handing it to
 * the image decoder; doing that on the main thread stalls rendering and input
 * for exactly as long as it takes, and a busy desktop sends many updates a
 * second.
 *
 * Two properties keep memory flat regardless of how long a session runs:
 *
 *  - nothing accumulates here. A frame is decoded, transferred, and dropped.
 *    The framebuffer lives in the canvas, which is a fixed size.
 *  - buffers are transferred rather than copied. An ImageBitmap sent through
 *    postMessage with a transfer list changes owner; the worker's reference is
 *    detached, so the same pixels are never resident twice.
 */

/** Frame types, matching internal/rdp/display.go. */
const FRAME_BITMAP = 1
const FRAME_RESIZE = 2
const FRAME_READY = 3
const FRAME_CLOSED = 4

/** The header is 12 bytes so pixels start 4-byte aligned. */
const HEADER = 12

export interface DisplayRect {
  x: number
  y: number
  width: number
  height: number
  bitmap: ImageBitmap
}

export type RDPWorkerResponse =
  | { type: 'rects'; rects: DisplayRect[] }
  | { type: 'resize'; width: number; height: number }
  | { type: 'ready'; width: number; height: number }
  | { type: 'closed' }
  | { type: 'error'; message: string }

self.onmessage = async (ev: MessageEvent<ArrayBuffer>) => {
  const buf = ev.data
  if (!(buf instanceof ArrayBuffer) || buf.byteLength < HEADER) return

  const view = new DataView(buf)
  const rects: DisplayRect[] = []
  // The bitmaps are what gets transferred; the source ArrayBuffer stays here
  // and is collected.
  const transfer: Transferable[] = []

  let at = 0
  try {
    // Several rectangles arrive in one message on purpose. A desktop update is
    // often a dozen small rectangles, and one WebSocket message per rectangle
    // would cost more in framing and event dispatch than the pixels do.
    while (at + HEADER <= buf.byteLength) {
      const kind = view.getUint8(at)
      const x = view.getUint16(at + 2, true)
      const y = view.getUint16(at + 4, true)
      const width = view.getUint16(at + 6, true)
      const height = view.getUint16(at + 8, true)

      if (kind !== FRAME_BITMAP) {
        at += HEADER
        switch (kind) {
          case FRAME_RESIZE:
            post({ type: 'resize', width, height })
            break
          case FRAME_READY:
            post({ type: 'ready', width, height })
            break
          case FRAME_CLOSED:
            post({ type: 'closed' })
            break
        }
        continue
      }

      const bytes = width * height * 4
      if (bytes <= 0 || at + HEADER + bytes > buf.byteLength) {
        // A frame claiming more than the message holds is malformed. Stopping
        // is right: continuing would read the next rectangle from the middle of
        // this one's pixels.
        post({ type: 'error', message: 'display frame is truncated' })
        return
      }

      // A view, not a copy. createImageBitmap reads it synchronously into the
      // bitmap, so the underlying buffer is free immediately afterwards.
      const pixels = new Uint8ClampedArray(buf, at + HEADER, bytes)
      const bitmap = await createImageBitmap(new ImageData(pixels, width, height))
      rects.push({ x, y, width, height, bitmap })
      transfer.push(bitmap)

      at += HEADER + bytes
    }
  } catch (err) {
    post({ type: 'error', message: err instanceof Error ? err.message : String(err) })
    return
  }

  if (rects.length > 0) {
    post({ type: 'rects', rects }, transfer)
  }
}

function post(msg: RDPWorkerResponse, transfer: Transferable[] = []) {
  ;(self as unknown as Worker).postMessage(msg, transfer)
}
