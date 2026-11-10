/**
 * Makes an H.264 stream say that it never reorders pictures, so a decoder
 * shows each one the moment it is decoded.
 *
 * A sequence parameter set that does not say how many pictures may be held
 * for reordering (the VUI's bitstream_restriction) leaves a decoder to
 * assume the most its level allows, and some do: Chrome's decoders, and
 * Apple's VideoToolbox under them, hold that many pictures back before
 * showing the first. A screen then lags many pictures behind what was
 * sent; the console stops waiting for a picture after a while, and when it
 * does come out it is drawn over whatever came since, as an old frame.
 *
 * Every stream drawn here has no B-frames (Windows' encoders send none for
 * Remote Desktop, nor does the agent's), so nothing is ever reordered, and
 * saying so is true. This is what WebRTC does to every H.264 stream it
 * sends, for the same reason (its SpsVuiRewriter).
 *
 * The rewrite changes only the VUI's restriction fields: max_num_reorder_frames
 * becomes 0 and max_dec_frame_buffering at least the reference frames. A
 * parameter set that already says 0, or that cannot be read, is left alone.
 */

/** Annex B with every sequence parameter set rewritten; the same array when none needed it. */
export function withoutReordering(h264: Uint8Array): Uint8Array {
  const units = nalUnits(h264)
  let changed = false
  const out: Uint8Array[] = []
  for (const u of units) {
    const body = h264.subarray(u.start, u.end)
    if (((body[0] ?? 0) & 0x1f) === 7) {
      const sps = rewriteSPS(body)
      if (sps) {
        out.push(h264.subarray(u.prefix, u.start), sps)
        changed = true
        continue
      }
    }
    out.push(h264.subarray(u.prefix, u.end))
  }
  if (!changed) return h264
  const total = out.reduce((n, p) => n + p.byteLength, 0)
  const joined = new Uint8Array(total)
  let at = 0
  for (const p of out) {
    joined.set(p, at)
    at += p.byteLength
  }
  return joined
}

/** One NAL unit: where its start code begins, and its body. */
interface Unit {
  prefix: number
  start: number
  end: number
}

function nalUnits(b: Uint8Array): Unit[] {
  const starts: Array<{ prefix: number; start: number }> = []
  for (let i = 0; i + 2 < b.byteLength; i++) {
    if (b[i] === 0 && b[i + 1] === 0 && b[i + 2] === 1) {
      const prefix = i > 0 && b[i - 1] === 0 ? i - 1 : i
      starts.push({ prefix, start: i + 3 })
      i += 2
    }
  }
  return starts.map((s, k) => ({
    ...s,
    end: k + 1 < starts.length ? (starts[k + 1]?.prefix ?? b.byteLength) : b.byteLength,
  }))
}

/**
 * A sequence parameter set NAL unit (header byte included, emulation
 * prevention in place) rewritten to say no picture is reordered, or null
 * when it already says so or cannot be read.
 */
export function rewriteSPS(nal: Uint8Array): Uint8Array | null {
  try {
    const r = new BitReader(unescape(nal.subarray(1)))
    const w = new BitWriter()
    const copy = (n: number) => w.bits(r.bits(n), n)
    const copyUE = () => {
      const v = r.ue()
      w.ue(v)
      return v
    }
    const copySE = () => w.se(r.se())

    const profile = r.bits(8)
    w.bits(profile, 8)
    copy(8) // constraint flags
    copy(8) // level
    copyUE() // seq_parameter_set_id
    if ([100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135].includes(profile)) {
      const chroma = copyUE()
      if (chroma === 3) copy(1)
      copyUE() // bit depth luma
      copyUE() // bit depth chroma
      copy(1) // qpprime_y_zero_transform_bypass
      const matrices = r.bits(1)
      w.bits(matrices, 1)
      if (matrices) {
        for (let i = 0; i < (chroma === 3 ? 12 : 8); i++) {
          const present = r.bits(1)
          w.bits(present, 1)
          if (!present) continue
          let last = 8
          let next = 8
          for (let j = 0; j < (i < 6 ? 16 : 64); j++) {
            if (next !== 0) {
              const delta = r.se()
              w.se(delta)
              next = (last + delta + 256) % 256
            }
            last = next === 0 ? last : next
          }
        }
      }
    }
    copyUE() // log2_max_frame_num_minus4
    const pocType = copyUE()
    if (pocType === 0) copyUE()
    else if (pocType === 1) {
      copy(1)
      copySE()
      copySE()
      const n = copyUE()
      for (let i = 0; i < n; i++) copySE()
    }
    const refs = copyUE()
    copy(1) // gaps_in_frame_num_value_allowed
    copyUE() // width in macroblocks
    copyUE() // height in map units
    const frameMbsOnly = r.bits(1)
    w.bits(frameMbsOnly, 1)
    if (!frameMbsOnly) copy(1)
    copy(1) // direct_8x8_inference
    const crop = r.bits(1)
    w.bits(crop, 1)
    if (crop) for (let i = 0; i < 4; i++) copyUE()

    const buffering = Math.max(refs, 1)
    const vui = r.bits(1)
    w.bits(1, 1)
    if (!vui) {
      // aspect ratio, overscan, signal type, chroma location, timing, two
      // HRDs and picture structure: none.
      w.bits(0, 8)
      writeRestriction(w, null, buffering)
    } else {
      if (r.bits(1) && w.bits(1, 1)) {
        const idc = r.bits(8)
        w.bits(idc, 8)
        if (idc === 255) copy(32)
      } else w.bits(0, 1)
      if (r.bits(1) && w.bits(1, 1)) copy(1)
      else w.bits(0, 1)
      if (r.bits(1) && w.bits(1, 1)) {
        copy(4)
        if (r.bits(1) && w.bits(1, 1)) copy(24)
        else w.bits(0, 1)
      } else w.bits(0, 1)
      if (r.bits(1) && w.bits(1, 1)) {
        copyUE()
        copyUE()
      } else w.bits(0, 1)
      if (r.bits(1) && w.bits(1, 1)) {
        copy(32)
        copy(32)
        copy(1)
      } else w.bits(0, 1)
      const hrd = () => {
        const n = copyUE()
        copy(8)
        for (let i = 0; i <= n; i++) {
          copyUE()
          copyUE()
          copy(1)
        }
        copy(20)
      }
      const nal = r.bits(1)
      w.bits(nal, 1)
      if (nal) hrd()
      const vcl = r.bits(1)
      w.bits(vcl, 1)
      if (vcl) hrd()
      if (nal || vcl) copy(1)
      copy(1) // pic_struct_present
      if (r.bits(1)) {
        const had = {
          mvOver: r.bits(1),
          bytesPerPic: r.ue(),
          bitsPerMb: r.ue(),
          mvH: r.ue(),
          mvV: r.ue(),
          reorder: r.ue(),
          buffering: r.ue(),
        }
        if (had.reorder === 0) return null
        writeRestriction(w, had, Math.max(had.buffering, buffering))
      } else writeRestriction(w, null, buffering)
    }
    w.trailing()
    return new Uint8Array([nal[0] ?? 0x67, ...escape(w.bytes())])
  } catch {
    return null
  }
}

