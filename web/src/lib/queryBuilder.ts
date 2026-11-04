/*
The visual query builder's model, and the SELECT it describes.

A query is a table, the tables joined to it along their foreign keys, the
columns (or keys inside a JSON column) to show, filters, an optional
summary (group by what is shown, with counts and sums), a sort and a limit.
Filter values become :name parameters, so they are fields under the editor
and travel as bind parameters, never as SQL. Identifiers come from the
catalogue and are quoted; nothing the person types is put into the SQL but
a value, and those only as parameters.
*/

import { ident, literal } from '~/lib/resultView'

export interface BTable { schema: string; name: string }

export interface BColumn { name: string; type: string }

/** A foreign key between two tables, from the catalogue. */
export interface ForeignKeyRef {
  name: string
  from: BTable
  fromCols: string[]
  to: BTable
  toCols: string[]
}

export interface BJoin {
  /** The alias of the table it joins to: t0 for the base. */
  on: string
  fk: ForeignKeyRef
  /** The join follows the key from its referencing side (true) or back to it. */
  forward: boolean
  kind: 'inner' | 'left'
  alias: string
}

/** A column, or a key inside a JSON column, of one of the query's tables. */
export interface BRef {
  alias: string
  column: string
  /** A key inside a json or jsonb column; dots go deeper (address.city). */
  key?: string
}

export type Aggregate = 'count' | 'count distinct' | 'sum' | 'avg' | 'min' | 'max'

export interface BMeasure { fn: Aggregate; ref?: BRef }

export type FilterOp = '=' | '<>' | '<' | '<=' | '>' | '>=' | 'contains' | 'starts with' | 'is null' | 'is not null'

export interface BFilter { ref: BRef; op: FilterOp; param: string }

export interface BSort { by: number; desc: boolean }

export interface BQuery {
  base: BTable
  joins: BJoin[]
  columns: BRef[]
  /** With measures, the columns are what the rows are grouped by. */
  measures: BMeasure[]
  filters: BFilter[]
  /** Index into the output: columns, then measures. */
  sort: BSort[]
  limit: number
}

export const tableName = (t: BTable) => `${ident(t.schema)}.${ident(t.name)}`

/** An expression for a ref: the column, or ->/->> down to a JSON key. */
export function refSQL(r: BRef): string {
  const col = `${r.alias}.${ident(r.column)}`
  if (!r.key) return col
  const path = r.key.split('.').filter(Boolean)
  return path.reduce((e, k, i) => `${e}${i === path.length - 1 ? '->>' : '->'}${literal(k)}`, col)
}

/** The name a ref shows as in the result. */
export function refLabel(r: BRef): string {
  return r.key ? `${r.column}_${r.key.replace(/\./g, '_')}` : r.column
}

const measureLabel = (m: BMeasure) => (m.ref ? `${m.fn.replace(' ', '_')}_${refLabel(m.ref)}` : 'count')

function measureSQL(m: BMeasure): string {
  if (!m.ref) return 'count(*)'
  const e = refSQL(m.ref)
  // A JSON key comes out as text; a sum or an average of it needs a number.
  const num = m.ref.key && (m.fn === 'sum' || m.fn === 'avg') ? `(${e})::numeric` : e
  switch (m.fn) {
    case 'count': return `count(${num})`
    case 'count distinct': return `count(DISTINCT ${num})`
    default: return `${m.fn}(${num})`
  }
}

function joinSQL(j: BJoin): string {
  const other = j.forward ? j.fk.to : j.fk.from
  const mine = j.forward ? j.fk.fromCols : j.fk.toCols
  const theirs = j.forward ? j.fk.toCols : j.fk.fromCols
  const cond = mine.map((c, i) => `${j.alias}.${ident(theirs[i]!)} = ${j.on}.${ident(c)}`).join(' AND ')
  return `${j.kind === 'left' ? 'LEFT JOIN' : 'JOIN'} ${tableName(other)} ${j.alias} ON ${cond}`
}

function filterSQL(f: BFilter): string {
  const e = refSQL(f.ref)
  const p = `:${f.param}`
  switch (f.op) {
    case 'is null': return `${e} IS NULL`
    case 'is not null': return `${e} IS NOT NULL`
    case 'contains': return `${e}::text ILIKE '%' || ${p} || '%'`
    case 'starts with': return `${e}::text ILIKE ${p} || '%'`
    default: return `${e} ${f.op} ${p}`
  }
}

