import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Code, Group, Loader, Modal, NumberInput, Paper, ScrollArea, SegmentedControl, Select,
  Stack, Switch, Table, Tabs, Text, TextInput, Tooltip,
} from '@mantine/core'
import { IconRefresh } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { notifyOk } from '~/lib/notify'
import { usePreview } from '~/components/DatabaseExtensions'
import { FindingsList, Stat } from '~/components/DatabasePerformance'
import { duration } from '~/lib/pgPerformance'
import { analyzeMSSQL, type MSFacts, type MSReport } from '~/lib/mssqlPerformance'
import { AdminRefused, PanelFrame, type Admin, type AdminFailure, type AdminOp, type AdminPanelProps } from '~/components/DatabaseAdmin'

/*
SQL Server's DBA tools for one database, the everyday work of SSMS's Activity
Monitor, maintenance plans and database properties: activity and blocking,
waits and costly queries, index and statistics upkeep, DBCC CHECKDB, the SQL
Server Agent jobs that work on this database, and its settings.

Each read and change is a fixed operation the agent runs as the admin login
(internal/tunnel/mssqldba.go). Every change opens a confirmation with the
agent's own preview of the statement; where this database's changes wait for
approval, the session asks for it with that text. Ending a session reaches
only sessions in this database or blocking one in it; a job only one with a
step here.
*/

interface Session {
  session: number; login: string; host?: string; program?: string; status: string; command?: string
  wait?: string; waitMs?: number; resource?: string; blockedBy?: number; elapsedMs?: number; cpuMs?: number
  reads?: number; openTran?: number; lastRequest?: string; statement?: string; elsewhere?: boolean; yours?: boolean; argus?: boolean
}
export interface MSIndexRow {
  schema: string; table: string; name?: string; type: string; primary?: boolean; unique?: boolean; disabled?: boolean
  pages: number; fragmentation: number; seeks: number; scans: number; lookups: number; updates: number; lastUsed?: string
}
interface Statistic { schema: string; table: string; name: string; auto?: boolean; lastUpdated?: string; rows: number; sampled: number; modified: number }
interface Missing { schema: string; table: string; equality?: string; range?: string; include?: string; seeks: number; impact: number; create: string }
interface Job {
  name: string; enabled: boolean; description?: string; category?: string; owner?: string; running?: boolean
  lastOutcome?: string; lastRun?: string; lastSecs?: number; nextRun?: string; schedules: number
}
interface Jobs {
  agent: string; jobs: Job[]
  steps?: { id: number; name: string; subsystem: string; database?: string; command?: string }[]
  history?: { step: number; name: string; outcome: string; when: string; secs: number; message?: string }[]
}
interface Setting { name: string; value: string }
interface Settings {
  settings: Setting[]; facts: Setting[]
  files: { name: string; type: string; path: string; sizeMb: number; usedMb: number; maxMb: number; growth: string; readonly?: boolean }[]
  server: { name: string; value: string; description?: string }[]
}

/** Loads one read, refreshes it, and keeps its failure. */
export function useRead<T>(admin: Admin, op: AdminOp, every?: number) {
  const [data, setData] = useState<T | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [busy, setBusy] = useState(false)
  const [asOf, setAsOf] = useState<Date | null>(null)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const key = JSON.stringify(op)
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await admin<T>(JSON.parse(key) as AdminOp)
    if (!alive.current) return
    if (r.error) setFailure(r.error)
    else { setFailure(null); setData(r.data ?? null); setAsOf(new Date()) }
    setBusy(false)
  }, [admin, key])
  useEffect(() => { void refresh() }, [refresh])
  useEffect(() => {
    if (!every) return
    const t = setInterval(() => void refresh(), every)
    return () => clearInterval(t)
  }, [every, refresh])
  return { data, failure, busy, asOf, refresh }
}

