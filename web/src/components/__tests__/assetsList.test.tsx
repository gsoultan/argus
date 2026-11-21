import { describe, expect, it } from 'vitest'
import { filterAssets, sortAssets, updateChips, type AssetFilter } from '~/components/myservers'
import { groupOf } from '~/components/AssetTable'
import { answerOf, offlineReason, parseAnswer } from '~/lib/reach'
import { assets as seed } from '~/lib/seed'
import type { Asset } from '~/types/domain'
import type { UpdatesWaitingRow } from '~/lib/updatesWaiting'
import type { AssetUsageRow } from '~/lib/assetsUsage'

/*
 * The Assets list: search and every filter narrow the same rows for the
 * cards and the table; an asset is Online, Offline or Blocked and nothing
 * else; the table groups by what needs someone and opens a row in place.
 */

const base = seed.find((a) => a.protocol !== 'rdp')!
const make = (over: Partial<Asset>): Asset => ({
  ...base, agentState: 'healthy', hostKeyState: 'pinned', health: 'reachable', tunnelConnected: false, tags: [], principals: ['ops'], ...over,
})

const core4 = make({ id: 'a1', hostname: 'core-04.core.northwind.id', alias: '', address: '10.50.5.197', os: 'Amazon Linux 2023', tags: ['production'], principals: ['deploy', 'ops'] })
const pay7 = make({ id: 'a2', hostname: 'pay-07.payments.northwind.id', alias: '', address: '10.20.3.7', os: 'Ubuntu 22.04', agentState: 'stale', agentLastSeenAt: new Date(Date.now() - 12 * 60_000).toISOString() })
const win = make({ id: 'a3', hostname: 'win-app-01.corp.northwind.id', alias: 'Finance desktop', address: '10.30.2.11', os: 'Windows Server 2022', protocol: 'rdp' })
const never = make({ id: 'a4', hostname: 'old-01.corp.northwind.id', alias: '', address: '10.54.6.173', os: 'Ubuntu 24.04', agentState: 'absent' })
const blocked = make({ id: 'a5', hostname: 'core-02.core.northwind.id', alias: '', address: '10.28.3.86', hostKeyState: 'changed' })
const all = [core4, pay7, win, never, blocked]

const none: AssetFilter = { text: '', show: 'all', kind: 'all', type: 'all', needs: false, lowOnly: false, needsUpdate: () => false, low: () => false }
const ids = (xs: Asset[]) => xs.map((a) => a.id)

describe('the three states', () => {
  it('calls an asset no agent ever reported from Offline, and says why', () => {
    expect(answerOf(never)).toBe('down')
    expect(offlineReason(never, () => '')).toBe('Agent never reported')
    expect(answerOf(pay7)).toBe('down')
    expect(offlineReason(pay7, () => '12 min ago')).toBe('Last seen 12 min ago')
    expect(answerOf(core4)).toBe('online')
    expect(offlineReason(core4, () => '')).toBeUndefined()
    expect(answerOf(blocked)).toBe('blocked')
  })

  it('reads an old ?status=noagent link as Offline', () => {
    expect(parseAnswer('noagent')).toBe('down')
    expect(parseAnswer('down')).toBe('down')
    expect(parseAnswer('nonsense')).toBeUndefined()
  })
})

describe('search and filters', () => {
  it('finds by name, alias, any address, OS, tag and account', () => {
    expect(ids(filterAssets(all, { ...none, text: 'core-04' }))).toEqual(['a1'])
    expect(ids(filterAssets(all, { ...none, text: 'finance' }))).toEqual(['a3'])
    expect(ids(filterAssets(all, { ...none, text: '10.20.3' }))).toEqual(['a2'])
    expect(ids(filterAssets(all, { ...none, text: 'windows' }))).toEqual(['a3'])
    expect(ids(filterAssets(all, { ...none, text: 'deploy' }))).toEqual(['a1'])
    expect(ids(filterAssets(all, { ...none, text: '  CORE-04  ' }))).toEqual(['a1'])
  })

  it('needs every word typed, in any order', () => {
    expect(ids(filterAssets(all, { ...none, text: 'production core' }))).toEqual(['a1'])
    expect(ids(filterAssets(all, { ...none, text: 'production pay' }))).toEqual([])
  })

  it('narrows by state, kind, updates and running low, together', () => {
    expect(ids(filterAssets(all, { ...none, show: 'down' }))).toEqual(['a2', 'a4'])
    expect(ids(filterAssets(all, { ...none, show: 'online' }))).toEqual(['a1', 'a3'])
    expect(ids(filterAssets(all, { ...none, show: 'blocked' }))).toEqual(['a5'])
    expect(ids(filterAssets(all, { ...none, kind: 'windows' }))).toEqual(['a3'])
    expect(ids(filterAssets(all, { ...none, needs: true, needsUpdate: (a) => a.id === 'a2' }))).toEqual(['a2'])
    expect(ids(filterAssets(all, { ...none, lowOnly: true, low: (a) => a.id === 'a1' }))).toEqual(['a1'])
    expect(ids(filterAssets(all, { ...none, show: 'down', text: 'old-01' }))).toEqual(['a4'])
  })

  it('puts blocked first, then offline, then by name', () => {
    expect(ids(sortAssets(all))).toEqual(['a5', 'a4', 'a2', 'a1', 'a3'])
  })
})

describe('updates', () => {
  const w = (over: Partial<UpdatesWaitingRow>): UpdatesWaitingRow => ({ assetId: 'x', hostname: 'x', updates: 0, security: 0, rebootRequired: false, ...over })
  it('says the agent from and to, a new release, security, other patches and a restart', () => {
    const chips = updateChips(w({ agent: 'behind', agentVersion: '1.38.2', releaseNext: 'Ubuntu 24.04 LTS', updates: 11, security: 2, rebootRequired: true }), '1.42.0')
    expect(chips.map((c) => c.label)).toEqual(['Agent 1.38.2 → 1.42.0', 'Ubuntu 24.04 LTS available', '2 security', '9 patches', 'Restart needed'])
  })
  it('says nothing for an asset with nothing waiting, so the line reads Up to date', () => {
    expect(updateChips(undefined, '1.42.0')).toEqual([])
  })
})

describe('the table', () => {
  const usage = (id: string, warn: boolean): AssetUsageRow => ({
    assetId: id, usageAt: new Date().toISOString(), cpu: 30, memory: 60, disk: { mount: '/var/lib/postgresql', percent: warn ? 91 : 40 },
    warnings: warn ? [{ resource: 'disk', level: 'low', mount: '/var/lib/postgresql', percent: 91, left: 1, total: 2 }] : [],
  })
  const waiting = new Map<string, UpdatesWaitingRow>([['a3', { assetId: 'a3', hostname: 'win', updates: 1, security: 1, rebootRequired: false }]])
  const usageBy = new Map<string, AssetUsageRow>([['a1', usage('a1', true)], ['a3', usage('a3', false)]])

  it('puts each asset in the most urgent group it fits', () => {
    expect(groupOf(blocked, undefined, undefined)).toBe('blocked')
    expect(groupOf(pay7, waiting.get('a2'), undefined)).toBe('offline')
    expect(groupOf(win, waiting.get('a3'), usageBy.get('a3'))).toBe('updates')
    expect(groupOf(core4, undefined, usageBy.get('a1'))).toBe('low')
    expect(groupOf(make({ id: 'z' }), undefined, undefined)).toBe('fine')
  })

})