/** The SELECT the builder describes. */
export function buildSQL(q: BQuery): string {
  const out: string[] = []
  const grouped = q.measures.length > 0
  const cols = q.columns.map((r) => {
    const e = refSQL(r)
    const label = refLabel(r)
    return r.key || label !== r.column || q.joins.length ? `${e} AS ${ident(label)}` : e
  })
  const all = [...cols, ...q.measures.map((m) => `${measureSQL(m)} AS ${ident(measureLabel(m))}`)]
  out.push(`SELECT ${all.length ? all.join(',\n       ') : `t0.*`}`)
  out.push(`FROM ${tableName(q.base)} t0`)
  for (const j of q.joins) out.push(joinSQL(j))
  if (q.filters.length) out.push(`WHERE ${q.filters.map(filterSQL).join('\n  AND ')}`)
  if (grouped && q.columns.length) out.push(`GROUP BY ${q.columns.map((_, i) => i + 1).join(', ')}`)
  const sorts = q.sort.filter((s) => s.by >= 0 && s.by < all.length)
  if (sorts.length) out.push(`ORDER BY ${sorts.map((s) => `${s.by + 1}${s.desc ? ' DESC' : ''}`).join(', ')}`)
  if (q.limit > 0) out.push(`LIMIT ${Math.floor(q.limit)}`)
  return `${out.join('\n')};`
}

/** A :name for a filter on ref that no other filter has. */
export function paramFor(q: BQuery, r: BRef): string {
  const base = refLabel(r).replace(/[^A-Za-z0-9_]/g, '_').replace(/^(\d)/, '_$1') || 'value'
  const used = new Set(q.filters.map((f) => f.param))
  let name = base
  for (let i = 2; used.has(name); i++) name = `${base}_${i}`
  return name
}

/** The next free table alias. */
export const nextAlias = (q: BQuery) => `t${q.joins.length + 1}`

/** Whether a type holds JSON whose keys can be picked. */
export const isJSON = (type: string) => type === 'json' || type === 'jsonb'

/*
Catalogue reads for the builder, run as the person's role.
*/

/** Tables and views the person may read. */
export const TABLES_SQL = `SELECT n.nspname::text, c.relname::text, c.relkind::text
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\\_%'
  AND n.nspname NOT LIKE 'argus\\_trash%' AND has_table_privilege(c.oid, 'SELECT')
ORDER BY n.nspname = 'public' DESC, 1, 2
LIMIT 2000`

/** A table's columns. $1 is schema.name. */
export const COLUMNS_SQL = `SELECT a.attname::text, format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`

/** The foreign keys from and to a table. $1 is schema.name. */
export const FKS_SQL = `SELECT k.conname::text,
  fn.nspname::text, fc.relname::text, (SELECT array_to_json(array_agg(a.attname ORDER BY x.n))::text FROM unnest(k.conkey) WITH ORDINALITY x(attnum, n) JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = x.attnum),
  tn.nspname::text, tc.relname::text, (SELECT array_to_json(array_agg(a.attname ORDER BY x.n))::text FROM unnest(k.confkey) WITH ORDINALITY x(attnum, n) JOIN pg_attribute a ON a.attrelid = k.confrelid AND a.attnum = x.attnum)
FROM pg_constraint k
JOIN pg_class fc ON fc.oid = k.conrelid JOIN pg_namespace fn ON fn.oid = fc.relnamespace
JOIN pg_class tc ON tc.oid = k.confrelid JOIN pg_namespace tn ON tn.oid = tc.relnamespace
WHERE k.contype = 'f' AND (k.conrelid = $1::regclass OR k.confrelid = $1::regclass)
ORDER BY 1`

/** The keys found in a JSON column, from the first 1,000 rows. $1 is schema.name; the column is quoted in. */
export function jsonKeysSQL(t: BTable, column: string): string {
  const c = ident(column)
  return `SELECT k, count(*)::text FROM (SELECT ${c}::jsonb AS j FROM ${tableName(t)} WHERE ${c} IS NOT NULL LIMIT 1000) s,
  jsonb_object_keys(CASE WHEN jsonb_typeof(s.j) = 'object' THEN s.j END) k
GROUP BY k ORDER BY count(*) DESC, k LIMIT 200`
}

export function parseFks(rows: (string | null)[][]): ForeignKeyRef[] {
  const arr = (s: string | null) => { try { return JSON.parse(s ?? '[]') as string[] } catch { return [] } }
  return rows.map((r) => ({
    name: r[0] ?? '', from: { schema: r[1] ?? '', name: r[2] ?? '' }, fromCols: arr(r[3] ?? null),
    to: { schema: r[4] ?? '', name: r[5] ?? '' }, toCols: arr(r[6] ?? null),
  }))
}
