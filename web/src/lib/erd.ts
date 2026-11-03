/*
The schema diagram's model and layout. Pure, so it is tested without a page.

The catalog is read by three plain statements (tables, columns, foreign
keys), recorded like any other and run as the person's own role, so the
diagram shows only what they may see. Each row is short: no cell comes near
the session's cell limit, however wide the schema.
*/

export interface ErdColumn { name: string; type: string; notNull: boolean; pk: boolean; fk: boolean }
export interface ErdTable {
  id: string
  name: string
  kind: string
  rows: number
  bytes: number
  columns: ErdColumn[]
}
export interface ErdLink {
  from: string
  to: string
  /** The referenced table when it is in another schema: schema.table. */
  external?: string
  name: string
  fromCols: string[]
  toCols: string[]
}
export interface Erd { tables: ErdTable[]; links: ErdLink[]; byId: Map<string, ErdTable> }

const lit = (s: string) => `'${s.replace(/'/g, "''")}'`
const KINDS = `('r','p','v','m','f')`

export function erdTablesSQL(schema: string): string {
  return `SELECT c.oid::int8, c.relname, c.relkind, greatest(c.reltuples, 0)::int8, pg_total_relation_size(c.oid)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ${lit(schema)} AND c.relkind IN ${KINDS} AND NOT c.relispartition
  AND (has_table_privilege(c.oid, 'SELECT,INSERT,UPDATE,DELETE,REFERENCES') OR has_any_column_privilege(c.oid, 'SELECT'))
ORDER BY c.relname LIMIT 2000`
}

export function erdColumnsSQL(schema: string): string {
  return `SELECT a.attrelid::int8, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
  coalesce(a.attnum = ANY (i.indkey), false)
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_index i ON i.indrelid = c.oid AND i.indisprimary
WHERE n.nspname = ${lit(schema)} AND c.relkind IN ${KINDS} AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attrelid, a.attnum LIMIT 60000`
}

export function erdLinksSQL(schema: string): string {
  const cols = (rel: string, keys: string) => `(SELECT string_agg(a.attname, ',' ORDER BY k.ord)
    FROM unnest(con.${keys}) WITH ORDINALITY k(attnum, ord) JOIN pg_attribute a ON a.attrelid = con.${rel} AND a.attnum = k.attnum)`
  return `SELECT con.conrelid::int8, con.confrelid::int8, fn.nspname, fc.relname, con.conname,
  ${cols('conrelid', 'conkey')}, ${cols('confrelid', 'confkey')}
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_class fc ON fc.oid = con.confrelid JOIN pg_namespace fn ON fn.oid = fc.relnamespace
WHERE con.contype = 'f' AND n.nspname = ${lit(schema)}
ORDER BY 1, con.conname LIMIT 20000`
}

export function erdIndexesSQL(schema: string, table: string): string {
  return `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = ${lit(schema)} AND tablename = ${lit(table)} ORDER BY 1`
}

type Cell = string | null

/** Builds the model from the three statements' rows. */
export function buildErd(schema: string, tables: Cell[][], columns: Cell[][], links: Cell[][]): Erd {
  const byId = new Map<string, ErdTable>()
  for (const r of tables) {
    byId.set(r[0]!, { id: r[0]!, name: r[1] ?? '', kind: r[2] ?? 'r', rows: Number(r[3] ?? 0), bytes: Number(r[4] ?? 0), columns: [] })
  }
  for (const r of columns) {
    const t = byId.get(r[0]!)
    if (t) t.columns.push({ name: r[1] ?? '', type: r[2] ?? '', notNull: r[3] === 't', pk: r[4] === 't', fk: false })
  }
  const out: ErdLink[] = []
  for (const r of links) {
    const from = byId.get(r[0]!)
    if (!from) continue
    const fromCols = (r[5] ?? '').split(',').filter(Boolean)
    const toCols = (r[6] ?? '').split(',').filter(Boolean)
    for (const c of from.columns) if (fromCols.includes(c.name)) c.fk = true
    const inSchema = r[2] === schema && byId.has(r[1]!)
    out.push({ from: from.id, to: inSchema ? r[1]! : `ext:${r[2]}.${r[3]}`, external: inSchema ? undefined : `${r[2]}.${r[3]}`,
      name: r[4] ?? '', fromCols, toCols })
  }
  return { tables: [...byId.values()], links: out, byId }
}

