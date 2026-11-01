import { describe, expect, it } from 'vitest'
import { busyWith } from '~/lib/update'

describe('when a new build can go in', () => {
  it.each(['/', '/assets', '/assets/abc', '/approvals', '/teams', '/sessions', '/settings'])(
    'goes in at once on %s', (path) => {
      expect(busyWith(path, false)).toBe(false)
    },
  )

  it.each(['/connect', '/sessions/s-1'])('waits on %s, where something live is open', (path) => {
    expect(busyWith(path, false)).toBe(true)
  })

  it('waits while a dialog is open', () => {
    expect(busyWith('/assets', true)).toBe(true)
  })
})
