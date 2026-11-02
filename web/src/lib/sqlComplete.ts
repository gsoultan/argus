/*
Autocomplete for the SQL editor: keywords, schemas, tables, views, columns and
functions, from what the agent reports the person's own role may use.

This is a reader of names, not a SQL parser, and it needs to be no more:
it finds the word under the cursor, the tables the statement names (with their
aliases), and whether the cursor is where a table or a column goes. Getting
that wrong costs a less useful list, never a wrong statement; nothing here
decides what runs. Postgres's grants do.

Columns are not in the catalogue. The editor asks for a table's columns when a
statement names it (`needs`), and calls `completeSQL` again when they arrive.
*/

/** The names a role may use, from the agent's "completion" answer. */
export interface SQLCatalog {
  /** The role's search_path, in order; an unqualified name resolves here. */
  path: string[]
  schemas: string[]
  relations: { schema: string; name: string; kind: string }[]
  functions: { schema: string; name: string; args: string; kind: string }[]
}

export interface SQLColumn { name: string; type: string }

export type SuggestionKind =
  | 'keyword' | 'schema' | 'table' | 'view' | 'sequence' | 'column' | 'function' | 'alias' | 'command' | 'key'

export interface Suggestion {
  label: string
  /** Text that replaces the word under the cursor. */
  insert: string
  kind: SuggestionKind
  detail?: string
}

export interface Completion {
  /** The span the chosen suggestion replaces. */
  from: number
  to: number
  /** The word typed so far, as typed. */
  prefix: string
  items: Suggestion[]
  /** Tables whose columns would improve this list and are not loaded yet. */
  needs?: { schema: string; name: string }[]
}

/** Columns the editor has loaded, by `schema.table`. */
export type ColumnsOf = (schema: string, name: string) => SQLColumn[] | undefined

export const SQL_KEYWORDS = [
  'SELECT', 'FROM', 'WHERE', 'AND', 'OR', 'NOT', 'IN', 'IS', 'NULL', 'IS NULL', 'IS NOT NULL', 'LIKE', 'ILIKE',
  'BETWEEN', 'EXISTS', 'JOIN', 'LEFT JOIN', 'RIGHT JOIN', 'INNER JOIN', 'FULL JOIN', 'CROSS JOIN', 'LATERAL',
  'ON', 'USING', 'GROUP BY', 'ORDER BY', 'HAVING', 'LIMIT', 'OFFSET', 'AS', 'DISTINCT', 'DISTINCT ON', 'ALL',
  'ANY', 'UNION', 'UNION ALL', 'INTERSECT', 'EXCEPT', 'WITH', 'RECURSIVE', 'CASE', 'WHEN', 'THEN', 'ELSE',
  'END', 'INSERT INTO', 'VALUES', 'UPDATE', 'SET', 'DELETE FROM', 'RETURNING', 'ON CONFLICT', 'DO NOTHING',
  'DO UPDATE SET', 'CREATE TABLE', 'CREATE INDEX', 'CREATE VIEW', 'CREATE SCHEMA', 'ALTER TABLE', 'DROP TABLE',
  'ADD COLUMN', 'DROP COLUMN', 'TRUNCATE', 'BEGIN', 'COMMIT', 'ROLLBACK', 'SAVEPOINT', 'EXPLAIN', 'EXPLAIN ANALYZE',
  'ANALYZE', 'VACUUM', 'ASC', 'DESC', 'NULLS FIRST', 'NULLS LAST', 'TRUE', 'FALSE', 'CAST', 'FILTER', 'OVER',
  'PARTITION BY', 'WINDOW', 'FETCH FIRST', 'ROWS ONLY', 'FOR UPDATE', 'SKIP LOCKED', 'SHOW', 'PRIMARY KEY',
  'REFERENCES', 'DEFAULT', 'CHECK', 'UNIQUE', 'CONSTRAINT', 'IF EXISTS', 'IF NOT EXISTS', 'MATERIALIZED VIEW',
  'REFRESH MATERIALIZED VIEW', 'INTERVAL', 'CURRENT_DATE', 'CURRENT_TIMESTAMP', 'CURRENT_USER', 'COPY',
  'GRANT', 'REVOKE', 'TABLE', 'ONLY', 'NATURAL',
]

