import { useMemo, useState } from 'react'
import { Box, Card, Group, SegmentedControl, SimpleGrid, Skeleton, Stack, Table, Text } from '@mantine/core'
import { useElementSize } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { absTime, now as clockNow } from '~/components/primitives'
import { span as spanWords, useTicker } from '~/components/server/RunPanel'
import { LowStrip, bitRate, diskFull, systemQuery } from '~/components/server/SystemCard'
import { get, live } from '~/lib/live'
import { bytes, warningTone } from '~/lib/resourcesLow'
import { FS } from '~/theme'
import type { Asset, ResourceWarning, ServerProcess, ServerSystem } from '~/types/domain'

/*
 * An asset's own metrics page: how it is now (the same System query the
 * Overview card reads) and how it has been, from the control plane's
 * 5-minute history (internal/control/usage_history.go). A stretch with no
 * report is drawn as a gap and shaded, never joined across: a line there
 * would claim figures nobody measured.
 */

export type MetricsRange = '6h' | '24h' | '7d' | '30d' | '90d' | '1y'

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
  /**
   * The busiest 1-minute load average over the processors: above 1, work
   * waited for a processor. These are missing from an older control plane
   * and null where the agent does not report them.
   */
  loadPerCore?: number | null
  /** Percent of time the hypervisor gave this machine's turn to another. */
  steal?: number | null
  stealMax?: number | null
  /** The most TCP connections open at once. */
  tcp?: number | null
  /** Disk activity over all physical disks: operations and bytes per second. */
  readOps?: number | null
  writeOps?: number | null
  readBps?: number | null
  writeBps?: number | null
  iopsMax?: number | null
  ioBpsMax?: number | null
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
  /** What changed on the server in the range, oldest first. Missing from an older control plane. */
  events?: MetricsEvent[]
  /** The cloud instance it runs as, when one in a connected account matches. */
  cloud?: MetricsCloud | null
}

/** A change Argus made or saw: a cloud change, a package update, or the server starting. */
export interface MetricsEvent {
  at: string
  kind: 'cloud' | 'update' | 'restart'
  summary: string
  by: string
}

export interface MetricsCloud {
  instanceId: string
  instanceType: string
  /** The size's baseline network bandwidth, bytes per second. */
  networkBps: number | null
  /** The attached volumes' provisioned totals; null unless every volume states one. */
  diskIops: number | null
  diskBps: number | null
  /** On-demand list price per hour in US dollars, in steps, oldest first. */
  cost: { from: string; perHour: number | null }[]
}

