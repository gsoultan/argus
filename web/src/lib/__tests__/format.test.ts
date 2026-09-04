import { describe, expect, it } from 'vitest'
import { absTime, bytes, duration, relTime } from '~/components/primitives'
import { stamp } from '~/lib/download'

describe('bytes', () => {
  it('scales units and keeps one decimal above a kilobyte', () => {
    expect(bytes(512)).toBe('512 B')
    expect(bytes(2048)).toBe('2.0 KB')
    expect(bytes(5 * 1024 ** 2)).toBe('5.0 MB')
  })
})

describe('duration', () => {
  const t0 = '2026-08-28T09:00:00.000Z'
  it('formats seconds, minutes and hours', () => {
    expect(duration(t0, '2026-08-28T09:00:45.000Z')).toBe('45s')
    expect(duration(t0, '2026-08-28T09:02:05.000Z')).toBe('2m 5s')
    expect(duration(t0, '2026-08-28T11:30:00.000Z')).toBe('2h 30m')
  })

  /** A clock skew must not render as a negative duration. */
  it('never goes below zero', () => {
    expect(duration(t0, '2026-08-28T08:00:00.000Z')).toBe('0s')
  })
})

describe('relTime / absTime', () => {
  it('renders a null timestamp as an em dash rather than "Invalid Date"', () => {
    expect(relTime(null)).toBe('—')
    expect(absTime(null)).toBe('—')
  })
})

describe('stamp', () => {
  it('produces a filesystem-safe, sortable stamp', () => {
    expect(stamp(new Date(2026, 8, 4, 14, 12, 5))).toBe('20260904-141205')
  })
})
