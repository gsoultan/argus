/*
What a statement looks like, near enough to stop an accident.

The console's copy of internal/tunnel/sqlclass.go. The agent decides; this
one only chooses whether to check a write before it runs and what the dialog
says. Both read internal/tunnel/testdata/sqlclass.json in their tests, so
they cannot drift apart without one failing.
*/

export interface SQLClass {
  verb: string
  object?: string
  targets?: string[]
  hasWhere?: boolean
  writes?: boolean
  cascade?: boolean
}

/** The agent's guard codes, in the answer's SQLSTATE field. */
export const GUARD_NO_WHERE = 'AR001'
export const GUARD_DROP = 'AR002'
export const GUARD_DROP_DB = 'AR003'
export const GUARD_CHECK_ONLY = 'AR004'

export interface PGGuard {
  code: string
  verb: string
  object?: string
  targets?: string[]
  cascade?: boolean
  confirm?: string
  reason: string
}

interface Tok { text: string; quoted: boolean; punct: string; depth: number }

const isLetter = (c: string) => /[\p{L}_]/u.test(c)
const isIdent = (c: string) => /[\p{L}\p{N}_$]/u.test(c)

function lex(s: string): Tok[] {
  const out: Tok[] = []
  let depth = 0
  let i = 0
  while (i < s.length) {
    const c = s.charAt(i)
    if (c === '-' && s.charAt(i + 1) === '-') {
      while (i < s.length && s.charAt(i) !== '\n') i++
    } else if (c === '/' && s.charAt(i + 1) === '*') {
      let n = 0
      while (i < s.length) {
        if (s.charAt(i) === '/' && s.charAt(i + 1) === '*') { n++; i += 2; continue }
        if (s.charAt(i) === '*' && s.charAt(i + 1) === '/') { n--; i += 2; if (n === 0) break; continue }
        i++
      }
    } else if (c === "'") {
      const backslash = i > 0 && (s.charAt(i - 1) === 'e' || s.charAt(i - 1) === 'E')
      let j = i + 1
      while (j < s.length) {
        if (backslash && s.charAt(j) === '\\') { j += 2; continue }
        if (s.charAt(j) === "'") {
          if (s.charAt(j + 1) === "'") { j += 2; continue }
          j++
          break
        }
        j++
      }
      i = j
    } else if (c === '"') {
      let j = i + 1
      let b = ''
      while (j < s.length) {
        if (s.charAt(j) === '"') {
          if (s.charAt(j + 1) === '"') { b += '"'; j += 2; continue }
          break
        }
        b += s.charAt(j)
        j++
      }
      out.push({ text: b, quoted: true, punct: '', depth })
      i = j + 1
    } else if (c === '$') {
      let j = i + 1
      while (j < s.length && /[A-Za-z0-9_\u0080-￿]/.test(s.charAt(j)) && !(/[0-9]/.test(s.charAt(j)) && j === i + 1)) j++
      if (s.charAt(j) === '$') {
        const tag = s.slice(i, j + 1)
        const end = s.indexOf(tag, j + 1)
        if (end < 0) return out
        i = end + tag.length
      } else {
        i = j
        while (i < s.length && /[0-9]/.test(s.charAt(i))) i++
      }
    } else if (c === '(') {
      out.push({ text: '', quoted: false, punct: c, depth })
      depth++
      i++
    } else if (c === ')') {
      if (depth > 0) depth--
      out.push({ text: '', quoted: false, punct: c, depth })
      i++
    } else if (c === ',' || c === ';' || c === '.' || c === '*') {
      out.push({ text: '', quoted: false, punct: c, depth })
      i++
    } else if (isLetter(c)) {
      let j = i
      while (j < s.length && isIdent(s.charAt(j))) j++
      out.push({ text: s.slice(i, j).toLowerCase(), quoted: false, punct: '', depth })
      i = j
    } else {
      i++
    }
  }
  return out
}

const NONE: Tok = { text: '', quoted: false, punct: '\0', depth: -1 }
const at = (t: Tok[], i: number): Tok => t[i] ?? NONE

const skip = (t: Tok[], i: number, w: string) => (i < t.length && !at(t, i).punct && !at(t, i).quoted && at(t, i).text === w ? i + 1 : i)

function closeParen(t: Tok[], i: number): number {
  const d = at(t, i).depth
  for (let j = i + 1; j < t.length; j++) if (at(t, j).punct === ')' && at(t, j).depth === d) return j + 1
  return t.length
}

