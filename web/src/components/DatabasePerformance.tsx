import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Code, Group, Loader, Paper, ScrollArea, SegmentedControl, Stack, Table, Text, Tooltip,
} from '@mantine/core'
import { IconCode, IconPlayerPlay, IconRefresh } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { PanelFrame, type AdminPanelProps, type Collected } from '~/components/DatabaseAdmin'
import {
  PG_PERF_SQL, analyzePostgres, bytes, count, diagnoseStatement, duration, explainFor, ms, oneLine, parse, poolSummaries,
  type Area, type Finding, type PerfReport, type PgPerfFacts, type PoolerStats, type Severity,
} from '~/lib/pgPerformance'

/*
The Performance panel for Postgres: why this database is heavy, and what to
change. It reads with ordinary recorded statements as the person's role (so
the session recording shows each one, and Postgres decides what a level may
see: below DBA other people's query text is hidden), plus, at DBA, what the
agent reads of the PgBouncer in front (pooler.stats). The findings come from
lib/pgPerformance, which is pure and tested; this file only gathers and
shows. A fix is never run from here: "Open in editor" puts it in a query tab,
where it is checked, approved and recorded like anything typed.
*/

export interface PerformancePanelProps extends AdminPanelProps {
  database: string
  viaPooler: boolean
  onOpenSql: (sql: string, name: string) => void
}

type Loaded = { facts: PgPerfFacts; report: PerfReport; asOf: Date }

const SEVERITY: Record<Severity, { label: string; color: string }> = {
  high: { label: 'High', color: 'rose' },
  medium: { label: 'Medium', color: 'amber' },
  low: { label: 'Low', color: 'slate' },
}
const AREA: Record<Area, string> = {
  queries: 'Queries', tables: 'Tables', indexes: 'Indexes', memory: 'Memory', connections: 'Connections',
  pooler: 'PgBouncer', maintenance: 'Maintenance', setup: 'Settings',
}
const FILTERS: { value: string; label: string; areas: Area[] }[] = [
  { value: 'all', label: 'All', areas: [] },
  { value: 'queries', label: 'Queries', areas: ['queries', 'memory'] },
  { value: 'data', label: 'Tables & indexes', areas: ['tables', 'indexes', 'maintenance'] },
  { value: 'conn', label: 'Connections & PgBouncer', areas: ['connections', 'pooler'] },
  { value: 'setup', label: 'Settings', areas: ['setup'] },
]

/** Reads every fact, one recorded statement at a time. */
async function gather(p: PerformancePanelProps): Promise<{ facts?: PgPerfFacts; error?: Collected['error'] }> {
  const ov = await p.collect(PG_PERF_SQL.overview, 1)
  if (ov.error || !ov.rows[0]) return { error: ov.error ?? { error: 'Postgres returned no statistics for this database' } }
  const overview = parse.overview(ov.rows[0])
  const rows = async <T,>(sql: string, max: number, fn: (r: (string | null)[]) => T): Promise<T[]> => {
    const r = await p.collect(sql, max)
    return r.error ? [] : r.rows.map(fn)
  }
  const activity = await rows(PG_PERF_SQL.activity, 500, parse.activity)
  const clients = await rows(PG_PERF_SQL.clients, 50, parse.clients)
  const settings = await rows(PG_PERF_SQL.settings, 100, parse.settings)
  let statements: PgPerfFacts['statements'] = null
  let statementsError: string | undefined
  if (overview.statements) {
    const r = await p.collect(PG_PERF_SQL.statements(overview.versionNum), 25)
    if (r.error) statementsError = r.error.error
    else statements = r.rows.map(parse.statements)
  }
  const tables = await rows(PG_PERF_SQL.tables, 200, parse.tables)
  const indexes = await rows(PG_PERF_SQL.indexes, 300, parse.indexes)
  const foreignKeys = await rows(PG_PERF_SQL.foreignKeys, 100, parse.foreignKeys)
  const slots = overview.replica ? [] : await rows(PG_PERF_SQL.slots, 100, parse.slots)
  const cp = await rows(PG_PERF_SQL.checkpoints(overview.versionNum), 1, parse.checkpoints)
  // Below DBA the agent does not read PgBouncer (null). An agent too old
  // to know pooler.stats answers an error: then PgBouncer is left out
  // unless this entry goes through one, which is said rather than guessed.
  let pooler: PoolerStats | null | undefined = null
  if (p.level === 'dba') {
    const r = await p.admin<PoolerStats>({ kind: 'pooler.stats' })
    if (r.data) pooler = r.data
    else if (p.viaPooler) pooler = { found: true, through: true, running: false, settings: {}, liveError: `The agent could not read PgBouncer: ${r.error?.error ?? 'no answer'}` }
    else pooler = undefined
  }
  return {
    facts: {
      database: p.database, overview, activity, clients, settings, statements, statementsError, tables, indexes,
      foreignKeys, slots, checkpoints: cp[0], pooler, viaPooler: p.viaPooler,
    },
  }
}

