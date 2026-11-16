import { describe, expect, it } from 'vitest'
import { daysUntil, familyOf, sameTotal, termWords } from './cloudReservations'

describe('reservations', () => {
  it('checks a typed total the way the control plane does', () => {
    expect(sameTotal('$3,592', 3592.4)).toBe(true)
    expect(sameTotal('3592', 3591.6)).toBe(true)
    expect(sameTotal('3,500', 3592.4)).toBe(false)
    expect(sameTotal('$99.50', 99.5)).toBe(true)
    expect(sameTotal('99.4', 99.5)).toBe(false)
    expect(sameTotal('', 0)).toBe(false)
    expect(sameTotal('three', 3)).toBe(false)
  })
  it('words terms and days', () => {
    expect(termWords('36', 'Partial Upfront')).toBe('3 yr · partial upfront')
    expect(daysUntil('2026-11-20T00:00:00Z', Date.parse('2026-11-16T00:00:00Z'))).toBe(4)
    expect(familyOf('m6i.large')).toBe('m6i')
  })
})
