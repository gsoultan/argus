import { describe, expect, it } from 'vitest'
import { addToQuery, columnStats, copyAs, emptyView, pretty, singleTable, viewRows, type Col } from '~/lib/resultView'

const cols: Col[] = [
  { name: 'id', type: 'integer', table: 7, attnum: 1 },
  { name: 'Name', type: 'text', table: 7, attnum: 2 },
  { name: 'meta', type: 'jsonb', table: 7, attnum: 3 },
]
const rows = [
  ['10', 'beta', '{"a":1}'],
  ['9', null, null],
  ['100', 'Alpha', '[1,2]'],
]

describe('the grid view', () => {
  it('sorts numbers as numbers and puts NULL last either way', () => {
    expect(viewRows(cols, rows, { ...emptyView(), sort: { col: 0, desc: false } })).toEqual([1, 0, 2])
    expect(viewRows(cols, rows, { ...emptyView(), sort: { col: 1, desc: false } })).toEqual([2, 0, 1])
    expect(viewRows(cols, rows, { ...emptyView(), sort: { col: 1, desc: true } })).toEqual([0, 2, 1])
  })

  it('filters on loaded rows', () => {
    expect(viewRows(cols, rows, { ...emptyView(), filters: { 1: { kind: 'contains', text: 'ALP' } } })).toEqual([2])
    expect(viewRows(cols, rows, { ...emptyView(), filters: { 1: { kind: 'null' } } })).toEqual([1])
  })

  it('sums up a column', () => {
    expect(columnStats(cols[0]!, rows, 0)).toMatchObject({ count: 3, nulls: 0, distinct: 3, min: '9', max: '100', sum: 119 })
    expect(columnStats(cols[1]!, rows, 1)).toMatchObject({ nulls: 1, distinct: 2, min: 'Alpha', max: 'beta' })
  })

  it('writes the sort and filters as SQL around the statement, values as parameters', () => {
    const a = addToQuery('SELECT * FROM t LIMIT 50;', cols, {
      sort: { col: 1, desc: true }, filters: { 1: { kind: 'contains', text: "o'k" }, 2: { kind: 'notnull' } }, hidden: [2],
    })
    expect(a.sql).toBe(`SELECT q.id, q."Name"\nFROM (\nSELECT * FROM t LIMIT 50\n) q\nWHERE q."Name"::text ILIKE '%' || :f_Name || '%'\n  AND q.meta IS NOT NULL\nORDER BY q."Name" DESC NULLS LAST`)
    expect(a.params).toEqual({ f_Name: "o'k" })
  })
})

describe('Copy as', () => {
  it('writes each format', () => {
    expect(copyAs('csv', cols, [['1', 'a,b', '=1+1']])).toBe('id,Name,meta\n1,"a,b",\'=1+1')
    expect(copyAs('tsv', cols.slice(0, 2), [['1', null]])).toBe('id\tName\n1\t')
    expect(JSON.parse(copyAs('json', cols, [['1', 'x', '{"a":[1]}']]))).toEqual([{ id: 1, Name: 'x', meta: { a: [1] } }])
    expect(copyAs('markdown', cols.slice(0, 2), [['1', 'a|b']])).toBe('| id | Name |\n| ---: | --- |\n| 1 | a\\|b |')
    expect(copyAs('insert', cols.slice(0, 2), [['1', "it's"], ['2', null]], 'public.users')).toBe(
      `INSERT INTO public.users (id, "Name") VALUES (1, 'it''s');\nINSERT INTO public.users (id, "Name") VALUES (2, NULL);`)
  })

  it('keeps a bigint past 2^53 as digits in JSON', () => {
    expect(copyAs('json', [{ name: 'n', type: 'bigint' }], [['9007199254740993']])).toContain('"9007199254740993"')
  })

  it('names the INSERT table only when every column came from the one table read', () => {
    expect(singleTable('SELECT * FROM public.users WHERE id = 1', cols)).toBe('public.users')
    expect(singleTable('SELECT * FROM a JOIN b ON true', cols)).toBeNull()
    expect(singleTable('SELECT * FROM a', [...cols, { name: 'x', type: 'int' }])).toBeNull()
  })

  it('pretty-prints JSON for the row panel', () => {
    expect(pretty(cols[2]!, '{"a":1}')).toEqual({ text: '{\n  "a": 1\n}', json: true })
    expect(pretty(cols[1]!, 'plain')).toEqual({ text: 'plain', json: false })
    expect(pretty(cols[1]!, null).text).toBe('NULL')
  })
})
