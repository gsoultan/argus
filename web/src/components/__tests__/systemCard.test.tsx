import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { SystemCardBody, bitRate, diskFull, diskTone, linkSpeed, uptime, utcOffset } from '~/components/server/SystemCard'
import { assets } from '~/lib/seed'
import { renderWithProviders } from '~/test/render'
import type { Asset, ServerSystem } from '~/types/domain'

/**
 * The System card: what the machine is and how busy it is, always with how
 * old the figures are. A filling disk is the only thing it colours, and a
 * quiet agent's figures are never shown as the present.
 */

const GB = 1024 ** 3
const NOW = Date.parse('2026-11-03T05:00:00Z')

const linux = (over: Partial<Asset> = {}): Asset => ({
  ...assets.find((a) => a.protocol !== 'rdp')!, agentState: 'healthy', ...over,
})

const report = (over: Partial<ServerSystem> = {}): ServerSystem => ({
  reported: true,
  os: 'Ubuntu 24.04.1 LTS',
  system: {
    kernel: '6.8.0-45-generic', arch: 'x86_64', cpuModel: 'Intel Xeon Gold 6248R', cpus: 8,
    memoryBytes: 32 * GB, machine: 'virtual', hypervisor: 'KVM', bootedAt: '2026-10-11T02:14:00Z',
    timeZone: 'Asia/Jakarta', utcOffsetSeconds: 7 * 3600,
  },
  usage: {
    load: [1.42, 1.3, 1.18], cpuBusy: 18, memTotal: 32 * GB, memAvailable: 12.5 * GB,
    swapTotal: 4 * GB, swapFree: 3.8 * GB,
    disks: [
      { mount: '/', fs: 'ext4', total: 100 * GB, used: 47 * GB, avail: 48 * GB },
      { mount: '/var/lib/postgresql', fs: 'xfs', total: 500 * GB, used: 455 * GB, avail: 45 * GB },
      { mount: '/backup', fs: 'ext4', total: 100 * GB, used: 97 * GB, avail: 2 * GB },
    ],
  },
  usageAt: '2026-11-03T04:59:30Z',
  ...over,
})

function show(asset: Asset, data: ServerSystem, onTab = vi.fn()) {
  renderWithProviders(<SystemCardBody asset={asset} data={data} onTab={onTab} now={NOW} />)
  return onTab
}

