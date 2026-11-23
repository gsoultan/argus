import { describe, expect, it } from 'vitest'
import {
  ackCmd, claimable, claimCmd, groupNameProblem, idleWords, newGroupCmd, placeId, stuckWork, validEntryId,
} from '~/lib/redisStreams'

describe('redis stream groups', () => {
  it('turns places into IDs', () => {
    expect(placeId('start', '')).toBe('0')
    expect(placeId('now', '')).toBe('$')
    expect(placeId('id', ' 1700000000000-3 ')).toBe('1700000000000-3')
    expect(placeId('id', 'yesterday')).toBeNull()
    expect(validEntryId('17')).toBe(true)
  })
  it('makes the command lines the edit takes', () => {
    expect(newGroupCmd('orders', 'audit', '$')).toEqual(['XGROUP', 'CREATE', 'orders', 'audit', '$'])
    expect(ackCmd('orders', 'g', ['1-0', '2-0'])).toEqual(['XACK', 'orders', 'g', '1-0', '2-0'])
    expect(claimCmd('orders', 'g', 'w2', 60_000.7, ['1-0'])).toEqual(['XCLAIM', 'orders', 'g', 'w2', '60000', '1-0'])
  })
  it('claims only a consumer’s entries idle long enough', () => {
    const p = [{ id: '1-0', consumer: 'a', idleMs: 600_000, deliveries: 1 }, { id: '2-0', consumer: 'a', idleMs: 1000, deliveries: 1 },
      { id: '3-0', consumer: 'b', idleMs: 600_000, deliveries: 1 }]
    expect(claimable(p, 'a', 300_000)).toEqual(['1-0'])
  })
  it('names stuck work and checks names', () => {
    expect(stuckWork([{ name: 'a', pending: 3, idleMs: 400_000 }, { name: 'b', pending: 9, idleMs: 1000 }])?.name).toBe('a')
    expect(stuckWork([{ name: 'b', pending: 9, idleMs: 1000 }])).toBeNull()
    expect(groupNameProblem('my group', [])).toMatch(/no spaces/)
    expect(groupNameProblem('g', [{ name: 'g', consumers: 0, pending: 0, lastDelivered: '0-0' }])).toMatch(/already/)
    expect(idleWords(6 * 60_000)).toBe('6 min')
    expect(idleWords(3000)).toBe('3 s')
  })
})
