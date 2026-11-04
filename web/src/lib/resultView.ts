/*
What the results grid does with the rows it already has: sort, filter,
hide, sum up, and copy them out in another format. Nothing here runs a
query; "Add to the query" writes the same sort and filters as SQL around the
statement so the database does it over every row, not just the loaded ones.
*/

import { tableRefs, tokenize } from '~/lib/sqlComplete'

export type Cell = string | null
export interface Col { name: string; type: string; table?: number; attnum?: number }

export type Filter =
  | { kind: 'contains'; text: string }
  | { kind: 'equals'; text: string }
  | { kind: 'null' }
  | { kind: 'notnull' }

export interface ViewState {
  sort: { col: number; desc: boolean } | null
  filters: Record<number, Filter>
  hidden: number[]
}

export const emptyView = (): ViewState => ({ sort: null, filters: {}, hidden: [] })

const NUMERIC = /^(smallint|integer|bigint|int[248]?|numeric|decimal|real|double precision|float[48]?|money|oid)(\(|$|\[)/i

export const isNumeric = (c: Col) => NUMERIC.test(c.type) && !c.type.endsWith('[]')
export const isJSONType = (c: Col) => /^jsonb?$/i.test(c.type)

function matches(v: Cell, f: Filter): boolean {
  switch (f.kind) {
    case 'null': return v === null
    case 'notnull': return v !== null
    case 'equals': return v !== null && v === f.text
    case 'contains': return v !== null && v.toLowerCase().includes(f.text.toLowerCase())
  }
}

/** The indices of the rows to show, filtered then sorted. Nulls sort last. */
export function viewRows(cols: Col[], rows: Cell[][], v: ViewState): number[] {
  const fs = Object.entries(v.filters).map(([k, f]) => [Number(k), f] as const)
  let idx = rows.map((_, i) => i)
  if (fs.length) idx = idx.filter((i) => fs.every(([c, f]) => matches(rows[i]![c] ?? null, f)))
  const s = v.sort
  if (s) {
    const num = cols[s.col] ? isNumeric(cols[s.col]!) : false
    const coll = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })
    idx.sort((a, b) => {
      const x = rows[a]![s.col] ?? null
      const y = rows[b]![s.col] ?? null
      if (x === null || y === null) return x === y ? a - b : x === null ? 1 : -1
      const d = num ? Number(x) - Number(y) : coll.compare(x, y)
      return (s.desc ? -d : d) || a - b
    })
  }
  return idx
}

export interface ColumnStats {
  count: number
  nulls: number
  distinct: number
  min?: string
  max?: string
  sum?: number
  avg?: number
}

/** Quick figures over the loaded values of one column. */
export function columnStats(col: Col, rows: Cell[][], c: number): ColumnStats {
  const seen = new Set<string>()
  let nulls = 0
  let min: string | undefined
  let max: string | undefined
  let sum = 0
  let nums = 0
  const num = isNumeric(col)
  const coll = new Intl.Collator(undefined, { numeric: true })
  for (const r of rows) {
    const v = r[c] ?? null
    if (v === null) { nulls++; continue }
    seen.add(v)
    if (num) {
      const n = Number(v)
      if (Number.isFinite(n)) {
        sum += n
        nums++
        if (min === undefined || n < Number(min)) min = v
        if (max === undefined || n > Number(max)) max = v
      }
    } else {
      if (min === undefined || coll.compare(v, min) < 0) min = v
      if (max === undefined || coll.compare(v, max) > 0) max = v
    }
  }
  const out: ColumnStats = { count: rows.length, nulls, distinct: seen.size, min, max }
  if (num && nums) { out.sum = sum; out.avg = sum / nums }
  return out
}

/** A Postgres identifier, quoted when it has to be. */
export function ident(n: string): string {
  return /^[a-z_][a-z0-9_$]*$/.test(n) ? n : `"${n.replace(/"/g, '""')}"`
}

/** A Postgres string literal. standard_conforming_strings is on by default. */
export function literal(v: string): string {
  return `'${v.replace(/'/g, "''")}'`
}

/**
 * The statement with the grid's filters and sort, done by the database. The
 * statement is wrapped, so it works for any SELECT: a trailing semicolon and
 * a trailing LIMIT stay inside the wrap and still apply first. Values go as
 * :name parameters, so the editor's parameter fields hold them.
 */
