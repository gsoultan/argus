/*
Just enough of .xlsx for importing into a table: write a template workbook,
and read the first sheet of one back as rows of text.

An .xlsx is a zip of XML parts. Writing stores them uncompressed, which
every spreadsheet program opens. Reading inflates them with the browser's
own DecompressionStream, so no library ships with the console for this.
*/

/* ── zip ─────────────────────────────────────────────────────────────── */

let crcTable: Uint32Array | null = null
function crc32(b: Uint8Array): number {
  if (!crcTable) {
    crcTable = new Uint32Array(256)
    for (let n = 0; n < 256; n++) {
      let c = n
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
      crcTable[n] = c >>> 0
    }
  }
  let c = 0xffffffff
  for (let i = 0; i < b.length; i++) c = crcTable[(c ^ b[i]!) & 0xff]! ^ (c >>> 8)
  return (c ^ 0xffffffff) >>> 0
}

/** A zip of the files, stored (not compressed). */
export function zipStore(files: { name: string; data: Uint8Array }[]): Uint8Array {
  const enc = new TextEncoder()
  const parts: Uint8Array[] = []
  const central: Uint8Array[] = []
  let offset = 0
  for (const f of files) {
    const name = enc.encode(f.name)
    const crc = crc32(f.data)
    const local = new Uint8Array(30 + name.length)
    const v = new DataView(local.buffer)
    v.setUint32(0, 0x04034b50, true); v.setUint16(4, 20, true); v.setUint16(6, 0x0800, true); v.setUint16(8, 0, true)
    v.setUint32(14, crc, true); v.setUint32(18, f.data.length, true); v.setUint32(22, f.data.length, true)
    v.setUint16(26, name.length, true)
    local.set(name, 30)
    const cen = new Uint8Array(46 + name.length)
    const w = new DataView(cen.buffer)
    w.setUint32(0, 0x02014b50, true); w.setUint16(4, 20, true); w.setUint16(6, 20, true); w.setUint16(8, 0x0800, true)
    w.setUint32(16, crc, true); w.setUint32(20, f.data.length, true); w.setUint32(24, f.data.length, true)
    w.setUint16(28, name.length, true); w.setUint32(42, offset, true)
    cen.set(name, 46)
    parts.push(local, f.data)
    central.push(cen)
    offset += local.length + f.data.length
  }
  const cenSize = central.reduce((n, c) => n + c.length, 0)
  const end = new Uint8Array(22)
  const e = new DataView(end.buffer)
  e.setUint32(0, 0x06054b50, true); e.setUint16(8, files.length, true); e.setUint16(10, files.length, true)
  e.setUint32(12, cenSize, true); e.setUint32(16, offset, true)
  const all = [...parts, ...central, end]
  const out = new Uint8Array(all.reduce((n, p) => n + p.length, 0))
  let at = 0
  for (const p of all) { out.set(p, at); at += p.length }
  return out
}

async function inflateRaw(data: Uint8Array): Promise<Uint8Array> {
  const ds = new DecompressionStream('deflate-raw')
  const w = ds.writable.getWriter()
  // Not awaited: the reader below is what drains it.
  void w.write(data as Uint8Array<ArrayBuffer>).then(() => w.close()).catch(() => {})
  return new Uint8Array(await new Response(ds.readable).arrayBuffer())
}

/** The files of a zip, by name, read only when asked for. */
export function unzip(buf: Uint8Array): Map<string, () => Promise<Uint8Array>> {
  const v = new DataView(buf.buffer, buf.byteOffset, buf.byteLength)
  let end = -1
  for (let i = buf.length - 22; i >= Math.max(0, buf.length - 65557); i--) {
    if (v.getUint32(i, true) === 0x06054b50) { end = i; break }
  }
  if (end < 0) throw new Error('this is not an Excel workbook (.xlsx)')
  const count = v.getUint16(end + 10, true)
  let p = v.getUint32(end + 16, true)
  const dec = new TextDecoder()
  const out = new Map<string, () => Promise<Uint8Array>>()
  for (let n = 0; n < count; n++) {
    if (v.getUint32(p, true) !== 0x02014b50) throw new Error('the workbook is damaged')
    const method = v.getUint16(p + 10, true)
    const size = v.getUint32(p + 20, true)
    const nameLen = v.getUint16(p + 28, true)
    const extra = v.getUint16(p + 30, true)
    const comment = v.getUint16(p + 32, true)
    const local = v.getUint32(p + 42, true)
    const name = dec.decode(buf.subarray(p + 46, p + 46 + nameLen))
    out.set(name, async () => {
      const start = local + 30 + v.getUint16(local + 26, true) + v.getUint16(local + 28, true)
      const data = buf.subarray(start, start + size)
      if (method === 0) return data
      if (method === 8) return inflateRaw(data)
      throw new Error('the workbook uses a compression this reader does not know')
    })
    p += 46 + nameLen + extra + comment
  }
  return out
}

