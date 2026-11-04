import { describe, expect, it } from 'vitest'
import {
  editable, editorKind, formatArray, insertStmt, parseArray, parseTarget, sourceTable, updateStmt, type EditTarget,
} from '~/lib/rowEdit'
import type { Col } from '~/lib/resultView'

/**
 * What makes a result editable, and the SQL its changes become: values only
 * ever as parameters, cast to the column's type, and an UPDATE that matches
 * nothing once someone else changed the row.
 */

const cols: Col[] = [
  { name: 'id', type: 'integer', table: 42, attnum: 1 },
  { name: 'Status', type: 'text', table: 42, attnum: 2 },
  { name: 'meta', type: 'jsonb', table: 42, attnum: 4 },
]

const target: EditTarget = {
  table: 'public.orders', oid: 42, pk: [1],
  columns: [
    { attnum: 1, name: 'id', type: 'integer', notNull: true, hasDefault: true, generated: false },
    { attnum: 2, name: 'Status', type: 'text', notNull: false, hasDefault: false, generated: false },
    { attnum: 3, name: 'total', type: 'numeric(10,2)', notNull: true, hasDefault: false, generated: false },
    { attnum: 4, name: 'meta', type: 'jsonb', notNull: false, hasDefault: false, generated: false },
    { attnum: 5, name: 'search', type: 'tsvector', notNull: false, hasDefault: false, generated: true },
  ],
}

describe('editable', () => {
  it('needs every column from one table, and its primary key', () => {
    expect(sourceTable(cols)).toEqual({ oid: 42 })
    expect(editable(cols, target)).toEqual({ ok: true, target, pkCols: [0] })
    expect(editable([...cols, { name: 'n', type: 'bigint' }], target)).toEqual({ ok: false, reason: 'A column is computed, not read from a table.' })
    expect(editable([...cols, { name: 'x', type: 'int', table: 7, attnum: 1 }], target)).toEqual({ ok: false, reason: 'The result joins more than one table.' })
    expect(editable(cols.slice(1), target)).toEqual({ ok: false, reason: 'The result leaves out the primary key.' })
    expect(editable(cols, { ...target, pk: [] })).toMatchObject({ ok: false, reason: expect.stringContaining('has no primary key') })
    expect(editable([...cols, cols[1]!], target)).toEqual({ ok: false, reason: 'The result shows a column twice.' })
  })

  it('reads the table description', () => {
    const t = parseTarget(42, [
      ['public.orders', '1', 'id', 'integer', 'true', 'true', 'false', null, '1', 'a'],
      ['public.orders', '2', 'mood', 'mood', 'false', 'false', 'false', '["sad","ok","happy"]', '1'],
    ])
    expect(t).toEqual({
      table: 'public.orders', oid: 42, pk: [1], columns: [
        { attnum: 1, name: 'id', type: 'integer', notNull: true, hasDefault: true, generated: false, identity: 'a', enumValues: undefined },
        { attnum: 2, name: 'mood', type: 'mood', notNull: false, hasDefault: false, generated: false, identity: '', enumValues: ['sad', 'ok', 'happy'] },
      ],
    })
  })
})

describe('statements', () => {
  it('updates by primary key while the row is as it was shown', () => {
    const s = updateStmt(target, cols, [0], ['7', 'new', '{"a":1}'], new Map([[1, 'paid'], [2, null]]))
    expect(s.sql).toBe(
      'UPDATE public.orders\n'
      + 'SET "Status" = CAST($1 AS text),\n    meta = CAST($2 AS jsonb)\n'
      + 'WHERE id = CAST($3 AS integer)\n'
      + '  AND "Status"::text IS NOT DISTINCT FROM CAST($4 AS text)::text\n'
      + '  AND meta::text IS NOT DISTINCT FROM CAST($5 AS jsonb)::text\n'
      + 'RETURNING *')
    expect(s.params).toEqual(['paid', null, '7', 'new', '{"a":1}'])
    expect(s.label).toBe('Change 2 values in the row with id 7')
  })

  it('inserts what was given and leaves the rest to Postgres', () => {
    const s = insertStmt(target, new Map([[3, '9.50'], [2, null], [5, 'x']]))
    expect(s.sql).toBe('INSERT INTO public.orders ("Status", total)\nVALUES (CAST($1 AS text), CAST($2 AS numeric(10,2)))\nRETURNING *')
    expect(s.params).toEqual([null, '9.50'])
    expect(insertStmt(target, new Map()).sql).toBe('INSERT INTO public.orders DEFAULT VALUES\nRETURNING *')
  })
})

describe('editors', () => {
  it('picks one by type', () => {
    expect(editorKind('boolean')).toBe('boolean')
    expect(editorKind('mood', ['a'])).toBe('enum')
    expect(editorKind('jsonb')).toBe('json')
    expect(editorKind('json')).toBe('json')
    expect(editorKind('text[]')).toBe('array')
    expect(editorKind('timestamp with time zone')).toBe('timestamp')
    expect(editorKind('numeric(10,2)')).toBe('number')
    expect(editorKind('character varying(40)')).toBe('text')
  })

  it('reads and writes one-dimensional arrays', () => {
    expect(parseArray('{}')).toEqual([])
    expect(parseArray('{a,"b c",NULL,"NULL","q\\"x",""}')).toEqual(['a', 'b c', null, 'NULL', 'q"x', ''])
    expect(parseArray('{{1,2},{3,4}}')).toBeNull()
    expect(parseArray('not')).toBeNull()
    const items = ['a', 'b c', null, 'NULL', 'q"x', '', 'back\\slash', '{x}']
    expect(parseArray(formatArray(items))).toEqual(items)
  })
})
