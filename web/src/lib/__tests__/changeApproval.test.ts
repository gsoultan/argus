import { describe, expect, it } from 'vitest'
import { combineChecks, deciderWords, impactOf, impactTiles, impactWords, writesNeedApproval } from '~/lib/changeApproval'

describe('change approval', () => {
  it('follows the switch, and the label when it is unset', () => {
    const db = (label: 'production' | 'staging', writeApproval?: boolean) =>
      ({ engine: 'postgres' as const, label, policy: writeApproval === undefined ? {} : { writeApproval } })
    expect(writesNeedApproval(db('production'))).toBe(true)
    expect(writesNeedApproval(db('staging'))).toBe(false)
    expect(writesNeedApproval(db('production', false))).toBe(false)
    expect(writesNeedApproval(db('staging', true))).toBe(true)
    expect(writesNeedApproval({ engine: 'redis', label: 'production', policy: {} })).toBe(true)
    expect(writesNeedApproval({ engine: 'rabbitmq', label: 'production', policy: {} })).toBe(false)
  })

  it('adds up the dry runs, cascades included, and keeps at most 20 preview rows', () => {
    const rows = (n: number) => Array.from({ length: n }, (_, i) => [String(i)])
    const i = combineChecks([
      { rows: 12, tables: [{ name: 'public.orders', updated: 12 }], preview: { change: 'update', columns: ['id'], rows: rows(12) } },
      { rows: 2, tables: [{ name: 'public.orders', deleted: 2 }, { name: 'public.order_items', deleted: 4 }],
        preview: { change: 'delete', columns: ['id'], rows: rows(12) } },
      null,
    ])
    expect(i.rows).toBe(18)
    expect(impactWords(i)).toBe('18 rows · 2 tables')
    expect(i.preview!.reduce((n, p) => n + p.rows.length, 0)).toBe(20)
    expect(impactTiles(i)).toEqual([
      { n: 12, what: 'updated', tables: ['orders'] },
      { n: 6, what: 'deleted', tables: ['orders', 'order_items'] },
    ])
    expect(combineChecks([null])).toEqual({})
  })

  it('reads what an older console stored', () => {
    expect(impactOf({ rows: 3 })).toEqual({ rows: 3 })
    expect(impactWords(impactOf({ rows: 1 }))).toBe('1 row')
    expect(impactOf(undefined)).toEqual({})
  })

  it('names who decides, never the asker', () => {
    expect(deciderWords('orders', null)).toBe('an admin or an approver')
    expect(deciderWords('orders', { needed: true, default: true, approvers: 'editors', editors: ['ana@x.io'] }, 'ana@x.io'))
      .toMatch(/no other editor/)
    expect(deciderWords('orders', { needed: true, default: true, approvers: 'editors', editors: ['ana@x.io', 'bo@x.io'] }, 'ana@x.io'))
      .toBe('an admin, an approver or another editor of orders')
  })
})
