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
  /** 'a' for GENERATED ALWAYS AS IDENTITY, 'd' for BY DEFAULT, '' for none. */
  identity?: string
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
  (SELECT array_to_string(i.indkey, ' ') FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary),
  a.attidentity::text
FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid
WHERE c.oid = $1::oid AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`

/** TARGET_SQL for a table named schema.name in $1. */
export const TARGET_BY_NAME_SQL = TARGET_SQL.replace('c.oid = $1::oid', 'c.oid = $1::regclass')

/** The rows of TARGET_SQL as an EditTarget. */
export function parseTarget(oid: number, rows: Cell[][]): EditTarget | null {
  if (rows.length === 0) return null
  const first = rows[0]!
  const columns: TableColumn[] = rows.map((r) => {
    let enumValues: string[] | undefined
    if (r[7]) { try { enumValues = JSON.parse(r[7]) as string[] } catch { /* none */ } }
    return {
      attnum: Number(r[1]), name: r[2] ?? '', type: r[3] ?? 'text', notNull: r[4] === 'true',
      hasDefault: r[5] === 'true', generated: r[6] === 'true', identity: r[9] ?? '', enumValues,
    }
  })
  const pk = (first[8] ?? '').split(' ').filter(Boolean).map(Number)
  return { table: first[0] ?? '', oid, pk, columns }
}

/** A statement and its values, ready to send. */
export interface Stmt {
  sql: string
  params: (string | null)[]
  label: string
  /**
   * A DELETE, sent without RETURNING so the trash can keep the row: it
   * answers with the trash's own line, or only its tag when there is no trash.
   */
  deletes?: boolean
}

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

/**
 * The DELETE for one row, by its primary key. It has no RETURNING, so a
 * database with the trash keeps the row and Undo can put it back.
 */
export function deleteStmt(t: EditTarget, cols: Col[], pkCols: number[], original: Cell[]): Stmt {
  const params: (string | null)[] = []
  const typeOf = (i: number) => t.columns.find((c) => c.attnum === cols[i]!.attnum)?.type ?? cols[i]!.type
  const cond = pkCols.map((i) => `${ident(cols[i]!.name)} = ${cast(params.push(original[i] ?? null), typeOf(i))}`)
  const key = pkCols.map((i) => `${cols[i]!.name} ${original[i]}`).join(', ')
  return { sql: `DELETE FROM ${t.table}\nWHERE ${cond.join('\n  AND ')}`, params, label: `Delete the row with ${key} from ${t.table}`, deletes: true }
}

/**
 * A shown row's values as a new row of its table: the key, identity and
 * generated columns are left to Postgres, and so is anything not shown.
 */
export function duplicateValues(t: EditTarget, cols: Col[], row: Cell[]): Map<number, Cell> {
  const m = new Map<number, Cell>()
  for (const c of t.columns) {
    if (t.pk.includes(c.attnum) || c.generated || c.identity) continue
    const i = cols.findIndex((x) => x.table === t.oid && x.attnum === c.attnum)
    if (i >= 0) m.set(c.attnum, row[i] ?? null)
    else if (c.notNull && !c.hasDefault) m.set(c.attnum, '')
  }
  return m
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

/*
Editing a result table by table. In a join, each column says which table
it came from; the columns of a table whose primary key is in the result can
be edited, and a save writes each table's part of a row as its own UPDATE.
A column the query builder made from a key inside a JSON column says so in
a comment the builder writes (see jsonKeyHints), and is written back into
its document with jsonb_set.
*/

/** A key inside a JSON column that a result column shows. */
export interface JsonKeySource {
  /** The table, schema.name as the builder quoted it. */
  table: string
  column: string
  path: string[]
}

/** One table of a result that can be edited. */
export interface EditGroup {
  target: EditTarget
  /** Result columns of the key, in key order. */
  pkCols: number[]
  /** Result columns that belong to this table, keys and JSON keys included. */
  cols: number[]
  /** Result columns that show a key inside one of the table's JSON columns. */
  keys: Map<number, { column: TableColumn; path: string[] }>
}

export type EditGroups =
  | { ok: true; groups: EditGroup[]; byCol: (EditGroup | undefined)[]; why: (string | undefined)[] }
  | { ok: false; reason: string }

// A block comment, so it stays when a script is split into statements
// (leading comments are left out of a statement).
const HINT = /\/\*\s*argus:json\s+(\{.*?\})\s*\*\//s

/** The JSON key sources a builder query names in its comment, by output column. */
export function jsonKeyHints(sql: string): Record<string, JsonKeySource> {
  const m = HINT.exec(sql)
  if (!m) return {}
  try {
    const raw = JSON.parse(m[1]!) as Record<string, [string, string, string[]]>
    const out: Record<string, JsonKeySource> = {}
    for (const [name, v] of Object.entries(raw)) {
      if (Array.isArray(v) && typeof v[0] === 'string' && typeof v[1] === 'string' && Array.isArray(v[2])) out[name] = { table: v[0], column: v[1], path: v[2].map(String) }
    }
    return out
  } catch { return {} }
}

/** The comment that tells the grid where JSON key columns came from. */
export function jsonKeyComment(sources: Record<string, JsonKeySource>): string {
  const o = Object.fromEntries(Object.entries(sources).map(([k, s]) => [k, [s.table, s.column, s.path]]))
  // "*/" inside a name would end the comment; "\/" is the same JSON.
  return Object.keys(o).length ? `/* argus:json ${JSON.stringify(o).replace(/\*\//g, '*\\/')} */` : ''
}

const sentence = (r: string) => `${r.charAt(0).toUpperCase()}${r.slice(1)}${r.endsWith('.') ? '' : '.'}`

/**
 * Which tables of a result can be edited. `targets` are the tables read so
 * far by oid; `named` maps a hint's table name to its oid.
 */
export function editableGroups(cols: Col[], targets: Map<number, EditTarget>, hints: Record<string, JsonKeySource> = {}, named: Map<string, number> = new Map()): EditGroups {
  if (cols.length === 0) return { ok: false, reason: 'The result has no columns.' }
  const oids = [...new Set(cols.map((c) => c.table).filter((t): t is number => Boolean(t)))]
  for (const h of Object.values(hints)) { const o = named.get(h.table); if (o && !oids.includes(o)) oids.push(o) }
  if (oids.length === 0) return { ok: false, reason: 'A column is computed, not read from a table.' }
  if (oids.some((o) => !targets.has(o))) return { ok: false, reason: 'Reading the table…' }
  const groups: EditGroup[] = []
  const byCol: (EditGroup | undefined)[] = cols.map(() => undefined)
  const why: (string | undefined)[] = cols.map(() => undefined)
  const reasons: string[] = []
  for (const oid of oids) {
    const target = targets.get(oid)!
    const mine = cols.map((c, i) => (c.table === oid ? i : -1)).filter((i) => i >= 0)
    const keys = new Map<number, { column: TableColumn; path: string[] }>()
    cols.forEach((c, i) => {
      const h = hints[c.name]
      if (c.table || !h || named.get(h.table) !== oid) return
      const column = target.columns.find((x) => x.name === h.column)
      if (column && /^jsonb?$/.test(column.type) && h.path.length) keys.set(i, { column, path: h.path })
    })
    let reason: string | undefined
    const attnums = mine.map((i) => cols[i]!.attnum)
    if (target.pk.length === 0) reason = `${target.table} has no primary key, so a row cannot be named safely`
    else if (new Set(attnums).size !== attnums.length) reason = `the result shows a column of ${target.table} twice`
    const pkCols = target.pk.map((a) => mine.find((i) => cols[i]!.attnum === a) ?? -1)
    if (!reason && pkCols.some((i) => i < 0)) reason = `the result leaves out the primary key of ${target.table}`
    if (reason) {
      for (const i of [...mine, ...keys.keys()]) why[i] = reason
      reasons.push(reason)
      continue
    }
    const g: EditGroup = { target, pkCols, cols: [...mine, ...keys.keys()], keys }
    for (const i of g.cols) byCol[i] = g
    groups.push(g)
  }
  for (let i = 0; i < cols.length; i++) if (!byCol[i] && !why[i]) why[i] = 'computed, not read from a table'
  if (groups.length === 0) return { ok: false, reason: sentence(reasons[0] ?? 'a column is computed, not read from a table') }
  return { ok: true, groups, byCol, why }
}

/**
 * The UPDATE for one table's part of a changed row. A plain column is set
 * to its value; a JSON key is set inside its document, keeping the value's
 * JSON type where it was a number, boolean, object or array. The row must
 * still hold what it was shown with, key by key.
 */
export function groupUpdateStmt(g: EditGroup, cols: Col[], original: Cell[], changed: Map<number, Cell>): Stmt {
  const t = g.target
  const params: (string | null)[] = []
  const p = (v: Cell) => `$${params.push(v)}`
  const typeOf = (i: number) => t.columns.find((c) => c.attnum === cols[i]!.attnum)?.type ?? cols[i]!.type
  const set: string[] = []
  const cond: string[] = []
  const docs = new Map<string, { column: TableColumn; edits: [string[], Cell][] }>()
  for (const [i, v] of changed) {
    const k = g.keys.get(i)
    if (k) {
      const d = docs.get(k.column.name) ?? { column: k.column, edits: [] }
      d.edits.push([k.path, v])
      docs.set(k.column.name, d)
      continue
    }
    set.push(`${ident(cols[i]!.name)} = ${cast(params.push(v), typeOf(i))}`)
  }
  for (const d of docs.values()) {
    const col = ident(d.column.name)
    let e = `COALESCE(${col}::jsonb, '{}'::jsonb)`
    for (const [path, v] of d.edits) {
      // The value keeps the JSON type the key had. The cast to jsonb is of
      // an expression that reads the row: Postgres folds a cast of a bare
      // parameter while planning, even in a branch it will not take, and
      // "gold" is not valid JSON. One text parameter serves every use.
      const pp = p(formatArray(path))
      const pv = p(v)
      e = `jsonb_set(${e}, CAST(${pp} AS text[]), CASE WHEN CAST(${pv} AS text) IS NULL THEN 'null'::jsonb`
        + ` ELSE CAST(CASE WHEN jsonb_typeof(${col}::jsonb #> CAST(${pp} AS text[])) IN ('number', 'boolean', 'object', 'array') THEN CAST(${pv} AS text)`
        + ` ELSE to_jsonb(CAST(${pv} AS text))::text END AS jsonb) END, true)`
    }
    set.push(`${col} = ${d.column.type === 'json' ? `(${e})::json` : e}`)
  }
  for (const i of g.pkCols) cond.push(`${ident(cols[i]!.name)} = ${cast(params.push(original[i] ?? null), typeOf(i))}`)
  for (const i of changed.keys()) {
    const k = g.keys.get(i)
    if (k) cond.push(`${ident(k.column.name)}::jsonb #>> CAST(${p(formatArray(k.path))} AS text[]) IS NOT DISTINCT FROM CAST(${p(original[i] ?? null)} AS text)`)
    else cond.push(`${ident(cols[i]!.name)}::text IS NOT DISTINCT FROM ${cast(params.push(original[i] ?? null), typeOf(i))}::text`)
  }
  const key = g.pkCols.map((i) => `${cols[i]!.name} ${original[i]}`).join(', ')
  return {
    sql: `UPDATE ${t.table}\nSET ${set.join(',\n    ')}\nWHERE ${cond.join('\n  AND ')}\nRETURNING *`,
    params, label: `Change ${changed.size} ${changed.size === 1 ? 'value' : 'values'} in ${t.table}, the row with ${key}`,
  }
}
