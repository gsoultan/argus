import { describe, expect, it } from 'vitest'
import { SCHEDULE_PRESETS, scheduleOf, weekRuns, windowWords } from '../cloud'

describe('weekRuns', () => {
  it('draws office hours on weekdays only', () => {
    const w = weekRuns(SCHEDULE_PRESETS[0]!.windows)
    expect(w[0]).toEqual([[480, 1140]])
    expect(w[4]).toEqual([[480, 1140]])
    expect(w[5]).toEqual([])
    expect(w[6]).toEqual([])
  })

  it('spreads Monday to Friday across the week', () => {
    const w = weekRuns([{ days: [1], start: '07:00', stopDay: 5, stop: '21:00' }])
    expect(w[0]).toEqual([[420, 1440]])
    expect(w[2]).toEqual([[0, 1440]])
    expect(w[4]).toEqual([[0, 1260]])
    expect(w[5]).toEqual([])
  })

  it('carries a night window past midnight, Sunday into Monday', () => {
    const w = weekRuns([{ days: [7], start: '22:00', stop: '06:00' }])
    expect(w[6]).toEqual([[1320, 1440]])
    expect(w[0]).toEqual([[0, 360]])
  })
})

describe('windowWords', () => {
  it('reads like a person would say it', () => {
    expect(windowWords(SCHEDULE_PRESETS[0]!.windows[0]!)).toBe('Mon–Fri 08:00–19:00')
    expect(windowWords(SCHEDULE_PRESETS[1]!.windows[0]!)).toBe('Mon 07:00 to Fri 21:00')
    expect(windowWords(SCHEDULE_PRESETS[2]!.windows[0]!)).toBe('Every day 07:00–22:00')
    expect(windowWords({ days: [6, 7], start: '22:00', stop: '06:00' })).toBe('Sat, Sun 22:00–06:00 (next day)')
  })
})

describe('scheduleOf', () => {
  it('reads the tag without minding its case', () => {
    expect(scheduleOf({ argusschedule: ' office ' })).toBe('office')
    expect(scheduleOf({ Name: 'web' })).toBe('')
  })
})
