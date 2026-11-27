import { useState } from 'react'
import { Box, Button, Card, Group, SegmentedControl, Skeleton, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { get, live } from '~/lib/live'
import { bytes } from '~/lib/resourcesLow'
import { FS } from '~/theme'
import type { Asset, ServerProcess } from '~/types/domain'

/*
 * Which programs use a server. Both tables count a program as every process
 * sharing its name, added up by the agent: twenty postgres workers are one
 * row. Names only; the agent never sends a command line, which can hold a
 * password.
 *
 * - ProgramsNow is the agent's latest report, beside the Now tiles.
 * - ProgramsHistory is what the control plane kept with each 5-minute point
 *   of the history (internal/control/usage_programs.go): each program at its
 *   busiest over the range, or over one point picked on a chart.
 */

/** One program over a stretch of history: its busiest moment by each measure. */
export interface ProgramUse {
  name: string
  cpu: number | null
  memory: number
  readBps: number | null
  writeBps: number | null
  ioHidden: boolean
  /** The most processes it ran as at once. */
  count: number
  /** How many of the stretch's 5-minute periods named it. */
  periods: number
}

export interface AssetPrograms {
  from: string
  to: string
  /** 5-minute periods in the stretch with programs reported. */
  periods: number
  programs: ProgramUse[]
}

/** The longest stretch programs are kept for. */
export const PROGRAMS_KEPT_MS = 30 * 86_400_000

/** Bytes per second, down to bytes: a quiet program writes kilobytes. */
export function rate(bps: number): string {
  if (bps >= 1024 ** 3) return `${(bps / 1024 ** 3).toFixed(1)} GB/s`
  if (bps >= 1024 ** 2) return `${(bps / 1024 ** 2).toFixed(bps >= 100 * 1024 ** 2 ? 0 : 1)} MB/s`
  if (bps >= 1024) return `${Math.round(bps / 1024)} KB/s`
  return `${Math.round(bps)} B/s`
}

function pct(v: number | null): string {
  return v === null ? '—' : `${v < 10 ? v.toFixed(1) : Math.round(v)}%`
}

const HIDDEN_WHY = 'The system would not let the agent read this program\'s disk counters. On Linux the agent needs CAP_SYS_PTRACE to read programs run by other users; the packaged service has it.'

/** A program's disk figures: a rate, refused, or not measured. */
function DiskCell({ v, hidden }: { v: number | null | undefined; hidden?: boolean }) {
  if (hidden && v == null) {
    return <Text span inherit c="dimmed" title={HIDDEN_WHY}>not readable</Text>
  }
  return <>{v == null ? '—' : rate(v)}</>
}

function Name({ name, count, sub }: { name: string; count: number; sub?: string }) {
  return (
    <>
      <Text span inherit ff="monospace">{name}</Text>
      {count > 1 && <Text span size={FS.micro} c="dimmed"> × {count}</Text>}
      {sub && <Text size={FS.micro} c="dimmed">{sub}</Text>}
    </>
  )
}

function Footnote({ disk }: { disk: boolean }) {
  return (
    <Text size={FS.micro} c="dimmed" px="md" py="xs" lh={1.5}>
      Each row adds up every process with that name, so memory two processes share is counted in each.
      {disk && ' Disk is what reached storage on Linux; on Windows it counts every file, device and network transfer.'}
    </Text>
  )
}

/**
 * What is using the server now, from the same report as the tiles: the
 * busiest programs by processor, then by memory, then by disk.
 */
export function ProgramsNow({ top, faded }: { top: ServerProcess[]; faded: boolean }) {
  // An agent or control plane older than disk by program sends none: no column.
  const disk = top.some((p) => p.readBps != null || p.ioHidden)
  return (
    <Card padding={0} mt="md" style={{ opacity: faded ? 0.6 : 1 }} data-testid="top-processes">
      <Text component="h3" fw={600} size="sm" px="md" pt="sm" pb={4} style={{ margin: 0 }}>
        What is using it <Text span inherit c="dimmed" fw={400}>(busiest programs by processor, memory and disk)</Text>
      </Text>
      <Table.ScrollContainer minWidth={disk ? 620 : 0} type="native">
        <Table verticalSpacing={4} fz={FS.meta} style={{ whiteSpace: 'nowrap' }}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Program</Table.Th>
              <Table.Th ta="right">Processor</Table.Th>
              <Table.Th ta="right">Memory</Table.Th>
              {disk && <Table.Th ta="right">Disk read</Table.Th>}
              {disk && <Table.Th ta="right">Disk write</Table.Th>}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {top.map((p) => (
              <Table.Tr key={`${p.name}-${p.pid}`}>
                <Table.Td>
                  <Name name={p.name} count={p.count ?? 1} />{' '}
                  {(p.count ?? 1) <= 1 && <Text span size={FS.micro} c="dimmed">pid {p.pid}</Text>}
                </Table.Td>
                <Table.Td ta="right">{pct(p.cpu)}</Table.Td>
                <Table.Td ta="right">{bytes(p.memory)}</Table.Td>
                {disk && <Table.Td ta="right"><DiskCell v={p.readBps} hidden={p.ioHidden} /></Table.Td>}
                {disk && <Table.Td ta="right"><DiskCell v={p.writeBps} hidden={p.ioHidden} /></Table.Td>}
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
      <Footnote disk={disk} />
    </Card>
  )
}

export const programsQuery = (asset: Asset, from: number, to: number) =>
  ({
    queryKey: ['asset', asset.id, 'programs', from, to] as const,
    queryFn: () => live.orFallback(
      () => get<AssetPrograms>(`/api/v1/assets/${encodeURIComponent(asset.id)}/programs?from=${encodeURIComponent(iso(from))}&to=${encodeURIComponent(iso(to))}`),
      async () => fixturePrograms(from, to),
    ),
    retry: false,
    placeholderData: (prev: AssetPrograms | undefined) => prev,
  })

/** To the second: the control plane reads RFC 3339 without fractions. */
function iso(t: number): string {
  return new Date(Math.floor(t / 1000) * 1000).toISOString().replace('.000Z', 'Z')
}

/** Sample programs for a database server. */
function fixturePrograms(from: number, to: number): AssetPrograms {
  const periods = Math.max(1, Math.round((to - from) / 300_000))
  const p = (name: string, cpu: number, memMB: number, r: number | null, w: number | null, count: number, seen = 1, ioHidden = false): ProgramUse =>
    ({ name, cpu, memory: memMB * 1024 ** 2, readBps: r, writeBps: w, ioHidden, count, periods: Math.max(1, Math.round(periods * seen)) })
  return {
    from: new Date(from).toISOString(), to: new Date(to).toISOString(), periods,
    programs: [
      p('postgres', 71.4, 6120, 38e6, 14e6, 23),
      p('java', 34.2, 3480, 1.2e6, 3.4e6, 1, 0.9),
      p('node', 12.8, 910, 40e3, 220e3, 4),
      p('backup-agent', 9.6, 140, 92e6, 0, 1, 0.04),
      p('nginx', 4.1, 96, 0, 18e3, 5),
      p('kworker', 2.2, 0, 0, 6e6, 31),
      p('mysqld', 1.4, 820, null, null, 1, 1, true),
      p('sshd', 0.3, 14, 0, 0, 3, 0.3),
    ],
  }
}

type SortBy = 'cpu' | 'memory' | 'disk'

const disk = (p: ProgramUse) => (p.readBps ?? 0) + (p.writeBps ?? 0)

/**
 * The busiest programs over the history's range, or over the one point
 * picked on a chart. Programs are kept 30 days: a longer range says so.
 */
export function ProgramsHistory({ asset, from, to, picked, step, onClear }: {
  asset: Asset
  from: number
  to: number
  /** The start of the point picked on a chart, or null for the whole range. */
  picked: number | null
  step: number
  onClear: () => void
}) {
  const [by, setBy] = useState<SortBy>('cpu')
  const a = picked ?? Math.max(from, to - PROGRAMS_KEPT_MS)
  const b = picked === null ? to : picked + step
  const tooLong = picked === null && to - from > PROGRAMS_KEPT_MS + 3_600_000
  const { data, isError, isLoading } = useQuery({ ...programsQuery(asset, a, b), enabled: !tooLong || picked !== null })
  const when = picked === null ? null
    : `${new Date(a).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}–${new Date(b).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })}`
  const rows = [...(data?.programs ?? [])].sort((x, y) =>
    by === 'cpu' ? (y.cpu ?? -1) - (x.cpu ?? -1) : by === 'memory' ? y.memory - x.memory : disk(y) - disk(x))
  const hasDisk = rows.some((p) => p.readBps !== null || p.ioHidden)
  return (
    <Card padding={0} data-testid="programs-history">
      <Group justify="space-between" wrap="wrap" gap="sm" px="md" py="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <Box>
          <Text component="h2" fw={600} size="sm" style={{ margin: 0 }}>Busiest programs</Text>
          <Text size={FS.micro} c="dimmed" data-testid="programs-when">
            {when ? `At ${when}` : tooLong ? 'Programs are kept for 30 days' : 'Over the whole range. Click a point on a chart to see one moment.'}
          </Text>
        </Box>
        <Group gap="xs">
          {picked !== null && <Button size="compact-xs" variant="subtle" onClick={onClear}>Whole range</Button>}
          <SegmentedControl size="xs" value={by} onChange={(v) => setBy(v as SortBy)} aria-label="Sort by"
            data={[{ value: 'cpu', label: 'Processor' }, { value: 'memory', label: 'Memory' }, { value: 'disk', label: 'Disk' }]} />
        </Group>
      </Group>
      {tooLong && picked === null ? (
        <Text size={FS.meta} c="dimmed" p="md" ta="center">
          Which programs ran is kept for the last 30 days. Pick 30 days or less, or click a point in the last 30 days.
        </Text>
      ) : isError ? (
        <Text size={FS.meta} c="dimmed" p="md">The programs could not be read. Try again in a moment.</Text>
      ) : isLoading || !data ? (
        <Skeleton h={160} m="md" />
      ) : rows.length === 0 ? (
        <Text size={FS.meta} c="dimmed" p="md" ta="center">
          No programs were reported {when ? 'at this point' : 'in this range'}. They are kept from the first report after the control plane and agent are updated.
        </Text>
      ) : (
        <>
          <Table.ScrollContainer minWidth={hasDisk ? 620 : 420} type="native">
            <Table verticalSpacing={4} fz={FS.meta} data-testid="programs-table" style={{ whiteSpace: 'nowrap' }}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Program</Table.Th>
                  <Table.Th ta="right">Processor</Table.Th>
                  <Table.Th ta="right">Memory</Table.Th>
                  {hasDisk && <Table.Th ta="right">Disk read</Table.Th>}
                  {hasDisk && <Table.Th ta="right">Disk write</Table.Th>}
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {rows.map((p) => (
                  <Table.Tr key={p.name}>
                    <Table.Td>
                      <Name name={p.name} count={p.count}
                        sub={picked === null && data.periods > 1 ? `among the busiest ${share(p.periods, data.periods)}` : undefined} />
                    </Table.Td>
                    <Table.Td ta="right">{pct(p.cpu)}</Table.Td>
                    <Table.Td ta="right">{bytes(p.memory)}</Table.Td>
                    {hasDisk && <Table.Td ta="right"><DiskCell v={p.readBps} hidden={p.ioHidden} /></Table.Td>}
                    {hasDisk && <Table.Td ta="right"><DiskCell v={p.writeBps} hidden={p.ioHidden} /></Table.Td>}
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
          <Text size={FS.micro} c="dimmed" px="md" pt="xs" lh={1.5}>
            Each figure is the program's busiest moment {when ? 'in these minutes' : 'in the range'}, so one row's figures can be from different moments.
            The agent names the programs using the most processor, memory and disk in each report; one not named was not among the busiest then.
          </Text>
          <Footnote disk={hasDisk} />
        </>
      )}
    </Card>
  )
}

function share(n: number, of: number): string {
  const f = n / of
  if (f >= 0.995) return 'all the time'
  if (f < 0.01) return `in ${n} of ${of} five-minute periods`
  return `${Math.round(f * 100)}% of the time`
}
