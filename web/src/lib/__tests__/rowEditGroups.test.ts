import { describe, expect, it } from 'vitest'
import { splitSQL } from '~/lib/sqlSplit'
import { editableGroups, groupUpdateStmt, jsonKeyComment, jsonKeyHints, type EditTarget } from '~/lib/rowEdit'
import type { Col } from '~/lib/resultView'

const col = (attnum: number, name: string, type: string) => ({ attnum, name, type, notNull: false, hasDefault: false, generated: false })
const orders: EditTarget = { table: 'orders', oid: 10, pk: [1], columns: [col(1, 'id', 'bigint'), col(2, 'customer_id', 'bigint'), col(3, 'meta', 'jsonb')] }
const customers: EditTarget = { table: 'customers', oid: 20, pk: [1], columns: [col(1, 'id', 'bigint'), col(2, 'name', 'text')] }
const targets = new Map([[10, orders], [20, customers]])

describe('Editing a join, table by table', () => {
  const cols: Col[] = [
    { name: 'id', type: 'bigint', table: 10, attnum: 1 },
    { name: 'name', type: 'text', table: 20, attnum: 2 },
    { name: 'id', type: 'bigint', table: 20, attnum: 1 },
    { name: 'meta_tier', type: 'text' },
    { name: 'total', type: 'numeric' },
  ]
  const hints = jsonKeyHints(`${jsonKeyComment({ meta_tier: { table: 'public.orders', column: 'meta', path: ['tier'] } })}SELECT …`)

  it('finds each table whose key is in the result, and the JSON key the builder named', () => {
    const r = editableGroups(cols, targets, hints, new Map([['public.orders', 10]]))
    if (!r.ok) throw new Error(r.reason)
    expect(r.groups.map((g) => g.target.table)).toEqual(['orders', 'customers'])
    expect(r.byCol.map((g) => g?.target.table)).toEqual(['orders', 'customers', 'customers', 'orders', undefined])
    expect(r.why[4]).toBe('computed, not read from a table')
  })

  it('leaves a table read-only when its key is missing, and says why', () => {
    const r = editableGroups(cols.slice(0, 2), targets)
    if (!r.ok) throw new Error(r.reason)
    expect(r.groups.map((g) => g.target.table)).toEqual(['orders'])
    expect(r.why[1]).toBe('the result leaves out the primary key of customers')
    expect(editableGroups(cols.slice(1, 2), targets)).toEqual({ ok: false, reason: 'The result leaves out the primary key of customers.' })
  })

  it('writes a JSON key back into its document, keeping its JSON type', () => {
    const r = editableGroups(cols, targets, hints, new Map([['public.orders', 10]]))
    if (!r.ok) throw new Error(r.reason)
    const s = groupUpdateStmt(r.byCol[3]!, cols, ['7', 'Ana', '3', 'gold', '1'], new Map([[3, 'platinum']]))
    expect(s.sql).toBe([
      'UPDATE orders',
      `SET meta = jsonb_set(COALESCE(meta::jsonb, '{}'::jsonb), CAST($1 AS text[]), CASE WHEN CAST($2 AS text) IS NULL THEN 'null'::jsonb`
        + ` ELSE CAST(CASE WHEN jsonb_typeof(meta::jsonb #> CAST($1 AS text[])) IN ('number', 'boolean', 'object', 'array') THEN CAST($2 AS text)`
        + ` ELSE to_jsonb(CAST($2 AS text))::text END AS jsonb) END, true)`,
      'WHERE id = CAST($3 AS bigint)',
      '  AND meta::jsonb #>> CAST($4 AS text[]) IS NOT DISTINCT FROM CAST($5 AS text)',
      'RETURNING *',
    ].join('\n'))
    expect(s.params).toEqual(['{tier}', 'platinum', '7', '{tier}', 'gold'])
    const c = groupUpdateStmt(r.byCol[1]!, cols, ['7', 'Ana', '3', 'gold', '1'], new Map([[1, 'Ana Sari']]))
    expect(c.sql).toContain('UPDATE customers\nSET name = CAST($1 AS text)\nWHERE id = CAST($2 AS bigint)')
    expect(c.params).toEqual(['Ana Sari', '3', 'Ana'])
  })

  it('keeps the note through a split script, whatever the names hold', () => {
    const note = jsonKeyComment({ 'a*/b': { table: 'public.orders', column: 'meta', path: ['x*/y'] } })
    const [stmt] = splitSQL(`-- mine\nSELECT ${note} 1;`)
    expect(jsonKeyHints(stmt!.sql)).toEqual({ 'a*/b': { table: 'public.orders', column: 'meta', path: ['x*/y'] } })
  })
})
