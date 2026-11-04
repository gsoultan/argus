import { describe, expect, it } from 'vitest'
import { deflateRawSync } from 'node:zlib'
import {
  canUpsert, csvTemplate, importStatements, mapHeader, parseCSV, planImport, xlsxTemplateSheets,
} from '~/lib/importData'
import { readXlsx, serialToISO, unzip, writeXlsx, zipStore } from '~/lib/xlsx'
import type { EditTarget } from '~/lib/rowEdit'

const orders: EditTarget = {
  table: 'public.orders', oid: 42, pk: [1],
  columns: [
    { attnum: 1, name: 'id', type: 'integer', notNull: true, hasDefault: false, generated: false, identity: '' },
    { attnum: 2, name: 'customer', type: 'text', notNull: true, hasDefault: false, generated: false, identity: '' },
    { attnum: 3, name: 'placed', type: 'date', notNull: true, hasDefault: true, generated: false, identity: '' },
    { attnum: 4, name: 'meta', type: 'jsonb', notNull: false, hasDefault: false, generated: false, identity: '' },
    { attnum: 5, name: 'twice', type: 'integer', notNull: false, hasDefault: true, generated: true, identity: '' },
    { attnum: 6, name: 'serial', type: 'bigint', notNull: true, hasDefault: true, generated: false, identity: 'a' },
  ],
}

describe('templates', () => {
  it('lists the columns that take a value', () => {
    expect(csvTemplate(orders)).toBe('﻿id,customer,placed,meta\r\n')
    const [data, about] = xlsxTemplateSheets(orders)
    expect(data!.rows).toEqual([['id', 'customer', 'placed', 'meta']])
    expect(about!.rows[1]).toEqual(['id', 'integer', 'a whole number; required'])
    expect(about!.rows[3]![2]).toBe('a date, as 2026-11-04; optional; left empty, the default is used')
  })

  it('writes a workbook that reads back', async () => {
    const book = writeXlsx(xlsxTemplateSheets(orders))
    expect([...unzip(book).keys()]).toContain('xl/worksheets/sheet2.xml')
    const back = await readXlsx(book)
    expect(back.sheet).toBe('Data')
    expect(back.rows).toEqual([['id', 'customer', 'placed', 'meta']])
  })
})

describe('reading Excel', () => {
  // A workbook as Excel writes it: compressed, shared strings, a date style.
  const xml = (s: string) => new TextEncoder().encode(s)
  const deflated = (name: string, s: string) => ({ name, data: xml(s) })
  function excelLike(): Uint8Array {
    const files = [
      deflated('xl/workbook.xml', '<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><workbookPr/><sheets><sheet name="Orders" sheetId="1" r:id="rId1"/></sheets></workbook>'),
      deflated('xl/_rels/workbook.xml.rels', '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="x" Target="worksheets/sheet1.xml"/></Relationships>'),
      deflated('xl/sharedStrings.xml', '<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>id</t></si><si><t>customer</t></si><si><t>placed</t></si><si><r><t>Ana </t></r><r><t>Sari</t></r></si></sst>'),
      deflated('xl/styles.xml', '<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><cellXfs count="2"><xf numFmtId="0"/><xf numFmtId="14"/></cellXfs></styleSheet>'),
      deflated('xl/worksheets/sheet1.xml', '<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>'
        + '<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c></row>'
        + '<row r="2"><c r="A2"><v>7</v></c><c r="B2" t="s"><v>3</v></c><c r="C2" s="1"><v>46330</v></c></row>'
        + '<row r="4"><c r="A4"><v>8</v></c><c r="C4" s="1"><v>46331</v></c></row>'
        + '</sheetData></worksheet>'),
    ]
    // Stored first, then each part's data swapped for its deflated form.
    const stored = zipStore(files)
    const out: number[] = []
    const central: number[][] = []
    let p = 0
    const v = new DataView(stored.buffer)
    for (const f of files) {
      const nameLen = v.getUint16(p + 26, true)
      const head = Array.from(stored.subarray(p, p + 30 + nameLen))
      const comp = deflateRawSync(f.data)
      const hv = new DataView(new Uint8Array(head).buffer)
      hv.setUint16(8, 8, true); hv.setUint32(18, comp.length, true)
      const at = out.length
      out.push(...new Uint8Array(hv.buffer), ...comp)
      central.push([at, comp.length, nameLen])
      p += 30 + nameLen + f.data.length
    }
    const cenStart = out.length
    const enc = new TextEncoder()
    files.forEach((f, i) => {
      const [at, size] = central[i]!
      const name = enc.encode(f.name)
      const c = new DataView(new ArrayBuffer(46))
      c.setUint32(0, 0x02014b50, true); c.setUint16(10, 8, true); c.setUint32(20, size!, true); c.setUint32(24, f.data.length, true)
      c.setUint16(28, name.length, true); c.setUint32(42, at!, true)
      out.push(...new Uint8Array(c.buffer), ...name)
    })
    const e = new DataView(new ArrayBuffer(22))
    e.setUint32(0, 0x06054b50, true); e.setUint16(8, files.length, true); e.setUint16(10, files.length, true)
    e.setUint32(12, out.length - cenStart, true); e.setUint32(16, cenStart, true)
    out.push(...new Uint8Array(e.buffer))
    return new Uint8Array(out)
  }

  it('reads shared strings, rich text, dates and gaps', async () => {
    const r = await readXlsx(excelLike())
    expect(r.sheet).toBe('Orders')
    expect(r.rows[0]).toEqual(['id', 'customer', 'placed'])
    expect(r.rows[1]).toEqual(['7', 'Ana Sari', '2026-11-04'])
    expect(r.rows[2]).toEqual([])
    expect(r.rows[3]).toEqual(['8', null, '2026-11-05'])
  })

  it('turns serials into dates and times', () => {
    expect(serialToISO(46330)).toBe('2026-11-04')
    expect(serialToISO(46330.5)).toBe('2026-11-04 12:00:00')
    expect(serialToISO(0.25)).toBe('06:00:00')
  })

  it('says when a file is not a workbook', async () => {
    await expect(readXlsx(new TextEncoder().encode('id,customer'))).rejects.toThrow(/not an Excel workbook/)
  })
})

