import { describe, expect, it } from 'vitest'
import { freeDevice, roleLacks, volumeMonthly, volumeOf, type CloudAccount, type CloudResource } from '../cloud'

const res = (over: Partial<CloudResource>): CloudResource => ({ kind: 'volume', id: 'vol-1', name: 'vol-1', region: 'eu-west-1', ...over })

describe('volumeOf', () => {
  it('reads size and type from the inventory', () => {
    expect(volumeOf(res({ size: '100 GiB gp3' }))).toEqual({ gb: 100, type: 'gp3' })
    expect(volumeOf(res({ size: '30 GiB' }))).toEqual({ gb: 30, type: '' })
    expect(volumeOf(res({}))).toEqual({ gb: 0, type: '' })
  })
})

describe('freeDevice', () => {
  it('skips the slots an instance uses, in either spelling', () => {
    const inst = res({ kind: 'instance', links: [
      { kind: 'volume', id: 'a', name: '/dev/xvda' }, { kind: 'volume', id: 'b', name: '/dev/xvdf' }, { kind: 'volume', id: 'c', name: '/dev/sdg' },
    ] })
    expect(freeDevice(inst)).toBe('/dev/sdh')
  })
})

describe('roleLacks', () => {
  it('names an old manage role only for what it lacks', () => {
    const a = { manageMissing: ['ec2:CreateVolume', 'ec2:AttachVolume'] } as CloudAccount
    expect(roleLacks(a, 'volume-create')).toBe(true)
    expect(roleLacks(a, 'snapshot-create')).toBe(false)
    expect(roleLacks({ manageMissing: [] as string[] } as CloudAccount, 'volume-create')).toBe(false)
    expect(roleLacks(undefined, 'volume-create')).toBe(false)
  })
})

describe('volumeMonthly', () => {
  it('prices gp3 at list price', () => {
    expect(volumeMonthly('gp3', 100)).toBeCloseTo(8)
    expect(volumeMonthly('sc1', 1000)).toBeCloseTo(15)
  })
})