/** The tables a table references and the ones that reference it. */
export function neighbours(erd: Erd, id: string): { parents: string[]; children: string[] } {
  const parents = new Set<string>()
  const children = new Set<string>()
  for (const l of erd.links) {
    if (l.from === id && !l.external && l.to !== id) parents.add(l.to)
    if (l.to === id && l.from !== id) children.add(l.from)
  }
  return { parents: [...parents], children: [...children] }
}

export const BOX_W = 220
export const HEAD_H = 30
export const ROW_H = 18
export const MAX_ROWS = 12

export function boxHeight(t: ErdTable): number {
  const shown = Math.min(t.columns.length, MAX_ROWS) + (t.columns.length > MAX_ROWS ? 1 : 0)
  return HEAD_H + shown * ROW_H + 8
}

export type Positions = Record<string, { x: number; y: number }>

/**
 * Where each shown table goes. Focused: the table in the middle, what it
 * references on the left, what references it on the right. Otherwise the
 * whole schema in a grid, the most connected first, so they sit together.
 */
export function layout(erd: Erd, shown: string[], focus: string | null): Positions {
  const pos: Positions = {}
  const gapX = 90
  const gapY = 28
  const column = (ids: string[], x: number) => {
    const hs = ids.map((id) => boxHeight(erd.byId.get(id)!))
    const total = hs.reduce((s, h) => s + h, 0) + gapY * Math.max(0, ids.length - 1)
    let y = -total / 2
    ids.forEach((id, i) => {
      pos[id] = { x, y }
      y += hs[i]! + gapY
    })
  }
  if (focus && erd.byId.has(focus)) {
    const { parents, children } = neighbours(erd, focus)
    const both = parents.filter((p) => children.includes(p))
    column(parents, -(BOX_W + gapX))
    column(children.filter((c) => !both.includes(c)), BOX_W + gapX)
    column([focus], 0)
    for (const id of shown) {
      if (!pos[id]) pos[id] = { x: 0, y: (pos[focus]!.y + boxHeight(erd.byId.get(focus)!) + gapY) }
    }
    return pos
  }
  const degree = new Map<string, number>()
  for (const l of erd.links) {
    degree.set(l.from, (degree.get(l.from) ?? 0) + 1)
    degree.set(l.to, (degree.get(l.to) ?? 0) + 1)
  }
  const ids = [...shown].sort((a, b) => (degree.get(b) ?? 0) - (degree.get(a) ?? 0) ||
    erd.byId.get(a)!.name.localeCompare(erd.byId.get(b)!.name))
  const cols = Math.max(1, Math.ceil(Math.sqrt(ids.length * 1.4)))
  const rowsH: number[] = []
  ids.forEach((id, i) => {
    const r = Math.floor(i / cols)
    rowsH[r] = Math.max(rowsH[r] ?? 0, boxHeight(erd.byId.get(id)!))
  })
  ids.forEach((id, i) => {
    const r = Math.floor(i / cols)
    const y = rowsH.slice(0, r).reduce((s, h) => s + h + gapY, 0)
    pos[id] = { x: (i % cols) * (BOX_W + gapX), y }
  })
  return pos
}

/** Which tables a view shows: all, or one and its neighbours. */
export function shownTables(erd: Erd, focus: string | null): string[] {
  if (!focus || !erd.byId.has(focus)) return erd.tables.map((t) => t.id)
  const { parents, children } = neighbours(erd, focus)
  return [...new Set([focus, ...parents, ...children])]
}

/** The y of a column's row inside its box, for an edge's end. */
export function columnY(t: ErdTable, col: string | undefined): number {
  const i = col ? t.columns.findIndex((c) => c.name === col) : -1
  if (i < 0 || i >= MAX_ROWS) return HEAD_H / 2
  return HEAD_H + i * ROW_H + ROW_H / 2 + 4
}