describe('CSV', () => {
  it('reads quotes, doubled quotes, newlines in a field and empty cells', () => {
    expect(parseCSV('﻿id,note\r\n1,"a, ""b""\nc"\r\n2,\r\n\r\n')).toEqual([['id', 'note'], ['1', 'a, "b"\nc'], ['2', null]])
  })
  it('works out a semicolon or tab separator', () => {
    expect(parseCSV('id;note\n1;x')).toEqual([['id', 'note'], ['1', 'x']])
    expect(parseCSV('id\tnote\n1\tx')).toEqual([['id', 'note'], ['1', 'x']])
  })
  it('keeps a quoted empty string apart from an empty cell', () => {
    expect(parseCSV('a,b\n"",\n')).toEqual([['a', 'b'], ['', null]])
  })
})

describe('checking and importing', () => {
  it('matches the header by name and says what is missing or unknown', () => {
    const m = mapHeader(orders, ['ID', 'Placed', 'extra', 'twice'])
    expect(m.columns.map((c) => c?.name ?? null)).toEqual(['id', 'placed', null, null])
    expect(m.unknown).toEqual(['extra', 'twice'])
    expect(m.missing).toEqual(['customer'])
  })

  it('finds the values a column will not take, by file row', () => {
    const m = mapHeader(orders, ['id', 'customer', 'placed', 'meta'])
    const p = planImport(m, [['1', 'ana', '2026-11-04', '{"a":1}'], ['x', '', '04/11/2026', '{bad']])
    expect(p.problems).toEqual([
      { row: 3, column: 'id', message: 'must be a whole number' },
      { row: 3, column: 'customer', message: 'is required' },
      { row: 3, column: 'placed', message: 'must be a date like 2026-11-04' },
      { row: 3, column: 'meta', message: 'is not valid JSON' },
    ])
  })

  it('sends every value as a parameter, defaults where a cell is empty', () => {
    const m = mapHeader(orders, ['id', 'customer', 'placed', 'meta'])
    const p = planImport(m, [['1', "o'brien", null, null], ['2', 'b', '2026-11-04', '{"a":[1]}']])
    const [s] = importStatements(orders, p, 'error')
    expect(s!.sql).toBe('INSERT INTO public.orders (id, customer, placed, meta)\nVALUES (CAST($1 AS integer), CAST($2 AS text), DEFAULT, NULL),\n'
      + '       (CAST($3 AS integer), CAST($4 AS text), CAST($5 AS date), CAST($6 AS jsonb))')
    expect(s!.params).toEqual(['1', "o'brien", '2', 'b', '2026-11-04', '{"a":[1]}'])
    expect([s!.first, s!.last]).toEqual([2, 3])
    expect(canUpsert(orders, p)).toBe(true)
    expect(importStatements(orders, p, 'update')[0]!.sql).toMatch(/ON CONFLICT \(id\) DO UPDATE SET customer = EXCLUDED\.customer, placed = EXCLUDED\.placed, meta = EXCLUDED\.meta$/)
    expect(importStatements(orders, p, 'skip')[0]!.sql).toMatch(/ON CONFLICT DO NOTHING$/)
  })

  it('splits a large file into statements of a few hundred rows', () => {
    const m = mapHeader(orders, ['id', 'customer'])
    const rows = Array.from({ length: 1201 }, (_, i) => [String(i), 'c'])
    const ss = importStatements(orders, planImport(m, rows), 'error')
    expect(ss.map((s) => [s.first, s.last])).toEqual([[2, 501], [502, 1001], [1002, 1202]])
    expect(ss[2]!.params).toHaveLength(402)
  })
})
