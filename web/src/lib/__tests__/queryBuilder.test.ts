import { describe, expect, it } from 'vitest'
import { buildSQL, paramFor, parseFks, refSQL, type BQuery, type ForeignKeyRef } from '~/lib/queryBuilder'

const orders = { schema: 'public', name: 'orders' }
const customers = { schema: 'public', name: 'customers' }
const fk: ForeignKeyRef = { name: 'orders_customer_fk', from: orders, fromCols: ['customer_id'], to: customers, toCols: ['id'] }
const base = (over: Partial<BQuery> = {}): BQuery => ({ base: orders, joins: [], columns: [], measures: [], filters: [], sort: [], limit: 100, ...over })

describe('query builder SQL', () => {
  it('reads every column with nothing picked', () => {
    expect(buildSQL(base())).toBe('SELECT t0.*\nFROM public.orders t0\nLIMIT 100;')
  })

  it('walks down to a JSON key', () => {
    expect(refSQL({ alias: 't0', column: 'meta', key: 'address.city' })).toBe(`t0.meta->'address'->>'city'`)
    expect(buildSQL(base({ columns: [{ alias: 't0', column: 'id' }, { alias: 't0', column: 'meta', key: 'tier' }], limit: 0 })))
      .toBe(`SELECT t0.id,\n       t0.meta->>'tier' AS meta_tier\nFROM public.orders t0;`)
  })

  it('joins along a foreign key, filters by parameters, groups and sorts', () => {
    const sql = buildSQL(base({
      joins: [{ on: 't0', fk, forward: true, kind: 'inner', alias: 't1' }],
      columns: [{ alias: 't1', column: 'name' }],
      measures: [{ fn: 'count' }, { fn: 'sum', ref: { alias: 't0', column: 'meta', key: 'total' } }],
      filters: [{ ref: { alias: 't0', column: 'status' }, op: '=', param: 'status' }, { ref: { alias: 't1', column: 'name' }, op: 'contains', param: 'name' }],
      sort: [{ by: 1, desc: true }],
      limit: 10,
    }))
    expect(sql).toBe([
      'SELECT t1.name AS name,',
      '       count(*) AS count,',
      `       sum((t0.meta->>'total')::numeric) AS sum_meta_total`,
      'FROM public.orders t0',
      'JOIN public.customers t1 ON t1.id = t0.customer_id',
      'WHERE t0.status = :status',
      `  AND t1.name::text ILIKE '%' || :name || '%'`,
      'GROUP BY 1',
      'ORDER BY 2 DESC',
      'LIMIT 10;',
    ].join('\n'))
  })

  it('follows a key back to the rows that point here', () => {
    const sql = buildSQL({ ...base({ base: customers }), joins: [{ on: 't0', fk, forward: false, kind: 'left', alias: 't1' }] })
    expect(sql).toContain('LEFT JOIN public.orders t1 ON t1.customer_id = t0.id')
  })

  it('quotes identifiers that need it', () => {
    expect(buildSQL(base({ base: { schema: 'Sales', name: 'order lines' }, columns: [{ alias: 't0', column: 'Qty' }] })))
      .toContain('SELECT t0."Qty"\nFROM "Sales"."order lines" t0')
  })

  it('names each filter parameter once', () => {
    const q = base({ filters: [{ ref: { alias: 't0', column: 'status' }, op: '=', param: 'status' }] })
    expect(paramFor(q, { alias: 't0', column: 'status' })).toBe('status_2')
    expect(paramFor(q, { alias: 't0', column: 'meta', key: 'a.b' })).toBe('meta_a_b')
  })

  it('reads foreign keys from the catalogue rows', () => {
    expect(parseFks([['k', 'public', 'orders', '["a","b"]', 'public', 'c', '["x","y"]']])[0])
      .toEqual({ name: 'k', from: orders, fromCols: ['a', 'b'], to: { schema: 'public', name: 'c' }, toCols: ['x', 'y'] })
  })
})
