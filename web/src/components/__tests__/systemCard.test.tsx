import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { SystemCardBody, diskFull, diskTone, uptime, utcOffset } from '~/components/server/SystemCard'
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

  it('sends an older agent to Settings to update, and says where details come from with none', () => {
    const onTab = show(linux(), { reported: false, os: 'Ubuntu 22.04', system: null, usage: null, usageAt: null })
    fireEvent.click(screen.getByRole('button', { name: 'Update agent' }))
    expect(onTab).toHaveBeenCalledWith('settings')
    expect(screen.getByText('Ubuntu 22.04')).toBeTruthy()
  })

  it('does not offer an update where there is no agent', () => {
    show(linux({ agentState: 'absent' }), { reported: false, os: '', system: null, usage: null, usageAt: null })
    expect(screen.getByText('No agent on this server')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Update agent' })).toBeNull()
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
