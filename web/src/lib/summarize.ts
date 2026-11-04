/*
Summarize a result: group its rows by a column (or a key inside a JSON
column, or a date's day, month or year) and count them or add up a number,
for a chart or a small table. Done in the browser on the rows the grid has
loaded; summaryQuery writes the same thing for the database to do over
every row.
*/

import { ident, literal, type Cell, type Col } from '~/lib/resultView'

export type Measure = 'count' | 'sum' | 'avg' | 'min' | 'max'
export type Bucket = 'value' | 'day' | 'month' | 'year'

export interface SummarySpec {
  by: number
  /** A key inside a JSON column; dots go deeper. */
  key?: string
  bucket: Bucket
  measure: Measure
  /** The numeric column, for anything but count. */
  of?: number
}

export interface SummaryRow { label: string; value: number; rows: number }

const NUMERIC = /^(smallint|integer|bigint|numeric|real|double precision|int[248]?|float[48]|decimal|money)/
export const isNumericCol = (c: Col) => NUMERIC.test(c.type)
export const isDateCol = (c: Col) => /^(date|timestamp)/.test(c.type)
export const isJSONCol = (c: Col) => c.type === 'json' || c.type === 'jsonb'

/** The keys found in a JSON column's objects, most common first, one level deep and one more. */
export function jsonKeys(rows: Cell[][], i: number, limit = 2000): string[] {
  const counts = new Map<string, number>()
  for (const r of rows.slice(0, limit)) {
    const v = r[i]
    if (!v || v[0] !== '{') continue
    let o: unknown
    try { o = JSON.parse(v) } catch { continue }
    if (!o || typeof o !== 'object' || Array.isArray(o)) continue
    for (const [k, x] of Object.entries(o as Record<string, unknown>)) {
      counts.set(k, (counts.get(k) ?? 0) + 1)
      if (x && typeof x === 'object' && !Array.isArray(x)) {
        for (const k2 of Object.keys(x as Record<string, unknown>)) counts.set(`${k}.${k2}`, (counts.get(`${k}.${k2}`) ?? 0) + 1)
      }
    }
  }
  return [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).map(([k]) => k).slice(0, 200)
}

/** A key's value inside JSON text, as text the way ->> gives it. */
export function jsonAt(v: Cell, key: string): Cell {
  if (v === null) return null
  let o: unknown
  try { o = JSON.parse(v) } catch { return null }
  for (const k of key.split('.')) {
    if (!o || typeof o !== 'object' || Array.isArray(o)) return null
    o = (o as Record<string, unknown>)[k]
  }
  if (o === undefined || o === null) return null
  return typeof o === 'string' ? o : JSON.stringify(o)
}

function bucketOf(v: string, b: Bucket): string {
  if (b === 'value') return v
  const d = v.slice(0, 10)
  return b === 'day' ? d : b === 'month' ? d.slice(0, 7) : d.slice(0, 4)
}

/** The summary of the rows, biggest first (or in order, for dates and numbers), at most `top` groups and "Other". */
export function summarize(cols: Col[], rows: Cell[][], s: SummarySpec, top = 30): SummaryRow[] {
  const groups = new Map<string, { sum: number; n: number; rows: number; min: number; max: number }>()
  for (const r of rows) {
    let v = r[s.by] ?? null
    if (s.key) v = jsonAt(v, s.key)
    const label = v === null ? '(null)' : bucketOf(v, s.bucket)
    const g = groups.get(label) ?? { sum: 0, n: 0, rows: 0, min: Infinity, max: -Infinity }
    g.rows++
    if (s.measure !== 'count' && s.of !== undefined) {
      const x = Number(r[s.of])
      if (r[s.of] !== null && Number.isFinite(x)) { g.sum += x; g.n++; g.min = Math.min(g.min, x); g.max = Math.max(g.max, x) }
    }
    groups.set(label, g)
  }
  const value = (g: { sum: number; n: number; rows: number; min: number; max: number }) => {
    switch (s.measure) {
      case 'count': return g.rows
      case 'sum': return g.sum
      case 'avg': return g.n ? g.sum / g.n : 0
      case 'min': return g.n ? g.min : 0
      case 'max': return g.n ? g.max : 0
    }
  }
  let out = [...groups.entries()].map(([label, g]) => ({ label, value: value(g), rows: g.rows }))
  const col = cols[s.by]
  const ordered = Boolean(col && !s.key && (isDateCol(col) || isNumericCol(col)))
  if (ordered) {
    out.sort((a, b) => (isNumericCol(col!) && s.bucket === 'value' ? Number(a.label) - Number(b.label) : a.label.localeCompare(b.label)))
  } else out.sort((a, b) => b.value - a.value || a.label.localeCompare(b.label))
  if (out.length > top && !ordered) {
    const rest = out.slice(top - 1)
    const other = rest.reduce((o, x) => ({ label: 'Other', value: s.measure === 'count' || s.measure === 'sum' ? o.value + x.value : NaN, rows: o.rows + x.rows }), { label: 'Other', value: 0, rows: 0 })
    out = [...out.slice(0, top - 1), ...(Number.isNaN(other.value) ? [] : [other])]
  }
  return out
}

/** The same summary as SQL over every row of the statement. */
export function summaryQuery(sql: string, cols: Col[], s: SummarySpec): string {
  const body = sql.trim().replace(/;\s*$/, '')
  const c = cols[s.by]!
  let by = `q.${ident(c.name)}`
  if (s.key) {
    const path = s.key.split('.')
    by = path.reduce((e, k, i) => `${e}${i === path.length - 1 ? '->>' : '->'}${literal(k)}`, `q.${ident(c.name)}::jsonb`)
  }
  if (s.bucket !== 'value') by = `date_trunc(${literal(s.bucket)}, ${by})::date`
  const of = s.of !== undefined ? `q.${ident(cols[s.of]!.name)}` : ''
  const m = s.measure === 'count' ? 'count(*)' : `${s.measure}(${of})`
  const order = s.bucket !== 'value' || (!s.key && (isDateCol(c) || isNumericCol(c))) ? '1' : '2 DESC'
  return `SELECT ${by} AS ${ident(s.key ? `${c.name}_${s.key.replace(/\./g, '_')}` : c.name)}, ${m} AS ${ident(s.measure === 'count' ? 'count' : `${s.measure}_${cols[s.of!]!.name}`)}\nFROM (\n${body}\n) q\nGROUP BY 1\nORDER BY ${order};`
}
