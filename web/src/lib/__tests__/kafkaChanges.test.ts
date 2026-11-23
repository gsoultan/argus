import { describe, expect, it } from 'vitest'
import { behindAfter, fromSettingValue, planCount, settingWords, toSettingValue } from '~/lib/kafkaChanges'

describe('Kafka settings in words', () => {
  it('reads values as people say them', () => {
    expect(settingWords({ kind: 'ms' }, '604800000')).toBe('7 days')
    expect(settingWords({ kind: 'ms' }, '3600000')).toBe('1 hour')
    expect(settingWords({ kind: 'ms' }, '-1')).toBe('no limit')
    expect(settingWords({ kind: 'bytes' }, '1073741824')).toBe('1 GB')
    expect(settingWords({ kind: 'bytes' }, '1000')).toBe('1,000 bytes')
    expect(settingWords({ kind: 'choice' }, 'compact')).toBe('compact')
  })
  it('turns an amount and a unit into what Kafka takes, and back', () => {
    expect(toSettingValue('ms', 7, 'days')).toBe('604800000')
    expect(toSettingValue('bytes', 2, 'MB')).toBe('2097152')
    expect(fromSettingValue('ms', '604800000')).toEqual({ amount: 7, unit: 'days' })
    expect(fromSettingValue('bytes', '1048576')).toEqual({ amount: 1, unit: 'MB' })
  })
  it('counts what a dry run moves', () => {
    const p = [{ topic: 't', partition: 0, start: 0, end: 20, now: 5, after: 12 }]
    expect(planCount(p, 'delete')).toBe(7)
    expect(planCount(p, 'reset')).toBe(7)
    expect(behindAfter(p[0]!)).toBe(8)
  })
})
