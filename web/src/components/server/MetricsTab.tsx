import { useMemo, useState } from 'react'
import { Box, Card, Group, SegmentedControl, SimpleGrid, Skeleton, Stack, Text } from '@mantine/core'
import { useElementSize } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { absTime, now as clockNow } from '~/components/primitives'
import { LowStrip, bitRate, diskFull, systemQuery } from '~/components/server/SystemCard'
import { get, live } from '~/lib/live'
import { bytes, warningTone } from '~/lib/resourcesLow'
import { FS } from '~/theme'
import type { Asset, ResourceWarning, ServerSystem } from '~/types/domain'

/*
 * An asset's own metrics page: how it is now (the same System query the
 * Overview card reads) and how it has been, from the control plane's
 * 5-minute history (internal/control/usage_history.go). A stretch with no
 * report is drawn as a gap and shaded, never joined across: a line there
 * would claim figures nobody measured.
 */

export type MetricsRange = '6h' | '24h' | '7d' | '30d'

export interface MetricsPoint {
  at: string
  cpu: number | null
  cpuMax: number | null
  mem: number | null
  swap: number | null
  disks: Record<string, number> | null
  /** Bytes per second, summed over interfaces. */
  rx: number | null
  tx: number | null
  rxMax: number | null
  txMax: number | null
  samples: number
}

export interface AssetMetrics {
  range: MetricsRange
  stepSeconds: number
  from: string
  to: string
  reported: boolean
  mounts: string[]
  points: MetricsPoint[]
}

const RANGES: { value: MetricsRange; label: string; span: number; step: number }[] = [
  { value: '6h', label: '6 h', span: 6 * 3600, step: 300 },
  { value: '24h', label: '24 h', span: 24 * 3600, step: 300 },
  { value: '7d', label: '7 days', span: 7 * 86400, step: 1800 },
  { value: '30d', label: '30 days', span: 30 * 86400, step: 7200 },
]

// A plain options object rather than queryOptions(): importing that here
// splits TanStack's shared chunk and adds a file to every page's first load.
export const metricsQuery = (asset: Asset, range: MetricsRange) =>
  ({
    queryKey: ['asset', asset.id, 'metrics', range] as const,
    queryFn: () => live.orFallback(
      () => get<AssetMetrics>(`/api/v1/assets/${encodeURIComponent(asset.id)}/metrics?range=${range}`),
      async () => fixtureMetrics(asset, range),
    ),
    refetchInterval: 60_000,
    retry: false,
    placeholderData: (prev: AssetMetrics | undefined) => prev,
  })

/** Sample history: a working day's rhythm, a disk filling, and an outage. */
function fixtureMetrics(a: Asset, range: MetricsRange): AssetMetrics {
  const r = RANGES.find((x) => x.value === range)!
  const to = clockNow()
  const from = to - r.span * 1000
  const base: AssetMetrics = { range, stepSeconds: r.step, from: new Date(from).toISOString(), to: new Date(to).toISOString(), reported: false, mounts: [], points: [] }
  if (a.agentState === 'absent' || a.kind === 'android') return base
  const points: MetricsPoint[] = []
  const n = Math.floor(r.span / r.step)
  let seed = 7
  const rnd = () => ((seed = (seed * 16807) % 2147483647) / 2147483647)
  for (let i = 0; i < n; i++) {
    if (i > n * 0.42 && i < n * 0.47) continue // the agent was quiet
    const t = from + (i + 1) * r.step * 1000
    const day = Math.sin(((t / 3_600_000) % 24) / 24 * 2 * Math.PI - 1.4)
    const cpu = Math.max(2, Math.min(99, 32 + 20 * day + (rnd() - 0.5) * 16))
    const f = i / n
    points.push({
      at: new Date(Math.floor(t / (r.step * 1000)) * r.step * 1000).toISOString(),
      cpu: Math.round(cpu * 10) / 10, cpuMax: Math.min(100, Math.round(cpu + 8 + rnd() * 18)),
      mem: Math.round((58 + 6 * day + rnd() * 3) * 10) / 10, swap: 5 + Math.round(rnd() * 3),
      disks: { '/': Math.round((46 + f) * 10) / 10, '/var/lib/postgresql': Math.round((86 + 5 * f) * 10) / 10 },
      rx: Math.round((3e6 + 2e6 * day + rnd() * 1e6)), tx: Math.round((9e6 + 6e6 * day + rnd() * 3e6)),
      rxMax: Math.round(6e6 + rnd() * 4e6), txMax: Math.round(16e6 + rnd() * 1e7), samples: r.step / 60,
    })
  }
  return { ...base, reported: true, mounts: ['/var/lib/postgresql', '/'], points }
}