/** Postgres built-ins worth offering; the catalogue covers user functions. */
export const PG_FUNCTIONS: [name: string, args: string][] = [
  ['count', 'any'], ['sum', 'numeric'], ['avg', 'numeric'], ['min', 'any'], ['max', 'any'],
  ['coalesce', 'value, …'], ['nullif', 'a, b'], ['greatest', 'value, …'], ['least', 'value, …'],
  ['now', ''], ['date_trunc', 'field, source'], ['date_part', 'field, source'], ['extract', 'field FROM source'],
  ['age', 'timestamp, timestamp'], ['to_char', 'value, format'], ['to_date', 'text, format'],
  ['to_timestamp', 'text, format'], ['make_interval', '…'], ['length', 'text'], ['char_length', 'text'],
  ['lower', 'text'], ['upper', 'text'], ['initcap', 'text'], ['substring', 'text FROM int FOR int'],
  ['substr', 'text, from, count'], ['trim', 'text'], ['btrim', 'text'], ['ltrim', 'text'], ['rtrim', 'text'],
  ['replace', 'text, from, to'], ['concat', 'value, …'], ['concat_ws', 'sep, value, …'], ['format', 'fmt, …'],
  ['left', 'text, n'], ['right', 'text, n'], ['lpad', 'text, length, fill'], ['rpad', 'text, length, fill'],
  ['split_part', 'text, delimiter, n'], ['position', 'substring IN text'], ['strpos', 'text, substring'],
  ['regexp_replace', 'text, pattern, replacement'], ['regexp_match', 'text, pattern'],
  ['regexp_matches', 'text, pattern'], ['starts_with', 'text, prefix'], ['md5', 'text'],
  ['string_agg', 'value, delimiter'], ['array_agg', 'value'], ['json_agg', 'value'], ['jsonb_agg', 'value'],
  ['json_build_object', 'key, value, …'], ['jsonb_build_object', 'key, value, …'], ['jsonb_set', 'target, path, value'],
  ['jsonb_array_elements', 'jsonb'], ['jsonb_each', 'jsonb'], ['jsonb_object_keys', 'jsonb'],
  ['jsonb_pretty', 'jsonb'], ['to_jsonb', 'value'], ['row_to_json', 'record'], ['array_length', 'array, dim'],
  ['cardinality', 'array'], ['unnest', 'array'], ['generate_series', 'start, stop, step'],
  ['row_number', ''], ['rank', ''], ['dense_rank', ''], ['ntile', 'n'], ['lag', 'value, offset'],
  ['lead', 'value, offset'], ['first_value', 'value'], ['last_value', 'value'], ['bool_and', 'bool'],
  ['bool_or', 'bool'], ['round', 'numeric, places'], ['floor', 'numeric'], ['ceil', 'numeric'], ['abs', 'numeric'],
  ['random', ''], ['gen_random_uuid', ''], ['pg_size_pretty', 'bigint'], ['pg_total_relation_size', 'regclass'],
  ['pg_relation_size', 'regclass'], ['pg_cancel_backend', 'pid'], ['pg_terminate_backend', 'pid'],
  ['current_setting', 'name'], ['set_config', 'name, value, is_local'], ['version', ''],
]

/* ── Tokens ─────────────────────────────────────────────────────────────── */

type TokKind = 'word' | 'qident' | 'str' | 'num' | 'punct' | 'comment' | 'param'

interface Tok {
  k: TokKind
  /** For words, lowercased (Postgres folds unquoted names); quoted names verbatim. */
  v: string
  s: number
  e: number
  /** A string, comment or quoted name that the text ends inside. */
  open?: boolean
}

const WORD_START = /[A-Za-z_\u0080-￿]/
const WORD_CHAR = /[A-Za-z0-9_$\u0080-￿]/

