import { useCallback, useEffect, useRef, useState } from 'react'
import { ActionIcon, Box, Button, Code, Group, Loader, Paper, ScrollArea, Stack, Table, Text, Tooltip } from '@mantine/core'
import { IconPlayerPlay, IconRefresh } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { AdminRefused, NeedsDBA, PanelFrame, type AdminFailure, type AdminPanelProps } from '~/components/DatabaseAdmin'
import { FindingsList, Stat } from '~/components/DatabasePerformance'
import { bytes, count, duration, ms, oneLine } from '~/lib/pgPerformance'
import { analyzeMySQL, diagnoseDigest, explainDigest, tables, type MyReport, type MySQLPerf } from '~/lib/mysqlPerformance'

/*
The Performance panel for MySQL and MariaDB. Unlike Postgres's, it cannot
read as the person's user: performance_schema and the server's status show
every schema, and the person's user is granted one schema only. So it is one
read-only admin operation, mysql.perf, that the agent runs as the server's
administrator, at DBA only (the control plane and the agent both refuse it
below). The analysis is lib/mysqlPerformance; fixes open in the editor and
are never run from here.
*/

export interface MySQLPerformanceProps extends AdminPanelProps {
  database: string
  onOpenSql: (sql: string, name: string) => void
}

export function MySQLPerformancePanel(props: MySQLPerformanceProps) {
  const { admin, level, narrow, onBack, onAskForAccess, onOpenSql } = props
  const [data, setData] = useState<{ perf: MySQLPerf; report: MyReport; asOf: Date } | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [busy, setBusy] = useState(false)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await admin<MySQLPerf>({ kind: 'mysql.perf' })
    if (!alive.current) return
    setBusy(false)
    if (r.error || !r.data) { setFailure(r.error ?? { error: 'no answer' }); return }
    setFailure(null)
    setData({ perf: r.data, report: analyzeMySQL(r.data), asOf: new Date() })
  }, [admin])
  const dba = level === 'dba'
  useEffect(() => { if (dba) void refresh() }, [dba, refresh])

  return (
    <PanelFrame title="Performance" narrow={narrow} onBack={onBack}
      blurb="Why this database is slow or heavy, and what to change. Read from the server's own statistics; nothing is changed unless you run it."
      right={dba ? (
        <Group gap="xs" wrap="nowrap">
          {data && !narrow && <Text size={FS.micro} c="dimmed">as of {data.asOf.toLocaleTimeString()}</Text>}
          <Tooltip label="Analyze again" withArrow>
            <ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Analyze again"><IconRefresh size={16} /></ActionIcon>
          </Tooltip>
        </Group>
      ) : undefined}>
      {!dba ? (
        <NeedsDBA level={level} what="The server’s statement statistics cover every schema and" account="administrator account" onAskForAccess={onAskForAccess} />
      ) : failure ? (
        <Box p="md"><AdminRefused error={failure} who="The agent" /></Box>
      ) : !data ? (
        <Group gap="xs" p="md"><Loader size={16} /><Text size={FS.meta} c="dimmed">Reading the server’s statistics…</Text></Group>
      ) : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Stack gap="md" p="md" maw={1100}>
            <Summary perf={data.perf} report={data.report} />
            <FindingsList findings={data.report.findings} narrow={narrow} onOpenSql={onOpenSql} countingS={data.report.countingS}
              engine={data.report.mariadb ? 'MariaDB' : 'MySQL'} connLabel="Connections" />
            {data.report.digests.length > 0 && <CostliestStatements report={data.report} narrow={narrow} onOpenSql={onOpenSql} />}
          </Stack>
        </ScrollArea>
      )}
    </PanelFrame>
  )
}

