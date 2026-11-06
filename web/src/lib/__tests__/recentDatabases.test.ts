import { beforeEach, describe, expect, it } from 'vitest'
import { loadRecentDatabases, noteDatabaseOpened } from '~/lib/recentDatabases'

describe('recently opened databases', () => {
  beforeEach(() => localStorage.clear())

  it('keeps the newest first, once each, and at most six', () => {
    for (let i = 1; i <= 8; i++) noteDatabaseOpened(`d${i}`, i)
    noteDatabaseOpened('d5', 100)
    const ids = loadRecentDatabases().map((r) => r.id)
    expect(ids).toEqual(['d5', 'd8', 'd7', 'd6', 'd4', 'd3'])
  })

  it('reads nothing from a value it did not write', () => {
    localStorage.setItem('argus-db-recent', '{"not":"a list"}')
    expect(loadRecentDatabases()).toEqual([])
    localStorage.setItem('argus-db-recent', '[{"id":3},{"id":"ok","at":1}]')
    expect(loadRecentDatabases()).toEqual([{ id: 'ok', at: 1 }])
  })
})
