import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { MetricsHistory, type AssetMetrics, type MetricsPoint } from '~/components/server/MetricsTab'
import { ProgramsNow, rate } from '~/components/server/Programs'
import { renderWithProviders } from '~/test/render'
import type { ServerProcess } from '~/types/domain'

/**
 * Programs: a row is every process with the name, a disk the system would
 * not let the agent read says so rather than showing nothing, an agent that
 * measures no disk gets no disk columns, and a click on a chart picks the
 * point Busiest programs then shows.
 */

describe('ProgramsNow', () => {
  it('shows a program\'s processes together and says when disk could not be read', () => {
    const top: ServerProcess[] = [
      { name: 'postgres', pid: 811, count: 23, cpu: 41.5, memory: 6 * 1024 ** 3, readBps: 3 * 1024 ** 2, writeBps: 512 * 1024 },
      { name: 'mysqld', pid: 902, count: 1, cpu: 2, memory: 800 * 1024 ** 2, readBps: null, writeBps: null, ioHidden: true },
      { name: 'sshd', pid: 77, count: 1, cpu: 0.2, memory: 12 * 1024 ** 2, readBps: 0, writeBps: 0 },
    ]
    renderWithProviders(<ProgramsNow top={top} faded={false} />)
    const t = screen.getByTestId('top-processes')
    expect(t.textContent).toContain('postgres × 23')
    expect(t.textContent).not.toContain('pid 811')
    expect(t.textContent).toContain('pid 77')
    expect(t.textContent).toContain('3.0 MB/s')
    expect(t.textContent).toContain('512 KB/s')
    expect(screen.getAllByText('not readable')).toHaveLength(2)
    expect(t.textContent).toContain('Disk write')
  })

  it('leaves the disk columns out for an agent that measures none', () => {
    renderWithProviders(<ProgramsNow top={[{ name: 'java', pid: 5, cpu: 10, memory: 1e9 }]} faded={false} />)
    const t = screen.getByTestId('top-processes')
    expect(t.textContent).not.toContain('Disk')
    expect(t.textContent).toContain('pid 5')
  })

  it('writes rates down to bytes', () => {
    expect(rate(512)).toBe('512 B/s')
    expect(rate(20 * 1024)).toBe('20 KB/s')
    expect(rate(250 * 1024 ** 2)).toBe('250 MB/s')
  })
})

describe('picking a point', () => {
  const FROM = Date.parse('2026-11-03T00:00:00Z')
  const point = (i: number): MetricsPoint => ({
    at: new Date(FROM + (i + 1) * 300_000).toISOString(),
    cpu: 20, cpuMax: 40, mem: 50, swap: 0, disks: { '/': 60 }, rx: 1, tx: 2, rxMax: 1, txMax: 2, samples: 5,
  })
  const m: AssetMetrics = {
    range: '6h', stepSeconds: 300, from: new Date(FROM).toISOString(), to: new Date(FROM + 6 * 3600_000).toISOString(),
    reported: true, mounts: ['/'], points: Array.from({ length: 72 }, (_, i) => point(i)),
  }

  it('hands the clicked point to Busiest programs and marks it on every chart', () => {
    const onPick = vi.fn()
    const { unmount } = renderWithProviders(<MetricsHistory m={m} onPick={onPick} />)
    const svg = screen.getByTestId('chart-cpu').querySelector('svg[role="img"]')!
    svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 520, height: 150, right: 520, bottom: 150, x: 0, y: 0, toJSON: () => ({}) })
    fireEvent.click(svg, { clientX: 60 + (520 - 68) / 2 })
    expect(onPick).toHaveBeenCalledTimes(1)
    const t = onPick.mock.calls[0]![0] as number
    expect(m.points.map((p) => Date.parse(p.at))).toContain(t)
    unmount()
    renderWithProviders(<MetricsHistory m={m} onPick={onPick} picked={t} />)
    expect(screen.getByTestId('chart-cpu-picked')).toBeTruthy()
    expect(screen.getByTestId('chart-mem-picked')).toBeTruthy()
  })
})
