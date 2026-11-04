/*
Importing a CSV or Excel file into a table.

The person downloads a template first: the table's columns as a header row,
and in Excel a second sheet saying what each column takes. They fill it in
and pick it; the console matches the header to the columns by name, checks
each value the way the column will, and shows what it found before anything
is sent. The rows then go as INSERT statements of a few hundred rows each,
every value a bind parameter cast to its column's type, all in one
transaction: a single failure rolls the whole import back.
*/

import { ident } from '~/lib/resultView'
import type { EditTarget, TableColumn } from '~/lib/rowEdit'

export const MAX_IMPORT_ROWS = 50_000
export const MAX_IMPORT_BYTES = 20 * 1024 * 1024
/** Bind parameters per statement, well under Postgres's 65,535. */
const PARAMS_PER_STATEMENT = 20_000
const ROWS_PER_STATEMENT = 500

export type Cell = string | null

/** The columns a template asks for: every one that takes a value. */
export function templateColumns(t: EditTarget): TableColumn[] {
  return t.columns.filter((c) => !c.generated && c.identity !== 'a')
}

/** A column must be given: NOT NULL with nothing for Postgres to fill in. */
export const required = (c: TableColumn) => c.notNull && !c.hasDefault && !c.generated

/** What a column takes, in words, for the template's notes. */
export function columnNote(c: TableColumn): string {
  const t = c.type
  const kind = c.enumValues?.length ? `one of: ${c.enumValues.join(', ')}`
    : /^(smallint|integer|bigint)/.test(t) ? 'a whole number'
    : /^(numeric|real|double|decimal)/.test(t) ? 'a number, with a dot for decimals'
    : t === 'boolean' ? 'true or false'
    : t === 'date' ? 'a date, as 2026-11-04'
    : /^timestamp/.test(t) ? 'a date and time, as 2026-11-04 14:30:00'
    : /^time/.test(t) ? 'a time, as 14:30:00'
    : t === 'uuid' ? 'a UUID'
    : /^jsonb?$/.test(t) ? 'JSON, as {"key": "value"}'
    : /\[\]$/.test(t) ? 'a list, as {a,b,c}'
    : 'text'
  const need = required(c) ? 'required' : c.hasDefault ? 'optional; left empty, the default is used' : 'optional; left empty, it is NULL'
  return `${kind}; ${need}`
}

/** The CSV template: the header row alone. */
export function csvTemplate(t: EditTarget): string {
  return `﻿${templateColumns(t).map((c) => csvField(c.name)).join(',')}\r\n`
}

