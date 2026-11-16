import { fireEvent, screen } from '@testing-library/react'
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

  // The tiles said "18% busy" and the chart "average 33%": both right, one
  // the last minute and one the whole range, but neither said which.
  it('names the range in every summary', () => {
    renderWithProviders(<MetricsHistory m={metrics(Array.from({ length: 72 }, (_, i) => point(i)))} />)
    for (const id of ['chart-cpu', 'chart-mem', 'chart-disks', 'chart-net']) {
      expect(screen.getByTestId(id).textContent).toContain('last 6 h:')
    }
  })

  it('reads out the exact values and time under the pointer', () => {
    renderWithProviders(<MetricsHistory m={metrics(Array.from({ length: 72 }, (_, i) => point(i)))} />)
    const cpu = screen.getByTestId('chart-cpu')
    const svg = cpu.querySelector('svg[role="img"]')!
    // jsdom has no layout: give the chart a box so the pointer maps to a time.
    svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 520, height: 150, right: 520, bottom: 150, x: 0, y: 0, toJSON: () => ({}) })
    // Half way across the plot is about point 35: average 55%, peak 75%.
    fireEvent.mouseMove(svg, { clientX: 60 + (520 - 68) / 2 })
    expect(cpu.textContent).toMatch(/average 5\d% · peak 7\d%/)
    expect(screen.getByTestId('chart-cpu-hover').querySelectorAll('circle')).toHaveLength(2)
    fireEvent.mouseLeave(svg)
    expect(screen.queryByTestId('chart-cpu-hover')).toBeNull()
    expect(cpu.textContent).toContain('last 6 h:')
  })

  // An older agent sends none of the newer figures, and Windows keeps no
  // load average or steal: no chart is drawn empty for them.
  it('draws load, steal, connections and disk activity only once reported', () => {
    const { unmount } = renderWithProviders(<MetricsHistory m={metrics(Array.from({ length: 72 }, (_, i) => point(i)))} />)
    for (const id of ['chart-load', 'chart-steal', 'chart-tcp', 'chart-iops', 'chart-disk-bytes']) {
      expect(screen.queryByTestId(id)).toBeNull()
    }
    unmount()
    const pts = Array.from({ length: 72 }, (_, i) => point(i, {
      loadPerCore: i === 10 ? 1.8 : 0.4, steal: null, stealMax: null, tcp: 120 + i,
      readOps: 300, writeOps: 100, readBps: 2e6, writeBps: 1e6, iopsMax: 900, ioBpsMax: 5e6,
    }))
    renderWithProviders(<MetricsHistory m={metrics(pts)} />)
    expect(screen.getByTestId('chart-load').textContent).toContain('busiest 1.80 per processor')
    expect(screen.getByTestId('chart-load').textContent).toContain('waiting above 1')
    expect(screen.queryByTestId('chart-steal')).toBeNull()
    expect(screen.getByTestId('chart-tcp').textContent).toContain('most open at once 191')
    expect(screen.getByTestId('chart-iops').textContent).toContain('busiest 900 a second')
    expect(screen.getByTestId('chart-disk-bytes').textContent).toMatch(/busiest 4\.8 MB\/s|busiest 5(\.0)? MB\/s/)
  })
  // A busy server is read against what its instance allows, what changed on
  // it, and what it cost while it did.
  it('draws the instance limits, the changes and the cost', () => {
    const pts = Array.from({ length: 72 }, (_, i) => point(i, { rx: 8e6, tx: 2e7, rxMax: 9e6, txMax: 4e7, readOps: 900, writeOps: 400, iopsMax: 2600, readBps: 1e6, writeBps: 1e6, ioBpsMax: 2e6 }))
    const at = (h: number) => new Date(FROM + h * 3600 * 1000).toISOString()
    renderWithProviders(<MetricsHistory m={{
      ...metrics(pts),
      events: [{ at: at(2), kind: 'cloud', summary: 'Resize web-01 to m6i.large', by: 'dana@example.com' },
        { at: at(4), kind: 'update', summary: 'Updated 3 packages', by: 'lee@example.com' }],
      cloud: { instanceId: 'i-1', instanceType: 'm6i.large', networkBps: 97_625_000, diskIops: 3000, diskBps: 125 * 1024 * 1024,
        cost: [{ from: at(0), perHour: 0.0832 }, { from: at(2), perHour: 0.096 }] },
    }} />)
    // Network peaks near the baseline: the line is drawn. Disk throughput is
    // far below what the volumes allow: the summary says how far instead.
    expect(screen.getByTestId('chart-net-limit').textContent).toContain('m6i.large baseline')
    expect(screen.getByTestId('chart-iops-limit').textContent).toContain('volumes allow 3.0k/s')
    expect(screen.queryByTestId('chart-disk-bytes-limit')).toBeNull()
    expect(screen.getByTestId('chart-disk-bytes').textContent).toMatch(/\d+% of what the volumes allow/)
    expect(Number(screen.getByTestId('chart-cpu').dataset.events)).toBe(2)
    const cost = screen.getByTestId('chart-cost')
    expect(cost.textContent).toContain('$0.096 an hour now')
    // Two hours at 0.0832 and four at 0.096.
    expect(cost.textContent).toContain('about $0.550 over the range')
    const list = screen.getByTestId('metrics-events')
    expect(list.textContent).toContain('Resize web-01 to m6i.large')
    expect(list.textContent).toContain('dana@example.com')
  })

  it('shows no cost or change list without them', () => {
    renderWithProviders(<MetricsHistory m={metrics(Array.from({ length: 72 }, (_, i) => point(i)))} />)
    expect(screen.queryByTestId('chart-cost')).toBeNull()
    expect(screen.queryByTestId('metrics-events')).toBeNull()
    expect(screen.queryByTestId('chart-net-limit')).toBeNull()
  })
})
