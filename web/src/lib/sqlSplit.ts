/*
Splitting a script into the statements Postgres would see.

The agent runs one statement per request over the extended protocol, which
refuses more than one ("cannot insert multiple commands into a prepared
statement"), and that is on purpose: each statement is recorded, guarded and
checked on its own. So a pasted script is cut here, the way psql cuts it, and
sent one statement at a time on the same session, where a BEGIN, a temp
table or a pg_temp function lasts until the script ends it.

A semicolon ends a statement unless it is inside a string ('…', E'…'), a
quoted name ("…"), a dollar-quoted body ($$…$$, $fn$…$fn$), a comment, a
parenthesis, or a SQL-standard function body (BEGIN ATOMIC … END).

Comments in front of a statement are left out of it, so a statement starts
with its verb (what the agent and txAfter read); comments inside one stay.
*/

export interface Statement {
  sql: string
  /** 1-based line of the statement's first character in the script. */
  line: number
}

const identStart = /[A-Za-z_\u0080-￿]/
const identChar = /[A-Za-z0-9_$\u0080-￿]/
const tagChar = /[A-Za-z0-9_\u0080-￿]/

export function splitSQL(script: string): Statement[] {
  const out: Statement[] = []
  const s = script
  let start = -1 // first character of the statement being read, -1 before one
  let depth = 0
  // SQL-standard bodies: BEGIN ATOMIC opens, CASE nests, END closes.
  let atomic = 0
  let prevWord = ''
  let i = 0

  const lineAt = (p: number) => {
    let n = 1
    for (let k = 0; k < p; k++) if (s.charCodeAt(k) === 10) n++
    return n
  }
  const end = (p: number) => {
    if (start >= 0) {
      const sql = s.slice(start, p).trim()
      if (sql) out.push({ sql, line: lineAt(start) })
    }
    start = -1
    depth = 0
    atomic = 0
    prevWord = ''
  }
  const mark = (p: number) => { if (start < 0) start = p }

  while (i < s.length) {
    const c = s.charAt(i)
    if (c === '-' && s.charAt(i + 1) === '-') {
      while (i < s.length && s.charAt(i) !== '\n') i++
      continue
    }
    if (c === '/' && s.charAt(i + 1) === '*') {
      let n = 0
      while (i < s.length) {
        if (s.charAt(i) === '/' && s.charAt(i + 1) === '*') { n++; i += 2; continue }
        if (s.charAt(i) === '*' && s.charAt(i + 1) === '/') { n--; i += 2; if (n === 0) break; continue }
        i++
      }
      continue
    }
    if (/\s/.test(c)) { i++; continue }
    mark(i)
    if (c === "'") {
      const prev = s.charAt(i - 1)
      const backslash = (prev === 'e' || prev === 'E') && !identChar.test(s.charAt(i - 2) || ' ')
      let j = i + 1
      while (j < s.length) {
        const d = s.charAt(j)
        if (backslash && d === '\\') { j += 2; continue }
        if (d === "'") {
          if (s.charAt(j + 1) === "'") { j += 2; continue }
          break
        }
        j++
      }
      i = j + 1
      prevWord = ''
      continue
    }
    if (c === '"') {
      let j = i + 1
      while (j < s.length) {
        if (s.charAt(j) === '"') {
          if (s.charAt(j + 1) === '"') { j += 2; continue }
          break
        }
        j++
      }
      i = j + 1
      prevWord = ''
      continue
    }
    if (c === '$') {
      // A dollar quote: $$ or $tag$, where the tag does not start with a digit.
      let j = i + 1
      if (j < s.length && !/[0-9]/.test(s.charAt(j))) while (j < s.length && tagChar.test(s.charAt(j))) j++
      if (s.charAt(j) === '$') {
        const tag = s.slice(i, j + 1)
        const close = s.indexOf(tag, j + 1)
        i = close < 0 ? s.length : close + tag.length
      } else {
        i++ // $1, a parameter
        while (i < s.length && /[0-9]/.test(s.charAt(i))) i++
      }
      prevWord = ''
      continue
    }
    if (identStart.test(c)) {
      let j = i
      while (j < s.length && identChar.test(s.charAt(j))) j++
      const w = s.slice(i, j).toLowerCase()
      if (w === 'atomic' && prevWord === 'begin') atomic++
      else if (atomic > 0 && w === 'case') atomic++
      else if (atomic > 0 && w === 'end') atomic--
      prevWord = w
      i = j
      continue
    }
    if (c === '(') depth++
    else if (c === ')') { if (depth > 0) depth-- }
    else if (c === ';' && depth === 0 && atomic === 0) {
      end(i)
      i++
      continue
    }
    prevWord = ''
    i++
  }
  end(s.length)
  return out
}
