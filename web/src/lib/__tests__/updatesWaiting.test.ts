import { describe, expect, it } from 'vitest'
import { needsUpdates, type UpdatesWaitingRow } from '~/lib/updatesWaiting'

const row = (over: Partial<UpdatesWaitingRow>): UpdatesWaitingRow =>
  ({ assetId: 'a', hostname: 'a', updates: 0, security: 0, rebootRequired: false, ...over })

describe('needsUpdates', () => {
  it('counts security, updates, a restart and an agent behind', () => {
    expect(needsUpdates(undefined)).toBe(false)
    expect(needsUpdates(row({}))).toBe(false)
    expect(needsUpdates(row({ security: 1 }))).toBe(true)
    expect(needsUpdates(row({ updates: 3 }))).toBe(true)
    expect(needsUpdates(row({ rebootRequired: true }))).toBe(true)
    expect(needsUpdates(row({ agent: 'behind' }))).toBe(true)
    // An agent that cannot update itself from here is not a button to press.
    expect(needsUpdates(row({ agent: 'cannot' }))).toBe(false)
  })
})