export function PerformancePanel(props: PerformancePanelProps) {
  const { narrow, onBack, onOpenSql, level } = props
  const [loaded, setLoaded] = useState<Loaded | null>(null)
  const [error, setError] = useState<Collected['error'] | null>(null)
  const [busy, setBusy] = useState(false)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const propsRef = useRef(props)
  propsRef.current = props
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await gather(propsRef.current)
    if (!alive.current) return
    setBusy(false)
    if (r.error || !r.facts) { setError(r.error ?? null); return }
    setError(null)
    setLoaded({ facts: r.facts, report: analyzePostgres(r.facts), asOf: new Date() })
  }, [])
  useEffect(() => { void refresh() }, [refresh])

  return (
    <PanelFrame title="Performance" narrow={narrow} onBack={onBack}
      blurb="Why this database is slow or heavy, and what to change. Every read here is a recorded statement; nothing is changed unless you run it."
      right={(
        <Group gap="xs" wrap="nowrap">
          {loaded && !narrow && <Text size={FS.micro} c="dimmed">as of {loaded.asOf.toLocaleTimeString()}</Text>}
          <Tooltip label="Analyze again" withArrow>
            <ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Analyze again"><IconRefresh size={16} /></ActionIcon>
          </Tooltip>
        </Group>
      )}>
      {error ? (
        <Box p="md">
          <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
            <Text size={FS.body} fw={600}>Postgres refused this: {error.error}</Text>
          </Box>
        </Box>
      ) : !loaded ? (
        <Group gap="xs" p="md"><Loader size={16} /><Text size={FS.meta} c="dimmed">Reading this database’s statistics…</Text></Group>
      ) : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Stack gap="md" p="md" maw={1100}>
            <Summary loaded={loaded} level={level} />
            <FindingsList findings={loaded.report.findings} narrow={narrow} onOpenSql={onOpenSql} countingS={loaded.report.countingS}
              engine="Postgres" connLabel="Connections & PgBouncer" partial={loaded.facts.statements === null} />
            {loaded.facts.statements && loaded.facts.statements.length > 0 && (
              <CostliestQueries facts={loaded.facts} narrow={narrow} onOpenSql={onOpenSql} />
            )}
            {loaded.facts.pooler?.found && <PoolerSection p={loaded.facts.pooler} narrow={narrow} />}
          </Stack>
        </ScrollArea>
      )}
    </PanelFrame>
  )
}