function Summary({ perf, report }: { perf: MySQLPerf; report: MyReport }) {
  const c = report.connections
  const high = report.findings.filter((f) => f.severity === 'high').length
  const medium = report.findings.filter((f) => f.severity === 'medium').length
  const size = tables(perf).reduce((a, t) => a + t.data + t.index, 0)
  return (
    <Paper withBorder p="sm" radius="md">
      <Group gap="lg" wrap="wrap">
        <Stat label="Findings" value={high || medium ? `${high} high, ${medium} medium` : 'none serious'}
          tone={high ? INK.denied : medium ? INK.pending : INK.verified} />
        <Stat label="Buffer pool hit" value={report.bufferHit === null ? '—' : `${(report.bufferHit * 100).toFixed(2)}%`}
          tone={report.bufferHit !== null && report.bufferHit < 0.99 ? INK.pending : undefined} />
        <Stat label="Connections" value={`${c.used} of ${c.max}`} tone={c.used / Math.max(1, c.max) >= 0.8 ? INK.pending : undefined} />
        <Stat label="Running now" value={String(c.running)} />
        <Stat label="Size" value={bytes(size)} />
        <Stat label="Server" value={perf.version.replace(/-0ubuntu.*$/, '').replace(/ \(.*\)$/, '')} />
      </Group>
      <Text size={FS.micro} c="dimmed" mt={8}>
        Counted over {duration(report.countingS)} since the server started or its status was last flushed.
        {perf.missing?.length ? ` Not read: ${perf.missing.join('; ')}.` : ''}
      </Text>
    </Paper>
  )
}

function CostliestStatements({ report, narrow, onOpenSql }: { report: MyReport; narrow: boolean; onOpenSql: (sql: string, name: string) => void }) {
  const [open, setOpen] = useState<number | null>(null)
  return (
    <Box>
      <Text fw={650} size={FS.body} mb={2}>Costliest queries</Text>
      <Text size={FS.micro} c="dimmed" mb={6}>From performance_schema, by total time, for this database. Pick one to see why it costs what it does.</Text>
      <Paper withBorder radius="md" style={{ overflow: 'hidden' }}>
        <Table verticalSpacing={4} fz={12} highlightOnHover>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Query</Table.Th>
              <Table.Th ta="right">Share</Table.Th>
              {!narrow && <Table.Th ta="right">Calls</Table.Th>}
              <Table.Th ta="right">Average</Table.Th>
              {!narrow && <Table.Th ta="right">Rows examined</Table.Th>}
              {!narrow && <Table.Th ta="right">Full scans</Table.Th>}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {report.digests.map((d, i) => {
              const on = open === i
              const dx = on ? diagnoseDigest(d) : null
              return [
                <Table.Tr key={i} onClick={() => setOpen(on ? null : i)} style={{ cursor: 'pointer' }} aria-expanded={on}>
                  <Table.Td style={{ fontFamily: 'var(--font-mono)', maxWidth: narrow ? 180 : 520, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={d.text}>
                    {oneLine(d.text, 160)}
                  </Table.Td>
                  <Table.Td ta="right" style={{ color: d.share >= 0.2 ? INK.pending : undefined, fontWeight: d.share >= 0.2 ? 600 : undefined }}>{(d.share * 100).toFixed(1)}%</Table.Td>
                  {!narrow && <Table.Td ta="right">{count(d.calls)}</Table.Td>}
                  <Table.Td ta="right">{ms(d.avgMs)}</Table.Td>
                  {!narrow && <Table.Td ta="right">{count(d.examined)}</Table.Td>}
                  {!narrow && <Table.Td ta="right" style={{ color: d.calls >= 5 && d.noIndex / d.calls >= 0.5 ? INK.pending : undefined }}>{count(d.noIndex)}</Table.Td>}
                </Table.Tr>,
                on && dx && (
                  <Table.Tr key={`${i}-why`}>
                    <Table.Td colSpan={narrow ? 3 : 6} style={{ background: 'var(--color-raised)' }}>
                      <Code block style={{ fontSize: 12, whiteSpace: 'pre-wrap', maxHeight: 200, overflow: 'auto' }}>{d.text}</Code>
                      {dx.reasons.length > 0 ? (
                        <Box component="ul" m={0} mt={6} pl={18} style={{ listStyle: 'disc' }}>
                          {dx.reasons.map((r, k) => <Text key={k} component="li" size={FS.meta}>{r}</Text>)}
                        </Box>
                      ) : <Text size={FS.meta} c="dimmed" mt={6}>Nothing about this query stands out on its own.</Text>}
                      {dx.fixes.length > 0 && <Text size={FS.meta} mt={6}><b>What to change: </b>{dx.fixes.join(' ')}</Text>}
                      <Button mt={8} size="compact-xs" variant="light" leftSection={<IconPlayerPlay size={12} />}
                        onClick={() => onOpenSql(explainDigest(d), 'Explain')}>Explain in editor</Button>
                    </Table.Td>
                  </Table.Tr>
                ),
              ]
            })}
          </Table.Tbody>
        </Table>
      </Paper>
    </Box>
  )
}
