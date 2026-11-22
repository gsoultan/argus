import { describe, expect, it } from 'vitest'
import { formatBytes, linksOf, type NetworkInterface } from '~/lib/networks'

const iface = (assetId: string, hostname: string, peers: NetworkInterface['peers']): NetworkInterface => ({
  assetId, hostname, name: 'wg0', addresses: [], up: true, rxBytes: 0, txBytes: 0, peers, peersKnown: true,
})

describe('linksOf', () => {
  it('keeps a link between two Argus servers once, at its worse end', () => {
    const links = linksOf([
      iface('a', 'db-01', [{ publicKey: 'B', allowedIps: [], rxBytes: 0, txBytes: 0, state: 'idle', peerAssetId: 'b', peerHostname: 'app-01', peerInterface: 'wg0' }]),
      iface('b', 'app-01', [
        { publicKey: 'A', allowedIps: [], rxBytes: 0, txBytes: 0, state: 'down', peerAssetId: 'a', peerHostname: 'db-01', peerInterface: 'wg0' },
        { publicKey: 'LAPTOP', allowedIps: [], rxBytes: 0, txBytes: 0, state: 'idle' },
      ]),
    ])
    expect(links).toHaveLength(2)
    expect(links[0]?.state).toBe('down')
    expect(links[0]?.a.hostname).toBe('app-01')
    expect(links[1]?.b.publicKey).toBe('LAPTOP')
  })
})

describe('formatBytes', () => {
  it('reads like people write it', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1500)).toBe('1.5 kB')
    expect(formatBytes(412_000_000_000)).toBe('412 GB')
  })
})
