// Makes the AVC444v2 fixture from a 4:4:4 sequence: each picture split into
// the two views Windows would send (splitV2), each view encoded as its own
// H.264 stream, and the last picture kept as the truth to compare against.
//
//   ffmpeg -f lavfi -i testsrc2=size=160x96:rate=10 -frames:v 6 -pix_fmt yuv444p \
//     -f rawvideo src444.yuv
//   bun scripts/gfx-check/make-avc444.ts src444.yuv 160 96 scripts/gfx-check
//
// It needs ffmpeg with libx264. Its output is committed; this is how to make
// it again.
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { splitV2, type Planes } from '../../src/lib/avc444'

const [src, ws, hs, out] = process.argv.slice(2)
if (!src || !ws || !hs || !out) throw new Error('usage: make-avc444.ts <src444.yuv> <width> <height> <dir>')
const w = Number(ws)
const h = Number(hs)
const frame = w * h * 3
const all = readFileSync(src)
const main: Buffer[] = []
const aux: Buffer[] = []
const yuv420 = (p: Planes) => Buffer.concat([p.y, p.u, p.v])
for (let at = 0; at + frame <= all.length; at += frame) {
  const plane = (i: number) => new Uint8Array(all.subarray(at + i * w * h, at + (i + 1) * w * h))
  const views = splitV2(w, h, plane(0), plane(1), plane(2))
  main.push(yuv420(views.main))
  aux.push(yuv420(views.aux))
  if (at + 2 * frame > all.length) writeFileSync(join(out, 'avc444-truth.yuv'), all.subarray(at, at + frame))
}
for (const [name, frames] of [['main', main], ['aux', aux]] as const) {
  const raw = join(out, `avc444-${name}.yuv`)
  writeFileSync(raw, Buffer.concat(frames))
  execFileSync('ffmpeg', [
    '-loglevel', 'error', '-y',
    '-f', 'rawvideo', '-pix_fmt', 'yuv420p', '-s', `${w}x${h}`, '-r', '10', '-i', raw,
    '-c:v', 'libx264', '-profile:v', 'high', '-crf', '8', '-bf', '0', '-g', '6',
    '-tune', 'zerolatency', '-x264-params', 'sliced-threads=0:threads=1',
    // Said in the stream, as ffmpeg made the 4:4:4 source: BT.601, limited
    // range. A stream that says nothing is taken as BT.709 by Chrome.
    '-colorspace', 'smpte170m', '-color_primaries', 'smpte170m', '-color_trc', 'smpte170m', '-color_range', 'tv',
    '-f', 'h264', join(out, `avc444-${name}.h264`),
  ])
  execFileSync('rm', [raw])
}
