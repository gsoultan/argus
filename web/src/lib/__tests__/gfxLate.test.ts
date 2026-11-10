import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Cmd, Codec } from '~/lib/gfx'
import { GraphicsRenderer } from '~/lib/gfxRenderer'

/*
A picture the decoder gives back after its frame stopped waiting for it
(a second) is older than the exact tiles screen sharing drew in the
meantime. Drawn over them, it was an old frame on screen that nothing
replaced. It is kept on its surface but not shown; with nothing drawn
since, as in Remote Desktop, it is shown as before.
*/

class Bytes {
  private out: number[] = []
  u8(v: number) {
    this.out.push(v & 0xff)
    return this
  }
  u16(v: number) {
    return this.u8(v).u8(v >> 8)
  }
  u32(v: number) {
    return this.u16(v & 0xffff).u16(v >>> 16)
  }
  bytes(b: ArrayLike<number>) {
    for (let i = 0; i < b.length; i++) this.out.push(b[i] ?? 0)
    return this
  }
  get(): Uint8Array {
    return new Uint8Array(this.out)
  }
}

const command = (cmd: number, body: Uint8Array) =>
  new Bytes().u16(cmd).u16(0).u32(8 + body.length).bytes(body).get()

const IDR = [0, 0, 0, 1, 0x65, 0x88, 0x84]

function picture(): Uint8Array {
  const bitmap = new Bytes().u32(1).u16(0).u16(0).u16(64).u16(64).u8(22).u8(100).bytes(IDR).get()
  const body = new Bytes().u16(1).u16(Codec.AVC420).u8(0x20)
  for (const v of [0, 0, 64, 64]) body.u16(v)
  return command(Cmd.WireToSurface1, body.u32(bitmap.length).bytes(bitmap).get())
}

let outputs: Array<(frame: unknown) => void> = []
let chunks: Array<{ timestamp: number }> = []

beforeEach(() => {
  vi.useFakeTimers()
  outputs = []
  chunks = []
  vi.stubGlobal(
    'OffscreenCanvas',
    class {
      width: number
      height: number
      constructor(w: number, h: number) {
        this.width = w
        this.height = h
      }
      getContext() {
        return { drawImage() {}, fillRect() {}, putImageData() {} }
      }
    },
  )
  vi.stubGlobal(
    'VideoDecoder',
    class {
      state = 'unconfigured'
      constructor(init: { output: (frame: unknown) => void }) {
        outputs.push(init.output)
      }
      configure() {
        this.state = 'configured'
      }
      decode(chunk: { timestamp: number }) {
        chunks.push(chunk)
      }
      close() {
        this.state = 'closed'
      }
    },
  )
  vi.stubGlobal(
    'EncodedVideoChunk',
    class {
      timestamp: number
      constructor(init: { timestamp: number }) {
        this.timestamp = init.timestamp
      }
    },
  )
  vi.stubGlobal('createImageBitmap', async () => ({ close() {} }))
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

async function lateRun(exactInBetween: boolean): Promise<number> {
  let posted = 0
  const r = new GraphicsRenderer({ rects: (rs) => (posted += rs.length), resize() {} })
  await r.command(command(Cmd.CreateSurface, new Bytes().u16(1).u16(64).u16(64).u8(0x20).get()))
  await r.command(command(Cmd.MapSurfaceToOutput, new Bytes().u16(1).u16(0).u32(0).u32(0).get()))
  await r.command(command(Cmd.EndFrame, new Bytes().u32(1).get()))
  posted = 0

  await r.command(picture())
  // The decoder holds the picture past the frame's wait.
  const end = r.command(command(Cmd.EndFrame, new Bytes().u32(2).get()))
  await vi.advanceTimersByTimeAsync(1100)
  await end
  expect(posted).toBe(0)

  if (exactInBetween) r.exactDrawn()
  const frame = {
    timestamp: chunks[0]!.timestamp,
    displayWidth: 64,
    displayHeight: 64,
    codedWidth: 64,
    codedHeight: 64,
    format: 'I420',
    colorSpace: { toJSON: () => ({}) },
    close() {},
  }
  outputs[0]!(frame)
  await r.command(command(Cmd.EndFrame, new Bytes().u32(3).get()))
  return posted
}

describe('a picture that comes out late', () => {
  it('is not shown over exact tiles drawn since', async () => {
    expect(await lateRun(true)).toBe(0)
  })

  it('is shown when nothing was drawn since', async () => {
    expect(await lateRun(false)).toBe(1)
  })
})
