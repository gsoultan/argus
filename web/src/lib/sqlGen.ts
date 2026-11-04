/*
Statements written for the person: a table's DDL, INSERT/UPDATE/DELETE/upsert
for a table or for rows of a result, and the CREATE or ALTER TABLE the table
designer describes.

Nothing here runs anything. What it writes opens in a tab, where Run treats
it like anything typed: counted first, guarded, recorded, and on production
approved. A table's template takes its values as :name parameters, so the
fields under the editor hold them; rows of a result are written out with
their values as literals, to copy or keep.
*/

import { ident, literal, type Cell, type Col } from '~/lib/resultView'
import type { EditTarget, TableColumn } from '~/lib/rowEdit'

export type DmlKind = 'select' | 'insert' | 'update' | 'delete' | 'upsert'

export const DML_LABEL: Record<DmlKind, string> = {
  select: 'SELECT', insert: 'INSERT', update: 'UPDATE', delete: 'DELETE', upsert: 'Upsert (INSERT … ON CONFLICT)',
}

/** schema.name, quoted where it must be. */
export function qualify(schema: string, name: string): string {
  return `${ident(schema)}.${ident(name)}`
}

/** A :name for a column that the editor reads as a parameter. */
function paramName(c: TableColumn, used: Set<string>): string {
  let base = /^[A-Za-z_][A-Za-z0-9_]*$/.test(c.name) ? c.name : `p${c.attnum}`
  if (used.has(base)) base = `${base}_${c.attnum}`
  used.add(base)
  return `:${base}`
}

/** Whether a column takes a value on INSERT. */
const writable = (c: TableColumn) => !c.generated && c.identity !== 'a'

/** A statement for a table, with :name parameters for its values. */
export function tableTemplate(t: EditTarget, kind: DmlKind): string {
  const used = new Set<string>()
  const p = new Map(t.columns.map((c) => [c.attnum, paramName(c, used)]))
  const pk = t.pk.map((a) => t.columns.find((c) => c.attnum === a)!).filter(Boolean)
  const nonKey = t.columns.filter((c) => !t.pk.includes(c.attnum) && !c.generated)
  const where = pk.length
    ? `WHERE ${pk.map((c) => `${ident(c.name)} = ${p.get(c.attnum)}`).join('\n  AND ')}`
    : '-- this table has no primary key: say which rows\nWHERE false'
  const ins = t.columns.filter(writable)
  const insert = `INSERT INTO ${t.table} (${ins.map((c) => ident(c.name)).join(', ')})\nVALUES (${ins.map((c) => p.get(c.attnum)).join(', ')})`
  switch (kind) {
    case 'select':
      return `SELECT ${t.columns.map((c) => ident(c.name)).join(', ')}\nFROM ${t.table}\n${where}\nLIMIT 100;`
    case 'insert':
      return `${insert}\nRETURNING *;`
    case 'update':
      return nonKey.length
        ? `UPDATE ${t.table}\nSET ${nonKey.map((c) => `${ident(c.name)} = ${p.get(c.attnum)}`).join(',\n    ')}\n${where}\nRETURNING *;`
        : `-- every column of ${t.table} is in its key or generated; there is nothing to set`
    case 'delete':
      return `DELETE FROM ${t.table}\n${where}\nRETURNING *;`
    case 'upsert': {
      if (!pk.length) return `-- ${t.table} has no primary key, so ON CONFLICT has nothing to match on\n${insert};`
      const set = nonKey.filter((c) => c.identity !== 'a')
      return `${insert}\nON CONFLICT (${pk.map((c) => ident(c.name)).join(', ')}) ${set.length
        ? `DO UPDATE SET\n    ${set.map((c) => `${ident(c.name)} = EXCLUDED.${ident(c.name)}`).join(',\n    ')}`
        : 'DO NOTHING'}\nRETURNING *;`
    }
  }
}

const NUMERIC = /^(smallint|integer|bigint|numeric|real|double precision|int[248]?|float[48]|decimal|oid)\b/

