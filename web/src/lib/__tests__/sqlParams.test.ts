import { describe, expect, it } from 'vitest'
import { bindParams, originalOffset, paramNames } from '~/lib/sqlParams'
import { charOffset, errorSpan, pgErrorOffset } from '~/components/SqlEditor'

describe(':name parameters', () => {
  it('finds names only where SQL is', () => {
    expect(paramNames(`SELECT * FROM t WHERE a = :a AND b = :b_2 OR a = :a`)).toEqual(['a', 'b_2'])
    expect(paramNames(`SELECT x::text, ':no', ":no", $$ :no $$, $f$ :no $f$ -- :no
      /* :no /* :no */ */ FROM t WHERE y = :yes`)).toEqual(['yes'])
    expect(paramNames(`SELECT arr[1:n], E'\\' :no', 'it''s :no' FROM t`)).toEqual([])
    expect(paramNames(`SELECT :a, (:b), f(:c,:d)`)).toEqual(['a', 'b', 'c', 'd'])
  })

  it('binds each name to one number and keeps NULL', () => {
    const b = bindParams(`SELECT :a, :b, :a`, { a: '1' })
    expect(b.sql).toBe('SELECT $1, $2, $1')
    expect(b.params).toEqual(['1', null])
    expect(bindParams('SELECT 1', {})).toEqual({ sql: 'SELECT 1', params: [], spans: [] })
  })

  it('maps an offset in the bound statement back to the text', () => {
    const src = `SELECT :customer, nope FROM t`
    const b = bindParams(src, {})
    expect(b.sql).toBe('SELECT $1, nope FROM t')
    expect(originalOffset(b, b.sql.indexOf('nope'))).toBe(src.indexOf('nope'))
    expect(originalOffset(b, b.sql.indexOf('$1') + 1)).toBe(src.indexOf(':customer'))
    expect(originalOffset(b, 3)).toBe(3)
  })
})

describe('error position', () => {
  it('counts characters the way Postgres does', () => {
    const s = `SELECT '😀', nope`
    // Postgres: position 13 is the n of nope (1-based characters).
    expect(s.slice(charOffset(s, 0, 12))).toBe('nope')
    expect(pgErrorOffset(s, 13, 0)).toBe(s.indexOf('nope'))
    // Run from a selection that starts at 10.
    expect(pgErrorOffset(s, 13, 10)).toBe(10 + s.indexOf('nope'))
  })

  it('marks the word at the position', () => {
    const s = 'SELECT nope FROM t'
    expect(errorSpan(s, 7)).toEqual({ from: 7, to: 11 })
    expect(errorSpan(s, 99)).toEqual({ from: s.length, to: s.length })
  })
})