/** Splits SQL into tokens; strings, quoted names and comments are single tokens. */
export function tokenize(text: string): Tok[] {
  const out: Tok[] = []
  let i = 0
  const n = text.length
  while (i < n) {
    const c = text[i]!
    if (/\s/.test(c)) { i++; continue }
    const s = i
    if (c === '-' && text[i + 1] === '-') {
      const nl = text.indexOf('\n', i)
      i = nl < 0 ? n : nl
      out.push({ k: 'comment', v: '', s, e: i, open: nl < 0 })
      continue
    }
    if (c === '/' && text[i + 1] === '*') {
      const end = text.indexOf('*/', i + 2)
      i = end < 0 ? n : end + 2
      out.push({ k: 'comment', v: '', s, e: i, open: end < 0 })
      continue
    }
    if (c === "'" || ((c === 'E' || c === 'e') && text[i + 1] === "'")) {
      i += c === "'" ? 1 : 2
      let closed = false
      while (i < n) {
        if (text[i] === '\\' && c !== "'") { i += 2; continue }
        if (text[i] === "'") {
          if (text[i + 1] === "'") { i += 2; continue }
          i++
          closed = true
          break
        }
        i++
      }
      out.push({ k: 'str', v: '', s, e: Math.min(i, n), open: !closed })
      continue
    }
    if (c === '$') {
      const m = /^\$([A-Za-z_][A-Za-z0-9_]*)?\$/.exec(text.slice(i, i + 64))
      if (m) {
        const tag = m[0]
        const end = text.indexOf(tag, i + tag.length)
        i = end < 0 ? n : end + tag.length
        out.push({ k: 'str', v: '', s, e: i, open: end < 0 })
        continue
      }
      let j = i + 1
      while (j < n && /[0-9]/.test(text[j]!)) j++
      out.push({ k: 'param', v: text.slice(i, j), s, e: j })
      i = j
      continue
    }
    if (c === '"') {
      let j = i + 1
      let v = ''
      let closed = false
      while (j < n) {
        if (text[j] === '"') {
          if (text[j + 1] === '"') { v += '"'; j += 2; continue }
          j++
          closed = true
          break
        }
        v += text[j]
        j++
      }
      out.push({ k: 'qident', v, s, e: j, open: !closed })
      i = j
      continue
    }
    if (WORD_START.test(c)) {
      let j = i + 1
      while (j < n && WORD_CHAR.test(text[j]!)) j++
      out.push({ k: 'word', v: text.slice(i, j).toLowerCase(), s, e: j })
      i = j
      continue
    }
    if (/[0-9]/.test(c)) {
      let j = i + 1
      while (j < n && /[0-9.eE_]/.test(text[j]!)) j++
      out.push({ k: 'num', v: text.slice(i, j), s, e: j })
      i = j
      continue
    }
    out.push({ k: 'punct', v: c, s, e: i + 1 })
    i++
  }
  return out
}

/* ── Statement reading ──────────────────────────────────────────────────── */

/** Words after which a table name goes. */
const REL_AFTER = new Set(['from', 'join', 'update', 'into', 'truncate', 'table', 'only', 'lateral'])

/** Words that end a table reference, so they are never read as its alias. */
const NOT_ALIAS = new Set([
  'where', 'join', 'left', 'right', 'inner', 'outer', 'full', 'cross', 'natural', 'on', 'using', 'group',
  'order', 'limit', 'offset', 'having', 'union', 'intersect', 'except', 'set', 'values', 'returning',
  'select', 'window', 'for', 'fetch', 'default', 'lateral', 'as', 'with', 'when', 'then', 'else', 'end',
  'and', 'or', 'not', 'into', 'from', 'do', 'overriding', 'tablesample',
])

export interface TableRef { schema?: string; name: string; alias?: string }

const isName = (t: Tok | undefined): t is Tok => !!t && (t.k === 'word' || t.k === 'qident')