/** The findings, filtered by area: shared by every engine's panel. */
export function FindingsList({ findings, narrow, onOpenSql, countingS, engine, connLabel, partial, openLabel = 'Open in editor', statement = 'statement' }: {
  findings: Finding[]; narrow: boolean; onOpenSql: (sql: string, name: string) => void; countingS: number
  engine: string; connLabel: string; partial?: boolean
  /** Where a fix goes, on its button; and what a fix is called. */
  openLabel?: string; statement?: string
}) {
  const [filter, setFilter] = useState('all')
  const filters = FILTERS.map((f) => (f.value === 'conn' ? { ...f, label: connLabel } : f))
  const areas = filters.find((x) => x.value === filter)?.areas ?? []
  const shown = findings.filter((x) => areas.length === 0 || areas.includes(x.area))
  return (
    <Box>
      <Group justify="space-between" mb={6} wrap="wrap" gap="xs">
        <Text fw={650} size={FS.body}>What makes it heavy</Text>
        {!narrow && <SegmentedControl size="xs" value={filter} onChange={setFilter} data={filters.map(({ value, label }) => ({ value, label }))} />}
      </Group>
      {shown.length === 0 ? (
        <Paper withBorder p="sm" radius="md">
          <Text size={FS.body} c={INK.verified} fw={600}>Nothing stands out{filter !== 'all' ? ' here' : ''}.</Text>
          <Text size={FS.meta} c="dimmed" mt={2}>
            The checks found no problem in the numbers {engine} keeps{partial ? ' (queries could not be checked, see above)' : ''}.
            They cover {duration(countingS)} of activity.
          </Text>
        </Paper>
      ) : (
        <Stack gap="xs">
          {shown.map((f) => <FindingCard key={f.id} f={f} openLabel={openLabel} statement={statement} onOpen={() => onOpenSql(f.sql!, f.title.slice(0, 40))} />)}
        </Stack>
      )}
    </Box>
  )
}

export function Stat({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <Box style={{ minWidth: 110 }}>
      <Text size={FS.micro} c="dimmed">{label}</Text>
      <Text size={FS.body} fw={650} c={tone}>{value}</Text>
    </Box>
  )
}

function Summary({ loaded, level }: { loaded: Loaded; level: AdminPanelProps['level'] }) {
  const { report, facts } = loaded
  const c = report.connections
  const high = report.findings.filter((f) => f.severity === 'high').length
  const medium = report.findings.filter((f) => f.severity === 'medium').length
  const p = facts.pooler
  const pooler = p === null ? (facts.viaPooler ? 'needs DBA to read' : 'not read below DBA')
    : !p ? '' : !p.found ? 'none on this server' : `${p.settings?.pool_mode ?? '?'} mode, port ${p.port}${p.liveError ? ' (file only)' : ''}`
  return (
    <Paper withBorder p="sm" radius="md">
      <Group gap="lg" wrap="wrap">
        <Stat label="Findings" value={high || medium ? `${high} high, ${medium} medium` : 'none serious'}
          tone={high ? INK.denied : medium ? INK.pending : INK.verified} />
        <Stat label="Cache hit" value={report.cacheHit === null ? '—' : `${(report.cacheHit * 100).toFixed(2)}%`}
          tone={report.cacheHit !== null && report.cacheHit < 0.98 ? INK.pending : undefined} />
        <Stat label="Connections" value={`${c.used} of ${c.max}`} tone={c.used / Math.max(1, c.max) >= 0.8 ? INK.pending : undefined} />
        <Stat label="Idle in transaction" value={String(c.idleInTx)} tone={c.idleInTx ? INK.pending : undefined} />
        <Stat label="Waiting on a lock" value={String(c.waitingOnLock)} tone={c.waitingOnLock ? INK.denied : undefined} />
        <Stat label="Size" value={bytes(facts.overview.dbBytes)} />
        {pooler && <Stat label="PgBouncer" value={pooler} />}
      </Group>
      <Text size={FS.micro} c="dimmed" mt={8}>
        Counted over {duration(report.countingS)}{facts.overview.statsReset ? ` since the statistics were reset (${facts.overview.statsReset.slice(0, 16)})` : ' since Postgres started'}.
        {level !== 'dba' && ' Below DBA, Postgres hides the text of other roles’ queries and Argus does not read PgBouncer.'}
      </Text>
    </Paper>
  )
}