const ms = (n?: number) => {
  if (!n) return ''
  if (n < 1000) return `${n} ms`
  const s = n / 1000
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)} s`
  const m = Math.floor(s / 60)
  return m < 60 ? `${m} min ${Math.round(s % 60)} s` : `${Math.floor(m / 60)} h ${m % 60} min`
}
export const when = (iso?: string) => (iso ? new Date(iso).toLocaleString() : '—')
const count = (n: number) => n.toLocaleString()
const mono = { fontFamily: 'var(--font-mono)' }
export const stickyHead = { position: 'sticky' as const, top: 0, background: 'var(--color-raised)', zIndex: 1 }

export function RefreshButton({ busy, onClick }: { busy: boolean; onClick: () => void }) {
  return <ActionIcon variant="subtle" onClick={onClick} loading={busy} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>
}

/** A change waiting for its Run: what it does, the agent's preview, and the result. */
export interface Pending { op: AdminOp; title: string; words: string; verb: string; danger?: boolean; done: string }

export function ConfirmOp({ admin, pending, onClose, onDone }: {
  admin: Admin; pending: Pending | null; onClose: () => void; onDone: () => void
}) {
  const preview = usePreview(admin, pending?.op ?? null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  useEffect(() => { setFailure(null) }, [pending])
  const run = async () => {
    if (!pending) return
    setBusy(true)
    const r = await admin(pending.op)
    setBusy(false)
    if (r.error) { setFailure(r.error); return }
    notifyOk(pending.done, r.ms ? `In ${ms(r.ms)}.` : '')
    onDone()
  }
  return (
    <Modal opened={Boolean(pending)} onClose={busy ? () => {} : onClose} title={pending?.title} size="lg">
      {pending && <>
        <Text size={FS.body}>{pending.words}</Text>
        <Text size={FS.micro} c="dimmed" mt="md">Runs as SQL Server’s admin login and is recorded in this session. On a database whose changes wait for approval, it is asked for first.</Text>
        <Code block mt={4} fz={12} style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{preview.sql || (preview.error ? `— ${preview.error.error}` : '…')}</Code>
        {failure && <Box mt="sm"><AdminRefused error={failure} who="SQL Server" /></Box>}
        <Group mt="md" gap="xs">
          <Button size="xs" color={pending.danger ? 'rose' : undefined} onClick={() => void run()} disabled={!preview.sql} loading={busy}>{pending.verb}</Button>
          <Button size="xs" variant="default" onClick={onClose} disabled={busy}>Cancel</Button>
        </Group>
        {busy && <Text size={FS.micro} c="dimmed" mt="xs">This can take minutes on a big database; the session waits for it.</Text>}
      </>}
    </Modal>
  )
}

export function Failed({ failure }: { failure: AdminFailure }) {
  return <Box p="md"><AdminRefused error={failure} who="SQL Server" /></Box>
}

/* ── Activity & blocking ─────────────────────────────────────────────────── */

/** The sessions at the head of a blocking chain: blocking others, blocked by none. */
export function headBlockers(sessions: Session[]): Set<number> {
  const blocked = new Map(sessions.filter((s) => s.blockedBy).map((s) => [s.session, s.blockedBy!]))
  const heads = new Set<number>()
  for (const b of blocked.values()) if (!blocked.has(b)) heads.add(b)
  return heads
}

export function MSSQLActivity({ admin, narrow, onBack }: AdminPanelProps) {
  const [auto, setAuto] = useState(true)
  const { data, failure, busy, asOf, refresh } = useRead<{ sessions: Session[] }>(admin, { kind: 'ms.activity' }, auto ? 10_000 : undefined)
  const [pending, setPending] = useState<Pending | null>(null)
  const rows = data?.sessions ?? []
  const heads = headBlockers(rows)
  const running = rows.filter((r) => r.command).length
  const blocked = rows.filter((r) => r.blockedBy).length
  const openTran = rows.filter((r) => r.openTran && !r.command).length
  const kill = (r: Session) => setPending({
    op: { kind: 'ms.kill', msdba: { session: r.session } }, title: `End session ${r.session}`, verb: 'End session', danger: true,
    done: `Session ${r.session} ended`,
    words: `${r.login} from ${r.host || 'an unknown host'}. SQL Server rolls back its open transaction${r.openTran ? ' (which can take as long as it ran)' : ''} and closes its connection.`,
  })
  return (
    <PanelFrame title="Activity & blocking" narrow={narrow} onBack={onBack}
      blurb="Every session connected to this database, what it runs and waits on, and who blocks whom. A session elsewhere that blocks one here is listed too."
      right={(
        <Group gap="xs" wrap="nowrap">
          <Switch size="xs" label={narrow ? '' : 'Every 10 s'} checked={auto} onChange={(e) => setAuto(e.currentTarget.checked)} aria-label="Refresh automatically" />
          <RefreshButton busy={busy} onClick={() => void refresh()} />
        </Group>
      )}>
      <Group gap="md" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }} wrap="wrap">
        <Text size={FS.meta}><b>{rows.length}</b> sessions</Text>
        <Text size={FS.meta}><b>{running}</b> running</Text>
        <Text size={FS.meta} c={openTran ? 'amber' : undefined}><b>{openTran}</b> idle with a transaction open</Text>
        <Text size={FS.meta} c={blocked ? 'rose' : undefined}><b>{blocked}</b> blocked</Text>
        {asOf && <Text size={FS.micro} c="dimmed" ml="auto">as of {asOf.toLocaleTimeString()}</Text>}
      </Group>
      {failure ? <Failed failure={failure} /> : !data ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Table verticalSpacing={4} fz={12} style={mono} striped highlightOnHover>
            <Table.Thead style={stickyHead}>
              <Table.Tr>
                <Table.Th>Session</Table.Th><Table.Th>Login</Table.Th>{!narrow && <Table.Th>From</Table.Th>}
                <Table.Th>Status</Table.Th>{!narrow && <Table.Th>Waiting on</Table.Th>}<Table.Th>Running</Table.Th>
                <Table.Th>Blocked by</Table.Th><Table.Th>Statement</Table.Th><Table.Th></Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((r) => (
                <Table.Tr key={r.session} style={{ background: r.blockedBy ? 'var(--mantine-color-rose-light)' : undefined }}>
                  <Table.Td>
                    {r.session}
                    {heads.has(r.session) && <Badge size="xs" color="rose" variant="filled" ml={4}>head blocker</Badge>}
                    {r.yours && <Badge size="xs" variant="light" ml={4}>you</Badge>}
                    {r.elsewhere && <Badge size="xs" variant="outline" color="slate" ml={4}>other database</Badge>}
                  </Table.Td>
                  <Table.Td>{r.login}</Table.Td>
                  {!narrow && <Table.Td style={{ maxWidth: 160, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={`${r.program ?? ''} on ${r.host ?? ''}`}>{r.host}{r.program ? ` · ${r.program}` : ''}</Table.Td>}
                  <Table.Td style={{ whiteSpace: 'nowrap', color: r.openTran && !r.command ? INK.pending : undefined }}>
                    {r.status}{r.openTran ? ` · ${r.openTran} tx` : ''}
                  </Table.Td>
                  {!narrow && <Table.Td style={{ whiteSpace: 'nowrap' }} title={r.resource}>{r.wait ? `${r.wait} ${ms(r.waitMs)}` : ''}</Table.Td>}
                  <Table.Td style={{ whiteSpace: 'nowrap' }}>{r.command ? `${r.command} ${ms(r.elapsedMs)}` : ''}</Table.Td>
                  <Table.Td>{r.blockedBy || ''}</Table.Td>
                  <Table.Td style={{ maxWidth: narrow ? 160 : 420, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={r.statement}>{r.statement}</Table.Td>
                  <Table.Td>{!r.yours && <Button size="compact-xs" variant="subtle" color="rose" onClick={() => kill(r)}>End</Button>}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
          {rows.length === 0 && <Text size={FS.meta} c="dimmed" p="md">Nobody else is connected.</Text>}
        </ScrollArea>
      )}
      <ConfirmOp admin={admin} pending={pending} onClose={() => setPending(null)} onDone={() => { setPending(null); void refresh() }} />
    </PanelFrame>
  )
}

/* ── Waits & costly queries ──────────────────────────────────────────────── */

/** What a wait type usually means, for the ones seen most. */
export const WAIT_WORDS: Record<string, string> = {
  CXPACKET: 'parallel queries waiting on each other',
  CXCONSUMER: 'parallel queries waiting on each other',
  PAGEIOLATCH_SH: 'reading data pages from disk',
  PAGEIOLATCH_EX: 'reading data pages from disk to change them',
  WRITELOG: 'writing the transaction log',
  LCK_M_X: 'blocked, waiting to change a row someone holds',
  LCK_M_S: 'blocked, waiting to read a row someone is changing',
  LCK_M_U: 'blocked, waiting to update',
  LCK_M_IX: 'blocked, waiting on a table or page lock',
  SOS_SCHEDULER_YIELD: 'busy CPU',
  ASYNC_NETWORK_IO: 'the client reading results slowly',
  RESOURCE_SEMAPHORE: 'queries waiting for memory to run',
  PAGELATCH_EX: 'contention on pages in memory (often tempdb)',
  PAGELATCH_SH: 'contention on pages in memory (often tempdb)',
  THREADPOOL: 'no worker threads free',
  IO_COMPLETION: 'disk I/O, often sorts and spills',
  BACKUPIO: 'backups reading or writing',
  HADR_SYNC_COMMIT: 'waiting for an availability group replica to harden the log',
}

/** Everything the advice reads, at once: four read-only operations. */
function useHealth(admin: Admin) {
  const [facts, setFacts] = useState<MSFacts | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [busy, setBusy] = useState(false)
  const [asOf, setAsOf] = useState<Date | null>(null)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const refresh = useCallback(async () => {
    setBusy(true)
    const [p, i, st, h] = await Promise.all([
      admin<MSFacts['performance']>({ kind: 'ms.performance', msdba: { sort: 'cpu' } }),
      admin<MSFacts['indexes']>({ kind: 'ms.indexes' }),
      admin<MSFacts['settings']>({ kind: 'ms.settings' }),
      admin<MSFacts['health']>({ kind: 'ms.health' }),
    ])
    if (!alive.current) return
    setBusy(false)
    const bad = [p, i, st, h].find((r) => r.error || !r.data)
    if (bad) { setFailure(bad.error ?? { error: 'no answer' }); return }
    setFailure(null)
    setFacts({ performance: p.data!, indexes: i.data!, settings: st.data!, health: h.data! })
    setAsOf(new Date())
  }, [admin])
  useEffect(() => { void refresh() }, [refresh])
  return { facts, failure, busy, asOf, refresh }
}

export function MSSQLPerformance({ admin, narrow, onBack, onOpenSql }: AdminPanelProps) {
  const [sort, setSort] = useState('cpu')
  const { facts, failure, busy, asOf, refresh } = useHealth(admin)
  const report = useMemo(() => (facts ? analyzeMSSQL(facts) : null), [facts])
  // The statements table sorted another way is its own read; by CPU it is
  // the one the advice already has.
  const [other, setOther] = useState<{ sort: string; perf: MSFacts['performance'] } | null>(null)
  useEffect(() => {
    if (sort === 'cpu') return
    let live = true
    void admin<MSFacts['performance']>({ kind: 'ms.performance', msdba: { sort } }).then((r) => { if (live && r.data) setOther({ sort, perf: r.data }) })
    return () => { live = false }
  }, [admin, sort, asOf])
  const perf = sort === 'cpu' ? facts?.performance : other?.sort === sort ? other.perf : undefined
  const open = onOpenSql ?? (() => {})
  return (
    <PanelFrame title="Performance" narrow={narrow} onBack={onBack}
      right={<Group gap="xs" wrap="nowrap">{asOf && !narrow && <Text size={FS.micro} c="dimmed">as of {asOf.toLocaleTimeString()}</Text>}<RefreshButton busy={busy} onClick={() => void refresh()} /></Group>}
      blurb="Why this database is slow or heavy, and what to change: what the server waits on, the costly statements, indexes, memory and settings. Nothing is changed unless you run it.">
      {failure ? <Failed failure={failure} /> : !facts || !report ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Stack gap="md" p="md" maw={1100}>
            <HealthSummary facts={facts} report={report} />
            <FindingsList findings={report.findings} narrow={narrow} onOpenSql={open} countingS={report.countingS} engine="SQL Server" connLabel="Blocking" />
          </Stack>
          <Box px="md" py="sm">
            <Text size={FS.body} fw={600}>Waits, since {when(facts.performance.since)}</Text>
            <Table verticalSpacing={3} fz={12} mt={4}>
              <Table.Thead><Table.Tr><Table.Th>Wait</Table.Th><Table.Th>Share</Table.Th>{!narrow && <Table.Th>Total</Table.Th>}{!narrow && <Table.Th>Of it, waiting for CPU</Table.Th>}<Table.Th>Usually</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {facts.performance.waits.map((w) => (
                  <Table.Tr key={w.wait}>
                    <Table.Td style={mono}>{w.wait}</Table.Td>
                    <Table.Td style={{ whiteSpace: 'nowrap' }}>{w.percent.toFixed(1)}%</Table.Td>
                    {!narrow && <Table.Td style={{ whiteSpace: 'nowrap' }}>{ms(w.waitMs)}</Table.Td>}
                    {!narrow && <Table.Td style={{ whiteSpace: 'nowrap' }}>{w.waitMs ? `${Math.round(100 * w.signalMs / w.waitMs)}%` : ''}</Table.Td>}
                    <Table.Td><Text size={FS.micro} c="dimmed">{WAIT_WORDS[w.wait] ?? ''}</Text></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Box>
          <Box px="md" py="sm">
            <Group justify="space-between" wrap="wrap">
              <Text size={FS.body} fw={600}>This database’s costly statements</Text>
              <SegmentedControl size="xs" value={sort} onChange={setSort}
                data={[{ value: 'cpu', label: 'CPU' }, { value: 'duration', label: 'Time' }, { value: 'reads', label: 'Reads' }, { value: 'executions', label: 'Runs' }]} />
            </Group>
            {!perf ? <Loader size={14} mt="xs" /> : (
              <>
                <Table verticalSpacing={3} fz={12} mt={4} striped>
                  <Table.Thead><Table.Tr><Table.Th>Statement</Table.Th><Table.Th>Runs</Table.Th><Table.Th>CPU, all</Table.Th>{!narrow && <Table.Th>Each: CPU</Table.Th>}{!narrow && <Table.Th>time</Table.Th>}{!narrow && <Table.Th>reads</Table.Th>}</Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {perf.queries.map((q, i) => (
                      <Table.Tr key={i}>
                        <Table.Td style={{ ...mono, maxWidth: narrow ? 180 : 520, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={q.statement}>{q.statement}</Table.Td>
                        <Table.Td>{count(q.executions)}</Table.Td>
                        <Table.Td style={{ whiteSpace: 'nowrap' }}>{ms(q.cpuMs) || '0 ms'}</Table.Td>
                        {!narrow && <Table.Td style={{ whiteSpace: 'nowrap' }}>{ms(q.avgCpuMs) || '<1 ms'}</Table.Td>}
                        {!narrow && <Table.Td style={{ whiteSpace: 'nowrap' }}>{ms(q.avgMs) || '<1 ms'}</Table.Td>}
                        {!narrow && <Table.Td>{count(q.avgReads)}</Table.Td>}
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
                {perf.queries.length === 0 && <Text size={FS.meta} c="dimmed" mt="xs">No cached statements of this database yet.</Text>}
              </>
            )}
          </Box>
        </ScrollArea>
      )}
    </PanelFrame>
  )
}

function HealthSummary({ facts, report }: { facts: MSFacts; report: MSReport }) {
  const h = facts.health
  const high = report.findings.filter((f) => f.severity === 'high').length
  const medium = report.findings.filter((f) => f.severity === 'medium').length
  const data = facts.settings.files.filter((f) => f.type === 'rows').reduce((a, f) => a + f.sizeMb, 0)
  return (
    <Paper withBorder p="sm" radius="md">
      <Group gap="lg" wrap="wrap">
        <Stat label="Findings" value={high || medium ? `${high} high, ${medium} medium` : 'none serious'} tone={high ? INK.denied : medium ? INK.pending : INK.verified} />
        <Stat label="Page life" value={h.pageLife ? `${count(h.pageLife)} s` : '—'} />
        <Stat label="Memory in use" value={`${(h.sqlMemoryMb / 1024).toFixed(1)} of ${(h.memoryMb / 1024).toFixed(1)} GB`} />
        <Stat label="Waiting for CPU" value={`${(report.signalShare * 100).toFixed(0)}%`} tone={report.signalShare >= 0.25 ? INK.pending : undefined} />
        <Stat label="Blocked now" value={String(h.blocked)} tone={h.blocked ? INK.pending : undefined} />
        <Stat label="Data" value={`${(data / 1024).toFixed(data >= 10240 ? 0 : 1)} GB`} />
        <Stat label="Server" value={`${h.version} · ${h.edition.replace(/ \(64-bit\)/, '').replace(/ Edition$/, '')}`} />
      </Group>
      <Text size={FS.micro} c="dimmed" mt={8}>
        Counted over {duration(report.countingS)}, since SQL Server started.
        {h.missing?.length ? ` Not read: ${h.missing.join('; ')}.` : ''}
      </Text>
    </Paper>
  )
}

/* ── Indexes & statistics ────────────────────────────────────────────────── */

/** What to do about an index, by the usual rule: reorganize from 5%, rebuild from 30%, and only past 1,000 pages. */
export function indexAdvice(x: MSIndexRow): 'rebuild' | 'reorganize' | '' {
  if (!x.name || x.disabled || x.pages < 1000) return ''
  if (x.fragmentation >= 30) return 'rebuild'
  if (x.fragmentation >= 5) return 'reorganize'
  return ''
}

export function MSSQLIndexes({ admin, narrow, onBack }: AdminPanelProps) {
  const { data, failure, busy, refresh } = useRead<{ indexes: MSIndexRow[]; statistics: Statistic[]; missing: Missing[]; truncated?: boolean }>(admin, { kind: 'ms.indexes' })
  const [tab, setTab] = useState<string | null>('indexes')
  const [filter, setFilter] = useState('')
  const [pending, setPending] = useState<Pending | null>(null)
  const [online, setOnline] = useState(false)
  const [sample, setSample] = useState<string>('default')
  const q = filter.trim().toLowerCase()
  const hit = (s: string, t: string, n?: string) => !q || `${s}.${t}`.toLowerCase().includes(q) || (n ?? '').toLowerCase().includes(q)
  const upkeep = (kind: 'ms.index.rebuild' | 'ms.index.reorganize', x: { schema: string; table: string; name?: string }) => {
    const rebuild = kind === 'ms.index.rebuild'
    const what = x.name ? `the index ${x.name} on ${x.schema}.${x.table}` : `every index on ${x.schema}.${x.table}`
    setPending({
      op: { kind, msdba: { schema: x.schema, table: x.table, index: x.name, online: rebuild && online ? true : undefined } },
      title: `${rebuild ? 'Rebuild' : 'Reorganize'} ${x.name ?? 'indexes'}`, verb: rebuild ? 'Rebuild' : 'Reorganize',
      done: rebuild ? 'Rebuilt' : 'Reorganized',
      words: rebuild
        ? `Rebuilds ${what} from scratch and updates its statistics. ${online ? 'Online, the table stays usable (Enterprise and Developer editions).' : 'Offline, the table is locked while it runs.'}`
        : `Reorganizes ${what} in place, page by page. It stays usable throughout and can be stopped without losing what was done.`,
    })
  }
  const stats = (x: { schema: string; table: string; name?: string }) => setPending({
    op: { kind: 'ms.stats.update', msdba: { schema: x.schema, table: x.table, index: x.name, fullScan: sample === 'full' ? true : undefined } },
    title: `Update statistics on ${x.schema}.${x.table}`, verb: 'Update', done: 'Statistics updated',
    words: `${x.name ? `Updates ${x.name}` : 'Updates every statistic on the table'}, ${sample === 'full' ? 'reading every row' : 'from SQL Server’s default sample'}. Plans that used it compile again.`,
  })
  return (
    <PanelFrame title="Indexes & statistics" narrow={narrow} onBack={onBack} right={<RefreshButton busy={busy} onClick={() => void refresh()} />}
      blurb="Each index’s size, fragmentation and use since the server started; each statistic’s age and how much changed since; and the indexes SQL Server says it missed.">
      <Group gap="sm" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }} wrap="wrap">
        <TextInput size="xs" placeholder="Filter by table or name…" value={filter} onChange={(e) => setFilter(e.currentTarget.value)} w={narrow ? '100%' : 240} aria-label="Filter indexes" />
        {tab === 'indexes' && <Switch size="xs" label="Rebuild online" checked={online} onChange={(e) => setOnline(e.currentTarget.checked)} />}
        {tab === 'statistics' && <SegmentedControl size="xs" value={sample} onChange={setSample} data={[{ value: 'default', label: 'Default sample' }, { value: 'full', label: 'Every row' }]} />}
      </Group>
      {failure ? <Failed failure={failure} /> : !data ? <Loader size={16} m="md" /> : (
        <Tabs value={tab} onChange={setTab} style={{ flex: 1, display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <Tabs.List px="md">
            <Tabs.Tab value="indexes">Indexes</Tabs.Tab>
            <Tabs.Tab value="statistics">Statistics</Tabs.Tab>
            <Tabs.Tab value="missing">Missing{data.missing.length ? ` (${data.missing.length})` : ''}</Tabs.Tab>
          </Tabs.List>
          <ScrollArea style={{ flex: 1 }} type="auto">
            <Tabs.Panel value="indexes">
              <Table verticalSpacing={3} fz={12} striped highlightOnHover>
                <Table.Thead style={stickyHead}><Table.Tr>
                  <Table.Th>Table</Table.Th><Table.Th>Index</Table.Th><Table.Th>Size</Table.Th><Table.Th>Fragmented</Table.Th>
                  {!narrow && <Table.Th>Reads</Table.Th>}{!narrow && <Table.Th>Writes</Table.Th>}<Table.Th></Table.Th>
                </Table.Tr></Table.Thead>
                <Table.Tbody>
                  {data.indexes.filter((x) => hit(x.schema, x.table, x.name)).map((x) => {
                    const advice = indexAdvice(x)
                    const reads = x.seeks + x.scans + x.lookups
                    return (
                      <Table.Tr key={`${x.schema}.${x.table}.${x.name ?? ''}`}>
                        <Table.Td style={mono}>{x.schema}.{x.table}</Table.Td>
                        <Table.Td style={mono}>
                          {x.name || <Text span size={FS.micro} c="dimmed">heap</Text>}
                          {x.primary && <Badge size="xs" variant="light" ml={4}>primary key</Badge>}
                          {x.disabled && <Badge size="xs" variant="outline" color="amber" ml={4}>disabled</Badge>}
                          {!x.primary && x.name && reads === 0 && x.updates > 0 && <Tooltip label="Written to but never read since the server started"><Badge size="xs" variant="outline" color="slate" ml={4}>unused</Badge></Tooltip>}
                        </Table.Td>
                        <Table.Td style={{ whiteSpace: 'nowrap' }}>{x.pages >= 128 ? `${count(Math.round(x.pages / 128))} MB` : `${x.pages} pages`}</Table.Td>
                        <Table.Td style={{ whiteSpace: 'nowrap', color: advice === 'rebuild' ? INK.denied : advice ? INK.pending : undefined }}>{x.fragmentation.toFixed(0)}%</Table.Td>
                        {!narrow && <Table.Td>{count(reads)}</Table.Td>}
                        {!narrow && <Table.Td>{count(x.updates)}</Table.Td>}
                        <Table.Td style={{ whiteSpace: 'nowrap' }}>
                          {x.name && !x.disabled && <>
                            <Button size="compact-xs" variant={advice === 'reorganize' ? 'light' : 'subtle'} onClick={() => upkeep('ms.index.reorganize', x)}>Reorganize</Button>
                            <Button size="compact-xs" variant={advice === 'rebuild' ? 'light' : 'subtle'} onClick={() => upkeep('ms.index.rebuild', x)}>Rebuild</Button>
                          </>}
                          {x.disabled && <Button size="compact-xs" variant="subtle" onClick={() => upkeep('ms.index.rebuild', x)}>Rebuild to enable</Button>}
                        </Table.Td>
                      </Table.Tr>
                    )
                  })}
                </Table.Tbody>
              </Table>
              {data.truncated && <Text size={FS.micro} c="dimmed" p="md">The 500 largest are shown.</Text>}
            </Tabs.Panel>
            <Tabs.Panel value="statistics">
              <Table verticalSpacing={3} fz={12} striped highlightOnHover>
                <Table.Thead style={stickyHead}><Table.Tr>
                  <Table.Th>Table</Table.Th><Table.Th>Statistic</Table.Th><Table.Th>Updated</Table.Th><Table.Th>Rows</Table.Th>
                  {!narrow && <Table.Th>Sampled</Table.Th>}<Table.Th>Changed since</Table.Th><Table.Th></Table.Th>
                </Table.Tr></Table.Thead>
                <Table.Tbody>
                  {data.statistics.filter((x) => hit(x.schema, x.table, x.name)).map((x) => {
                    const stale = x.rows > 0 && x.modified > Math.max(500, x.rows * 0.2)
                    return (
                      <Table.Tr key={`${x.schema}.${x.table}.${x.name}`}>
                        <Table.Td style={mono}>{x.schema}.{x.table}</Table.Td>
                        <Table.Td style={mono}>{x.name}{x.auto && <Text span size={FS.micro} c="dimmed" ml={4}>auto</Text>}</Table.Td>
                        <Table.Td style={{ whiteSpace: 'nowrap' }}>{when(x.lastUpdated)}</Table.Td>
                        <Table.Td>{count(x.rows)}</Table.Td>
                        {!narrow && <Table.Td>{x.rows ? `${Math.round(100 * x.sampled / x.rows)}%` : ''}</Table.Td>}
                        <Table.Td style={{ color: stale ? INK.pending : undefined }}>{count(x.modified)}</Table.Td>
                        <Table.Td><Button size="compact-xs" variant={stale ? 'light' : 'subtle'} onClick={() => stats(x)}>Update</Button></Table.Td>
                      </Table.Tr>
                    )
                  })}
                </Table.Tbody>
              </Table>
            </Tabs.Panel>
            <Tabs.Panel value="missing">
              <Box p="md">
                <Text size={FS.meta} c="dimmed">What SQL Server’s optimizer wished it had while compiling plans, since the server started. Weigh each against the writes an index costs; copy the statement into the Query tab to make one.</Text>
                {data.missing.filter((x) => hit(x.schema, x.table)).map((x, i) => (
                  <Box key={i} mt="sm">
                    <Group gap="xs"><Text size={FS.meta} ff="monospace" fw={600}>{x.schema}.{x.table}</Text>
                      <Text size={FS.micro} c="dimmed">{count(x.seeks)} uses · {x.impact.toFixed(0)}% cheaper</Text></Group>
                    <Code block fz={12} mt={2} style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{x.create}</Code>
                  </Box>
                ))}
                {data.missing.length === 0 && <Text size={FS.meta} mt="sm">None.</Text>}
              </Box>
            </Tabs.Panel>
          </ScrollArea>
        </Tabs>
      )}
      <ConfirmOp admin={admin} pending={pending} onClose={() => setPending(null)} onDone={() => { setPending(null); void refresh() }} />
    </PanelFrame>
  )
}

/* ── Integrity (DBCC) ────────────────────────────────────────────────────── */

export function MSSQLIntegrity({ admin, narrow, onBack }: AdminPanelProps) {
  const { data, failure, busy, refresh } = useRead<{ lastGood?: string; pageVerify: string; suspect: { file: number; page: number; event: string; errors: number; when: string }[] }>(admin, { kind: 'ms.integrity' })
  const [pending, setPending] = useState<Pending | null>(null)
  const [physical, setPhysical] = useState(false)
  const [table, setTable] = useState('')
  const days = data?.lastGood ? Math.floor((Date.now() - new Date(data.lastGood).getTime()) / 86_400_000) : null
  const check = () => {
    const t = table.trim()
    const dot = t.indexOf('.')
    const one = dot > 0 ? { schema: t.slice(0, dot), table: t.slice(dot + 1) } : t ? { schema: 'dbo', table: t } : {}
    setPending({
      op: { kind: 'ms.checkdb', msdba: { ...one, physicalOnly: physical || undefined } },
      title: t ? `Check ${t}` : 'Check the database', verb: 'Check', done: 'No problems found',
      words: `${t ? 'DBCC CHECKTABLE reads the table' : 'DBCC CHECKDB reads the whole database'} and checks every page and how they fit together. It reads a lot and uses tempdb; on a big database run it out of hours${physical ? '' : ', or check pages only'}.`,
    })
  }
  return (
    <PanelFrame title="Integrity" narrow={narrow} onBack={onBack} right={<RefreshButton busy={busy} onClick={() => void refresh()} />}
      blurb="Whether SQL Server has found damage in this database: when DBCC CHECKDB last found it clean, and the pages it found bad while reading them.">
      {failure ? <Failed failure={failure} /> : !data ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Box p="md">
            <Text size={FS.body}>
              Last clean check: <b>{data.lastGood ? when(data.lastGood) : 'never'}</b>
              {days !== null && days > 7 && <Text span c={INK.pending}> ({days} days ago; weekly is the usual minimum)</Text>}
            </Text>
            <Text size={FS.meta} mt={4} c={data.pageVerify === 'CHECKSUM' ? undefined : INK.pending}>
              Pages are verified by {data.pageVerify.toLowerCase().replace(/_/g, ' ')}{data.pageVerify === 'CHECKSUM' ? '.' : '; CHECKSUM catches more damage (Settings).'}
            </Text>
            <Group mt="md" gap="sm" align="end" wrap="wrap">
              <TextInput size="xs" label="One table (optional)" placeholder="schema.table" value={table} onChange={(e) => setTable(e.currentTarget.value)} w={220} />
              <Switch size="xs" label="Pages only (faster)" checked={physical} onChange={(e) => setPhysical(e.currentTarget.checked)} />
              <Button size="xs" onClick={check}>Check…</Button>
            </Group>
            <Text size={FS.body} fw={600} mt="lg">Damaged pages</Text>
            {data.suspect.length === 0 ? <Text size={FS.meta} mt={4}>None recorded.</Text> : (
              <Table verticalSpacing={3} fz={12} mt={4}>
                <Table.Thead><Table.Tr><Table.Th>File</Table.Th><Table.Th>Page</Table.Th><Table.Th>What</Table.Th><Table.Th>Times</Table.Th><Table.Th>Last</Table.Th></Table.Tr></Table.Thead>
                <Table.Tbody>
                  {data.suspect.map((p) => (
                    <Table.Tr key={`${p.file}:${p.page}`} style={{ color: p.event === 'restored' || p.event === 'repaired' ? undefined : INK.denied }}>
                      <Table.Td>{p.file}</Table.Td><Table.Td>{p.page}</Table.Td><Table.Td>{p.event}</Table.Td><Table.Td>{p.errors}</Table.Td><Table.Td>{when(p.when)}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )}
            <Text size={FS.micro} c="dimmed" mt="lg">Repair options are not offered here: REPAIR_ALLOW_DATA_LOSS throws data away. Restore the damaged pages or the database from a backup instead.</Text>
          </Box>
        </ScrollArea>
      )}
      <ConfirmOp admin={admin} pending={pending} onClose={() => setPending(null)} onDone={() => { setPending(null); void refresh() }} />
    </PanelFrame>
  )
}

/* ── SQL Server Agent jobs ───────────────────────────────────────────────── */

const OUTCOME_COLOR: Record<string, string> = { succeeded: 'teal', failed: 'rose', retry: 'amber', cancelled: 'slate', 'in progress': 'sky' }

export function MSSQLJobs({ admin, narrow, onBack }: AdminPanelProps) {
  const [job, setJob] = useState<string | null>(null)
  const { data, failure, busy, refresh } = useRead<Jobs>(admin, { kind: 'ms.jobs', msdba: job ? { job } : undefined })
  const [pending, setPending] = useState<Pending | null>(null)
  const act = (j: Job, kind: 'ms.job.start' | 'ms.job.stop' | 'ms.job.enable') => {
    const enable = !j.enabled
    setPending({
      op: { kind, msdba: { job: j.name, enabled: kind === 'ms.job.enable' ? enable : undefined } },
      title: kind === 'ms.job.start' ? `Start ${j.name}` : kind === 'ms.job.stop' ? `Stop ${j.name}` : `${enable ? 'Enable' : 'Disable'} ${j.name}`,
      verb: kind === 'ms.job.start' ? 'Start' : kind === 'ms.job.stop' ? 'Stop' : enable ? 'Enable' : 'Disable',
      danger: kind === 'ms.job.stop', done: kind === 'ms.job.start' ? 'Started' : kind === 'ms.job.stop' ? 'Stopped' : enable ? 'Enabled' : 'Disabled',
      words: kind === 'ms.job.start' ? 'SQL Server Agent runs the job now, from its first step, besides its schedule.'
        : kind === 'ms.job.stop' ? 'SQL Server Agent stops the running step; what it changed in a finished transaction stays.'
          : enable ? 'Its schedules run it again.' : 'Its schedules stop running it until it is enabled; it can still be started by hand.',
    })
  }
  const one = job ? data?.jobs.find((j) => j.name === job) : undefined
  return (
    <PanelFrame title={job ?? 'Agent jobs'} narrow={narrow} onBack={job ? () => setJob(null) : onBack} right={<RefreshButton busy={busy} onClick={() => void refresh()} />}
      blurb="SQL Server Agent jobs with a step in this database: when they last ran and how it went, when they run next; start, stop, enable and disable them.">
      {data && data.agent && data.agent !== 'Running' && (
        <Box px="md" py={6} style={{ background: 'var(--mantine-color-amber-light)', color: 'var(--mantine-color-amber-light-color)' }}>
          <Text size={FS.meta}>SQL Server Agent is {data.agent.toLowerCase()}: no job runs, on its schedule or by hand, until it is started on the server.</Text>
        </Box>
      )}
      {failure ? <Failed failure={failure} /> : !data ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          {!job ? (
            <>
              <Table verticalSpacing={4} fz={12} striped highlightOnHover>
                <Table.Thead style={stickyHead}><Table.Tr>
                  <Table.Th>Job</Table.Th><Table.Th>Last run</Table.Th>{!narrow && <Table.Th>Took</Table.Th>}<Table.Th>Next</Table.Th>{!narrow && <Table.Th>Owner</Table.Th>}<Table.Th></Table.Th>
                </Table.Tr></Table.Thead>
                <Table.Tbody>
                  {data.jobs.map((j) => (
                    <Table.Tr key={j.name}>
                      <Table.Td>
                        <Text size={FS.meta} fw={600} c={INK.brand} style={{ cursor: 'pointer' }} role="button" tabIndex={0}
                          onClick={() => setJob(j.name)} onKeyDown={(e) => { if (e.key === 'Enter') setJob(j.name) }}>{j.name}</Text>
                        {!j.enabled && <Badge size="xs" variant="outline" color="amber" ml={4}>disabled</Badge>}
                        {j.running && <Badge size="xs" variant="light" color="sky" ml={4}>running</Badge>}
                      </Table.Td>
                      <Table.Td style={{ whiteSpace: 'nowrap' }}>
                        {j.lastOutcome && <Badge size="xs" variant="light" color={OUTCOME_COLOR[j.lastOutcome] ?? 'slate'} mr={4}>{j.lastOutcome}</Badge>}
                        {j.lastRun ? when(j.lastRun) : 'never'}
                      </Table.Td>
                      {!narrow && <Table.Td>{j.lastRun ? ms(j.lastSecs ? j.lastSecs * 1000 : 0) || 'under 1 s' : ''}</Table.Td>}
                      <Table.Td style={{ whiteSpace: 'nowrap' }}>{j.nextRun ? when(j.nextRun) : j.schedules ? '—' : 'no schedule'}</Table.Td>
                      {!narrow && <Table.Td>{j.owner}</Table.Td>}
                      <Table.Td style={{ whiteSpace: 'nowrap' }}>
                        {j.running
                          ? <Button size="compact-xs" variant="subtle" color="rose" onClick={() => act(j, 'ms.job.stop')}>Stop</Button>
                          : <Button size="compact-xs" variant="subtle" onClick={() => act(j, 'ms.job.start')}>Start</Button>}
                        <Button size="compact-xs" variant="subtle" color="slate" onClick={() => act(j, 'ms.job.enable')}>{j.enabled ? 'Disable' : 'Enable'}</Button>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
              {data.jobs.length === 0 && <Text size={FS.meta} c="dimmed" p="md">No SQL Server Agent job has a step in this database.</Text>}
            </>
          ) : (
            <Box p="md">
              {one && <Group gap="xs" mb="sm">
                {one.running
                  ? <Button size="xs" color="rose" variant="light" onClick={() => act(one, 'ms.job.stop')}>Stop</Button>
                  : <Button size="xs" onClick={() => act(one, 'ms.job.start')}>Start now</Button>}
                <Button size="xs" variant="default" onClick={() => act(one, 'ms.job.enable')}>{one.enabled ? 'Disable' : 'Enable'}</Button>
                {one.description && <Text size={FS.meta} c="dimmed">{one.description}</Text>}
              </Group>}
              <Text size={FS.body} fw={600}>Steps</Text>
              {(data.steps ?? []).map((s) => (
                <Box key={s.id} mt="xs">
                  <Text size={FS.meta}><b>{s.id}. {s.name}</b> <Text span c="dimmed">{s.subsystem}{s.database ? ` in ${s.database}` : ''}</Text></Text>
                  {s.command && <Code block fz={12} mt={2} style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word', maxHeight: 160, overflow: 'auto' }}>{s.command}</Code>}
                </Box>
              ))}
              <Text size={FS.body} fw={600} mt="lg">Recent runs</Text>
              <Table verticalSpacing={3} fz={12} mt={4}>
                <Table.Thead><Table.Tr><Table.Th>When</Table.Th><Table.Th>Step</Table.Th><Table.Th>Outcome</Table.Th>{!narrow && <Table.Th>Took</Table.Th>}<Table.Th>Message</Table.Th></Table.Tr></Table.Thead>
                <Table.Tbody>
                  {(data.history ?? []).map((h, i) => (
                    <Table.Tr key={i} style={{ fontWeight: h.step === 0 ? 600 : undefined }}>
                      <Table.Td style={{ whiteSpace: 'nowrap' }}>{when(h.when)}</Table.Td>
                      <Table.Td>{h.step === 0 ? 'the job' : `${h.step}. ${h.name}`}</Table.Td>
                      <Table.Td><Badge size="xs" variant="light" color={OUTCOME_COLOR[h.outcome] ?? 'slate'}>{h.outcome}</Badge></Table.Td>
                      {!narrow && <Table.Td>{ms(h.secs * 1000) || 'under 1 s'}</Table.Td>}
                      <Table.Td style={{ maxWidth: 420, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={h.message}>{h.message}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
              {(data.history ?? []).length === 0 && <Text size={FS.meta} c="dimmed" mt={4}>It has not run yet.</Text>}
            </Box>
          )}
        </ScrollArea>
      )}
      <ConfirmOp admin={admin} pending={pending} onClose={() => setPending(null)} onDone={() => { setPending(null); void refresh() }} />
    </PanelFrame>
  )
}

/* ── Database settings ───────────────────────────────────────────────────── */

/** The settings changed here, in words, with the values each takes; what to know before changing one. */
export const MS_SETTINGS: { name: string; label: string; values?: string[]; warn?: string }[] = [
  { name: 'RECOVERY', label: 'Recovery model', values: ['FULL', 'BULK_LOGGED', 'SIMPLE'], warn: 'Simple ends log backups, restoring to a point in time, log shipping and availability groups; it waits for approval on every database.' },
  { name: 'COMPATIBILITY_LEVEL', label: 'Compatibility level', values: ['100', '110', '120', '130', '140', '150', '160'], warn: 'Changes how queries are optimized; plans compile again.' },
  { name: 'PAGE_VERIFY', label: 'Page verify', values: ['CHECKSUM', 'TORN_PAGE_DETECTION', 'NONE'] },
  { name: 'AUTO_CLOSE', label: 'Auto close', values: ['OFF', 'ON'], warn: 'On closes the database when the last user leaves; the next one waits for it to open.' },
  { name: 'AUTO_SHRINK', label: 'Auto shrink', values: ['OFF', 'ON'], warn: 'On shrinks files in the background, fragmenting indexes; it is rarely wanted.' },
  { name: 'AUTO_CREATE_STATISTICS', label: 'Create statistics automatically', values: ['ON', 'OFF'] },
  { name: 'AUTO_UPDATE_STATISTICS', label: 'Update statistics automatically', values: ['ON', 'OFF'] },
  { name: 'AUTO_UPDATE_STATISTICS_ASYNC', label: '… without waiting for them', values: ['OFF', 'ON'] },
  { name: 'READ_COMMITTED_SNAPSHOT', label: 'Read committed snapshot', values: ['OFF', 'ON'], warn: 'Readers stop blocking writers, using row versions in tempdb. It needs every other connection out of the database to change.' },
  { name: 'ALLOW_SNAPSHOT_ISOLATION', label: 'Allow snapshot isolation', values: ['OFF', 'ON'] },
  { name: 'QUERY_STORE', label: 'Query Store', values: ['OFF', 'ON'] },
  { name: 'TARGET_RECOVERY_TIME', label: 'Target recovery time (seconds)' },
]

export function MSSQLSettings({ admin, narrow, onBack }: AdminPanelProps) {
  const { data, failure, busy, refresh } = useRead<Settings>(admin, { kind: 'ms.settings' })
  const [pending, setPending] = useState<Pending | null>(null)
  const [edits, setEdits] = useState<Record<string, string>>({})
  const current = (n: string) => data?.settings.find((s) => s.name === n)?.value ?? ''
  const change = (s: typeof MS_SETTINGS[number], value: string) => setPending({
    op: { kind: 'ms.db.set', msdba: { setting: s.name, value } },
    title: `${s.label}: ${value}`, verb: 'Change', danger: s.name === 'RECOVERY' && value === 'SIMPLE', done: `${s.label} is ${value}`,
    words: `From ${current(s.name)} to ${value}. ${s.warn ?? ''}`,
  })
  return (
    <PanelFrame title="Settings" narrow={narrow} onBack={onBack} right={<RefreshButton busy={busy} onClick={() => void refresh()} />}
      blurb="This database’s options, its files and how they grow, and the server options a DBA looks at first (to read; they are the server’s, not this database’s).">
      {failure ? <Failed failure={failure} /> : !data ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Box p="md" maw={860}>
            <Table verticalSpacing={4} fz={12}>
              <Table.Tbody>
                {MS_SETTINGS.map((s) => {
                  const now = current(s.name)
                  const v = edits[s.name] ?? now
                  return (
                    <Table.Tr key={s.name}>
                      <Table.Td w={narrow ? undefined : 280}>
                        <Text size={FS.meta} fw={600}>{s.label}</Text>
                        {s.warn && !narrow && <Text size={FS.micro} c="dimmed">{s.warn}</Text>}
                      </Table.Td>
                      <Table.Td w={200}>
                        {s.values
                          ? <Select size="xs" data={s.values} value={v} allowDeselect={false} aria-label={s.label}
                              onChange={(x) => x && setEdits((e) => ({ ...e, [s.name]: x }))} />
                          : <NumberInput size="xs" min={0} max={300} value={Number(v)} aria-label={s.label}
                              onChange={(x) => setEdits((e) => ({ ...e, [s.name]: String(x) }))} />}
                      </Table.Td>
                      <Table.Td>{v !== now && <Button size="compact-xs" onClick={() => change(s, v)}>Change…</Button>}</Table.Td>
                    </Table.Tr>
                  )
                })}
              </Table.Tbody>
            </Table>
            <Group gap="lg" mt="md" wrap="wrap">
              {data.facts.map((f) => (
                <Box key={f.name}><Text size={FS.micro} c="dimmed">{f.name}</Text><Text size={FS.meta}>{f.name === 'Created' ? when(f.value) : f.value}</Text></Box>
              ))}
            </Group>
            <Text size={FS.body} fw={600} mt="lg">Files</Text>
            <Table verticalSpacing={3} fz={12} mt={4}>
              <Table.Thead><Table.Tr><Table.Th>Name</Table.Th><Table.Th>Kind</Table.Th><Table.Th>Size</Table.Th><Table.Th>Used</Table.Th><Table.Th>Grows by</Table.Th>{!narrow && <Table.Th>Up to</Table.Th>}{!narrow && <Table.Th>Path</Table.Th>}</Table.Tr></Table.Thead>
              <Table.Tbody>
                {data.files.map((f) => (
                  <Table.Tr key={f.name}>
                    <Table.Td style={mono}>{f.name}</Table.Td><Table.Td>{f.type}</Table.Td><Table.Td>{count(f.sizeMb)} MB</Table.Td>
                    <Table.Td>{f.sizeMb ? `${Math.round(100 * f.usedMb / f.sizeMb)}%` : ''}</Table.Td>
                    <Table.Td style={{ color: f.growth.endsWith('%') ? INK.pending : undefined }}>{f.growth}</Table.Td>
                    {!narrow && <Table.Td>{f.maxMb < 0 ? 'the disk' : `${count(f.maxMb)} MB`}</Table.Td>}
                    {!narrow && <Table.Td style={{ ...mono, maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={f.path}>{f.path}</Table.Td>}
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
            <Text size={FS.body} fw={600} mt="lg">Server options</Text>
            <Table verticalSpacing={3} fz={12} mt={4}>
              <Table.Tbody>
                {data.server.map((c) => (
                  <Table.Tr key={c.name}>
                    <Table.Td><Text size={FS.meta}>{c.name}</Text>{!narrow && c.description && <Text size={FS.micro} c="dimmed">{c.description}</Text>}</Table.Td>
                    <Table.Td style={mono}>{c.value}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Box>
        </ScrollArea>
      )}
      <ConfirmOp admin={admin} pending={pending}
        onClose={() => setPending(null)} onDone={() => { setPending(null); setEdits({}); void refresh() }} />
    </PanelFrame>
  )
}