export function MetricsTab({ asset }: { asset: Asset }) {
  const [range, setRange] = useState<MetricsRange>('24h')
  const { data: sys } = useQuery(systemQuery(asset))
  const { data: m, isError, isLoading } = useQuery(metricsQuery(asset, range))
  const fresh = !!sys?.usageAt && asset.agentState === 'healthy' && clockNow() - Date.parse(sys.usageAt) <= 5 * 60_000

  return (
    <Stack gap="md">
      {sys?.usage && fresh && (sys.warnings?.length ?? 0) > 0 && (
        <Box mx={-16} mt={-16}><LowStrip warnings={sys.warnings!} faded={false} /></Box>
      )}
      {sys && <NowTiles sys={sys} fresh={fresh} />}
      <Card padding={0}>
        <Group justify="space-between" wrap="wrap" gap="sm" px="md" py="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
          <Text component="h2" fw={600} size="sm" style={{ margin: 0 }}>Resource history</Text>
          <SegmentedControl size="xs" value={range} onChange={(v) => setRange(v as MetricsRange)} aria-label="Range"
            data={RANGES.map((r) => ({ value: r.value, label: r.label }))} />
        </Group>
        {isError ? (
          <Text size={FS.meta} c="dimmed" p="md">The history could not be read. Try again in a moment.</Text>
        ) : isLoading || !m ? (
          <Skeleton h={260} m="md" />
        ) : !m.reported ? (
          <Text size={FS.meta} c="dimmed" p="md" ta="center">
            No agent reports for this asset, so there is no history to show. Install the Argus agent to start one.
          </Text>
        ) : m.points.length === 0 ? (
          <Text size={FS.meta} c="dimmed" p="md" ta="center">
            Nothing reported in this range yet. History starts with the first report after the control plane is updated, about one a minute.
          </Text>
        ) : (
          <MetricsHistory m={m} />
        )}
        <Text size={FS.micro} c="dimmed" px="md" py="sm" lh={1.5} style={{ borderTop: '1px solid var(--color-line)' }}>
          Each point averages what the agent reported in {stepLabel(m?.stepSeconds ?? 300)}; dashed lines are the busiest moment in it.
          A shaded stretch is when the agent did not report: nothing is drawn there. Kept for 30 days.
        </Text>
      </Card>
    </Stack>
  )
}

function stepLabel(s: number): string {
  return s >= 3600 ? `${s / 3600} hours` : `${s / 60} minutes`
}

function NowTiles({ sys, fresh }: { sys: ServerSystem; fresh: boolean }) {
  const u = sys.usage
  if (!u) return null
  const w = (r: ResourceWarning['resource'], mount?: string) =>
    fresh ? sys.warnings?.find((x) => x.resource === r && (mount === undefined || x.mount === mount)) : undefined
  const fullest = [...u.disks].sort((a, b) => diskFull(b) - diskFull(a))[0]
  const rx = (u.net ?? []).reduce((s, n) => s + (n.rxBps ?? 0), 0)
  const tx = (u.net ?? []).reduce((s, n) => s + (n.txBps ?? 0), 0)
  const memUsed = u.memTotal ? Math.round(((u.memTotal - u.memAvailable) / u.memTotal) * 100) : null
  return (
    <SimpleGrid cols={{ base: 2, md: 4 }} spacing="md" style={{ opacity: fresh ? 1 : 0.6 }}>
      <Tile label="Processor" value={u.cpuBusy !== null ? `${Math.round(u.cpuBusy)}% busy` : '—'} tone={warningTone(w('cpu'))}
        sub={[sys.system?.cpus ? `${sys.system.cpus} CPUs` : '', u.load ? `load ${u.load[0].toFixed(2)}` : ''].filter(Boolean).join(' · ')} />
      <Tile label="Memory" value={memUsed !== null ? `${memUsed}% used` : '—'} tone={warningTone(w('memory'))}
        sub={u.memTotal ? `${bytes(u.memAvailable)} of ${bytes(u.memTotal)} left` : ''} />
      <Tile label="Fullest disk" value={fullest ? `${diskFull(fullest)}% full` : '—'} tone={fullest ? warningTone(w('disk', fullest.mount)) : undefined}
        sub={fullest ? `${fullest.mount} · ${bytes(fullest.avail)} left` : 'No disks reported'} mono />
      <Tile label="Network" value={u.net?.length ? `↓ ${bitRate(rx)}` : '—'}
        sub={u.net?.length ? `↑ ${bitRate(tx)} · last minute` : 'Not reported by this agent'} />
      {!fresh && sys.usageAt && (
        <Text size={FS.micro} c="amber.7" style={{ gridColumn: '1 / -1' }}>
          These are from {absTime(sys.usageAt)}; the agent has not reported since.
        </Text>
      )}
    </SimpleGrid>
  )
}

