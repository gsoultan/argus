import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MetricsHistory, type AssetMetrics, type MetricsPoint } from '~/components/server/MetricsTab'
import { renderWithProviders } from '~/test/render'

/**
 * The history charts: a stretch without reports is a shaded gap, never a
 * line drawn across it, and the low threshold is marked on the percentages.
 */

const FROM = Date.parse('2026-11-03T00:00:00Z')
const STEP = 300

const point = (i: number, over: Partial<MetricsPoint> = {}): MetricsPoint => ({
  at: new Date(FROM + (i + 1) * STEP * 1000).toISOString(),
  cpu: 20 + i, cpuMax: 40 + i, mem: 50, swap: 0, disks: { '/': 60 },
  rx: 1000, tx: 2000, rxMax: 1500, txMax: 3000, samples: 5, ...over,
})

const metrics = (points: MetricsPoint[]): AssetMetrics => ({
  range: '6h', stepSeconds: STEP, from: new Date(FROM).toISOString(),
  to: new Date(FROM + 6 * 3600 * 1000).toISOString(), reported: true, mounts: ['/'], points,
})

describe('MetricsHistory', () => {
  it('breaks the line where the agent did not report', () => {
    const pts = [...Array.from({ length: 30 }, (_, i) => point(i)), ...Array.from({ length: 30 }, (_, i) => point(i + 40))]
    renderWithProviders(<MetricsHistory m={metrics(pts)} />)
    const cpu = screen.getByTestId('chart-cpu')
    // Two lines (average and peak), each in two pieces either side of the hole.
    expect(cpu.querySelectorAll('polyline')).toHaveLength(4)
    // The hole in the middle; the last point is recent enough to need none.
    expect(Number(cpu.dataset.gaps)).toBe(1)
    expect(cpu.textContent).toContain('no reports')
    expect(cpu.textContent).toContain('low at 90%')
  })

  it('leaves swap out when there is none and says what was not reported', () => {
    const pts = Array.from({ length: 72 }, (_, i) => point(i, { rx: null, tx: null, rxMax: null, txMax: null }))
    renderWithProviders(<MetricsHistory m={metrics(pts)} />)
    expect(screen.getByTestId('chart-mem').textContent).not.toContain('swap')
    expect(screen.getByTestId('chart-net').textContent).toContain('not reported')
    expect(Number(screen.getByTestId('chart-cpu').dataset.gaps)).toBe(0)
    expect(screen.getByTestId('chart-disks').textContent).toContain('/')
  })
})
