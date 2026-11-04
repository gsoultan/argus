/*
Editing rows of a result in place: what makes a result editable, and the
SQL a set of changes becomes.

A result is editable when every column came from one table (the agent says
where each column came from) and the table's primary key is among them.
Every value travels as a bind parameter, cast to the column's own type, so
nothing typed into a cell is ever SQL.

An UPDATE is keyed on the primary key and only applies while the row still
holds the values it was shown with (IS NOT DISTINCT FROM, so NULL compares
too). If someone changed the row since, it updates nothing, and the save
stops and says so instead of overwriting their change.
*/

import { ident, type Cell, type Col } from '~/lib/resultView'

/** One column of the table being edited, as pg_attribute describes it. */
export interface TableColumn {
  attnum: number
  name: string
  type: string
  notNull: boolean
  /** Postgres fills it in: a default, an identity, or a generated column. */
  hasDefault: boolean
  generated: boolean
  /** The labels of an enum type, in order. */
  enumValues?: string[]
}

export interface EditTarget {
  /** The table, as regclass prints it (quoted where it must be). */
  table: string
  oid: number
  /** Primary key attribute numbers, in key order. */
  pk: number[]
  columns: TableColumn[]
}

export type Editable =
  | { ok: true; target: EditTarget; pkCols: number[] }
  | { ok: false; reason: string }

/** The table oid every column of a result came from, or why there is none. */
export function sourceTable(cols: Col[]): { oid: number } | { reason: string } {
  if (cols.length === 0) return { reason: 'The result has no columns.' }
  const oid = cols[0]!.table
  if (!oid) return { reason: 'A column is computed, not read from a table.' }
  if (cols.some((c) => !c.table)) return { reason: 'A column is computed, not read from a table.' }
  if (cols.some((c) => c.table !== oid)) return { reason: 'The result joins more than one table.' }
  return { oid }
}

/** Whether a result can be edited in place, given its table. */
export function editable(cols: Col[], target: EditTarget | null): Editable {
  const src = sourceTable(cols)
  if ('reason' in src) return { ok: false, reason: src.reason }
  if (!target || target.oid !== src.oid) return { ok: false, reason: 'Reading the table…' }
  if (target.pk.length === 0) return { ok: false, reason: `${target.table} has no primary key, so a row cannot be named safely.` }
  const pkCols = target.pk.map((a) => cols.findIndex((c) => c.attnum === a))
  if (pkCols.some((i) => i < 0)) return { ok: false, reason: 'The result leaves out the primary key.' }
  const seen = new Set<number>()
  for (const c of cols) {
    if (seen.has(c.attnum!)) return { ok: false, reason: 'The result shows a column twice.' }
    seen.add(c.attnum!)
  }
  return { ok: true, target, pkCols }
}

/** Reads a table's columns and primary key, as the person's role. $1 is its oid. */
export const TARGET_SQL = `SELECT c.oid::regclass::text, a.attnum::text, a.attname::text,
  format_type(a.atttypid, a.atttypmod), a.attnotnull::text,
  (a.atthasdef OR a.attidentity <> '')::text, (a.attgenerated <> '')::text,
  (SELECT array_to_json(array_agg(e.enumlabel ORDER BY e.enumsortorder))::text FROM pg_enum e WHERE e.enumtypid = a.atttypid),
  (SELECT array_to_string(i.indkey, ' ') FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)
FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid
WHERE c.oid = $1::oid AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`

/** The rows of TARGET_SQL as an EditTarget. */
export function parseTarget(oid: number, rows: Cell[][]): EditTarget | null {
  if (rows.length === 0) return null
  const first = rows[0]!
  const columns: TableColumn[] = rows.map((r) => {
    let enumValues: string[] | undefined
    if (r[7]) { try { enumValues = JSON.parse(r[7]) as string[] } catch { /* none */ } }
    return {
      attnum: Number(r[1]), name: r[2] ?? '', type: r[3] ?? 'text', notNull: r[4] === 'true',
      hasDefault: r[5] === 'true', generated: r[6] === 'true', enumValues,
    }
  })
  const pk = (first[8] ?? '').split(' ').filter(Boolean).map(Number)
  return { table: first[0] ?? '', oid, pk, columns }
}

/** A statement and its values, ready to send. */
export interface Stmt { sql: string; params: (string | null)[]; label: string }

const cast = (n: number, type: string) => `CAST($${n} AS ${type})`

/**
 * The UPDATE for one changed row. `changed` maps a result column to its new
 * value; `original` is the row as the grid was given it.
 */
