import { describe, expect, it } from 'vitest'
import { budgetState, change, daysLeft, dollars, parseThresholds, totals, type CloudBudget, type CloudCostRead, type Costs } from '~/lib/cloudCosts'

const costs = (o: Partial<Costs>): Costs => ({
  month: '2026-11', currency: 'USD', monthToDate: 0, lastMonth: 0, lastMonthToDate: 0, forecast: null, days: [], services: [], ...o,
})
const read = (id: string, c: Costs | null): CloudCostRead => ({
  accountId: id, accountName: id, cloudAccountId: '1', environment: 'production', readAt: c ? '2026-11-09T00:00:00Z' : null, costs: c,
})

describe('cloud costs', () => {
  it('writes dollars with cents', () => {
    expect(dollars(1234.5)).toBe('$1,234.50')
    expect(dollars(0)).toBe('$0.00')
    expect(dollars(-3)).toBe('-$3.00')
  })

  it('compares only with something', () => {
    expect(change(120, 100)).toBe(20)
    expect(change(50, 0)).toBeNull()
  })

  it('sums the accounts read for the newest month', () => {
    const t = totals([
      read('a', costs({ monthToDate: 10, lastMonth: 30, forecast: 40, days: [{ date: '2026-11-01', amount: 10 }],
        services: [{ service: 'EC2', thisMonth: 10, lastMonth: 30 }] })),
      read('b', costs({ monthToDate: 5.5, lastMonth: 1, forecast: 9, days: [{ date: '2026-11-01', amount: 5.5 }],
        services: [{ service: 'EC2', thisMonth: 5, lastMonth: 1 }, { service: 'S3', thisMonth: 0.5, lastMonth: 0 }] })),
      read('stale', costs({ month: '2026-10', monthToDate: 999 })),
      read('never', null),
    ])
    expect(t.read).toBe(2)
    expect(t.monthToDate).toBe(15.5)
    expect(t.forecast).toBe(49)
    expect(t.days).toEqual([{ date: '2026-11-01', amount: 15.5 }])
    expect(t.services.map((s) => [s.service, s.thisMonth])).toEqual([['EC2', 15], ['S3', 0.5]])
  })

  it('splits the forecast into what AWS predicts', () => {
    expect(totals([read('a', costs({ forecast: 40, forecastRest: 25 })), read('b', costs({ forecast: 9, forecastRest: 4.5 }))]).forecastRest).toBe(29.5)
    expect(totals([read('a', costs({ forecast: 40, forecastRest: 25 })), read('b', costs({ forecast: 9 }))]).forecastRest).toBeNull()
  })

  it('counts the days left in the month, today included', () => {
    expect(daysLeft('2026-11', new Date('2026-11-13T16:00:00Z'))).toBe(18)
    expect(daysLeft('2026-11', new Date('2026-11-30T23:00:00Z'))).toBe(1)
    expect(daysLeft('2026-12', new Date('2026-12-01T00:00:00Z'))).toBe(31)
  })

  it('has no forecast when one account has none', () => {
    expect(totals([read('a', costs({ forecast: 1 })), read('b', costs({}))]).forecast).toBeNull()
  })

  it('places a budget against its thresholds', () => {
    const b: CloudBudget = { id: '1', accountId: '', name: 'all', amount: 100, thresholds: [50, 80, 100], forecast: true,
      createdBy: 'x', createdAt: '', spent: 85, projected: 140, unread: 0 }
    expect(budgetState(b)).toEqual({ pct: 85, over: false, forecastOver: true, passed: 80 })
    expect(budgetState({ ...b, spent: 10, projected: null }).passed).toBe(0)
  })

  it('reads alert thresholds', () => {
    expect(parseThresholds('100, 80 80%')).toEqual([80, 100])
    expect(parseThresholds('')).toEqual([])
    expect(parseThresholds('0')).toBeNull()
    expect(parseThresholds('fifty')).toBeNull()
    expect(parseThresholds('1 2 3 4 5 6')).toBeNull()
  })
})