/* ── writing ─────────────────────────────────────────────────────────── */

const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')
  // XML 1.0 cannot carry most control characters at all.
  // eslint-disable-next-line no-control-regex
  .replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f]/g, '')

function colName(i: number): string {
  let s = ''
  for (i++; i > 0; i = Math.floor((i - 1) / 26)) s = String.fromCharCode(65 + ((i - 1) % 26)) + s
  return s
}

function sheetXML(rows: string[][], boldFirst: boolean, widths: number[]): string {
  const cols = widths.length ? `<cols>${widths.map((w, i) => `<col min="${i + 1}" max="${i + 1}" width="${w}" customWidth="1"/>`).join('')}</cols>` : ''
  const body = rows.map((r, ri) => `<row r="${ri + 1}">${r.map((c, ci) =>
    `<c r="${colName(ci)}${ri + 1}" t="inlineStr"${boldFirst && ri === 0 ? ' s="1"' : ''}><is><t xml:space="preserve">${esc(c)}</t></is></c>`).join('')}</row>`).join('')
  return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">${boldFirst ? '<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>' : ''}${cols}<sheetData>${body}</sheetData></worksheet>`
}

/** A workbook of sheets of text, the first row of each in bold. */
export function writeXlsx(sheets: { name: string; rows: string[][] }[]): Uint8Array {
  const enc = new TextEncoder()
  const f = (name: string, s: string) => ({ name, data: enc.encode(s) })
  const ns = 'http://schemas.openxmlformats.org/'
  return zipStore([
    f('[Content_Types].xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="${ns}package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>${sheets.map((_, i) => `<Override PartName="/xl/worksheets/sheet${i + 1}.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`).join('')}</Types>`),
    f('_rels/.rels', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="${ns}package/2006/relationships"><Relationship Id="rId1" Type="${ns}officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`),
    f('xl/workbook.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="${ns}spreadsheetml/2006/main" xmlns:r="${ns}officeDocument/2006/relationships"><sheets>${sheets.map((s, i) => `<sheet name="${esc(s.name.slice(0, 31))}" sheetId="${i + 1}" r:id="rId${i + 1}"/>`).join('')}</sheets></workbook>`),
    f('xl/_rels/workbook.xml.rels', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="${ns}package/2006/relationships">${sheets.map((_, i) => `<Relationship Id="rId${i + 1}" Type="${ns}officeDocument/2006/relationships/worksheet" Target="worksheets/sheet${i + 1}.xml"/>`).join('')}<Relationship Id="rId${sheets.length + 1}" Type="${ns}officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`),
    f('xl/styles.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="${ns}spreadsheetml/2006/main"><fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts><fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills><borders count="1"><border/></borders><cellStyleXfs count="1"><xf/></cellStyleXfs><cellXfs count="2"><xf fontId="0"/><xf fontId="1" applyFont="1"/></cellXfs></styleSheet>`),
    ...sheets.map((s, i) => f(`xl/worksheets/sheet${i + 1}.xml`,
      sheetXML(s.rows, true, (s.rows[0] ?? []).map((_, c) => Math.min(60, Math.max(10, ...s.rows.map((r) => (r[c] ?? '').length + 2))))))),
  ])
}

/* ── reading ─────────────────────────────────────────────────────────── */

const BUILTIN_DATE = new Set([14, 15, 16, 17, 18, 19, 20, 21, 22, 45, 46, 47])

/** Whether a number format shows a date or a time. */
function isDateFormat(code: string): boolean {
  const bare = code.replace(/"[^"]*"|\[[^\]]*\]|\\./g, '')
  return /[dmyhs]/i.test(bare) && !/^general$/i.test(bare.trim())
}

/** A spreadsheet date serial as ISO text: a date, or a date and time. */
export function serialToISO(n: number, date1904 = false): string {
  const ms = Math.round((n + (date1904 ? 1462 : 0)) * 86400000) + Date.UTC(1899, 11, 30)
  const d = new Date(ms)
  const iso = d.toISOString()
  const timePart = iso.slice(11, 19)
  if (Number.isInteger(n)) return iso.slice(0, 10)
  if (n < 1) return timePart
  return `${iso.slice(0, 10)} ${timePart}`
}

const parseXML = (b: Uint8Array) => new DOMParser().parseFromString(new TextDecoder().decode(b), 'application/xml')
const kids = (el: Element | Document, tag: string) => Array.from(el.getElementsByTagName(tag))

/** Column number (0-based) of a cell reference like "AB12". */
function refCol(ref: string): number {
  let n = 0
  for (const ch of ref.replace(/\d+$/, '')) n = n * 26 + (ch.charCodeAt(0) - 64)
  return n - 1
}

function textOf(el: Element): string {
  // A rich string is several runs; their text in order, phonetic hints left out.
  return kids(el, 't').filter((t) => t.parentElement?.tagName !== 'rPh').map((t) => t.textContent ?? '').join('')
}

/** The first sheet of a workbook as rows of text; an empty cell is null. */
export async function readXlsx(buf: Uint8Array): Promise<{ sheet: string; rows: (string | null)[][] }> {
  const files = unzip(buf)
  const get = async (name: string) => { const f = files.get(name); return f ? parseXML(await f()) : null }
  const wb = await get('xl/workbook.xml')
  if (!wb) throw new Error('this is not an Excel workbook (.xlsx)')
  const date1904 = kids(wb, 'workbookPr')[0]?.getAttribute('date1904') === '1' || kids(wb, 'workbookPr')[0]?.getAttribute('date1904') === 'true'
  const first = kids(wb, 'sheet')[0]
  if (!first) throw new Error('the workbook has no sheets')
  const rid = first.getAttribute('r:id') ?? first.getAttributeNS('http://schemas.openxmlformats.org/officeDocument/2006/relationships', 'id')
  const rels = await get('xl/_rels/workbook.xml.rels')
  let target = 'worksheets/sheet1.xml'
  for (const r of rels ? kids(rels, 'Relationship') : []) if (r.getAttribute('Id') === rid) target = r.getAttribute('Target') ?? target
  const path = target.startsWith('/') ? target.slice(1) : `xl/${target.replace(/^\.\//, '')}`
  const sheet = await get(path)
  if (!sheet) throw new Error('the first sheet is missing from the workbook')

  const ss = await get('xl/sharedStrings.xml')
  const strings = ss ? kids(ss, 'si').map(textOf) : []
  const styles = await get('xl/styles.xml')
  const custom = new Map<number, string>()
  for (const f of styles ? kids(styles, 'numFmt') : []) custom.set(Number(f.getAttribute('numFmtId')), f.getAttribute('formatCode') ?? '')
  const xfs = styles ? kids(kids(styles, 'cellXfs')[0] ?? styles, 'xf') : []
  const dateStyle = xfs.map((x) => {
    const id = Number(x.getAttribute('numFmtId') ?? 0)
    return BUILTIN_DATE.has(id) || (custom.has(id) && isDateFormat(custom.get(id)!))
  })

  const rows: (string | null)[][] = []
  for (const row of kids(sheet, 'row')) {
    const ri = Number(row.getAttribute('r') ?? rows.length + 1) - 1
    const out: (string | null)[] = []
    let next = 0
    for (const c of kids(row, 'c')) {
      const ci = c.getAttribute('r') ? refCol(c.getAttribute('r')!) : next
      next = ci + 1
      const t = c.getAttribute('t') ?? 'n'
      const v = kids(c, 'v')[0]?.textContent ?? null
      let val: string | null
      if (t === 's') val = v === null ? null : strings[Number(v)] ?? ''
      else if (t === 'inlineStr') val = kids(c, 'is')[0] ? textOf(kids(c, 'is')[0]!) : ''
      else if (t === 'b') val = v === null ? null : v === '1' ? 'true' : 'false'
      else if (t === 'e') val = null
      else if (t === 'str' || t === 'd') val = v
      else if (v !== null && dateStyle[Number(c.getAttribute('s') ?? 0)]) val = serialToISO(Number(v), date1904)
      else val = v
      while (out.length < ci) out.push(null)
      out[ci] = val === '' ? null : val
    }
    while (rows.length < ri) rows.push([])
    rows[ri] = out
  }
  return { sheet: first.getAttribute('name') ?? 'Sheet1', rows }
}