/** The tables a statement names, with their aliases, in order. */
export function tableRefs(toks: Tok[]): TableRef[] {
  const refs: TableRef[] = []
  for (let i = 0; i < toks.length; i++) {
    const t = toks[i]!
    if (t.k !== 'word' || !['from', 'join', 'update', 'into', 'truncate'].includes(t.v)) continue
    let j = i + 1
    for (;;) {
      while (toks[j]?.k === 'word' && (toks[j]!.v === 'only' || toks[j]!.v === 'lateral' || toks[j]!.v === 'table')) j++
      const a = toks[j]
      if (!isName(a)) break
      let ref: TableRef = { name: a.v }
      j++
      if (toks[j]?.v === '.' && isName(toks[j + 1])) {
        ref = { schema: a.v, name: toks[j + 1]!.v }
        j += 2
      }
      if (toks[j]?.k === 'word' && toks[j]!.v === 'as') j++
      const al = toks[j]
      if (isName(al) && !(al.k === 'word' && NOT_ALIAS.has(al.v)) && toks[j + 1]?.v !== '.') {
        ref.alias = al.v
        j++
      }
      refs.push(ref)
      // FROM a, b: a comma keeps reading tables; anything else ends the list.
      if (t.v === 'from' && toks[j]?.v === ',') { j++; continue }
      break
    }
  }
  return refs
}

/* ── Completion ─────────────────────────────────────────────────────────── */

const PLAIN = /^[a-z_][a-z0-9_$]*$/

/** Quotes a name the way Postgres needs it typed. */
export function quoteIdent(n: string): string {
  return PLAIN.test(n) && !RESERVED.has(n) ? n : `"${n.replace(/"/g, '""')}"`
}

// Names that must be quoted even when lowercase, because they are reserved.
const RESERVED = new Set([
  'all', 'analyse', 'analyze', 'and', 'any', 'array', 'as', 'asc', 'asymmetric', 'both', 'case', 'cast', 'check',
  'collate', 'column', 'constraint', 'create', 'current_catalog', 'current_date', 'current_role', 'current_time',
  'current_timestamp', 'current_user', 'default', 'deferrable', 'desc', 'distinct', 'do', 'else', 'end', 'except',
  'false', 'fetch', 'for', 'foreign', 'from', 'grant', 'group', 'having', 'in', 'initially', 'intersect', 'into',
  'lateral', 'leading', 'limit', 'localtime', 'localtimestamp', 'not', 'null', 'offset', 'on', 'only', 'or',
  'order', 'placing', 'primary', 'references', 'returning', 'select', 'session_user', 'some', 'symmetric',
  'table', 'then', 'to', 'trailing', 'true', 'union', 'unique', 'user', 'using', 'variadic', 'when', 'where',
  'window', 'with',
])

const MAX_ITEMS = 60

/**
 * What to offer at `cursor` in `text`. `explicit` is a request (Ctrl+Space):
 * it lists everything that fits even with nothing typed.
 */
