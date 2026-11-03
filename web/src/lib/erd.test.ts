import { describe, expect, it } from 'vitest'
import { buildErd, erdTablesSQL, layout, neighbours, shownTables } from './erd'

const tables = [['1', 'customers', 'r', '84000', '1'], ['2', 'orders', 'r', '1200000', '2'], ['3', 'shipments', 'r', '9', '3']]
const columns = [
  ['1', 'id', 'bigint', 't', 't'], ['1', 'email', 'text', 'f', 'f'],
  ['2', 'id', 'bigint', 't', 't'], ['2', 'customer_id', 'bigint', 'f', 'f'],
  ['3', 'id', 'bigint', 't', 't'], ['3', 'order_id', 'bigint', 'f', 'f'],
]
const links = [
  ['2', '1', 'public', 'customers', 'orders_customer_id_fkey', 'customer_id', 'id'],
  ['3', '2', 'public', 'orders', 'shipments_order_id_fkey', 'order_id', 'id'],
  ['2', '99', 'billing', 'accounts', 'orders_account_fkey', 'account_id', 'id'],
]

describe('erd', () => {
  const erd = buildErd('public', tables, columns, links)

  it('marks keys and keeps links to other schemas apart', () => {
    expect(erd.byId.get('2')!.columns.find((c) => c.name === 'customer_id')!.fk).toBe(true)
    expect(erd.byId.get('1')!.columns[0]!.pk).toBe(true)
    expect(erd.links[2]!.external).toBe('billing.accounts')
  })

  it('knows a table’s neighbours', () => {
    expect(neighbours(erd, '2')).toEqual({ parents: ['1'], children: ['3'] })
    expect(shownTables(erd, '1').sort()).toEqual(['1', '2'])
    expect(shownTables(erd, null)).toHaveLength(3)
  })

  it('puts parents left and children right of the focus', () => {
    const p = layout(erd, shownTables(erd, '2'), '2')
    expect(p['1']!.x).toBeLessThan(p['2']!.x)
    expect(p['3']!.x).toBeGreaterThan(p['2']!.x)
  })

  it('lays the whole schema out without overlaps', () => {
    const p = layout(erd, shownTables(erd, null), null)
    const spots = Object.values(p).map((v) => `${v.x},${v.y}`)
    expect(new Set(spots).size).toBe(3)
  })

  it('quotes the schema name as a literal', () => {
    expect(erdTablesSQL("it's")).toContain("n.nspname = 'it''s'")
  })
})