const RANGES: { value: MetricsRange; label: string; span: number; step: number }[] = [
  { value: '6h', label: '6 h', span: 6 * 3600, step: 300 },
  { value: '24h', label: '24 h', span: 24 * 3600, step: 300 },
  { value: '7d', label: '7 days', span: 7 * 86400, step: 1800 },
  { value: '30d', label: '30 days', span: 30 * 86400, step: 7200 },
  { value: '90d', label: '90 days', span: 90 * 86400, step: 6 * 3600 },
  { value: '1y', label: '1 year', span: 365 * 86400, step: 86400 },
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
      rxMax: Math.round(6e6 + rnd() * 4e6), txMax: Math.round(16e6 + rnd() * 1e7),
      loadPerCore: Math.round((cpu / 60 + rnd() * 0.3) * 100) / 100,
      steal: Math.round(rnd() * 15) / 10, stealMax: Math.round(rnd() * 40) / 10,
      tcp: Math.round(180 + 120 * day + rnd() * 40),
      readOps: Math.round(400 + 300 * day + rnd() * 100), writeOps: Math.round(250 + 150 * day + rnd() * 80),
      readBps: Math.round(18e6 + 9e6 * day), writeBps: Math.round(7e6 + 3e6 * day),
      iopsMax: Math.round(1400 + rnd() * 900), ioBpsMax: Math.round(40e6 + rnd() * 2e7),
      samples: r.step / 60,
    })
  }
  const ago = (f: number) => new Date(from + (to - from) * f).toISOString()
  return {
    ...base, reported: true, mounts: ['/var/lib/postgresql', '/'], points,
    events: [
      { at: ago(0.3), kind: 'update', summary: 'Updated 14 packages and restarted', by: 'automatic' },
      { at: ago(0.31), kind: 'restart', summary: 'Server started', by: '' },
      { at: ago(0.62), kind: 'cloud', summary: 'Resize db-01 from m6i.large to m6i.xlarge', by: 'dana@example.com' },
    ],
    cloud: {
      instanceId: 'i-0a41c7e2f9b3d5608', instanceType: 'm6i.xlarge', networkBps: 195_250_000, diskIops: 3000, diskBps: 125 * 1024 * 1024,
      cost: [{ from: base.from, perHour: 0.096 }, { from: ago(0.62), perHour: 0.192 }],
    },
  }
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
          A shaded stretch is when the agent did not report: nothing is drawn there. Kept 30 days in detail, and 13 months by the hour
          for the 90-day and 1-year ranges.
          {(m?.events?.length ?? 0) > 0 && ' Markers are changes Argus made or saw here; a change made in the cloud console or by another tool is not shown.'}
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
    <Box>
      <Group justify="space-between" gap="xs" mb={6}>
        <Text component="h2" fw={600} size="sm" style={{ margin: 0 }}>Now <Text span inherit c="dimmed" fw={400}>(the last minute)</Text></Text>
        {sys.usageAt && <ReportedAgo at={sys.usageAt} fresh={fresh} />}
      </Group>
    <SimpleGrid cols={{ base: 2, md: 4 }} spacing="md" style={{ opacity: fresh ? 1 : 0.6 }}>
      <Tile label="Processor" value={u.cpuBusy !== null ? `${Math.round(u.cpuBusy)}% busy` : '—'} tone={warningTone(w('cpu'))}
        sub={[sys.system?.cpus ? `${sys.system.cpus} CPUs` : '', u.load ? `load ${u.load[0].toFixed(2)}` : '',
          u.steal ? `steal ${u.steal.toFixed(1)}%` : ''].filter(Boolean).join(' · ')} />
      <Tile label="Memory" value={memUsed !== null ? `${memUsed}% used` : '—'} tone={warningTone(w('memory'))}
        sub={u.memTotal ? `${bytes(u.memAvailable)} of ${bytes(u.memTotal)} left` : ''} />
      <Tile label="Fullest disk" value={fullest ? `${diskFull(fullest)}% full` : '—'} tone={fullest ? warningTone(w('disk', fullest.mount)) : undefined}
        sub={fullest ? `${fullest.mount} · ${bytes(fullest.avail)} left` : 'No disks reported'} mono />
      <Tile label="Network" value={u.net?.length ? `↓ ${bitRate(rx)}` : '—'}
        sub={u.net?.length ? `↑ ${bitRate(tx)}` : 'Not reported by this agent'} />
      {!fresh && sys.usageAt && (
        <Text size={FS.micro} c="amber.7" style={{ gridColumn: '1 / -1' }}>
          These are from {absTime(sys.usageAt)}; the agent has not reported since.
        </Text>
      )}
    </SimpleGrid>
    {u.top && u.top.length > 0 && <TopProcesses top={u.top} faded={!fresh} />}
    </Box>
  )
}

/**
 * What is using the server now, from the same report as the tiles: the
 * busiest programs by processor, then by memory. Names only; the agent never
 * sends a command line, which can hold a password.
 */