function Tile({ label, value, sub, tone, mono }: { label: string; value: string; sub: string; tone?: 'amber' | 'rose'; mono?: boolean }) {
  return (
    <Card padding="md" data-testid={`now-${label}`} data-tone={tone}>
      <Text size={FS.meta} c="dimmed">{label}</Text>
      <Text fw={650} size="lg" mt={4} c={tone ? `${tone}.7` : undefined}>{value}</Text>
      <Text size={FS.micro} c="dimmed" mt={2} ff={mono ? 'monospace' : undefined} truncate>{sub}</Text>
    </Card>
  )
}

/* ── Charts ─────────────────────────────────────────────────────────────── */

const C = {
  azure: 'var(--mantine-color-azure-6)', azureSoft: 'var(--mantine-color-azure-3)',
  teal: 'var(--mantine-color-teal-6)', sky: 'var(--mantine-color-sky-6)',
  slate: 'var(--mantine-color-slate-5)', rose: 'var(--mantine-color-rose-6)', amber: 'var(--mantine-color-amber-6)',
}
const DISK_COLORS = [C.azure, C.teal, C.sky, C.slate, C.amber, C.rose]

interface Line { label: string; color: string; dashed?: boolean; values: (number | null)[] }

export function MetricsHistory({ m }: { m: AssetMetrics }) {
  const times = useMemo(() => m.points.map((p) => Date.parse(p.at)), [m])
  const from = Date.parse(m.from)
  const to = Date.parse(m.to)
  const step = m.stepSeconds * 1000
  const pick = (f: (p: MetricsPoint) => number | null) => m.points.map(f)
  const pct = (v: number) => `${Math.round(v)}%`
  const avg = (vs: (number | null)[]) => {
    const xs = vs.filter((v): v is number => v !== null)
    return xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : null
  }
  const peak = (vs: (number | null)[]) => Math.max(0, ...vs.filter((v): v is number => v !== null))
  const cpu = pick((p) => p.cpu)
  const cpuMax = pick((p) => p.cpuMax)
  const mem = pick((p) => p.mem)
  const swap = pick((p) => p.swap)
  const hasSwap = swap.some((v) => v !== null && v > 0)
  const rx = pick((p) => (p.rx === null ? null : p.rx * 8))
  const tx = pick((p) => (p.tx === null ? null : p.tx * 8))
  const netPeak = Math.max(peak(pick((p) => (p.rxMax === null ? null : p.rxMax * 8))), peak(pick((p) => (p.txMax === null ? null : p.txMax * 8))), peak(rx), peak(tx))
  const mounts = m.mounts.slice(0, DISK_COLORS.length)
  const lastMem = [...mem].reverse().find((v) => v !== null)
  const common = { times, from, to, step }
  const cpuAvg = avg(cpu)
  return (
    <SimpleGrid cols={{ base: 1, lg: 2 }} spacing={0} className="metrics-history">
      <Chart title="Processor" testId="chart-cpu" {...common} max={100} fmt={pct} threshold={90}
        summary={cpuAvg === null ? 'not reported' : `average ${Math.round(cpuAvg)}% · peak ${Math.round(peak(cpuMax))}%`}
        lines={[{ label: 'average', color: C.azure, values: cpu }, { label: 'peak', color: C.azureSoft, dashed: true, values: cpuMax }]} />
      <Chart title="Memory used" testId="chart-mem" {...common} max={100} fmt={pct} threshold={90}
        summary={lastMem == null ? 'not reported' : `latest ${Math.round(lastMem)}% · peak ${Math.round(peak(mem))}%`}
        lines={[{ label: 'memory', color: C.teal, values: mem }, ...(hasSwap ? [{ label: 'swap', color: C.slate, dashed: true, values: swap }] : [])]} />
      <Chart title="Disks" testId="chart-disks" {...common} max={100} fmt={pct} threshold={90}
        summary={mounts.length ? `fullest: ${mounts[0]}` : 'not reported'}
        lines={mounts.map((mt, i) => ({ label: mt, color: DISK_COLORS[i]!, values: pick((p) => p.disks?.[mt] ?? null) }))} />
      <Chart title="Network" testId="chart-net" {...common} max={niceMax(netPeak * 1.05)} fmt={bitRateShort}
        summary={netPeak > 0 ? `all interfaces · peak ${bitRate(netPeak / 8)}` : 'not reported'}
        lines={[{ label: 'in', color: C.teal, values: rx }, { label: 'out', color: C.sky, values: tx }]} />
    </SimpleGrid>
  )
}

