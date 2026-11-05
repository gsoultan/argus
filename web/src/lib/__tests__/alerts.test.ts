import { describe, expect, it } from 'vitest'
import { agoText } from '~/components/AlertPanel'
import { urgent, type ConsoleAlert } from '~/lib/alerts'

const alert = (p: Partial<ConsoleAlert>): ConsoleAlert => ({
  seq: 1, at: new Date().toISOString(), kind: 'server', severity: 'info', title: 't', detail: '', link: '', read: false, ...p,
})

describe('the bell', () => {
  it('says how long ago in words', () => {
    const now = Date.parse('2026-11-03T12:00:00Z')
    expect(agoText('2026-11-03T11:59:30Z', now)).toBe('just now')
    expect(agoText('2026-11-03T11:48:00Z', now)).toBe('12 min ago')
    expect(agoText('2026-11-03T09:00:00Z', now)).toBe('3 h ago')
    expect(agoText('2026-11-02T10:00:00Z', now)).toBe('yesterday')
    expect(agoText('2026-10-30T12:00:00Z', now)).toBe('4 days ago')
  })

  it('chimes and keeps a pop-up only for what someone has to act on', () => {
    expect(urgent(alert({ kind: 'approval', severity: 'warning' }))).toBe(true)
    expect(urgent(alert({ kind: 'security', severity: 'critical' }))).toBe(true)
    // A decision on your own request is news, not a task.
    expect(urgent(alert({ kind: 'approval', severity: 'info' }))).toBe(false)
    expect(urgent(alert({ kind: 'signin', severity: 'warning' }))).toBe(false)
    // A request decided or taken back asks nobody to act, so it does not chime.
    expect(urgent(alert({ kind: 'approval', severity: 'warning', closed: 'Cancelled by ana@example.com' }))).toBe(false)
  })
})
