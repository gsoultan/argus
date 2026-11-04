import { describe, expect, it } from 'vitest'
import { jsonAt, jsonKeys, summarize, summaryQuery } from '~/lib/summarize'

const cols = [{ name: 'status', type: 'text' }, { name: 'total', type: 'numeric' }, { name: 'at', type: 'timestamp with time zone' }, { name: 'meta', type: 'jsonb' }]
const rows = [
  ['paid', '10', '2026-01-03 10:00:00+00', '{"tier":"gold","address":{"city":"Bandung"}}'],
  ['paid', '5.5', '2026-01-20 10:00:00+00', '{"tier":"silver"}'],
  ['open', '7', '2026-02-01 10:00:00+00', '{"tier":"gold","address":{"city":"Jakarta"}}'],
  [null, null, '2026-02-02 10:00:00+00', null],
]

describe('summarize', () => {
  it('counts per value, biggest first', () => {
    expect(summarize(cols, rows, { by: 0, bucket: 'value', measure: 'count' })).toEqual([
      { label: 'paid', value: 2, rows: 2 }, { label: '(null)', value: 1, rows: 1 }, { label: 'open', value: 1, rows: 1 },
    ])
  })

  it('adds a number per month, in order', () => {
    expect(summarize(cols, rows, { by: 2, bucket: 'month', measure: 'sum', of: 1 })).toEqual([
      { label: '2026-01', value: 15.5, rows: 2 }, { label: '2026-02', value: 7, rows: 2 },
    ])
  })

  it('groups by a key inside JSON', () => {
    expect(jsonKeys(rows, 3)).toEqual(['tier', 'address', 'address.city'])
    expect(jsonAt(rows[0]![3]!, 'address.city')).toBe('Bandung')
    expect(summarize(cols, rows, { by: 3, key: 'tier', bucket: 'value', measure: 'avg', of: 1 })[0]).toEqual({ label: 'gold', value: 8.5, rows: 2 })
  })

  it('folds the long tail into Other', () => {
    const many = Array.from({ length: 40 }, (_, i) => [`s${i}`, '1', null, null])
    const out = summarize(cols, many, { by: 0, bucket: 'value', measure: 'count' }, 10)
    expect(out).toHaveLength(10)
    expect(out[9]).toEqual({ label: 'Other', value: 31, rows: 31 })
  })

  it('writes the same summary for the database', () => {
    expect(summaryQuery('SELECT * FROM orders;', cols, { by: 3, key: 'address.city', bucket: 'value', measure: 'sum', of: 1 })).toBe(
      `SELECT q.meta::jsonb->'address'->>'city' AS meta_address_city, sum(q.total) AS sum_total\nFROM (\nSELECT * FROM orders\n) q\nGROUP BY 1\nORDER BY 2 DESC;`)
    expect(summaryQuery('SELECT * FROM orders', cols, { by: 2, bucket: 'day', measure: 'count' })).toContain(`date_trunc('day', q.at)::date AS at, count(*) AS count`)
  })
})
