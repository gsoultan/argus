import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { classifySQL, needsCheck, type SQLClass } from '../sqlClass'

// The agent's own cases: the same file internal/tunnel/sqlclass_test.go reads.
const file = `${process.cwd()}/../internal/tunnel/testdata/sqlclass.json`
const cases = JSON.parse(readFileSync(file, 'utf8')) as { sql: string; want: SQLClass }[]

describe('classifySQL', () => {
  it('has the shared cases', () => {
    expect(cases.length).toBeGreaterThanOrEqual(20)
  })
  for (const c of cases) {
    it(JSON.stringify(c.sql), () => {
      expect(classifySQL(c.sql)).toEqual(c.want)
    })
  }
})

describe('needsCheck', () => {
  it('checks writes that may touch many rows, not a plain insert', () => {
    expect(needsCheck('DELETE FROM orders WHERE id = 1')).toBe(true)
    expect(needsCheck('update orders set a = 1')).toBe(true)
    expect(needsCheck('INSERT INTO t SELECT * FROM s')).toBe(true)
    expect(needsCheck("INSERT INTO t VALUES ('select')")).toBe(false)
    expect(needsCheck('SELECT 1')).toBe(false)
    expect(needsCheck('DROP TABLE t')).toBe(false)
  })
})