function FindingCard({ f, onOpen, openLabel, statement }: { f: Finding; onOpen: () => void; openLabel: string; statement: string }) {
  const [showSql, setShowSql] = useState(false)
  const sev = SEVERITY[f.severity]
  return (
    <Paper withBorder p="sm" radius="md" style={{ borderLeft: `3px solid var(--mantine-color-${sev.color}-6)` }}>
      <Group gap={6} wrap="nowrap" align="flex-start">
        <Badge size="sm" variant="light" color={sev.color} style={{ flexShrink: 0 }}>{sev.label}</Badge>
        <Badge size="sm" variant="outline" color="slate" style={{ flexShrink: 0 }}>{AREA[f.area]}</Badge>
        <Text size={FS.body} fw={650} style={{ minWidth: 0, wordBreak: 'break-word' }}>{f.title}</Text>
      </Group>
      <Text size={FS.meta} mt={6}>{f.why}</Text>
      <Box component="ul" m={0} mt={4} pl={18} style={{ listStyle: 'disc' }}>
        {f.evidence.map((e, i) => <Text key={i} component="li" size={FS.meta} c="dimmed" style={{ wordBreak: 'break-word' }}>{e}</Text>)}
      </Box>
      <Text size={FS.meta} mt={6}><b>What to change: </b>{f.fix}</Text>
      {f.sql && (
        <>
          <Group gap="xs" mt={8}>
            <Button size="compact-xs" variant="default" leftSection={<IconCode size={12} />} onClick={() => setShowSql((v) => !v)}>
              {showSql ? `Hide ${statement}` : `Show ${statement}`}
            </Button>
            <Button size="compact-xs" variant="light" leftSection={<IconPlayerPlay size={12} />} onClick={onOpen}>{openLabel}</Button>
          </Group>
          {showSql && <Code block mt={6} style={{ fontSize: 12, whiteSpace: 'pre-wrap' }}>{f.sql}</Code>}
        </>
      )}
    </Paper>
  )
}

