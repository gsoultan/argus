import { describe, expect, it } from 'vitest'
import { TABS_BY_TYPE, tabLabel } from '~/components/server/tabsByType'
import { assetTypeOf } from '~/lib/assetType'

/**
 * Every type of asset keeps the same tab order, leaves out what does not
 * apply, and names a tab in its own words. Nothing a server page had is
 * dropped: its tabs are today's plus Agent, which was a card in Settings.
 */
describe('tabs by type', () => {
  it('gives a server every tab, with the agent out of Settings', () => {
    expect(TABS_BY_TYPE.server.map((t) => tabLabel(t, 'server'))).toEqual(
      ['Overview', 'Metrics', 'Access', 'Sessions', 'Apps', 'Packages', 'Disks', 'Logs', 'Agent', 'Settings'])
  })

  it('gives a workstation updates but no log cleanup or disk growing', () => {
    expect(TABS_BY_TYPE.workstation.map((t) => tabLabel(t, 'workstation'))).toEqual(
      ['Overview', 'Metrics', 'Access', 'Sessions', 'Apps', 'Updates', 'Agent', 'Settings'])
  })

  it('gives a device its app, and nothing Android does for itself', () => {
    expect(TABS_BY_TYPE.device.map((t) => tabLabel(t, 'android'))).toEqual(
      ['Overview', 'Access', 'Sessions', 'Apps', 'App on device', 'Settings'])
  })

  it('reads a row from an older control plane, with no kind, as a server', () => {
    expect(assetTypeOf({})).toBe('server')
    expect(assetTypeOf({ kind: 'android' })).toBe('device')
    expect(assetTypeOf({ kind: 'workstation' })).toBe('workstation')
  })
})