export function completeSQL(
  text: string, cursor: number, catalog: SQLCatalog | null, columnsOf: ColumnsOf, explicit = false,
): Completion | null {
  const toks = tokenize(text)
  // Inside a string or comment there is nothing to complete.
  for (const t of toks) {
    if (t.s < cursor && (cursor < t.e || (t.open && cursor === t.e))) {
      if (t.k === 'str' || t.k === 'comment') return null
    }
  }

  // The word under the cursor: a plain word, or a quoted name being typed.
  let from = cursor
  let to = cursor
  let prefix = ''
  let quoted = false
  const word = toks.find((t) => (t.k === 'word' || t.k === 'qident') && t.s < cursor && cursor <= t.e)
  if (word) {
    from = word.s
    to = word.e
    quoted = word.k === 'qident'
    prefix = text.slice(quoted ? word.s + 1 : word.s, Math.min(cursor, word.e))
    if (quoted && prefix.endsWith('"') && !word.open) prefix = prefix.slice(0, -1)
  }
  if (!explicit && !prefix) {
    // Typing only opens the list once there is a word, or right after a dot.
    if (text[cursor - 1] !== '.') return null
  }

  // The statement around the cursor.
  let start = 0
  let end = toks.length
  for (let i = 0; i < toks.length; i++) {
    const t = toks[i]!
    if (t.k === 'punct' && t.v === ';') {
      if (t.e <= cursor) start = i + 1
      else { end = i; break }
    }
  }
  const stmt = toks.slice(start, end).filter((t) => t.k !== 'comment' && t !== word)
  const before = stmt.filter((t) => t.e <= from)
  const refs = tableRefs(stmt)

  const cat = catalog ?? { path: ['public'], schemas: [], relations: [], functions: [] }
  const resolve = (schema: string | undefined, name: string) => {
    if (schema) return cat.relations.find((r) => r.schema === schema && r.name === name)
    for (const p of cat.path) {
      const r = cat.relations.find((x) => x.schema === p && x.name === name)
      if (r) return r
    }
    return cat.relations.find((r) => r.name === name)
  }
  const needs: { schema: string; name: string }[] = []
  const colsFor = (schema: string | undefined, name: string): { rel: { schema: string; name: string } | undefined; cols: SQLColumn[] } => {
    const rel = resolve(schema, name)
    if (!rel) return { rel, cols: [] }
    const cols = columnsOf(rel.schema, rel.name)
    if (!cols) {
      if (!needs.some((x) => x.schema === rel.schema && x.name === rel.name)) needs.push({ schema: rel.schema, name: rel.name })
      return { rel, cols: [] }
    }
    return { rel, cols }
  }

  const items: (Suggestion & { rank: number })[] = []
  const lowerTyped = prefix.toLowerCase()
  const add = (s: Suggestion, rank: number) => items.push({ ...s, rank })
  const relSuggestion = (r: { schema: string; name: string; kind: string }, qualify: boolean): Suggestion => {
    const kind: SuggestionKind = r.kind === 'view' || r.kind === 'matview' ? 'view' : r.kind === 'sequence' ? 'sequence' : 'table'
    // Unqualified when the search_path finds this very table by its name.
    const plain = !qualify || resolve(undefined, r.name) === r
    return {
      label: r.name, kind, detail: r.schema,
      insert: plain ? quoteIdent(r.name) : `${quoteIdent(r.schema)}.${quoteIdent(r.name)}`,
    }
  }

  // After a dot: what the qualifier holds.
  const dot = before[before.length - 1]
  if (dot?.k === 'punct' && dot.v === '.' && dot.e === from) {
    const q1 = before[before.length - 2]
    if (!isName(q1)) return null
    const q0 = before[before.length - 3]?.v === '.' ? before[before.length - 4] : undefined
    if (q0 && isName(q0)) {
      // schema.table.column
      for (const c of colsFor(q0.v, q1.v).cols) add({ label: c.name, insert: quoteIdent(c.name), kind: 'column', detail: c.type }, 0)
    } else {
      const ref = refs.find((r) => r.alias === q1.v) ?? refs.find((r) => !r.alias && r.name === q1.v)
      const asTable = ref ? colsFor(ref.schema, ref.name) : resolve(undefined, q1.v) ? colsFor(undefined, q1.v) : null
      if (asTable) {
        for (const c of asTable.cols) add({ label: c.name, insert: quoteIdent(c.name), kind: 'column', detail: c.type }, 0)
      }
      if (cat.schemas.includes(q1.v)) {
        for (const r of cat.relations) if (r.schema === q1.v) add(relSuggestion(r, false), 1)
        for (const f of cat.functions) if (f.schema === q1.v) add({ label: f.name, insert: `${quoteIdent(f.name)}(`, kind: 'function', detail: `(${f.args})` }, 2)
      }
    }
    return finish()
  }

  // Where a table goes: after FROM, JOIN, UPDATE, INTO, and after a comma in
  // a FROM list.
  const prev = before[before.length - 1]
  let relContext = prev?.k === 'word' && REL_AFTER.has(prev.v)
  if (!relContext && prev?.v === ',') {
    let depth = 0
    for (let i = before.length - 1; i >= 0; i--) {
      const t = before[i]!
      if (t.v === ')') depth++
      else if (t.v === '(') { if (depth === 0) break; depth-- }
      else if (depth === 0 && t.k === 'word' && (t.v === 'select' || t.v === 'where' || t.v === 'set' || t.v === 'by')) break
      else if (depth === 0 && t.k === 'word' && t.v === 'from') { relContext = true; break }
    }
  }
  if (relContext) {
    for (const r of cat.relations) if (r.kind !== 'sequence') add(relSuggestion(r, true), 0)
    for (const s of cat.schemas) add({ label: s, insert: quoteIdent(s), kind: 'schema' }, 2)
    if (prev?.v === 'from' || prev?.v === ',') {
      for (const f of cat.functions) add({ label: f.name, insert: `${quoteIdent(f.name)}(`, kind: 'function', detail: `${f.schema} · (${f.args})` }, 3)
    }
    return finish()
  }

  // Anywhere else: the named tables' columns first, then the tables
  // themselves (for qualifying), functions, and keywords.
  const seenCols = new Set<string>()
  for (const ref of refs) {
    const { cols } = colsFor(ref.schema, ref.name)
    for (const c of cols) {
      if (seenCols.has(c.name)) continue
      seenCols.add(c.name)
      add({ label: c.name, insert: quoteIdent(c.name), kind: 'column', detail: `${c.type} · ${ref.alias ?? ref.name}` }, 0)
    }
    add({ label: ref.alias ?? ref.name, insert: quoteIdent(ref.alias ?? ref.name), kind: 'alias', detail: ref.alias ? ref.name : 'table' }, 1)
  }
  const kwStyle = prefix && prefix === prefix.toLowerCase() ? (k: string) => k.toLowerCase() : (k: string) => k
  for (const k of SQL_KEYWORDS) add({ label: k, insert: kwStyle(k), kind: 'keyword' }, 2)
  const fnSeen = new Set<string>()
  for (const f of cat.functions) {
    const key = `${f.schema}.${f.name}`
    if (fnSeen.has(key)) continue
    fnSeen.add(key)
    const onPath = cat.path.includes(f.schema)
    add({ label: f.name, insert: `${onPath ? '' : quoteIdent(f.schema) + '.'}${quoteIdent(f.name)}(`, kind: 'function', detail: `${f.schema} · (${f.args})` }, 3)
  }
  for (const [name, args] of PG_FUNCTIONS) add({ label: name, insert: `${name}(`, kind: 'function', detail: `(${args})` }, 3)
  // With nothing typed, a list of every table is noise; with a prefix it helps.
  if (lowerTyped) {
    for (const r of cat.relations) if (r.kind !== 'sequence') add(relSuggestion(r, true), 4)
  }
  return finish()

  function finish(): Completion | null {
    const scored: { s: Suggestion & { rank: number }; score: number; i: number }[] = []
    const seen = new Set<string>()
    for (const it of items) {
      const key = `${it.kind}:${it.insert}`
      if (seen.has(key)) continue
      const label = it.label.toLowerCase()
      let score: number
      if (!lowerTyped) score = 0
      else if (label === lowerTyped) score = 0
      else if (label.startsWith(lowerTyped)) score = 1
      else if ((it.kind === 'column' || it.kind === 'table' || it.kind === 'view') && lowerTyped.length >= 2 && label.includes(lowerTyped)) score = 2
      else continue
      seen.add(key)
      scored.push({ s: it, score, i: scored.length })
    }
    // Columns keep the table's order, which is how people think of them;
    // everything else puts the shortest match first.
    scored.sort((a, b) =>
      a.score - b.score || a.s.rank - b.s.rank ||
      (a.s.kind === 'column' && b.s.kind === 'column' ? a.i - b.i
        : a.s.label.length - b.s.label.length || a.s.label.localeCompare(b.s.label)))
    const out = scored.slice(0, MAX_ITEMS).map(({ s }) => ({ label: s.label, insert: s.insert, kind: s.kind, detail: s.detail }))
    if (out.length === 0 && needs.length === 0) return null
    return { from, to, prefix, items: out, needs: needs.length ? needs : undefined }
  }
}

/** Builds the catalogue from the agent's "completion" answer. */
export function catalogFrom(objects: { name: string; kind: string; detail?: string; schema?: string }[]): SQLCatalog {
  const path: [number, string][] = []
  const cat: SQLCatalog = { path: [], schemas: [], relations: [], functions: [] }
  for (const o of objects) {
    const schema = o.schema ?? ''
    switch (o.kind) {
      case 'path': path.push([Number(o.detail) || 0, o.name]); break
      case 'schema': cat.schemas.push(o.name); break
      case 'function': case 'procedure': cat.functions.push({ schema, name: o.name, args: o.detail ?? '', kind: o.kind }); break
      default: cat.relations.push({ schema, name: o.name, kind: o.kind })
    }
  }
  cat.path = path.sort((a, b) => a[0] - b[0]).map(([, n]) => n)
  return cat
}