function csvField(s: string): string {
  return /[",\r\n;]/.test(s) || /^\s|\s$/.test(s) ? `"${s.replace(/"/g, '""')}"` : s
}

/** The Excel template's sheets: the header to fill in, and what each column takes. */
export function xlsxTemplateSheets(t: EditTarget): { name: string; rows: string[][] }[] {
  const cols = templateColumns(t)
  return [
    { name: 'Data', rows: [cols.map((c) => c.name)] },
    { name: 'Columns', rows: [['Column', 'Type', 'What it takes'], ...cols.map((c) => [c.name, c.type, columnNote(c)])] },
  ]
}

/** Parses CSV (or semicolon- or tab-separated text), quotes and all. */
export function parseCSV(text: string): Cell[][] {
  const s = text.replace(/^﻿/, '')
  const firstLine = s.slice(0, s.search(/\r?\n|$/))
  const counts = [',', ';', '\t'].map((d) => ({ d, n: firstLine.split(d).length - 1 }))
  const sep = counts.sort((a, b) => b.n - a.n)[0]!.n > 0 ? counts[0]!.d : ','
  const rows: Cell[][] = []
  let row: Cell[] = []
  let field = ''
  let quoted = false
  let wasQuoted = false
  const endField = () => { row.push(field === '' && !wasQuoted ? null : field); field = ''; wasQuoted = false }
  for (let i = 0; i < s.length; i++) {
    const c = s[i]!
    if (quoted) {
      if (c === '"') {
        if (s[i + 1] === '"') { field += '"'; i++ } else quoted = false
      } else field += c
      continue
    }
    if (c === '"' && field === '') { quoted = true; wasQuoted = true; continue }
    if (c === sep) { endField(); continue }
    if (c === '\r' || c === '\n') {
      if (c === '\r' && s[i + 1] === '\n') i++
      endField()
      rows.push(row)
      row = []
      continue
    }
    field += c
  }
  if (field !== '' || wasQuoted || row.length) { endField(); rows.push(row) }
  // A blank line is no row; a row of quoted empty strings is one.
  return rows.filter((r) => r.some((v) => v !== null && (v.trim() !== '' || v === '')))
}

export interface Mapping {
  /** For each file column, the table column it fills, or null when it is left out. */
  columns: (TableColumn | null)[]
  /** File headers that name no column. */
  unknown: string[]
  /** Required columns the file does not have. */
  missing: string[]
  /** A column named twice. */
  twice: string[]
}

/** Matches a file's header to the table's columns, by name, ignoring case and spaces. */
export function mapHeader(t: EditTarget, header: Cell[]): Mapping {
  const norm = (s: string) => s.trim().toLowerCase().replace(/[\s-]+/g, '_')
  const byName = new Map(templateColumns(t).map((c) => [norm(c.name), c]))
  const seen = new Set<string>()
  const out: Mapping = { columns: [], unknown: [], missing: [], twice: [] }
  for (const h of header) {
    const c = h === null ? undefined : byName.get(norm(h))
    if (!c) { if (h !== null && h.trim()) out.unknown.push(h); out.columns.push(null); continue }
    if (seen.has(c.name)) { out.twice.push(c.name); out.columns.push(null); continue }
    seen.add(c.name)
    out.columns.push(c)
  }
  for (const c of templateColumns(t)) if (required(c) && !seen.has(c.name)) out.missing.push(c.name)
  return out
}

export interface Problem { row: number; column: string; message: string }

const DATE = /^\d{4}-\d{2}-\d{2}$/
const DATETIME = /^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}(:?\d{2})?)?$/
const UUID = /^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$/i

/** Why a value will not go into a column, or null. Postgres has the last word; this catches the common slips first. */
export function checkValue(c: TableColumn, v: string): string | null {
  const t = c.type
  const s = v.trim()
  if (c.enumValues?.length && !c.enumValues.includes(v)) return `must be one of ${c.enumValues.join(', ')}`
  if (/^(smallint|integer|bigint)$/.test(t) && !/^[+-]?\d+$/.test(s)) return 'must be a whole number'
  if (/^(numeric|real|double precision|decimal)/.test(t) && !/^[+-]?(\d+\.?\d*|\.\d+)(e[+-]?\d+)?$/i.test(s)) return 'must be a number (use a dot for decimals)'
  if (t === 'boolean' && !/^(t|f|true|false|yes|no|y|n|1|0|on|off)$/i.test(s)) return 'must be true or false'
  if (t === 'date' && !DATE.test(s)) return 'must be a date like 2026-11-04'
  if (/^timestamp/.test(t) && !DATETIME.test(s) && !DATE.test(s)) return 'must be a date and time like 2026-11-04 14:30:00'
  if (t === 'uuid' && !UUID.test(s)) return 'must be a UUID'
  if (/^jsonb?$/.test(t)) { try { JSON.parse(v) } catch { return 'is not valid JSON' } }
  return null
}

export interface Plan {
  /** The table columns the file fills, in file order. */
  columns: TableColumn[]
  /** For each kept file column, its index in the file. */
  from: number[]
  rows: Cell[][]
  problems: Problem[]
}

/** The data rows checked against the mapping. Row numbers are the file's, counting the header as 1. */
export function planImport(mapping: Mapping, data: Cell[][]): Plan {
  const from = mapping.columns.map((c, i) => (c ? i : -1)).filter((i) => i >= 0)
  const columns = from.map((i) => mapping.columns[i]!)
  const problems: Problem[] = []
  const rows = data.map((r, ri) => from.map((fi, k) => {
    const c = columns[k]!
    const v = r[fi] ?? null
    if (v === null || v.trim() === '') {
      if (required(c)) problems.push({ row: ri + 2, column: c.name, message: 'is required' })
      return null
    }
    const why = checkValue(c, v)
    if (why) problems.push({ row: ri + 2, column: c.name, message: why })
    return v
  }))
  return { columns, from, rows, problems }
}

export type OnConflict = 'error' | 'skip' | 'update'

/**
 * The INSERT statements for a plan, a few hundred rows each. An empty cell is
 * the column's default when it has one, otherwise NULL.
 */
export function importStatements(t: EditTarget, plan: Plan, onConflict: OnConflict): { sql: string; params: Cell[]; first: number; last: number }[] {
  const n = plan.columns.length
  if (n === 0 || plan.rows.length === 0) return []
  const per = Math.max(1, Math.min(ROWS_PER_STATEMENT, Math.floor(PARAMS_PER_STATEMENT / n)))
  const names = plan.columns.map((c) => ident(c.name)).join(', ')
  const keyNames = t.pk.map((a) => t.columns.find((c) => c.attnum === a)?.name).filter((x): x is string => Boolean(x))
  const keyed = keyNames.length > 0 && keyNames.every((k) => plan.columns.some((c) => c.name === k))
  let tail = ''
  if (onConflict === 'skip') tail = '\nON CONFLICT DO NOTHING'
  if (onConflict === 'update' && keyed) {
    const set = plan.columns.filter((c) => !keyNames.includes(c.name))
    tail = `\nON CONFLICT (${keyNames.map(ident).join(', ')}) ${set.length
      ? `DO UPDATE SET ${set.map((c) => `${ident(c.name)} = EXCLUDED.${ident(c.name)}`).join(', ')}` : 'DO NOTHING'}`
  }
  const out: { sql: string; params: Cell[]; first: number; last: number }[] = []
  for (let start = 0; start < plan.rows.length; start += per) {
    const chunk = plan.rows.slice(start, start + per)
    const params: Cell[] = []
    const values = chunk.map((r) => `(${r.map((v, k) => {
      const c = plan.columns[k]!
      if (v === null) return c.hasDefault ? 'DEFAULT' : 'NULL'
      params.push(v)
      return `CAST($${params.length} AS ${c.type})`
    }).join(', ')})`)
    out.push({
      sql: `INSERT INTO ${t.table} (${names})\nVALUES ${values.join(',\n       ')}${tail}`,
      params, first: start + 2, last: start + chunk.length + 1,
    })
  }
  return out
}

/** Whether an upsert can be offered: the file has every primary key column. */
export function canUpsert(t: EditTarget, plan: Plan): boolean {
  return t.pk.length > 0 && t.pk.every((a) => plan.columns.some((c) => c.attnum === a))
}