function readName(t: Tok[], i: number): [string, number] {
  if (i >= t.length || at(t, i).punct) return ['', i]
  let name = at(t, i).text
  i++
  while (i + 1 < t.length && at(t, i).punct === '.' && !at(t, i + 1).punct) {
    name += '.' + at(t, i + 1).text
    i += 2
  }
  return [name, i]
}

function readNameList(t: Tok[], i: number): string[] {
  const names: string[] = []
  while (i < t.length) {
    i = skip(t, i, 'only')
    const [name, j] = readName(t, i)
    if (!name || ['cascade', 'restrict', 'restart', 'continue', 'with'].includes(name)) break
    names.push(name)
    i = j
    if (at(t, i).punct === '*') i++
    if (at(t, i).punct === ',') { i++; continue }
    break
  }
  return names
}

const hasTopWord = (t: Tok[], w: string) => t.some((x) => x.depth === 0 && !x.punct && !x.quoted && x.text === w)

function lastWord(t: Tok[]): string {
  for (let i = t.length - 1; i >= 0; i--) {
    if (!at(t, i).punct && at(t, i).depth === 0) return at(t, i).quoted ? '' : at(t, i).text
  }
  return ''
}

/** classifySQL mirrors tunnel.ClassifySQL. Absent fields are left out, as the Go JSON omits them. */
export function classifySQL(sql: string): SQLClass {
  let t = lex(sql)
  const semi = t.findIndex((x) => x.punct === ';' && x.depth === 0)
  if (semi >= 0) t = t.slice(0, semi)
  const c: SQLClass = { verb: '' }
  let i = 0
  if (at(t, 0) && at(t, 0).text === 'with' && !at(t, 0).quoted) {
    i = skip(t, 1, 'recursive')
    while (i < t.length) {
      if (at(t, i).punct) break
      i++
      if (at(t, i).punct === '(') i = closeParen(t, i)
      i = skip(t, i, 'as')
      i = skip(t, i, 'not')
      i = skip(t, i, 'materialized')
      if (at(t, i).punct !== '(') break
      if (['insert', 'update', 'delete', 'merge'].includes(at(t, i + 1).text ?? '')) c.writes = true
      i = closeParen(t, i)
      if (at(t, i).punct === ',' && at(t, i).depth === 0) { i++; continue }
      break
    }
  }
  if (i >= t.length || at(t, i).punct) return c
  c.verb = at(t, i).text
  const rest = t.slice(i + 1)
  switch (c.verb) {
    case 'insert':
    case 'merge':
      c.writes = true
      break
    case 'update': {
      c.writes = true
      const [name] = readName(rest, skip(rest, 0, 'only'))
      if (name) c.targets = [name]
      if (hasTopWord(rest, 'where')) c.hasWhere = true
      break
    }
    case 'delete': {
      c.writes = true
      const [name] = readName(rest, skip(rest, skip(rest, 0, 'from'), 'only'))
      if (name) c.targets = [name]
      if (hasTopWord(rest, 'where')) c.hasWhere = true
      break
    }
    case 'truncate': {
      c.writes = true
      const names = readNameList(rest, skip(rest, 0, 'table'))
      if (names.length) c.targets = names
      if (lastWord(rest) === 'cascade') c.cascade = true
      break
    }
    case 'drop': {
      let j = 0
      if (at(rest, 0) && at(rest, 0).text === 'materialized' && !at(rest, 0).quoted) {
        c.object = 'materialized view'
        j = 2
      } else if (at(rest, 0) && !at(rest, 0).punct) {
        c.object = at(rest, 0).text
        j = 1
      }
      j = skip(rest, j, 'concurrently')
      if (at(rest, j).text === 'if' && at(rest, j + 1).text === 'exists') j += 2
      const names = readNameList(rest, j)
      if (names.length) c.targets = names
      if (lastWord(rest) === 'cascade') c.cascade = true
      break
    }
  }
  return c
}

/**
 * needsCheck says the console should count a statement before running it:
 * an UPDATE, DELETE or MERGE, or an INSERT that copies rows from a query.
 * A plain INSERT … VALUES is what it says it is.
 */
export function needsCheck(sql: string): boolean {
  const c = classifySQL(sql)
  if (c.verb === 'update' || c.verb === 'delete' || c.verb === 'merge') return true
  if (c.verb === 'insert') return /\bselect\b/i.test(sql.replace(/'(?:[^']|'')*'/g, "''"))
  return false
}
