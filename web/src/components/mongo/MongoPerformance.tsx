import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ActionIcon, Box, Group, Loader, Modal, Paper, ScrollArea, Stack, Table, Text, Tooltip } from '@mantine/core'
import { IconRefresh } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { AdminRefused, PanelFrame } from '~/components/DatabaseAdmin'
import { FindingsList, Stat } from '~/components/DatabasePerformance'
import { bytes, count, duration, ms } from '~/lib/pgPerformance'
import { analyzeMongo, at, type MongoPerf, type MongoReport } from '~/lib/mongoPerformance'
import type { StoreSession } from '~/components/useStoreSession'

/*
MongoDB's Performance panel, over its console: why this database is heavy and
what to change. One read-only operation, perf, DBA only (the relay and the
agent both refuse it below), read as the person's own MongoDB user. A fix is
put on the command line, never run from here.
*/

export function MongoPerformance({ s, opened, onClose, narrow, onUse }: {
  s: StoreSession
  opened: boolean
  onClose: () => void
  narrow: boolean
  /** Puts a command on the command line. */
  onUse: (cmd: string) => void
}) {
  const [data, setData] = useState<{ perf: MongoPerf; report: MongoReport; asOf: Date } | null>(null)
  const [failure, setFailure] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const refresh = useCallback(async () => {
    setBusy(true)
    const a = await s.ask({ op: 'perf' })
    if (!alive.current) return
    setBusy(false)
    if (a.type !== 'result' || !a.data) { setFailure(a.error ?? 'no answer'); return }
    setFailure(null)
    const perf = a.data as MongoPerf
    setData({ perf, report: analyzeMongo(perf), asOf: new Date() })
  }, [s])
  useEffect(() => { if (opened) void refresh() }, [opened, refresh])

  return (
    <Modal opened={opened} onClose={onClose} size="xl" fullScreen={narrow} withCloseButton={false} padding={0}>
      <Box style={{ display: 'flex', flexDirection: 'column', height: narrow ? '100dvh' : '85dvh' }}>
        <PanelFrame title="Performance" narrow={narrow} onBack={onClose}
          blurb="Why this database is slow or heavy, and what to change. Read from MongoDB's own statistics; nothing is changed unless you run it."
          right={(
            <Group gap="xs" wrap="nowrap">
              {data && !narrow && <Text size={FS.micro} c="dimmed">as of {data.asOf.toLocaleTimeString()}</Text>}
              <Tooltip label="Analyze again" withArrow>
                <ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Analyze again"><IconRefresh size={16} /></ActionIcon>
              </Tooltip>
            </Group>
          )}>
          {failure ? (
            <Box p="md"><AdminRefused error={{ error: failure }} who="MongoDB" /></Box>
          ) : !data ? (
            <Group gap="xs" p="md"><Loader size={16} /><Text size={FS.meta} c="dimmed">Reading MongoDB’s statistics…</Text></Group>
          ) : (
            <ScrollArea style={{ flex: 1 }} type="auto">
              <Stack gap="md" p="md">
                <Summary perf={data.perf} report={data.report} />
                <FindingsList findings={data.report.findings} narrow={narrow} countingS={data.report.countingS} engine="MongoDB" connLabel="Connections" openLabel="Put on the command line" statement="command"
                  onOpenSql={(cmd) => { onUse(cmd); onClose() }} />
                {data.perf.slow.length > 0 && <SlowOps perf={data.perf} narrow={narrow} />}
              </Stack>
            </ScrollArea>
          )}
        </PanelFrame>
      </Box>
    </Modal>
  )
}

function Summary({ perf, report }: { perf: MongoPerf; report: MongoReport }) {
  const high = report.findings.filter((f) => f.severity === 'high').length
  const medium = report.findings.filter((f) => f.severity === 'medium').length
  const c = report.connections
  const data = at(perf.db, 'dataSize')
  const idx = at(perf.db, 'indexSize')
  const was = perf.profile?.was
  return (
    <Paper withBorder p="sm" radius="md">
      <Group gap="lg" wrap="wrap">
        <Stat label="Findings" value={high ? `${high} high, ${medium} medium` : medium ? `${medium} medium` : 'none serious'} tone={high ? INK.denied : medium ? INK.pending : INK.verified} />
        <Stat label="Cache in use" value={report.cacheFill === null ? '—' : report.cacheFill < 0.01 ? 'under 1%' : `${(report.cacheFill * 100).toFixed(0)}%`} tone={report.cacheFill !== null && report.cacheFill >= 0.95 ? INK.pending : undefined} />
        <Stat label="Connections" value={`${count(c.current)} of ${count(c.current + c.available)}`} />
        <Stat label="Examined per returned" value={report.examinedPerReturned === null ? '—' : count(Math.round(report.examinedPerReturned))}
          tone={report.examinedPerReturned !== null && report.examinedPerReturned >= 100 ? INK.pending : undefined} />
        <Stat label="Data, indexes" value={`${bytes(data)}, ${bytes(idx)}`} />
        <Stat label="Server" value={`MongoDB ${perf.version}`} />
      </Group>
      <Text size={FS.micro} c="dimmed" mt={8}>
        Counted over {duration(report.countingS)}, since MongoDB started.
        {was === undefined ? '' : was === 0 ? ' The profiler is off, so slow operations are not listed.' : ` The profiler keeps operations slower than ${perf.profile?.slowms ?? 100} ms.`}
        {perf.missing?.length ? ` Not read: ${perf.missing.join('; ')}.` : ''}
      </Text>
    </Paper>
  )
}

function SlowOps({ perf, narrow }: { perf: MongoPerf; narrow: boolean }) {
  const rows = useMemo(() => perf.slow.slice(0, 15), [perf])
  return (
    <Box>
      <Text fw={650} size={FS.body} mb={2}>Slowest operations</Text>
      <Text size={FS.micro} c="dimmed" mb={6}>From the profiler (system.profile), grouped by collection, kind and plan, by total time.</Text>
      <Paper withBorder radius="md" style={{ overflow: 'hidden' }}>
        <Table verticalSpacing={4} fz={12}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Collection</Table.Th><Table.Th>Plan</Table.Th><Table.Th ta="right">Runs</Table.Th><Table.Th ta="right">Total</Table.Th>
              {!narrow && <Table.Th ta="right">Examined</Table.Th>}{!narrow && <Table.Th ta="right">Returned</Table.Th>}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rows.map((x, i) => (
              <Table.Tr key={i}>
                <Table.Td style={{ fontFamily: 'var(--font-mono)' }}>{x._id.ns.slice(x._id.ns.indexOf('.') + 1)} <Text span size={FS.micro} c="dimmed">{x._id.op}</Text></Table.Td>
                <Table.Td style={{ fontFamily: 'var(--font-mono)', color: x._id.plan === 'COLLSCAN' ? INK.pending : undefined }}>{x._id.plan ?? '—'}</Table.Td>
                <Table.Td ta="right">{count(x.count)}</Table.Td>
                <Table.Td ta="right">{ms(x.totalMs)}</Table.Td>
                {!narrow && <Table.Td ta="right">{count(x.examined)}</Table.Td>}
                {!narrow && <Table.Td ta="right">{count(x.returned)}</Table.Td>}
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Paper>
    </Box>
  )
}
