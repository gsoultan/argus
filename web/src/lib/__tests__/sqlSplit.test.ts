import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { splitSQL } from '../sqlSplit'

// The agent's test runs these same statements, one request each, on a real
// Postgres (internal/agent/postgres_script_test.go).
const dir = `${process.cwd()}/../internal/agent/testdata`
const script = readFileSync(`${dir}/script.sql`, 'utf8')
const want = JSON.parse(readFileSync(`${dir}/script.statements.json`, 'utf8')) as { sql: string; line: number; from: number }[]

const sqls = (s: string) => splitSQL(s).map((x) => x.sql)

describe('splitSQL', () => {
  it('cuts a script with temp tables, $$ bodies and a ROLLBACK the way Postgres reads it', () => {
    expect(splitSQL(script)).toEqual(want)
    expect(want).toHaveLength(12)
    expect(want[0]!.sql).toBe('BEGIN')
    expect(want.at(-1)!.sql).toBe('ROLLBACK')
    for (const w of want) expect(script.slice(w.from, w.from + w.sql.length)).toBe(w.sql)
  })

  it('leaves one statement alone', () => {
    expect(sqls('SELECT 1')).toEqual(['SELECT 1'])
    expect(sqls('  SELECT 1;  ')).toEqual(['SELECT 1'])
    expect(sqls('-- only a comment\n/* and another */')).toEqual([])
    expect(sqls(';;')).toEqual([])
  })

  it('drops comments in front of a statement and keeps those inside', () => {
    expect(sqls('-- first\nSELECT 1; /* two */ SELECT /* x; */ 2 -- y;\n')).toEqual(['SELECT 1', 'SELECT /* x; */ 2 -- y;'])
  })

  it('does not cut inside strings, names, dollar quotes or parentheses', () => {
    expect(sqls(`SELECT 'a;b', "c;d", $$e;f$$, $t$g;$$;h$t$; SELECT 2`)).toEqual([`SELECT 'a;b', "c;d", $$e;f$$, $t$g;$$;h$t$`, 'SELECT 2'])
    expect(sqls(`SELECT E'x\\';y'; SELECT 'it''s;'`)).toEqual([`SELECT E'x\\';y'`, `SELECT 'it''s;'`])
    expect(sqls(`SELECT 'x\\'; SELECT 2`)).toEqual([`SELECT 'x\\'`, 'SELECT 2'])
    expect(sqls(`CREATE RULE r AS ON INSERT TO t DO ALSO (NOTIFY a; NOTIFY b); SELECT 1`)).toEqual(['CREATE RULE r AS ON INSERT TO t DO ALSO (NOTIFY a; NOTIFY b)', 'SELECT 1'])
  })

  it('reads $1 as a parameter and a$b$ as a name, not as dollar quotes', () => {
    expect(sqls('PREPARE p AS SELECT $1; EXECUTE p(1)')).toEqual(['PREPARE p AS SELECT $1', 'EXECUTE p(1)'])
    expect(sqls('SELECT a$b$ FROM t; SELECT 2')).toEqual(['SELECT a$b$ FROM t', 'SELECT 2'])
  })

  it('keeps a BEGIN ATOMIC body whole, CASE … END inside it included', () => {
    const fn = `CREATE FUNCTION f(i int) RETURNS int LANGUAGE sql BEGIN ATOMIC
  SELECT CASE WHEN i > 0 THEN 1 ELSE 0 END;
  SELECT i;
END`
    expect(sqls(`${fn}; SELECT f(1)`)).toEqual([fn, 'SELECT f(1)'])
    expect(sqls('BEGIN; SELECT 1; END; SELECT 2')).toEqual(['BEGIN', 'SELECT 1', 'END', 'SELECT 2'])
  })

  it('sends an unterminated quote as it is, for Postgres to refuse', () => {
    expect(sqls(`SELECT 1; SELECT 'open; SELECT 3`)).toEqual(['SELECT 1', `SELECT 'open; SELECT 3`])
    expect(sqls('SELECT $$open; SELECT 3')).toEqual(['SELECT $$open; SELECT 3'])
  })

  it('says on which line each statement starts', () => {
    expect(splitSQL('SELECT 1;\n\n-- c\nSELECT\n 2;').map((x) => x.line)).toEqual([1, 4])
  })

  it('gives each statement its offset as a string index, past astral characters too', () => {
    const s = "SELECT '😀';\n  SELECT 2"
    const got = splitSQL(s)
    expect(got.map((x) => x.from)).toEqual([0, s.indexOf('SELECT 2')])
    expect(s.slice(got[1]!.from)).toBe('SELECT 2')
  })
})