/** A value as SQL. Postgres reads an untyped literal as the column's type. */
export function sqlValue(type: string, v: Cell): string {
  if (v === null) return 'NULL'
  if (NUMERIC.test(type) && /^-?\d+(\.\d+)?(e[+-]?\d+)?$/i.test(v)) return v
  if (type === 'boolean') return v === 't' || v === 'true' ? 'TRUE' : 'FALSE'
  return literal(v)
}

/** Statements for rows of a result that came from one table with its key. */
export function rowStatements(t: EditTarget, cols: Col[], rows: Cell[][], kind: Exclude<DmlKind, 'select'>): string {
  const byAttnum = new Map(t.columns.map((c) => [c.attnum, c]))
  const shown = cols.map((c, i) => ({ i, col: byAttnum.get(c.attnum ?? -1) })).filter((x): x is { i: number; col: TableColumn } => Boolean(x.col))
  const key = t.pk.map((a) => shown.find((x) => x.col.attnum === a))
  const keyed = key.every(Boolean) && key.length > 0
  const val = (x: { i: number; col: TableColumn }, r: Cell[]) => sqlValue(x.col.type, r[x.i] ?? null)
  const where = (r: Cell[]) => `WHERE ${key.map((x) => `${ident(x!.col.name)} = ${val(x!, r)}`).join(' AND ')}`
  const ins = shown.filter((x) => !x.col.generated)
  const over = ins.some((x) => x.col.identity === 'a') ? ' OVERRIDING SYSTEM VALUE' : ''
  const insert = (r: Cell[]) => `INSERT INTO ${t.table} (${ins.map((x) => ident(x.col.name)).join(', ')})${over}\nVALUES (${ins.map((x) => val(x, r)).join(', ')})`
  const nonKey = ins.filter((x) => !t.pk.includes(x.col.attnum))
  if (kind !== 'insert' && !keyed) return `-- ${t.table}'s primary key is not in the result, so a row cannot be named`
  return rows.map((r) => {
    switch (kind) {
      case 'insert': return `${insert(r)};`
      case 'update': return nonKey.length
        ? `UPDATE ${t.table} SET ${nonKey.map((x) => `${ident(x.col.name)} = ${val(x, r)}`).join(', ')}\n${where(r)};`
        : `-- nothing to set in ${t.table} but its key`
      case 'delete': return `DELETE FROM ${t.table} ${where(r)};`
      case 'upsert': return `${insert(r)}\nON CONFLICT (${key.map((x) => ident(x!.col.name)).join(', ')}) ${nonKey.length
        ? `DO UPDATE SET ${nonKey.map((x) => `${ident(x.col.name)} = EXCLUDED.${ident(x.col.name)}`).join(', ')}`
        : 'DO NOTHING'};`
    }
  }).join('\n\n')
}

/*
DDL, written by the database from its own catalogue, as the person's role.
One statement answers with one text cell. $1 is the object, schema.name.
*/

