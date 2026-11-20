import { describe, expect, it } from 'vitest'
import { ITEMS_EVERY, ITEMS_SETTLING_EVERY, itemsRefetchEvery, latestRead, LOOK_AGAIN_AFTER, settling, storedRead, type CloudItem, type CloudResource } from '../cloud'

const item = (r: Partial<CloudResource>, accountId = 'acct-1'): CloudItem => ({
  accountId, accountName: 'prod', environment: 'production', firstSeen: '2026-11-15T06:17:00Z', seenAt: '2026-11-15T06:18:00Z',
  madeByArgus: false, resource: { kind: 'snapshot', id: 'snap-1', name: 'db', region: 'ap-southeast-3', ...r },
} as CloudItem)

describe('latestRead', () => {
  it('shows the panel the latest read, not the row that was clicked', () => {
    const clicked = item({ state: 'pending', details: { progress: '0%' } })
    const now = item({ state: 'completed', details: { progress: '100%' } })
    expect(latestRead(clicked, [undefined, [item({ id: 'snap-2' }), now]])).toBe(now)
  })

  it('matches on account and kind too, and keeps the clicked copy once it is gone', () => {
    const clicked = item({ state: 'pending' })
    const other = item({ state: 'completed' }, 'acct-2')
    const volume = item({ kind: 'volume', state: 'in-use' })
    expect(latestRead(clicked, [[other, volume]])).toBe(clicked)
    expect(latestRead(null, [[other]])).toBeNull()
  })
})

describe('itemsRefetchEvery', () => {
  it('asks again quickly while anything is between two states', () => {
    expect(itemsRefetchEvery([item({ state: 'completed' }), item({ state: 'pending' })])).toBe(ITEMS_SETTLING_EVERY)
    expect(itemsRefetchEvery([item({ kind: 'instance', state: 'stopping' })])).toBe(ITEMS_SETTLING_EVERY)
    expect(itemsRefetchEvery([item({ state: 'completed' })])).toBe(ITEMS_EVERY)
    expect(itemsRefetchEvery(undefined)).toBe(ITEMS_EVERY)
  })

  it('asks again quickly for what is changing under a state that does not say so', () => {
    // A disk being detached is still in-use; a target group waiting on a new target is only degraded.
    expect(itemsRefetchEvery([item({ kind: 'volume', state: 'in-use', details: { settling: 'yes' } })])).toBe(ITEMS_SETTLING_EVERY)
    expect(itemsRefetchEvery([item({ kind: 'target-group', state: 'degraded', details: { settling: 'yes' } })])).toBe(ITEMS_SETTLING_EVERY)
    expect(itemsRefetchEvery([item({ kind: 'volume', state: 'in-use', details: {} })])).toBe(ITEMS_EVERY)
  })

  it('treats only states on their way somewhere as settling', () => {
    for (const s of ['pending', 'stopping', 'shutting-down', 'creating', 'deleting', 'provisioning', 'modifying', 'optimizing', 'uploading']) expect(settling(s)).toBe(true)
    for (const s of ['running', 'stopped', 'completed', 'available', 'in-use', 'active', undefined]) expect(settling(s)).toBe(false)
  })
})

describe('looking again after a change', () => {
  it('looks more than once, past the two minutes the control plane keeps reading the region', () => {
    expect(LOOK_AGAIN_AFTER.length).toBeGreaterThan(1)
    expect(Math.max(...LOOK_AGAIN_AFTER)).toBeGreaterThanOrEqual(120_000)
  })

  it('repeats only what is stored, never what is read from the provider or audited at each read', () => {
    for (const k of ['accounts', 'items', 'changes', 'reservations', 'scale-groups']) expect(storedRead(['cloud', k, 'x'])).toBe(true)
    for (const k of ['console-output', 'quote', 'remove-plan', 'instance-settings', 'launch-options', 'connect-check']) expect(storedRead(['cloud', k])).toBe(false)
    expect(storedRead(['asset', 'items'])).toBe(false)
  })
})