export function addToQuery(sql: string, cols: Col[], v: ViewState): { sql: string; params: Record<string, string> } {
  const body = sql.trim().replace(/;\s*$/, '')
  const where: string[] = []
  const params: Record<string, string> = {}
  for (const [k, f] of Object.entries(v.filters)) {
    const c = cols[Number(k)]
    if (!c) continue
    const col = `q.${ident(c.name)}`
    const p = `f_${c.name.replace(/[^A-Za-z0-9_]/g, '_').replace(/^([0-9])/, '_$1')}`.slice(0, 40)
    switch (f.kind) {
      case 'null': where.push(`${col} IS NULL`); break
      case 'notnull': where.push(`${col} IS NOT NULL`); break
      case 'equals': where.push(`${col}::text = :${p}`); params[p] = f.text; break
      case 'contains': where.push(`${col}::text ILIKE '%' || :${p} || '%'`); params[p] = f.text; break
    }
  }
  const s = v.sort && cols[v.sort.col] ? `\nORDER BY q.${ident(cols[v.sort.col]!.name)}${v.sort.desc ? ' DESC' : ''} NULLS LAST` : ''
  const shown = cols.map((c, i) => (v.hidden.includes(i) ? null : `q.${ident(c.name)}`)).filter(Boolean)
  const list = v.hidden.length && shown.length ? shown.join(', ') : '*'
  return {
    sql: `SELECT ${list}\nFROM (\n${body}\n) q${where.length ? `\nWHERE ${where.join('\n  AND ')}` : ''}${s}`,
    params,
  }
}

export type CopyFormat = 'csv' | 'tsv' | 'json' | 'markdown' | 'insert'

const csvCell = (c: Cell, sep: string) => {
  if (c === null) return ''
  // A cell that starts like a formula is quoted so a spreadsheet shows it
  // rather than runs it.
  const safe = /^[=+\-@\t\r]/.test(c) ? `'${c}` : c
  return safe.includes(sep) || /["\n\r]/.test(safe) ? `"${safe.replace(/"/g, '""')}"` : safe
}

/** A JSON value for a cell: numbers, booleans and JSON columns as themselves. */
export function cellJSON(col: Col, v: Cell): unknown {
  if (v === null) return null
  if (isNumeric(col) && /^-?\d+(\.\d+)?(e[+-]?\d+)?$/i.test(v)) {
    const n = Number(v)
    // Past 2^53 a JSON number would change the value; keep the digits.
    return Number.isSafeInteger(n) || !/^-?\d+$/.test(v) ? n : v
  }
  if (col.type === 'boolean') return v === 't' || v === 'true'
  if (isJSONType(col)) { try { return JSON.parse(v) } catch { return v } }
  return v
}

/** The rows, in a format to paste somewhere else. `table` names the INSERT target. */
export function copyAs(fmt: CopyFormat, cols: Col[], rows: Cell[][], table = 'query_result'): string {
  switch (fmt) {
    case 'csv':
    case 'tsv': {
      const sep = fmt === 'csv' ? ',' : '\t'
      return [cols.map((c) => csvCell(c.name, sep)).join(sep), ...rows.map((r) => r.map((c) => csvCell(c, sep)).join(sep))].join('\n')
    }
    case 'json':
      return JSON.stringify(rows.map((r) => Object.fromEntries(cols.map((c, i) => [c.name, cellJSON(c, r[i] ?? null)]))), null, 2)
    case 'markdown': {
      const esc = (s: string) => s.replace(/\\/g, '\\\\').replace(/\|/g, '\\|').replace(/\r?\n/g, '<br>')
      return [
        `| ${cols.map((c) => esc(c.name)).join(' | ')} |`,
        `| ${cols.map((c) => (isNumeric(c) ? '---:' : '---')).join(' | ')} |`,
        ...rows.map((r) => `| ${r.map((c) => (c === null ? '' : esc(c))).join(' | ')} |`),
      ].join('\n')
    }
    case 'insert': {
      const target = table.split('.').map(ident).join('.')
      const names = cols.map((c) => ident(c.name)).join(', ')
      return rows.map((r) => `INSERT INTO ${target} (${names}) VALUES (${r.map((c, i) =>
        c === null ? 'NULL' : isNumeric(cols[i]!) && /^-?\d+(\.\d+)?$/.test(c) ? c : literal(c)).join(', ')});`).join('\n')
    }
  }
}

/**
 * The table an INSERT of these rows would go back into: the statement reads
 * one table and every column came from the same table. Otherwise null.
 */
export function singleTable(sql: string, cols: Col[]): string | null {
  const refs = tableRefs(tokenize(sql))
  if (refs.length !== 1 || cols.length === 0) return null
  const t = cols[0]!.table
  if (!t || cols.some((c) => c.table !== t)) return null
  const r = refs[0]!
  return r.schema ? `${r.schema}.${r.name}` : r.name
}

/** A value pretty-printed when it is JSON, otherwise as it is. */
export function pretty(col: Col, v: Cell): { text: string; json: boolean } {
  if (v === null) return { text: 'NULL', json: false }
  const t = v.trimStart()
  if (isJSONType(col) || ((t.startsWith('{') || t.startsWith('[')) && !col.type.endsWith('[]'))) {
    try { return { text: JSON.stringify(JSON.parse(v), null, 2), json: true } } catch { /* not JSON after all */ }
  }
  return { text: v, json: false }
}