function TopProcesses({ top, faded }: { top: ServerProcess[]; faded: boolean }) {
  return (
    <Card padding={0} mt="md" style={{ opacity: faded ? 0.6 : 1 }} data-testid="top-processes">
      <Text component="h3" fw={600} size="sm" px="md" pt="sm" pb={4} style={{ margin: 0 }}>
        What is using it <Text span inherit c="dimmed" fw={400}>(busiest by processor, then by memory)</Text>
      </Text>
      <Table.ScrollContainer minWidth={0} type="native">
        <Table verticalSpacing={4} fz={FS.meta}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Program</Table.Th>
              <Table.Th ta="right">Processor</Table.Th>
              <Table.Th ta="right">Memory</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {top.map((p) => (
              <Table.Tr key={p.pid}>
                <Table.Td>
                  <Text span inherit ff="monospace">{p.name}</Text>{' '}
                  <Text span size={FS.micro} c="dimmed">pid {p.pid}</Text>
                </Table.Td>
                <Table.Td ta="right">{p.cpu === null ? '—' : pct1(p.cpu)}</Table.Td>
                <Table.Td ta="right">{bytes(p.memory)}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </Card>
  )
}

/**
 * How old the Now figures are, counting up. The agent sends them about once
 * a minute, so up to a minute is as fresh as they get.
 */
function ReportedAgo({ at, fresh }: { at: string; fresh: boolean }) {
  const t = useTicker(5_000)
  return (
    <Text size={FS.micro} c={fresh ? 'dimmed' : 'amber.7'} title={absTime(at)} data-testid="now-reported">
      Reported {spanWords(Math.max(0, t - Date.parse(at)))} ago · the agent reports about once a minute
    </Text>
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
  const events = useMemo(() => (m.events ?? []).map((e) => ({ ...e, t: Date.parse(e.at) })), [m])
  const common = { times, from, to, step, events }
  const cl = m.cloud ?? null
  const cpuAvg = avg(cpu)
  // Every summary names its range, so it is not read against the Now tiles.
  const over = `last ${RANGES.find((r) => r.value === m.range)?.label ?? m.range}`
  // The newer figures are drawn only once the agent has sent one, so an
  // older agent, or Windows for load and steal, shows no empty charts.
  const opt = (f: (p: MetricsPoint) => number | null | undefined) => pick((p) => f(p) ?? null)
  const seen = (vs: (number | null)[]) => vs.some((v) => v !== null)
  const load = opt((p) => p.loadPerCore)
  const steal = opt((p) => p.steal)
  const stealMax = opt((p) => p.stealMax)
  const tcp = opt((p) => p.tcp)
  const rops = opt((p) => p.readOps)
  const wops = opt((p) => p.writeOps)
  const rbps = opt((p) => p.readBps)
  const wbps = opt((p) => p.writeBps)
  const iopsPeak = Math.max(peak(opt((p) => p.iopsMax)), peak(rops), peak(wops))
  const ioBpsPeak = Math.max(peak(opt((p) => p.ioBpsMax)), peak(rbps), peak(wbps))
  // A limit is drawn when the use comes near enough to it to read against;
  // far below, the summary says how far, so the line does not flatten the chart.
  const near = (peakV: number, lim: number | null | undefined) => !!lim && peakV >= lim * 0.25
  const share = (peakV: number, lim: number | null | undefined, whose: string) =>
    lim ? ` · ${peakV / lim < 0.01 ? 'under 1' : Math.round((peakV / lim) * 100)}% of ${whose}` : ''
  const netLim = cl?.networkBps ? cl.networkBps * 8 : null
  const count = (v: number) => (v >= 10_000 ? `${Math.round(v / 1000)}k` : v >= 1000 ? `${(v / 1000).toFixed(1)}k` : `${Math.round(v)}`)
  return (
    <SimpleGrid cols={{ base: 1, lg: 2 }} spacing={0} className="metrics-history">
      <Chart title="Processor" testId="chart-cpu" {...common} max={100} fmt={pct} threshold={90}
        summary={cpuAvg === null ? 'not reported' : `${over}: average ${Math.round(cpuAvg)}% · peak ${Math.round(peak(cpuMax))}%`}
        lines={[{ label: 'average', color: C.azure, values: cpu }, { label: 'peak', color: C.azureSoft, dashed: true, values: cpuMax }]} />
      <Chart title="Memory used" testId="chart-mem" {...common} max={100} fmt={pct} threshold={90}
        summary={lastMem == null ? 'not reported' : `${over}: latest ${Math.round(lastMem)}% · peak ${Math.round(peak(mem))}%`}
        lines={[{ label: 'memory', color: C.teal, values: mem }, ...(hasSwap ? [{ label: 'swap', color: C.slate, dashed: true, values: swap }] : [])]} />
      <Chart title="Disks" testId="chart-disks" {...common} max={100} fmt={pct} threshold={90}
        summary={mounts.length ? `${over}: fullest ${mounts[0]}` : 'not reported'}
        lines={mounts.map((mt, i) => ({ label: mt, color: DISK_COLORS[i]!, values: pick((p) => p.disks?.[mt] ?? null) }))} />
      <Chart title="Network" testId="chart-net" {...common} max={niceMax(Math.max(netPeak, near(netPeak, netLim) ? netLim! : 0) * 1.05)} fmt={bitRateShort}
        limit={near(netPeak, netLim) ? { value: netLim!, label: `${cl!.instanceType} baseline ${bitRateShort(netLim!)}` } : undefined}
        summary={netPeak > 0 ? `${over}: all interfaces · peak ${bitRate(netPeak / 8)}${share(netPeak, netLim, `the ${cl?.instanceType} baseline`)}` : 'not reported'}
        lines={[{ label: 'in', color: C.teal, values: rx }, { label: 'out', color: C.sky, values: tx }]} />
      {seen(load) && (
        <Chart title="Load per processor" testId="chart-load" {...common} max={Math.max(2, niceMax(peak(load) * 1.05))}
          fmt={(v) => v.toFixed(v < 10 ? 1 : 0)} threshold={1} thresholdLabel="waiting above 1"
          summary={`${over}: busiest ${peak(load).toFixed(2)} per processor`}
          lines={[{ label: 'busiest minute', color: C.azure, values: load }]} />
      )}
      {seen(steal) && (
        <Chart title="Processor steal" testId="chart-steal" {...common} max={Math.max(10, niceMax(peak(stealMax) * 1.05))} fmt={pct1}
          summary={`${over}: average ${pct1(avg(steal) ?? 0)} · peak ${pct1(peak(stealMax))}`}
          lines={[{ label: 'average', color: C.amber, values: steal }, { label: 'peak', color: C.slate, dashed: true, values: stealMax }]} />
      )}
      {seen(tcp) && (
        <Chart title="TCP connections" testId="chart-tcp" {...common} max={niceMax(peak(tcp) * 1.05)} fmt={count}
          summary={`${over}: most open at once ${Math.round(peak(tcp))}`}
          lines={[{ label: 'open', color: C.teal, values: tcp }]} />
      )}
      {seen(rops) && (
        <Chart title="Disk operations" testId="chart-iops" {...common} max={niceMax(Math.max(iopsPeak, near(iopsPeak, cl?.diskIops) ? cl!.diskIops! : 0) * 1.05)}
          fmt={(v) => `${count(v)}/s`}
          limit={near(iopsPeak, cl?.diskIops) ? { value: cl!.diskIops!, label: `volumes allow ${count(cl!.diskIops!)}/s` } : undefined}
          summary={`${over}: all disks · busiest ${count(iopsPeak)} a second${share(iopsPeak, cl?.diskIops, 'what the volumes allow')}`}
          lines={[{ label: 'reads', color: C.azure, values: rops }, { label: 'writes', color: C.rose, values: wops }]} />
      )}
      {seen(rbps) && (
        <Chart title="Disk throughput" testId="chart-disk-bytes" {...common} max={niceMax(Math.max(ioBpsPeak, near(ioBpsPeak, cl?.diskBps) ? cl!.diskBps! : 0) * 1.05)}
          fmt={(v) => `${bytes(v)}/s`}
          limit={near(ioBpsPeak, cl?.diskBps) ? { value: cl!.diskBps!, label: `volumes allow ${bytes(cl!.diskBps!)}/s` } : undefined}
          summary={`${over}: all disks · busiest ${bytes(ioBpsPeak)}/s${share(ioBpsPeak, cl?.diskBps, 'what the volumes allow')}`}
          lines={[{ label: 'read', color: C.azure, values: rbps }, { label: 'written', color: C.rose, values: wbps }]} />
      )}
      {cl && cl.cost.length > 0 && <CostChart cloud={cl} from={from} to={to} step={step} events={events} />}
      {events.length > 0 && <EventList events={events} />}
    </SimpleGrid>
  )
}

type TimedEvent = MetricsEvent & { t: number }

const EVENT_COLOR: Record<MetricsEvent['kind'], string> = { cloud: C.sky, update: C.teal, restart: C.slate }
const EVENT_WORD: Record<MetricsEvent['kind'], string> = { cloud: 'Cloud', update: 'Updates', restart: 'Started' }

/**
 * What the instance cost by the hour over the range, on the same time axis:
 * its list price now, walked back through the starts, stops and resizes
 * Argus applied. Nothing while stopped.
 */
function CostChart({ cloud, from, to, step, events }: {
  cloud: MetricsCloud; from: number; to: number; step: number; events: TimedEvent[]
}) {
  const steps = cloud.cost.map((c) => ({ t: Math.max(from, Date.parse(c.from)), v: c.perHour }))
  // One point a step across the range, so the line is drawn whether or not
  // the agent reported: the bill runs either way.
  const n = Math.min(400, Math.max(2, Math.round((to - from) / step)))
  const times: number[] = []
  const values: (number | null)[] = []
  for (let i = 0; i <= n; i++) {
    const t = from + ((to - from) * i) / n
    let v: number | null = null
    for (const s of steps) if (s.t <= t) v = s.v
    times.push(t)
    values.push(v)
  }
  let total = 0
  let known = true
  steps.forEach((s, i) => {
    const end = i + 1 < steps.length ? steps[i + 1]!.t : to
    if (s.v === null) known = false
    else total += s.v * ((end - s.t) / 3_600_000)
  })
  const nowPrice = steps[steps.length - 1]?.v ?? null
  const peakV = Math.max(0, ...values.filter((v): v is number => v !== null))
  const usd = (v: number) => `$${v < 1 ? v.toFixed(3) : v.toFixed(2)}`
  return (
    <Chart title="Instance cost" testId="chart-cost" times={times} from={from} to={to} step={((to - from) / n) * 1.01}
      events={events} max={niceMax(Math.max(0.01, peakV) * 1.1)} fmt={usd}
      summary={`${cloud.instanceType} · ${nowPrice === null ? 'price not known' : `${usd(nowPrice)} an hour now`}` +
        (known ? ` · about ${usd(total)} over the range` : '')}
      lines={[{ label: 'on-demand list price per hour (us-east-1 prices)', color: C.amber, values }]} />
  )
}

/** The markers as a list, so they can be read without hovering. */
function EventList({ events }: { events: TimedEvent[] }) {
  return (
    <Box p="md" style={{ borderTop: '1px solid var(--color-line)', gridColumn: '1 / -1' }} data-testid="metrics-events">
      <Text size={FS.meta} fw={600} mb={4}>Changes in this range</Text>
      <Stack gap={2}>
        {[...events].reverse().map((e) => (
          <Group key={`${e.at}-${e.kind}-${e.summary}`} gap="xs" wrap="nowrap" align="baseline">
            <svg width="8" height="8" aria-hidden style={{ flex: 'none' }}><rect width="8" height="8" rx="2" fill={EVENT_COLOR[e.kind]} /></svg>
            <Text size={FS.micro} c="dimmed" w={110} style={{ flex: 'none' }} title={absTime(e.at)}>
              {new Date(e.t).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}
            </Text>
            <Text size={FS.micro} style={{ minWidth: 0 }}>
              <Text span inherit c="dimmed">{EVENT_WORD[e.kind]} · </Text>{e.summary}
              {e.by && <Text span inherit c="dimmed"> · {e.by}</Text>}
            </Text>
          </Group>
        ))}
      </Stack>
    </Box>
  )
}

function pct1(v: number): string {
  return `${v < 10 ? v.toFixed(1) : Math.round(v)}%`
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

function Chart({ title, summary, lines, times, from, to, step, max, fmt, threshold, thresholdLabel, limit, events = [], testId }: {
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
  /** What the dashed line means; "low at" the value when not given. */
  thresholdLabel?: string
  /** What the instance or its volumes allow, drawn as a solid line. */
  limit?: { value: number; label: string }
  /** Changes in the range, drawn as markers. */
  events?: TimedEvent[]
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

  const onMove = (e: { currentTarget: SVGSVGElement; clientX: number }) => {
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

  const shownEvents = events.filter((e) => e.t >= from && e.t <= to)
  const hoverEvent = hover === null ? undefined
    : shownEvents.filter((e) => Math.abs(e.t - times[hover]!) <= step).sort((a, b) => Math.abs(a.t - times[hover]!) - Math.abs(b.t - times[hover]!))[0]
  const hoverText = hover === null ? null
    : `${new Date(times[hover]!).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })} · ` +
      (hoverEvent ? `${hoverEvent.summary} · ` : '') +
      lines.map((l) => `${l.label} ${l.values[hover] == null ? '—' : fmt(l.values[hover]!)}`).join(' · ')

  return (
    <Box p="md" ref={ref} className="metrics-chart" data-testid={testId} data-gaps={gaps.length} data-events={shownEvents.length}
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
        onTouchStart={(e) => e.touches[0] && onMove({ currentTarget: e.currentTarget, clientX: e.touches[0].clientX })}
        onTouchMove={(e) => e.touches[0] && onMove({ currentTarget: e.currentTarget, clientX: e.touches[0].clientX })}
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
            <text x={W - pr - 2} y={y(threshold) - 3} fontSize="10" textAnchor="end" fill="var(--mantine-color-amber-7)">{thresholdLabel ?? `low at ${fmt(threshold)}`}</text>
          </g>
        )}
        {limit && (
          <g data-testid={`${testId}-limit`}>
            <line x1={pl} x2={W - pr} y1={y(limit.value)} y2={y(limit.value)} stroke={C.rose} strokeWidth={1.2} />
            <text x={pl + 4} y={y(limit.value) - 3} fontSize="10" fill="var(--mantine-color-rose-7)">{limit.label}</text>
          </g>
        )}
        {shownEvents.map((e) => (
          <g key={`${e.at}-${e.kind}-${e.summary}`}>
            <line x1={x(e.t)} x2={x(e.t)} y1={pt} y2={pt + ih} stroke={EVENT_COLOR[e.kind]} strokeWidth={1} strokeDasharray="2 2" />
            <path d={`M${x(e.t) - 4},${pt} L${x(e.t) + 4},${pt} L${x(e.t)},${pt + 6} Z`} fill={EVENT_COLOR[e.kind]}>
              <title>{e.summary}</title>
            </path>
          </g>
        ))}
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
          <g data-testid={`${testId}-hover`}>
            <line x1={x(times[hover]!)} x2={x(times[hover]!)} y1={pt} y2={pt + ih} stroke="var(--mantine-color-dimmed)" strokeWidth={1} />
            {lines.map((l) => l.values[hover] == null ? null : (
              <circle key={l.label} cx={x(times[hover]!)} cy={y(l.values[hover]!)} r={3} fill={l.color}
                stroke="var(--mantine-color-body)" strokeWidth={1.5} />
            ))}
          </g>
        )}
      </svg>
    </Box>
  )
}