function CostliestQueries({ facts, narrow, onOpenSql }: { facts: PgPerfFacts; narrow: boolean; onOpenSql: (sql: string, name: string) => void }) {
  const [open, setOpen] = useState<string | null>(null)
  const list = facts.statements ?? []
  return (
    <Box>
      <Text fw={650} size={FS.body} mb={2}>Costliest queries</Text>
      <Text size={FS.micro} c="dimmed" mb={6}>From pg_stat_statements, by total time. Pick one to see why it costs what it does.</Text>
      <Paper withBorder radius="md" style={{ overflow: 'hidden' }}>
        <Table verticalSpacing={4} fz={12} highlightOnHover>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Query</Table.Th>
              <Table.Th ta="right">Share</Table.Th>
              {!narrow && <Table.Th ta="right">Calls</Table.Th>}
              <Table.Th ta="right">Average</Table.Th>
              {!narrow && <Table.Th ta="right">Total</Table.Th>}
              {!narrow && <Table.Th ta="right">Rows</Table.Th>}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {list.map((q) => {
              const on = open === q.queryid
              const d = on ? diagnoseStatement(q) : null
              return [
                <Table.Tr key={q.queryid} onClick={() => setOpen(on ? null : q.queryid)} style={{ cursor: 'pointer' }} aria-expanded={on}>
                  <Table.Td style={{ fontFamily: 'var(--font-mono)', maxWidth: narrow ? 180 : 520, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={q.query}>
                    {q.query === '<insufficient privilege>' ? <Text span size={FS.meta} c="dimmed">another role’s query, hidden below DBA</Text> : oneLine(q.query, 160)}
                  </Table.Td>
                  <Table.Td ta="right" style={{ color: q.share >= 0.2 ? INK.pending : undefined, fontWeight: q.share >= 0.2 ? 600 : undefined }}>{(q.share * 100).toFixed(1)}%</Table.Td>
                  {!narrow && <Table.Td ta="right">{count(q.calls)}</Table.Td>}
                  <Table.Td ta="right">{ms(q.meanMs)}</Table.Td>
                  {!narrow && <Table.Td ta="right">{ms(q.totalMs)}</Table.Td>}
                  {!narrow && <Table.Td ta="right">{count(q.rows)}</Table.Td>}
                </Table.Tr>,
                on && d && (
                  <Table.Tr key={`${q.queryid}-why`}>
                    <Table.Td colSpan={narrow ? 3 : 6} style={{ background: 'var(--color-raised)' }}>
                      <Code block style={{ fontSize: 12, whiteSpace: 'pre-wrap', maxHeight: 200, overflow: 'auto' }}>{q.query}</Code>
                      {d.reasons.length > 0 ? (
                        <Box component="ul" m={0} mt={6} pl={18} style={{ listStyle: 'disc' }}>
                          {d.reasons.map((r, i) => <Text key={i} component="li" size={FS.meta}>{r}</Text>)}
                        </Box>
                      ) : <Text size={FS.meta} c="dimmed" mt={6}>Nothing about this query stands out on its own.</Text>}
                      {d.fixes.length > 0 && <Text size={FS.meta} mt={6}><b>What to change: </b>{d.fixes.join(' ')}</Text>}
                      {q.query !== '<insufficient privilege>' && (
                        <Button mt={8} size="compact-xs" variant="light" leftSection={<IconPlayerPlay size={12} />}
                          onClick={() => onOpenSql(explainFor(q, facts.overview.versionNum), 'Explain')}>Explain in editor</Button>
                      )}
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

const POOLER_SETTINGS = [
  'pool_mode', 'default_pool_size', 'reserve_pool_size', 'max_client_conn', 'max_db_connections', 'max_user_connections',
  'idle_transaction_timeout', 'query_wait_timeout', 'max_prepared_statements', 'server_idle_timeout',
]

function PoolerSection({ p, narrow }: { p: PoolerStats; narrow: boolean }) {
  const pools = poolSummaries(p)
  return (
    <Box>
      <Text fw={650} size={FS.body} mb={2}>PgBouncer</Text>
      <Text size={FS.micro} c="dimmed" mb={6}>
        Port {p.port}{p.version ? `, ${p.version}` : ''}{p.runsAs ? `, runs as ${p.runsAs}` : ''}.
        {' '}{p.settingsFrom === 'console' ? 'Settings as PgBouncer reports them, defaults included.' : `Settings as written in ${p.config ?? 'pgbouncer.ini'}; anything not written is PgBouncer’s default.`}
        {p.through ? '' : ' This entry does not go through it.'}
      </Text>
      <Paper withBorder p="sm" radius="md">
        <Group gap="lg" wrap="wrap">
          {POOLER_SETTINGS.filter((k) => p.settings?.[k] !== undefined).map((k) => (
            <Box key={k} style={{ minWidth: 130 }}>
              <Text size={FS.micro} c="dimmed" ff="monospace">{k}</Text>
              <Text size={FS.body} fw={600}>{p.settings![k]}</Text>
            </Box>
          ))}
        </Group>
      </Paper>
      {pools.length > 0 && (
        <Paper withBorder radius="md" mt="xs" style={{ overflow: 'hidden' }}>
          <Table verticalSpacing={4} fz={12}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Pool</Table.Th>
                <Table.Th ta="right">Clients</Table.Th>
                <Table.Th ta="right">Waiting</Table.Th>
                <Table.Th ta="right">Busy / size</Table.Th>
                {!narrow && <Table.Th ta="right">Idle servers</Table.Th>}
                <Table.Th ta="right">Longest wait</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {pools.map((x) => (
                <Table.Tr key={`${x.database}/${x.user}`}>
                  <Table.Td style={{ fontFamily: 'var(--font-mono)' }}>{x.database}/{x.user}</Table.Td>
                  <Table.Td ta="right">{x.clActive}</Table.Td>
                  <Table.Td ta="right" style={{ color: x.clWaiting ? INK.denied : undefined, fontWeight: x.clWaiting ? 600 : undefined }}>{x.clWaiting}</Table.Td>
                  <Table.Td ta="right">{x.svActive} / {x.size}</Table.Td>
                  {!narrow && <Table.Td ta="right">{x.svIdle}</Table.Td>}
                  <Table.Td ta="right">{x.maxWaitS ? `${x.maxWaitS.toFixed(1)} s` : '—'}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Paper>
      )}
    </Box>
  )
}
