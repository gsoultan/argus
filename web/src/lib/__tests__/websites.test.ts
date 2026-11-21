import { describe, expect, it } from 'vitest'
import { formatDuration, formatUptime, requestSummary, sendsBody, SITE_METHODS, sliceRounds, stateLabel, type WebsiteRound } from '~/lib/websites'

describe('websites', () => {
  it('never rounds a gap up to 100%', () => {
    expect(formatUptime(99.999)).toBe('99.99%')
    expect(formatUptime(100)).toBe('100%')
    expect(formatUptime(97.46)).toBe('97.4%')
    expect(formatUptime(null)).toBe('—')
  })

  it('says durations in words', () => {
    expect(formatDuration(4)).toBe('4 min')
    expect(formatDuration(125)).toBe('2 h 5 min')
    expect(formatDuration(60 * 27)).toBe('1 d 3 h')
  })

  it('shows a failure not yet confirmed as checking again, never down', () => {
    expect(stateLabel({ state: 'up', paused: false, failingRounds: 1 }).label).toBe('Checking again')
    expect(stateLabel({ state: 'unknown', paused: false, failingRounds: 0 }).label).toBe('Cannot check')
    expect(stateLabel({ state: 'down', paused: true, failingRounds: 3 }).label).toBe('Paused')
  })

  it('keeps the worst outcome of a slice, so a short outage still shows', () => {
    const now = Date.parse('2026-11-20T12:00:00Z')
    const at = (minsAgo: number) => new Date(now - minsAgo * 60_000).toISOString()
    const rounds: WebsiteRound[] = [
      { at: at(30), secs: 60, outcome: 'up', ms: 100, detail: '' },
      { at: at(29), secs: 60, outcome: 'down', ms: 30_000, detail: '' },
      { at: at(28), secs: 60, outcome: 'up', ms: 300, detail: '' },
    ]
    const slices = sliceRounds(rounds, 96, now)
    const hit = slices.filter(Boolean)
    expect(hit).toHaveLength(1)
    expect(hit[0]!.outcome).toBe('down')
    expect(hit[0]!.ms).toBe(200)
    expect(hit[0]!.count).toBe(3)
  })
})

describe('requestSummary', () => {
  it('says nothing for a plain GET', () => {
    expect(requestSummary({ method: 'GET', body: '', headerNames: [] })).toBe('')
  })
  it('names what is sent', () => {
    expect(requestSummary({ method: 'POST', body: '{}', headerNames: ['X-Api-Key'] })).toBe('POST, a body and the X-Api-Key header')
    expect(requestSummary({ method: 'GET', body: '', headerNames: ['A', 'B'] })).toBe('GET and 2 headers')
    expect(requestSummary({ method: 'HEAD', body: '', headerNames: [] })).toBe('HEAD')
  })
})

describe('sendsBody', () => {
  it('is only POST, PUT and PATCH', () => {
    expect(SITE_METHODS.filter(sendsBody)).toEqual(['POST', 'PUT', 'PATCH'])
  })
})