/** A table's (or partitioned table's) CREATE TABLE, constraints, indexes and comments. */
export const TABLE_DDL_SQL = `WITH t AS (SELECT c.oid, c.relkind, c.relpersistence FROM pg_class c WHERE c.oid = $1::regclass),
cols AS (
  SELECT string_agg('    ' || quote_ident(a.attname) || ' ' || format_type(a.atttypid, a.atttypmod)
    || CASE WHEN a.attcollation <> 0 AND a.attcollation <> ty.typcollation
         THEN ' COLLATE ' || (SELECT quote_ident(n.nspname) || '.' || quote_ident(co.collname) FROM pg_collation co JOIN pg_namespace n ON n.oid = co.collnamespace WHERE co.oid = a.attcollation) ELSE '' END
    || CASE a.attidentity WHEN 'a' THEN ' GENERATED ALWAYS AS IDENTITY' WHEN 'd' THEN ' GENERATED BY DEFAULT AS IDENTITY' ELSE '' END
    || CASE WHEN a.attgenerated = 's' THEN ' GENERATED ALWAYS AS (' || pg_get_expr(d.adbin, d.adrelid) || ') STORED'
            WHEN d.adbin IS NOT NULL AND a.attidentity = '' THEN ' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid) ELSE '' END
    || CASE WHEN a.attnotnull AND a.attidentity = '' THEN ' NOT NULL' ELSE '' END,
    E',\\n' ORDER BY a.attnum) AS s
  FROM pg_attribute a JOIN pg_type ty ON ty.oid = a.atttypid
  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
  WHERE a.attrelid = (SELECT oid FROM t) AND a.attnum > 0 AND NOT a.attisdropped),
cons AS (
  SELECT string_agg('    CONSTRAINT ' || quote_ident(conname) || ' ' || pg_get_constraintdef(oid, true),
    E',\\n' ORDER BY contype = 'f', contype <> 'p', conname) AS s
  FROM pg_constraint WHERE conrelid = (SELECT oid FROM t) AND contype IN ('p', 'u', 'c', 'f', 'x') AND conislocal),
idx AS (
  SELECT string_agg(pg_get_indexdef(i.indexrelid) || ';', E'\\n' ORDER BY i.indexrelid::regclass::text) AS s
  FROM pg_index i WHERE i.indrelid = (SELECT oid FROM t)
    AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid AND k.contype IN ('p', 'u', 'x'))),
com AS (
  SELECT string_agg(x, E'\\n' ORDER BY o) AS s FROM (
    SELECT 0 AS o, 'COMMENT ON TABLE ' || (SELECT oid FROM t)::regclass::text || ' IS ' || quote_literal(d.description) || ';' AS x
      FROM pg_description d WHERE d.objoid = (SELECT oid FROM t) AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
    UNION ALL
    SELECT d.objsubid, 'COMMENT ON COLUMN ' || (SELECT oid FROM t)::regclass::text || '.' || quote_ident(a.attname) || ' IS ' || quote_literal(d.description) || ';'
      FROM pg_description d JOIN pg_attribute a ON a.attrelid = d.objoid AND a.attnum = d.objsubid
      WHERE d.objoid = (SELECT oid FROM t) AND d.classoid = 'pg_class'::regclass AND d.objsubid > 0) c)
SELECT 'CREATE ' || CASE (SELECT relpersistence FROM t) WHEN 'u' THEN 'UNLOGGED ' ELSE '' END || 'TABLE '
  || (SELECT oid FROM t)::regclass::text || E' (\\n' || concat_ws(E',\\n', cols.s, cons.s) || E'\\n)'
  || CASE WHEN (SELECT relkind FROM t) = 'p' THEN ' PARTITION BY ' || pg_get_partkeydef((SELECT oid FROM t)) ELSE '' END
  || ';' || coalesce(E'\\n\\n' || idx.s, '') || coalesce(E'\\n\\n' || com.s, '')
FROM cols, cons, idx, com`

/** A view's or materialized view's definition. */
export const VIEW_DDL_SQL = `SELECT CASE c.relkind WHEN 'm' THEN 'CREATE MATERIALIZED VIEW ' ELSE 'CREATE OR REPLACE VIEW ' END
  || c.oid::regclass::text || E' AS\\n' || rtrim(pg_get_viewdef(c.oid, true), E'; \\n')
  || CASE c.relkind WHEN 'm' THEN E'\\nWITH DATA;' ELSE ';' END
FROM pg_class c WHERE c.oid = $1::regclass`

/** Every function or procedure of that name in that schema. $1 schema, $2 name. */
export const FUNCTION_DDL_SQL = `SELECT string_agg(rtrim(pg_get_functiondef(p.oid), E'\\n') || ';', E'\\n\\n' ORDER BY p.oid)
FROM pg_proc p WHERE p.pronamespace = $1::regnamespace AND p.proname = $2 AND p.prokind IN ('f', 'p')`

/** A sequence's CREATE SEQUENCE. */
export const SEQUENCE_DDL_SQL = `SELECT 'CREATE SEQUENCE ' || $1::regclass::text || ' AS ' || format_type(s.seqtypid, NULL)
  || ' INCREMENT BY ' || s.seqincrement || ' MINVALUE ' || s.seqmin || ' MAXVALUE ' || s.seqmax
  || ' START WITH ' || s.seqstart || ' CACHE ' || s.seqcache || CASE WHEN s.seqcycle THEN ' CYCLE' ELSE ' NO CYCLE' END || ';'
FROM pg_sequence s WHERE s.seqrelid = $1::regclass`

