/**
 * A minimal zip writer for "download as .zip": stored entries, no
 * compression, so it needs nothing but a CRC-32. The files are already in the
 * browser's memory by the time they are zipped; compressing them again here
 * would cost time for archives that are mostly logs and tarballs anyway.
 *
 * Limits are zip's own without ZIP64: under 65,535 entries and 4 GiB.
 */

const TABLE = (() => {
  const t = new Uint32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    t[n] = c >>> 0
  }
  return t
})()

export function crc32(data: Uint8Array): number {
  let c = 0xffffffff
  for (let i = 0; i < data.length; i++) c = TABLE[(c ^ data[i]!) & 0xff]! ^ (c >>> 8)
  return (c ^ 0xffffffff) >>> 0
}

export interface ZipEntry {
  name: string
  data: Uint8Array<ArrayBuffer>
  modified?: Date
}

/** The most a zip made here may hold, in bytes. */
export const ZIP_LIMIT = 0xffffffff - 1024 * 1024

function dosTime(d: Date): { time: number; date: number } {
  const year = Math.max(1980, d.getFullYear())
  return {
    time: (d.getHours() << 11) | (d.getMinutes() << 5) | Math.floor(d.getSeconds() / 2),
    date: ((year - 1980) << 9) | ((d.getMonth() + 1) << 5) | d.getDate(),
  }
}

export function makeZip(entries: ZipEntry[]): Blob {
  const enc = new TextEncoder()
  const parts: BlobPart[] = []
  const central: Uint8Array<ArrayBuffer>[] = []
  let offset = 0
  const used = new Set<string>()
  for (const e of entries) {
    let name = e.name
    for (let i = 1; used.has(name); i++) name = `${i}-${e.name}`
    used.add(name)
    const nameBytes = enc.encode(name)
    const crc = crc32(e.data)
    const { time, date } = dosTime(e.modified ?? new Date())
    const local = new DataView(new ArrayBuffer(30))
    local.setUint32(0, 0x04034b50, true)
    local.setUint16(4, 20, true)
    local.setUint16(6, 0x0800, true) // names are UTF-8
    local.setUint16(8, 0, true) // stored
    local.setUint16(10, time, true)
    local.setUint16(12, date, true)
    local.setUint32(14, crc, true)
    local.setUint32(18, e.data.length, true)
    local.setUint32(22, e.data.length, true)
    local.setUint16(26, nameBytes.length, true)
    local.setUint16(28, 0, true)
    parts.push(local.buffer, nameBytes, e.data)

    const c = new DataView(new ArrayBuffer(46))
    c.setUint32(0, 0x02014b50, true)
    c.setUint16(4, 20, true)
    c.setUint16(6, 20, true)
    c.setUint16(8, 0x0800, true)
    c.setUint16(10, 0, true)
    c.setUint16(12, time, true)
    c.setUint16(14, date, true)
    c.setUint32(16, crc, true)
    c.setUint32(20, e.data.length, true)
    c.setUint32(24, e.data.length, true)
    c.setUint16(28, nameBytes.length, true)
    c.setUint32(42, offset, true)
    const rec = new Uint8Array(46 + nameBytes.length)
    rec.set(new Uint8Array(c.buffer), 0)
    rec.set(nameBytes, 46)
    central.push(rec)
    offset += 30 + nameBytes.length + e.data.length
  }
  const centralSize = central.reduce((n, r) => n + r.length, 0)
  const end = new DataView(new ArrayBuffer(22))
  end.setUint32(0, 0x06054b50, true)
  end.setUint16(8, entries.length, true)
  end.setUint16(10, entries.length, true)
  end.setUint32(12, centralSize, true)
  end.setUint32(16, offset, true)
  return new Blob([...parts, ...central, end.buffer], { type: 'application/zip' })
}
