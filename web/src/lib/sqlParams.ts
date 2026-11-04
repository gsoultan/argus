/*
:name parameters in the query editor.

`SELECT * FROM orders WHERE customer_id = :customer AND placed > :since`
shows a field for customer and one for since under the editor. On run, each
name becomes $1, $2 … (the same name, the same number) and the values go
with the statement as bind parameters, so a value is never pasted into SQL
and cannot change what the statement does.

A name is found only where SQL is: not in a string, a quoted identifier, a
comment or a dollar-quoted body, and not in a cast (`x::text`). A value is
text, or null for NULL; Postgres works out its type from where it is used.
*/

export interface ParamUse { name: string; from: number; to: number }

const isStart = (c: string) => /[A-Za-z_]/.test(c)
const isPart = (c: string) => /[A-Za-z0-9_]/.test(c)

/** Every :name in the text, in order, with where it is. */
export function paramUses(s: string): ParamUse[] {
  const out: ParamUse[] = []
  let i = 0
  while (i < s.length) {
    const c = s.charAt(i)
    const n = s.charAt(i + 1)
    if (c === '-' && n === '-') {
      while (i < s.length && s.charAt(i) !== '\n') i++
    } else if (c === '/' && n === '*') {
      let depth = 0
      while (i < s.length) {
        if (s.charAt(i) === '/' && s.charAt(i + 1) === '*') { depth++; i += 2; continue }
        if (s.charAt(i) === '*' && s.charAt(i + 1) === '/') { depth--; i += 2; if (depth === 0) break; continue }
        i++
      }
    } else if (c === "'") {
      const backslash = i > 0 && /[eE]/.test(s.charAt(i - 1)) && !isPart(s.charAt(i - 2) || ' ')
      let j = i + 1
      while (j < s.length) {
        if (backslash && s.charAt(j) === '\\') { j += 2; continue }
        if (s.charAt(j) === "'") { if (s.charAt(j + 1) === "'") { j += 2; continue } j++; break }
        j++
      }
      i = j
    } else if (c === '"') {
      let j = i + 1
      while (j < s.length) {
        if (s.charAt(j) === '"') { if (s.charAt(j + 1) === '"') { j += 2; continue } j++; break }
        j++
      }
      i = j
    } else if (c === '$' && !(i > 0 && isPart(s.charAt(i - 1)))) {
      let j = i + 1
      while (j < s.length && isPart(s.charAt(j)) && !(j === i + 1 && /[0-9]/.test(s.charAt(j)))) j++
      if (s.charAt(j) === '$') {
        const tag = s.slice(i, j + 1)
        const end = s.indexOf(tag, j + 1)
        i = end < 0 ? s.length : end + tag.length
      } else {
        i = Math.max(j, i + 1)
      }
    } else if (c === ':' && n === ':') {
      i += 2
    } else if (c === ':' && isStart(n) && !(i > 0 && (s.charAt(i - 1) === ':' || isPart(s.charAt(i - 1))))) {
      // Not after a word or a digit, so an array slice `a[1:n]` stays a slice.
      let j = i + 1
      while (j < s.length && isPart(s.charAt(j))) j++
      out.push({ name: s.slice(i + 1, j), from: i, to: j })
      i = j
    } else {
      i++
    }
  }
  return out
}

/** The distinct names, in the order they first appear. */
export function paramNames(s: string): string[] {
  return [...new Set(paramUses(s).map((u) => u.name))]
}

export interface Bound {
  sql: string
  params: (string | null)[]
  /** Where each $n is in sql, and the :name it stands for in the original. */
  spans: { from: number; to: number; origFrom: number; origTo: number }[]
}

/** The statement with $1, $2 … for each name, and the values in that order. */
export function bindParams(s: string, values: Record<string, string | null | undefined>): Bound {
  const uses = paramUses(s)
  if (uses.length === 0) return { sql: s, params: [], spans: [] }
  const order = [...new Set(uses.map((u) => u.name))]
  let out = ''
  let at = 0
  const spans: Bound['spans'] = []
  for (const u of uses) {
    out += s.slice(at, u.from)
    const ph = '$' + (order.indexOf(u.name) + 1)
    spans.push({ from: out.length, to: out.length + ph.length, origFrom: u.from, origTo: u.to })
    out += ph
    at = u.to
  }
  out += s.slice(at)
  return { sql: out, params: order.map((n) => values[n] ?? null), spans }
}

/** An offset in the bound statement, as an offset in the text it came from. */
export function originalOffset(b: Bound, offset: number): number {
  let shift = 0
  for (const sp of b.spans) {
    if (offset < sp.from) break
    if (offset < sp.to) return sp.origFrom
    shift = sp.origTo - sp.to
  }
  return offset + shift
}