/** What DDL a tree object kind has, and how to ask for it. */
export function ddlQuery(kind: string, schema: string, name: string): { sql: string; params: string[] } | null {
  const q = qualify(schema, name)
  switch (kind) {
    case 'table': case 'partitioned table': return { sql: TABLE_DDL_SQL, params: [q] }
    case 'view': case 'matview': return { sql: VIEW_DDL_SQL, params: [q] }
    case 'function': case 'procedure': return { sql: FUNCTION_DDL_SQL, params: [ident(schema), name] }
    case 'sequence': return { sql: SEQUENCE_DDL_SQL, params: [q] }
    default: return null
  }
}

/*
The table designer: a table as a list of columns, and the statement that
makes it, or that changes the table it was opened on into it.
*/

export interface DesignColumn {
  /** The column's name when the designer opened, for a column that exists. */
  was?: string
  name: string
  type: string
  notNull: boolean
  /** A default expression, as SQL. Empty for none. */
  default: string
  pk: boolean
  unique: boolean
}

export interface Design {
  schema: string
  name: string
  columns: DesignColumn[]
  /** The primary key constraint's name, for a table that has one. */
  pkName?: string
}

/** Reads a table as the designer edits it. $1 is schema.name. */
export const DESIGN_SQL = `SELECT a.attname::text, format_type(a.atttypid, a.atttypmod), a.attnotnull::text,
  CASE WHEN a.attidentity = 'a' THEN 'GENERATED ALWAYS AS IDENTITY' WHEN a.attidentity = 'd' THEN 'GENERATED BY DEFAULT AS IDENTITY'
       WHEN a.attgenerated = '' THEN pg_get_expr(d.adbin, d.adrelid) END,
  (SELECT k.conname::text FROM pg_constraint k WHERE k.conrelid = a.attrelid AND k.contype = 'p' AND a.attnum = ANY (k.conkey)),
  EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conrelid = a.attrelid AND k.contype = 'u' AND k.conkey = ARRAY[a.attnum])::text,
  (a.attgenerated <> '')::text
FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`

/** DESIGN_SQL's rows as a design. Generated columns are left out: the designer does not change them. */
export function parseDesign(schema: string, name: string, rows: Cell[][]): Design {
  let pkName: string | undefined
  const columns = rows.filter((r) => r[6] !== 'true').map((r) => {
    if (r[4]) pkName = r[4]
    return { was: r[0] ?? '', name: r[0] ?? '', type: r[1] ?? 'text', notNull: r[2] === 'true', default: r[3] ?? '', pk: Boolean(r[4]), unique: r[5] === 'true' }
  })
  return { schema, name, columns, pkName }
}

const IDENTITY = /^GENERATED (ALWAYS|BY DEFAULT) AS IDENTITY$/

function columnDef(c: DesignColumn, inlineKey: boolean): string {
  const d = c.default.trim()
  return `${ident(c.name)} ${c.type.trim()}${IDENTITY.test(d) ? ` ${d}` : d ? ` DEFAULT ${d}` : ''}`
    + `${c.notNull && !c.pk && !IDENTITY.test(d) ? ' NOT NULL' : ''}${inlineKey && c.pk ? ' PRIMARY KEY' : ''}${c.unique && !c.pk ? ' UNIQUE' : ''}`
}

/** What is wrong with a design, if anything, in words. */
export function designProblems(d: Design): string[] {
  const out: string[] = []
  if (!d.name.trim()) out.push('The table needs a name.')
  if (d.columns.length === 0) out.push('A table needs at least one column.')
  const seen = new Set<string>()
  d.columns.forEach((c, i) => {
    if (!c.name.trim()) out.push(`Column ${i + 1} needs a name.`)
    else if (seen.has(c.name)) out.push(`Two columns are called ${c.name}.`)
    seen.add(c.name)
    if (!c.type.trim()) out.push(`${c.name || `Column ${i + 1}`} needs a type.`)
    if (/;/.test(c.type) || /;/.test(c.default)) out.push(`${c.name || `Column ${i + 1}`}: a type or default cannot hold a semicolon.`)
  })
  return out
}

