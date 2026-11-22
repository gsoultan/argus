import { describe, expect, it } from 'vitest'
import { formatBps, formatSpeed, formatUptimeSec, interfaceLabel, securityWords } from '~/lib/networkDevices'
import { readyToRead } from '~/components/NetworkDevices'

describe('network devices', () => {
  it('says traffic, speed and uptime in words', () => {
    expect(formatBps(undefined)).toBe('—')
    expect(formatBps(950)).toBe('950 b/s')
    expect(formatBps(412_000_000)).toBe('412 Mb/s')
    expect(formatBps(1_250_000_000)).toBe('1.3 Gb/s')
    expect(formatSpeed(10000)).toBe('10 Gb/s')
    expect(formatSpeed(100)).toBe('100 Mb/s')
    expect(formatSpeed(0)).toBe('—')
    expect(formatUptimeSec(30)).toBe('under a minute')
    expect(formatUptimeSec(86_400 * 12 + 3600 * 4)).toBe('12 d 4 h')
    expect(interfaceLabel({ index: 3, name: 'Gi0/3', alias: 'uplink' })).toBe('Gi0/3 (uplink)')
    expect(interfaceLabel({ index: 3, name: '' })).toBe('Interface 3')
  })

  it('names how a device is read, warning when it is in the clear', () => {
    expect(securityWords({ version: 'v2c', authProto: '', privProto: '' })).toMatch(/in the clear/)
    expect(securityWords({ version: 'v3', authProto: 'SHA', privProto: '' })).toMatch(/not encrypted/)
    expect(securityWords({ version: 'v3', authProto: 'SHA256', privProto: 'AES' })).toBe('SNMPv3, SHA256 and AES encryption')
  })

  it('can be read only with an address, a server and credentials, which a saved device keeps', () => {
    const v = {
      name: '', host: '10.0.0.2', port: 161, version: 'v3' as const, community: '', user: 'ro', authProto: 'SHA', authPass: '',
      privProto: 'AES', privPass: '', watch: [], everySeconds: 60, timeoutSeconds: 10, checkFrom: ['a1'], paused: false,
    }
    expect(readyToRead(v, false)).toBe(false)
    expect(readyToRead({ ...v, authPass: '12345678', privPass: '12345678' }, false)).toBe(true)
    expect(readyToRead(v, true)).toBe(true)
    expect(readyToRead({ ...v, checkFrom: [] }, true)).toBe(false)
    expect(readyToRead({ ...v, version: 'v2c' }, false)).toBe(false)
    expect(readyToRead({ ...v, version: 'v2c', community: 'public' }, false)).toBe(true)
  })
})