export function updateStmt(t: EditTarget, cols: Col[], pkCols: number[], original: Cell[], changed: Map<number, Cell>): Stmt {
  const params: (string | null)[] = []
  const typeOf = (i: number) => t.columns.find((c) => c.attnum === cols[i]!.attnum)?.type ?? cols[i]!.type
  const set: string[] = []
  const cond: string[] = []
  for (const [i, v] of changed) {
    params.push(v)
    set.push(`${ident(cols[i]!.name)} = ${cast(params.length, typeOf(i))}`)
  }
  for (const i of pkCols) {
    params.push(original[i] ?? null)
    cond.push(`${ident(cols[i]!.name)} = ${cast(params.length, typeOf(i))}`)
  }
  // Still as it was shown: someone else's change in the meantime stops it.
  // Compared as text, since some types (json, point, xml) have no equality
  // at all; the shown value goes through the column's type first, so both
  // sides take the same cast to text (boolean's is `true`, not the `t` the
  // grid was sent, and inet's adds the mask).
  for (const i of changed.keys()) {
    params.push(original[i] ?? null)
    cond.push(`${ident(cols[i]!.name)}::text IS NOT DISTINCT FROM ${cast(params.length, typeOf(i))}::text`)
  }
  const key = pkCols.map((i) => `${cols[i]!.name} ${original[i]}`).join(', ')
  return {
    sql: `UPDATE ${t.table}\nSET ${set.join(',\n    ')}\nWHERE ${cond.join('\n  AND ')}\nRETURNING *`,
    params, label: `Change ${changed.size} ${changed.size === 1 ? 'value' : 'values'} in the row with ${key}`,
  }
}

/** The INSERT for one new row. Columns left empty are left to Postgres. */
export function insertStmt(t: EditTarget, values: Map<number, Cell>): Stmt {
  const names: string[] = []
  const vals: string[] = []
  const params: (string | null)[] = []
  for (const c of t.columns) {
    if (c.generated || !values.has(c.attnum)) continue
    params.push(values.get(c.attnum) ?? null)
    names.push(ident(c.name))
    vals.push(cast(params.length, c.type))
  }
  return {
    sql: names.length
      ? `INSERT INTO ${t.table} (${names.join(', ')})\nVALUES (${vals.join(', ')})\nRETURNING *`
      : `INSERT INTO ${t.table} DEFAULT VALUES\nRETURNING *`,
    params, label: `Add a row to ${t.table}`,
  }
}

/** What a column is, for picking its editor. */
export type EditorKind = 'boolean' | 'enum' | 'json' | 'array' | 'timestamp' | 'number' | 'text'

export function editorKind(type: string, enumValues?: string[]): EditorKind {
  if (enumValues?.length) return 'enum'
  if (type === 'boolean') return 'boolean'
  if (/^jsonb?$/.test(type)) return 'json'
  if (type.endsWith('[]')) return 'array'
  if (/^(timestamp|date|time)/.test(type)) return 'timestamp'
  if (/^(smallint|integer|bigint|numeric|real|double precision|decimal)/.test(type)) return 'number'
  return 'text'
}

/**
 * The items of a one-dimensional Postgres array literal, `{a,"b c",NULL}`.
 * Returns null when it is not one (nested, or not an array).
 */
export function parseArray(v: string): (string | null)[] | null {
  const s = v.trim()
  if (!s.startsWith('{') || !s.endsWith('}')) return null
  const body = s.slice(1, -1)
  if (body === '') return []
  const out: (string | null)[] = []
  let i = 0
  while (i <= body.length) {
    if (body.charAt(i) === '{') return null
    if (body.charAt(i) === '"') {
      let j = i + 1
      let item = ''
      while (j < body.length && body.charAt(j) !== '"') {
        if (body.charAt(j) === '\\') j++
        item += body.charAt(j)
        j++
      }
      out.push(item)
      i = j + 1
    } else {
      let j = i
      while (j < body.length && body.charAt(j) !== ',') j++
      const raw = body.slice(i, j).trim()
      out.push(raw === 'NULL' ? null : raw)
      i = j
    }
    if (i >= body.length) break
    if (body.charAt(i) !== ',') return null
    i++
  }
  return out
}

/** A one-dimensional Postgres array literal for the items. */
export function formatArray(items: (string | null)[]): string {
  return `{${items.map((x) => (x === null ? 'NULL'
    : x === '' || /[{},"\\\s]/.test(x) || x.toUpperCase() === 'NULL' ? `"${x.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"` : x)).join(',')}}`
}