function writeRestriction(
  w: BitWriter,
  had: {
    mvOver: number
    bytesPerPic: number
    bitsPerMb: number
    mvH: number
    mvV: number
  } | null,
  buffering: number,
) {
  w.bits(1, 1) // bitstream_restriction_flag
  w.bits(had?.mvOver ?? 1, 1)
  w.ue(had?.bytesPerPic ?? 2)
  w.ue(had?.bitsPerMb ?? 1)
  w.ue(had?.mvH ?? 16)
  w.ue(had?.mvV ?? 16)
  w.ue(0) // max_num_reorder_frames
  w.ue(buffering)
}

/** The RBSP: emulation prevention bytes (00 00 03) taken out. */
function unescape(b: Uint8Array): Uint8Array {
  const out: number[] = []
  let zeros = 0
  for (const v of b) {
    if (zeros >= 2 && v === 3) {
      zeros = 0
      continue
    }
    out.push(v)
    zeros = v === 0 ? zeros + 1 : 0
  }
  return new Uint8Array(out)
}

/** The RBSP with emulation prevention bytes put in. */
function escape(b: Uint8Array): Uint8Array {
  const out: number[] = []
  let zeros = 0
  for (const v of b) {
    if (zeros >= 2 && v <= 3) {
      out.push(3)
      zeros = 0
    }
    out.push(v)
    zeros = v === 0 ? zeros + 1 : 0
  }
  return new Uint8Array(out)
}

class BitReader {
  private at = 0
  private b: Uint8Array
  constructor(b: Uint8Array) {
    this.b = b
  }
  bits(n: number): number {
    let v = 0
    for (let i = 0; i < n; i++) {
      const byte = this.b[this.at >> 3]
      if (byte === undefined) throw new Error('the parameter set ended early')
      v = v * 2 + ((byte >> (7 - (this.at & 7))) & 1)
      this.at++
    }
    return v
  }
  ue(): number {
    let zeros = 0
    while (this.bits(1) === 0) {
      if (++zeros > 31) throw new Error('a number in the parameter set is too long')
    }
    return 2 ** zeros - 1 + this.bits(zeros)
  }
  se(): number {
    const k = this.ue()
    return k % 2 === 1 ? (k + 1) / 2 : -k / 2
  }
}

class BitWriter {
  private out: number[] = []
  private cur = 0
  private n = 0
  /** Writes v in n bits; returns v, so a flag can be copied in a condition. */
  bits(v: number, n: number): number {
    for (let i = n - 1; i >= 0; i--) this.bit(Math.floor(v / 2 ** i) % 2)
    return v
  }
  ue(v: number) {
    const len = Math.floor(Math.log2(v + 1))
    this.bits(0, len)
    this.bits(v + 1, len + 1)
  }
  se(v: number) {
    this.ue(v > 0 ? 2 * v - 1 : -2 * v)
  }
  trailing() {
    this.bit(1)
    while (this.n !== 0) this.bit(0)
  }
  bytes(): Uint8Array {
    return new Uint8Array(this.out)
  }
  private bit(v: number) {
    this.cur = (this.cur << 1) | v
    if (++this.n === 8) {
      this.out.push(this.cur)
      this.cur = 0
      this.n = 0
    }
  }
}
