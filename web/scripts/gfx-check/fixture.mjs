// Builds a capture, as deskdrill -capture writes one, from testsrc.h264: the
// commands Windows sends for a desktop drawn in H.264 (AVC444v2, main view
// only), one picture per frame, the larger ones split into parts.
//
//   node scripts/gfx-check/fixture.mjs <capture> [avc444]
//
// With avc444 it is built from avc444-main.h264 and avc444-aux.h264 instead,
// the two views of a 4:4:4 picture (make-avc444.ts), sent together as
// Windows sends them: the check then compares the screen with
// avc444-truth.yuv, the last picture as it was before encoding.
//
// testsrc.h264 is ffmpeg's test pattern, 320x240, one slice per picture:
//   ffmpeg -f lavfi -i testsrc2=size=320x240:rate=10 -frames:v 12 -c:v libx264 \
//     -profile:v high -pix_fmt yuv420p -bf 0 -g 6 -tune zerolatency \
//     -x264-params sliced-threads=0:threads=1 -f h264 testsrc.h264
import { readFileSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const [out, mode] = process.argv.slice(2)
if (!out) {
  console.error('usage: fixture.mjs <capture> [avc444]')
  process.exit(2)
}
const full = mode === 'avc444'
const W = full ? 160 : 320
const H = full ? 96 : 240
// Smaller than the agent's 96 KiB, so the fixture's key frames come in parts.
const PART = 8 << 10

const here = dirname(fileURLToPath(import.meta.url))
const stream = readFileSync(join(here, full ? 'avc444-main.h264' : 'testsrc.h264'))
const auxStream = full ? readFileSync(join(here, 'avc444-aux.h264')) : null

/** Pictures: everything up to and including each slice NAL. */
function pictures(b) {
  const out = []
  let from = 0
  for (let i = 0; i + 3 < b.length; i++) {
    if (b[i] !== 0 || b[i + 1] !== 0 || b[i + 2] !== 1) continue
    const type = b[i + 3] & 0x1f
    if (type !== 1 && type !== 5) continue
    // The picture runs to the next start code, less its leading zero.
    let next = b.indexOf(Buffer.from([0, 0, 1]), i + 3)
    if (next < 0) next = b.length
    else if (b[next - 1] === 0) next--
    out.push(b.subarray(from, next))
    from = next
    i = next - 1
  }
  return out
}

const u16 = (...v) => Buffer.from(new Uint16Array(v).buffer)
const u32 = (...v) => Buffer.from(new Uint32Array(v).buffer)
const command = (cmd, body) => Buffer.concat([u16(cmd, 0), u32(8 + body.length), body])
function frame(kind, p, total = 0) {
  const pad = (4 - (p.length % 4)) % 4
  return Buffer.concat([Buffer.from([kind, 0]), u16(0), u32(p.length, total), p, Buffer.alloc(pad)])
}
function graphics(c) {
  if (c.length <= PART) return frame(6, c)
  const parts = []
  for (let at = 0; at < c.length; at += PART) parts.push(frame(7, c.subarray(at, at + PART), c.length))
  return Buffer.concat(parts)
}

const messages = [Buffer.concat([Buffer.from([3, 0]), u16(0, 0, W, H, 0)])]
messages.push(
  Buffer.concat([
    graphics(command(0x0e, Buffer.concat([u32(W, H, 1), Buffer.alloc(340 - 8 - 12)]))), // ResetGraphics
    graphics(command(0x09, Buffer.concat([u16(1, W, H), Buffer.from([0x20])]))), // CreateSurface
    // MapSurfaceToScaledOutput, at the surface's own size: what Windows
    // Server 2025 sends, rather than MapSurfaceToOutput. A console that
    // knew only the latter decoded every picture and showed none of them.
    graphics(command(0x17, Buffer.concat([u16(1, 0), u32(0, 0, W, H)]))),
    // SolidFill, blue: what a picture that never decodes would leave.
    graphics(command(0x04, Buffer.concat([u16(1), Buffer.from([255, 0, 0, 0]), u16(1, 0, 0, W, H)]))),
  ]),
)
const auxPictures = auxStream ? pictures(auxStream) : []
pictures(stream).forEach((h264, i) => {
  const view = (h) => Buffer.concat([u32(1), u16(0, 0, W, H), Buffer.from([22, 100]), h])
  const main = view(h264)
  const aux = auxPictures[i]
  const bitmap = aux
    ? Buffer.concat([u32(main.length), main, view(aux)]) // LC 0: both views
    : Buffer.concat([u32((1 << 30) >>> 0), main]) // LC 1: the main view only
  const wire = Buffer.concat([u16(1, 0x000f), Buffer.from([0x20]), u16(0, 0, W, H), u32(bitmap.length), bitmap])
  messages.push(
    Buffer.concat([
      graphics(command(0x0b, u32(0, i))), // StartFrame
      graphics(command(0x01, wire)), // WireToSurface1
      graphics(command(0x0c, u32(i))), // EndFrame
    ]),
  )
})
writeFileSync(out, Buffer.concat(messages.flatMap((m) => [u32(m.length), m])))
console.log(`${messages.length} messages`)
