/**
 * Turns the gateway's binary display frames into ImageBitmaps.
 *
 * Screen updates arrive as the target sent them -- RDP's run-length encoding,
 * usually -- and are expanded here by the gateway's own decoder compiled to
 * WebAssembly (cmd/argus-pixels-wasm). Expanding them in the gateway and
 * sending RGBA cost sixteen to fifty times the bytes, which is what a slow link
 * felt as lag.
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

import { loadPixels, type PixelsModule } from '~/lib/rdpPixels'

/** Frame types, matching internal/rdp/display.go. */
const FRAME_BITMAP = 1
const FRAME_RESIZE = 2
const FRAME_READY = 3
const FRAME_CLOSED = 4
const FRAME_ENCODED = 5

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

/** The decoder, fetched once per worker and shared by every frame. */
const pixels: Promise<PixelsModule> = loadPixels(
  fetch(new URL('./rdp-pixels.wasm', import.meta.url)),
)

/**
 * Messages are handled strictly in order.
 *
 * Each one awaits createImageBitmap, and an async onmessage lets the next
 * message start while the last is still waiting -- so a later update could be
 * posted, and drawn, before an earlier one, leaving stale pixels on screen
 * until something repainted them.
 */
let queue: Promise<void> = Promise.resolve()

self.onmessage = (ev: MessageEvent<ArrayBuffer>) => {
  const buf = ev.data
  queue = queue.then(() => handle(buf))
}

async function handle(buf: ArrayBuffer) {
  if (!(buf instanceof ArrayBuffer) || buf.byteLength < HEADER) return

  const view = new DataView(buf)
  // Started together and awaited together: createImageBitmap is the slow part,
  // and a dozen of them in flight at once finish sooner than one after another.
  const pending: Array<Promise<DisplayRect>> = []

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

      if (kind === FRAME_ENCODED) {
        const format = view.getUint8(at + 1)
        const length = view.getUint16(at + 10, true)
        const padded = (length + 3) & ~3
        if (at + HEADER + padded > buf.byteLength) {
          post({ type: 'error', message: 'display frame is truncated' })
          return
        }
        const rgba = (await pixels).decode(new Uint8Array(buf, at + HEADER, length), width, height, format)
        at += HEADER + padded
        if (!rgba) {
          // One bad rectangle leaves a stale patch until it is next painted.
          // Ending the session over it would lose the operator's work for a
          // cosmetic fault.
          continue
        }
        pending.push(bitmapRect(new ImageData(rgba, width, height), x, y))
        continue
      }

      if (kind !== FRAME_BITMAP) {
        at += HEADER
        // Whatever was drawn before this frame is delivered before it, so a
        // close never overtakes the last paint.
        await flush(pending)
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
      const raw = new Uint8ClampedArray(buf, at + HEADER, bytes)
      pending.push(bitmapRect(new ImageData(raw, width, height), x, y))

      at += HEADER + bytes
    }

    await flush(pending)
  } catch (err) {
    post({ type: 'error', message: err instanceof Error ? err.message : String(err) })
  }
}

/** Posts the rectangles decoded so far and empties the list. */
async function flush(pending: Array<Promise<DisplayRect>>) {
  if (pending.length === 0) return
  const rects = await Promise.all(pending.splice(0))
  // The bitmaps are what gets transferred; the source ArrayBuffer stays here
  // and is collected.
  post({ type: 'rects', rects }, rects.map((r) => r.bitmap))
}

async function bitmapRect(image: ImageData, x: number, y: number): Promise<DisplayRect> {
  const bitmap = await createImageBitmap(image)
  return { x, y, width: image.width, height: image.height, bitmap }
}

function post(msg: RDPWorkerResponse, transfer: Transferable[] = []) {
  ;(self as unknown as Worker).postMessage(msg, transfer)
}