/** The CREATE TABLE a design describes. */
export function createTable(d: Design): string {
  const keys = d.columns.filter((c) => c.pk)
  const inline = keys.length === 1
  const lines = d.columns.map((c) => `    ${columnDef(c, inline)}`)
  if (keys.length > 1) lines.push(`    PRIMARY KEY (${keys.map((c) => ident(c.name)).join(', ')})`)
  return `CREATE TABLE ${qualify(d.schema, d.name)} (\n${lines.join(',\n')}\n);`
}

/**
 * The ALTER TABLE statements that change `from` into `to`, in an order that
 * works: renames first, then drops, changes and adds, the key last. Each is
 * its own statement, so Run checks and records each one.
 */
export function alterTable(from: Design, to: Design): string[] {
  const t = qualify(from.schema, from.name)
  const out: string[] = []
  const before = new Map(from.columns.map((c) => [c.was ?? c.name, c]))
  const kept = to.columns.filter((c) => c.was !== undefined && before.has(c.was))
  for (const c of kept) if (c.name !== c.was) out.push(`ALTER TABLE ${t} RENAME COLUMN ${ident(c.was!)} TO ${ident(c.name)};`)
  const keptNames = new Set(kept.map((c) => c.was))
  for (const c of from.columns) if (!keptNames.has(c.was ?? c.name)) out.push(`ALTER TABLE ${t} DROP COLUMN ${ident(c.name)};`)
  const pkFrom = from.columns.filter((c) => c.pk).map((c) => c.was ?? c.name)
  const pkTo = to.columns.filter((c) => c.pk).map((c) => c.was ?? c.name)
  const keyChanged = pkFrom.join('\u0000') !== pkTo.join('\u0000') || to.columns.some((c) => c.pk && c.was === undefined)
  if (keyChanged && from.pkName) out.push(`ALTER TABLE ${t} DROP CONSTRAINT ${ident(from.pkName)};`)
  for (const c of kept) {
    const o = before.get(c.was!)!
    const col = `ALTER TABLE ${t} ALTER COLUMN ${ident(c.name)}`
    if (o.type.trim() !== c.type.trim()) out.push(`${col} TYPE ${c.type.trim()} USING ${ident(c.name)}::${c.type.trim()};`)
    const od = o.default.trim(), nd = c.default.trim()
    if (od !== nd) {
      if (IDENTITY.test(od) && !nd) out.push(`${col} DROP IDENTITY;`)
      else if (IDENTITY.test(nd)) out.push(`${col} ADD ${nd};`)
      else out.push(nd ? `${col} SET DEFAULT ${nd};` : `${col} DROP DEFAULT;`)
    }
    const wasNN = o.notNull, isNN = c.notNull || c.pk
    if (wasNN !== isNN && !(o.pk && !c.pk && !c.notNull && keyChanged)) out.push(`${col} ${isNN ? 'SET' : 'DROP'} NOT NULL;`)
    if (o.unique !== c.unique && c.unique) out.push(`ALTER TABLE ${t} ADD UNIQUE (${ident(c.name)});`)
  }
  for (const c of to.columns) if (c.was === undefined) out.push(`ALTER TABLE ${t} ADD COLUMN ${columnDef({ ...c, pk: false }, false)};`)
  if (keyChanged && to.columns.some((c) => c.pk)) {
    out.push(`ALTER TABLE ${t} ADD PRIMARY KEY (${to.columns.filter((c) => c.pk).map((c) => ident(c.name)).join(', ')});`)
  }
  return out
}

/** A new, empty column for the designer. */
export function blankColumn(n: number): DesignColumn {
  return { name: `column_${n}`, type: 'text', notNull: false, default: '', pk: false, unique: false }
}

/** Types the designer offers first; any other may be typed. */
export const COMMON_TYPES = [
  'bigint', 'integer', 'smallint', 'numeric(12,2)', 'real', 'double precision', 'boolean', 'text', 'varchar(255)',
  'uuid', 'date', 'timestamptz', 'timestamp', 'time', 'interval', 'jsonb', 'json', 'bytea', 'text[]', 'integer[]', 'inet',
]