describe('SystemCard', () => {
  it('names what is running low first and colours those meters, by the control plane’s answer', () => {
    show(linux(), report({
      usage: { ...report().usage!, cpuBusy: 93, memAvailable: 1.25 * GB },
      warnings: [
        { resource: 'memory', level: 'critical', percent: 3, left: 1.25 * GB, total: 32 * GB },
        { resource: 'disk', level: 'critical', mount: '/backup', percent: 98, left: 2 * GB, total: 100 * GB },
        { resource: 'cpu', level: 'low', percent: 93, left: 0, total: 0 },
      ],
    }))
    const strip = screen.getByTestId('system-low')
    expect(strip.getAttribute('data-tone')).toBe('rose')
    expect(strip.textContent).toContain('Running low on 3 resources')
    expect(strip.textContent).toContain('Memory: 1.3 GB of 32 GB left (3%)')
    expect(strip.textContent).toContain('Disk /backup is 98% full, 2 GB left')
    expect(screen.getByTestId('meter-memory').getAttribute('data-tone')).toBe('rose')
    expect(screen.getByTestId('meter-cpu').getAttribute('data-tone')).toBe('amber')
    expect(screen.getByTestId('disk-/backup').getAttribute('data-tone')).toBe('rose')
    // With the control plane's warnings, a disk it did not warn about is not coloured.
    expect(screen.getByTestId('disk-/var/lib/postgresql').getAttribute('data-tone')).toBeNull()
  })

  it('says nothing is low by saying nothing', () => {
    show(linux(), report({ warnings: [] }))
    expect(screen.queryByTestId('system-low')).toBeNull()
    expect(screen.getByTestId('meter-memory').getAttribute('data-tone')).toBeNull()
  })

  it('shows a Windows machine’s figures without inventing a load average, and traffic per interface', () => {
    show(linux({ protocol: 'rdp' }), report({
      os: 'Windows 11 Pro',
      usage: {
        load: null, cpuBusy: 22, memTotal: 16 * GB, memAvailable: 6 * GB, swapTotal: 0, swapFree: 0,
        disks: [{ mount: 'C:', fs: 'ntfs', total: 476 * GB, used: 437 * GB, avail: 39 * GB }],
        net: [
          { name: 'Ethernet', rxBps: 300_000, txBps: 38_750, speedMbps: 1000 },
          { name: 'vEthernet', rxBps: null, txBps: null, speedMbps: 0 },
        ],
      },
      network: [{ name: 'Ethernet', mac: '00:1a:2b:3c:4d:5e', addresses: [{ address: '192.168.1.42', prefix: 24, family: 'ipv4', public: false }] }],
      warnings: [{ resource: 'disk', level: 'low', mount: 'C:', percent: 92, left: 39 * GB, total: 476 * GB }],
    }))
    expect(screen.getByText(/keeps no load average/)).toBeTruthy()
    expect(screen.queryByText(/Load 0\.00/)).toBeNull()
    expect(screen.getByTestId('system-low').textContent).toContain('Running low on disk')
    const eth = screen.getByTestId('traffic-Ethernet').textContent
    expect(eth).toContain('2.4 Mbit/s')
    expect(eth).toContain('310 kbit/s')
    expect(eth).toContain('of 1 Gbit/s link')
    // Traffic on an interface holding no address is still shown, and an
    // interface with no earlier reading has no rate rather than zero.
    expect(screen.getByTestId('traffic-vEthernet').textContent).toContain('—')
  })

  it('writes traffic in bits, the way links are sold', () => {
    expect(bitRate(0)).toBe('0 bit/s')
    expect(bitRate(100)).toBe('800 bit/s')
    expect(bitRate(125_000)).toBe('1 Mbit/s')
    expect(bitRate(14_000_000)).toBe('112 Mbit/s')
    expect(bitRate(1.25e9)).toBe('10 Gbit/s')
    expect(linkSpeed(866)).toBe('866 Mbit/s')
    expect(linkSpeed(2500)).toBe('2.5 Gbit/s')
  })

  it('shows what the machine is and how busy it is, colouring only filling disks', () => {
    show(linux(), report())
    expect(screen.getByText('6.8.0-45-generic')).toBeTruthy()
    expect(screen.getByText('Intel Xeon Gold 6248R')).toBeTruthy()
    expect(screen.getByText('Virtual machine (KVM)')).toBeTruthy()
    expect(screen.getByText('Asia/Jakarta (UTC+7)')).toBeTruthy()
    expect(screen.getByText(/Reported by the agent/)).toBeTruthy()
    expect(screen.queryByText(/has not reported since/)).toBeNull()

    expect(screen.getByTestId('disk-/').getAttribute('data-tone')).toBeNull()
    expect(screen.getByTestId('disk-/var/lib/postgresql').getAttribute('data-tone')).toBe('amber')
    expect(screen.getByTestId('disk-/backup').getAttribute('data-tone')).toBe('rose')
  })

  it('fades a quiet agent’s figures and says when they are from', () => {
    show(linux({ agentState: 'stale' }), report({ usageAt: '2026-11-03T02:00:00Z' }))
    expect(screen.getByText(/has not reported since/)).toBeTruthy()
    expect(document.querySelector('[data-quiet]')).toBeTruthy()
  })

  it('treats an old sample as quiet even while the heartbeat says healthy', () => {
    show(linux(), report({ usageAt: '2026-11-03T04:40:00Z' }))
    expect(screen.getByText(/has not reported since/)).toBeTruthy()
  })

  it('sends an older agent to the Agent tab to update, and says where details come from with none', () => {
    const onTab = show(linux(), { reported: false, os: 'Ubuntu 22.04', system: null, usage: null, usageAt: null })
    fireEvent.click(screen.getByRole('button', { name: 'Update agent' }))
    expect(onTab).toHaveBeenCalledWith('agent')
    expect(screen.getByText('Ubuntu 22.04')).toBeTruthy()
  })

  it('does not offer an update where there is no agent', () => {
    show(linux({ agentState: 'absent' }), { reported: false, os: '', system: null, usage: null, usageAt: null })
    expect(screen.getByText('No agent on this server')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Update agent' })).toBeNull()
  })

  it('lists each interface\'s addresses and says which are public', () => {
    show(linux(), report({
      network: [
        { name: 'eth0', mac: '52:54:00:ab:cd:ef', addresses: [
          { address: '203.0.113.10', prefix: 24, family: 'ipv4', public: true },
          { address: '2001:db8::10', prefix: 64, family: 'ipv6', public: true },
        ] },
        { name: 'docker0', mac: '', addresses: [{ address: '172.17.0.1', prefix: 16, family: 'ipv4', public: false }] },
      ],
    }))
    const eth0 = screen.getByTestId('iface-eth0')
    expect(eth0.textContent).toContain('52:54:00:ab:cd:ef')
    expect(eth0.textContent).toContain('203.0.113.10/24public')
    expect(eth0.textContent).toContain('2001:db8::10/64public')
    expect(screen.getByTestId('iface-docker0').textContent).toContain('172.17.0.1/16private')
    expect(screen.queryByText(/No interface on this server holds a public address/)).toBeNull()
  })

  it('does not let a server behind NAT read as having no public address', () => {
    show(linux(), report({
      network: [{ name: 'ens5', mac: '', addresses: [{ address: '10.0.1.5', prefix: 20, family: 'ipv4', public: false }] }],
    }))
    expect(screen.getByText(/No interface on this server holds a public address/)).toBeTruthy()
  })

  it('shows an older agent\'s flat list, and nothing when no addresses came', () => {
    show(linux(), report({
      reported: false, system: null, usage: null, usageAt: null,
      network: [{ name: '', mac: '', addresses: [{ address: '10.0.0.5', prefix: -1, family: 'ipv4', public: false }] }],
    }))
    expect(screen.getByTestId('iface-addresses').textContent).toBe('Addresses10.0.0.5private')
  })

  it('has no network section when nothing was reported', () => {
    show(linux(), report({ network: undefined }))
    expect(screen.queryByTestId('system-network')).toBeNull()
  })

  it('counts a disk the way df does, root reserve excluded', () => {
    expect(diskFull({ mount: '/', fs: 'ext4', total: 100, used: 90, avail: 5 })).toBe(95)
    expect(diskTone(89)).toBeUndefined()
    expect(diskTone(90)).toBe('amber')
    expect(diskTone(95)).toBe('rose')
  })

  it('formats offsets and uptime', () => {
    expect(utcOffset(0)).toBe('UTC')
    expect(utcOffset(19800)).toBe('UTC+5:30')
    expect(utcOffset(-18000)).toBe('UTC−5')
    expect(uptime('2026-10-11T02:14:00Z', NOW)).toBe('23 days')
    expect(uptime('2026-11-03T04:00:00Z', NOW)).toBe('1 hour')
  })
})