function niceMax(v: number): number {
  if (v <= 0) return 1000
  const p = Math.pow(10, Math.floor(Math.log10(v)))
  for (const k of [1, 2, 2.5, 5, 10]) if (k * p >= v) return k * p
  return 10 * p
}

/** Bits per second on an axis: short, no decimals past what reads. */
function bitRateShort(bits: number): string {
  return bitRate(bits / 8).replace('bit/s', 'b/s').replace(/\.0 /, ' ')
}

function timeLabel(t: number, spanMs: number): string {
  const d = new Date(t)
  return spanMs > 2 * 86_400_000
    ? d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
    : d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}

function Chart({ title, summary, lines, times, from, to, step, max, fmt, threshold, testId }: {
  title: string
  summary: string
  lines: Line[]
  times: number[]
  from: number
  to: number
  step: number
  max: number
  fmt: (v: number) => string
  threshold?: number
  testId: string
}) {
  const { ref, width } = useElementSize()
  const [hover, setHover] = useState<number | null>(null)
  const W = Math.max(260, Math.round(width) || 520)
  const H = 150
  const pl = 60
  const pr = 8
  const pt = 6
  const pb = 18
  const iw = W - pl - pr
  const ih = H - pt - pb
  const span = Math.max(1, to - from)
  const x = (t: number) => pl + ((t - from) / span) * iw
  const y = (v: number) => pt + ih - (ih * Math.min(Math.max(v, 0), max)) / max

  // Stretches with no report: more than two steps between points, or
  // before the first and after the last.
  const gaps: [number, number][] = []
  const gapAfter = 2 * step
  if (times.length) {
    if (times[0]! - from > gapAfter) gaps.push([from, times[0]!])
    for (let i = 1; i < times.length; i++) if (times[i]! - times[i - 1]! > gapAfter) gaps.push([times[i - 1]! + step, times[i]!])
    if (to - times[times.length - 1]! > gapAfter + step) gaps.push([times[times.length - 1]! + step, to])
  }

  const paths = lines.map((l) => {
    const segs: string[][] = []
    let cur: string[] = []
    l.values.forEach((v, i) => {
      const t = times[i]!
      if (v === null || (i > 0 && t - times[i - 1]! > gapAfter)) {
        if (cur.length) segs.push(cur)
        cur = []
      }
      if (v !== null) cur.push(`${x(t).toFixed(1)},${y(v).toFixed(1)}`)
    })
    if (cur.length) segs.push(cur)
    return { l, segs }
  })

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const box = e.currentTarget.getBoundingClientRect()
    const t = from + ((e.clientX - box.left) / box.width * W - pl) / iw * span
    let best = -1
    let bd = Infinity
    times.forEach((tt, i) => {
      const d = Math.abs(tt - t)
      if (d < bd) { bd = d; best = i }
    })
    setHover(best >= 0 && bd <= step * 1.5 ? best : null)
  }

  const hoverText = hover === null ? null
    : `${new Date(times[hover]!).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })} · ` +
      lines.map((l) => `${l.label} ${l.values[hover] == null ? '—' : fmt(l.values[hover]!)}`).join(' · ')

  return (
    <Box p="md" ref={ref} className="metrics-chart" data-testid={testId} data-gaps={gaps.length}
      style={{ borderTop: '1px solid var(--color-line)' }}>
      <Group justify="space-between" gap="xs" wrap="nowrap" mb={4}>
        <Text size={FS.meta} fw={600}>{title}</Text>
        <Text size={FS.micro} c="dimmed" truncate>{hoverText ?? summary}</Text>
      </Group>
      <Group gap="sm" mb={4}>
        {lines.map((l) => (
          <Group key={l.label} gap={5} wrap="nowrap">
            <svg width="14" height="4" aria-hidden><line x1="0" y1="2" x2="14" y2="2" stroke={l.color} strokeWidth="3" strokeDasharray={l.dashed ? '3 2' : undefined} /></svg>
            <Text size={FS.micro} c="dimmed" ff={title === 'Disks' ? 'monospace' : undefined}>{l.label}</Text>
          </Group>
        ))}
      </Group>
      <svg viewBox={`0 0 ${W} ${H}`} width="100%" height={H} role="img" onMouseMove={onMove} onMouseLeave={() => setHover(null)}
        aria-label={`${title}: ${summary}`}>
        {gaps.map(([a, b], i) => (
          <g key={i}>
            <rect x={x(a)} y={pt} width={Math.max(1, x(b) - x(a))} height={ih} fill="var(--mantine-color-default-hover)" />
            {x(b) - x(a) > 60 && (
              <text x={(x(a) + x(b)) / 2} y={pt + ih / 2} fontSize="10" textAnchor="middle" fill="var(--mantine-color-dimmed)">no reports</text>
            )}
          </g>
        ))}
        {[0, 1, 2, 3, 4].map((g) => {
          const yy = pt + ih - (ih * g) / 4
          return (
            <g key={g}>
              <line x1={pl} x2={W - pr} y1={yy} y2={yy} stroke="var(--color-line)" />
              <text x={pl - 6} y={yy + 3} fontSize="10" textAnchor="end" fill="var(--mantine-color-dimmed)">{fmt((max * g) / 4)}</text>
            </g>
          )
        })}
        {threshold !== undefined && (
          <g>
            <line x1={pl} x2={W - pr} y1={y(threshold)} y2={y(threshold)} stroke={C.amber} strokeDasharray="4 3" />
            <text x={W - pr - 2} y={y(threshold) - 3} fontSize="10" textAnchor="end" fill="var(--mantine-color-amber-7)">low at {fmt(threshold)}</text>
          </g>
        )}
        {[0, 0.5, 1].map((f) => (
          <text key={f} x={pl + iw * f} y={H - 4} fontSize="10" textAnchor={f === 0 ? 'start' : f === 1 ? 'end' : 'middle'}
            fill="var(--mantine-color-dimmed)">{f === 1 ? 'now' : timeLabel(from + span * f, span)}</text>
        ))}
        {paths.map(({ l, segs }) => segs.map((s, i) => (
          s.length === 1
            ? <circle key={`${l.label}-${i}`} cx={s[0]!.split(',')[0]} cy={s[0]!.split(',')[1]} r={1.8} fill={l.color} />
            : <polyline key={`${l.label}-${i}`} points={s.join(' ')} fill="none" stroke={l.color} strokeWidth={1.6}
                strokeLinejoin="round" strokeDasharray={l.dashed ? '4 3' : undefined} />
        )))}
        {hover !== null && (
          <line x1={x(times[hover]!)} x2={x(times[hover]!)} y1={pt} y2={pt + ih} stroke="var(--mantine-color-dimmed)" strokeWidth={1} />
        )}
      </svg>
    </Box>
  )
}

