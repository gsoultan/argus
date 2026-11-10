import { describe, expect, it } from 'vitest'
import { addressesOf, isIpLiteral } from '~/lib/assetAddresses'
import type { AssetAddress } from '~/types/domain'

const v4 = (address: string, pub: boolean, iface = 'eth0'): AssetAddress =>
  ({ address, family: 'ipv4', public: pub, interface: iface })

describe('addressesOf', () => {
  it('puts a public IPv4 first, then private, then IPv6, and a bridge last', () => {
    const got = addressesOf({
      hostname: 'pay-01.example', address: 'pay-01.example',
      addresses: [
        v4('172.17.0.1', false, 'docker0'),
        { address: 'fd00::5', family: 'ipv6', public: false, interface: 'eth0' },
        v4('10.0.0.5', false),
        v4('203.0.113.10', true),
      ],
    })
    expect(got.map((a) => a.address)).toEqual(['203.0.113.10', '10.0.0.5', 'fd00::5', '172.17.0.1'])
  })

  it('shows an entered IP only when the agent reported nothing, as for an operator', () => {
    expect(addressesOf({ hostname: 'a', address: '192.0.2.10' }).map((a) => a.address)).toEqual(['192.0.2.10'])
    expect(addressesOf({ hostname: 'a', address: '192.0.2.10', addresses: [v4('10.0.0.5', false)] })
      .map((a) => a.address)).toEqual(['10.0.0.5'])
    // A DNS name is not an IP address, so the list shows nothing.
    expect(addressesOf({ hostname: 'a', address: 'a.example' })).toEqual([])
  })

  it('on the detail page, leads with what was entered when the agent did not report it', () => {
    const got = addressesOf({ hostname: 'a', address: 'a.example', addresses: [v4('10.0.0.5', false)] }, true)
    expect(got.map((a) => [a.address, !!a.entered])).toEqual([['a.example', true], ['10.0.0.5', false]])
    // The same address once, as the agent reported it.
    expect(addressesOf({ hostname: 'a', address: '10.0.0.5', addresses: [v4('10.0.0.5', false)] }, true))
      .toEqual([v4('10.0.0.5', false)])
    // An address that is just the hostname repeats the title.
    expect(addressesOf({ hostname: 'a.example', address: 'a.example' }, true)).toEqual([])
  })
})

describe('isIpLiteral', () => {
  it('reads IPv4 and IPv6, bracketed or not, and nothing else', () => {
    expect(['10.0.0.5', '::1', '[fd00::5]', '2001:db8::1'].every(isIpLiteral)).toBe(true)
    expect(['pay-01', '10.0.0', '300.1.1.1', 'a.example'].some(isIpLiteral)).toBe(false)
  })
})
