import { useEffect, useState } from 'react'
import {
  ActionIcon, Alert, Anchor, Badge, Box, Button, Code, Group, Loader, Menu, Modal, NumberInput, Progress, Radio,
  ScrollArea, SegmentedControl, Stack, Switch, Table, Text, TextInput, Textarea, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconArrowLeft, IconCheck, IconDotsVertical, IconDownload, IconInfoCircle, IconLock, IconPin, IconPinnedOff,
  IconFlask, IconPlus, IconRefresh, IconRestore, IconShieldCheck, IconTrash, IconX,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { absTime, bytes, relTime } from '~/components/primitives'
import { backupDownloadUrl } from '~/lib/live'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  backupCatalogQuery, backupTestQuery, databaseBackupsQuery, useCancelBackup, useCancelBackupTest, useCancelRestore, useDeleteBackup,
  usePinBackup, useRestoreBackup, useSetBackupSchedule, useStartBackup, useTestBackup, useTryBackupChecks, useUndoRestore,
} from '~/lib/queries'
import { FS, SP, INK } from '~/theme'
import type {
  BackupDestination, BackupSchedule, BackupTest, BackupsView, CheckResult, DBLevel, Database, DatabaseBackup, DatabaseRestore, TestCheck,
} from '~/types/domain'

/*
Backups of one database. The agent on the database's server takes them with
pg_dump as the local admin account, encrypts them before they touch a disk,
and keeps them on the server, in the control plane's storage, or both. The
console only ever sees the control plane's record: state, size, where the
file is, and whether pg_restore could read it back.

What a person may do follows their level on the database: anyone who can
open it sees the list; Owner and above back it up and restore into a new
database; DBA deletes, pins, downloads and replaces, and a replace waits for
an approver and a typed name on top of that.
*/

const RANK: Record<DBLevel, number> = { read: 1, write: 2, owner: 3, dba: 4 }
const atLeast = (level: DBLevel | '', min: DBLevel) => Boolean(level) && RANK[level as DBLevel] >= RANK[min]

/** Who or what started a backup, in words. */
function startedBy(b: DatabaseBackup): string {
  return b.startedBy.startsWith('schedule:') ? 'Scheduled' : b.startedBy
}

function whereIs(b: DatabaseBackup): string {
  if (b.onServer && b.onControl) return 'server + control plane'
  if (b.onServer) return 'server'
  if (b.onControl) return 'control plane'
  return '—'
}

function stageWords(stage: string, detail: string): string {
  const s: Record<string, string> = {
    starting: 'starting pg_dump', schema: 'dumping the schema', data: 'dumping', verifying: 'reading it back with pg_restore',
    uploading: 'sending to the control plane', restoring: 'restoring', preparing: 'preparing', finishing: 'finishing',
    creating: 'creating the database', renaming: 'renaming the current database', downloading: 'fetching the backup',
    room: 'checking free space', fetching: 'fetching the backup', checking: 'checking the copy', counting: 'counting rows',
    dropping: 'dropping the copy', queued: 'waiting for the server',
  }
  const w = s[stage] ?? stage
  return detail ? `${w} ${detail}` : w
}

function StateBadge({ state }: { state: DatabaseBackup['state'] | DatabaseRestore['state'] }) {
  const color: Record<string, string> = {
    running: 'azure', done: 'teal', failed: 'rose', skipped: 'slate', missed: 'amber', deleted: 'slate',
    pending: 'amber', denied: 'rose', undone: 'slate',
  }
  const word: Record<string, string> = { pending: 'waiting for approval', undone: 'put back' }
  return <Badge size="xs" variant="light" color={color[state] ?? 'slate'} radius="sm">{word[state] ?? state}</Badge>
}

/* ── The panel in the workspace ─────────────────────────────────────────── */

export function BackupsPanel({ database, level, narrow, onBack, admin }: {
  database: Database
  level: DBLevel | ''
  narrow: boolean
  onBack: () => void
  /** The person is an admin, so the schedule link goes somewhere for them. */
  admin: boolean
}) {
  const { data, error, isPending, refetch, isFetching } = useQuery(databaseBackupsQuery(database.id))
  const cancel = useCancelBackup()
  const [starting, startNow] = useDisclosure(false)
  const [restoring, setRestoring] = useState<DatabaseBackup | null>(null)
  const [report, setReport] = useState<string | null>(null)
  const canBackup = atLeast(level, 'owner')
  const dba = level === 'dba'

  const onCancel = async (b: DatabaseBackup) => {
    try {
      await cancel.mutateAsync({ id: database.id, backupId: b.id })
      notifyOk('Stopping', 'pg_dump is being stopped; the partial file is removed.')
    } catch (err) {
      notifyError('Not stopped', err)
    }
  }

  return (
    <Box style={{ display: 'flex', flexDirection: 'column', minHeight: 0, flex: 1 }}>
      <Group gap="xs" px="md" py={SP.cozy} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <ActionIcon variant="subtle" onClick={onBack} aria-label="Back to the query editor"><IconArrowLeft size={16} /></ActionIcon>
        <Box style={{ minWidth: 0, flex: 1 }}>
          <Text fw={650} size={FS.body}>Backups</Text>
          {!narrow && <Text size={FS.micro} c="dimmed" lineClamp={1}>Encrypted before they touch a disk. Only the control plane can open them.</Text>}
        </Box>
        <Tooltip label="Refresh" withArrow>
          <ActionIcon variant="subtle" onClick={() => void refetch()} loading={isFetching} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>
        </Tooltip>
        {canBackup && (
          <Button size="xs" onClick={startNow.open} disabled={Boolean(data?.running) || !database.tunnelConnected}
            title={!database.tunnelConnected ? 'The agent is offline' : undefined}>
            Back up now
          </Button>
        )}
      </Group>
      <ScrollArea style={{ flex: 1 }} type="auto">
        <Stack gap="md" p="md">
          {error && <Alert color="rose" variant="light">{(error as Error).message}</Alert>}
          {isPending && <Loader size="sm" />}
          {data?.running && <RunningCard b={data.running} view={data} onCancel={canBackup ? () => void onCancel(data.running!) : undefined} />}
          {data?.testing && <TestingCard t={data.testing} database={database} dba={dba} onOpen={() => setReport(data.testing!.id)} />}
          {data?.restores.filter((r) => r.state === 'running' || r.state === 'pending').map((r) => (
            <RestoreCard key={r.id} r={r} database={database} dba={dba} />
          ))}
          {data && <ScheduleLine s={data.schedule} database={database} admin={admin} storage={data.storage} />}
          {data && data.restores.some((r) => r.state === 'done' && r.renamedTo && !r.keptDropped) && (
            <KeptCopies restores={data.restores} database={database} dba={dba} />
          )}
          {data && <LastGoodCard view={data} onOpen={setReport} />}
          {data && <BackupTable view={data} database={database} level={level} onRestore={setRestoring} onReport={setReport} />}
          {data && (
            <Text size={FS.micro} c="dimmed">
              Retention keeps the last {data.schedule.keepLast}{data.schedule.keepDays ? ` and anything newer than ${data.schedule.keepDays} days` : ''}.
              Pinned backups are kept until you unpin them, and so is the newest one that passed a test restore. Deleting one is audited.
            </Text>
          )}
        </Stack>
      </ScrollArea>
      <StartModal database={database} opened={starting} onClose={startNow.close} view={data} />
      {restoring && (
        <RestoreModal database={database} b={restoring} level={level} opened onClose={() => setRestoring(null)} />
      )}
      {report && <TestReportModal database={database} testId={report} narrow={narrow} onClose={() => setReport(null)} />}
    </Box>
  )
}

/** A backup in progress. pg_dump has no percentage, so the bar is scaled to the previous run and says so. */
function RunningCard({ b, view, onCancel }: { b: DatabaseBackup; view: BackupsView; onCancel?: () => void }) {
  const last = view.backups.find((x) => x.state === 'done' && x.id !== b.id)
  const [, tick] = useState(0)
  useEffect(() => { const t = setInterval(() => tick((n) => n + 1), 1000); return () => clearInterval(t) }, [])
  const secs = Math.max(0, Math.round((Date.now() - Date.parse(b.startedAt)) / 1000))
  const stale = Date.now() - Date.parse(b.reportedAt) > view.staleAfterS * 1000
  const pct = last && last.bytes > 0 ? Math.min(99, Math.round((b.bytes / last.bytes) * 100)) : null
  const by = startedBy(b)
  return (
    <Box p="sm" style={{ border: '1px solid var(--mantine-color-azure-4)', borderRadius: 8, background: 'var(--mantine-color-azure-light)' }}>
      <Group gap="xs" wrap="nowrap">
        <span style={{ width: 7, height: 7, borderRadius: 4, background: 'var(--mantine-color-azure-6)' }} />
        <Text fw={600} size={FS.body}>Backing up</Text>
        <Text size={FS.micro} c="dimmed" lineClamp={1}>{by === 'Scheduled' ? 'scheduled run' : `started by ${by}`}{b.reason ? ` · ${b.reason}` : ''}</Text>
        <Text size={FS.micro} ff="monospace" ml="auto" style={{ whiteSpace: 'nowrap' }}>{bytes(b.bytes)} so far · {secs} s</Text>
        {onCancel && <Button size="compact-xs" variant="subtle" color="rose" onClick={onCancel}>Stop</Button>}
      </Group>
      <Progress value={pct ?? 100} animated={pct === null} size="sm" mt={6} color="azure" />
      <Group justify="space-between" mt={4}>
        <Text size={FS.micro} ff="monospace">{stageWords(b.stage, b.detail)}</Text>
        <Text size={FS.micro} c={stale ? 'amber' : 'dimmed'}>
          {stale ? `nothing reported for ${relTime(b.reportedAt)}` : pct !== null ? `Last backup was ${bytes(last!.bytes)}, so this is about ${pct} %` : 'first backup, so no estimate'}
        </Text>
      </Group>
    </Box>
  )
}

function RestoreCard({ r, database, dba }: { r: DatabaseRestore; database: Database; dba: boolean }) {
  const cancel = useCancelRestore()
  const onCancel = async () => {
    try {
      await cancel.mutateAsync({ id: database.id, restoreId: r.id })
    } catch (err) {
      notifyError('Not stopped', err)
    }
  }
  const pending = r.state === 'pending'
  return (
    <Box p="sm" style={{ border: `1px solid var(--mantine-color-${pending ? 'amber' : 'azure'}-4)`, borderRadius: 8, background: `var(--mantine-color-${pending ? 'amber' : 'azure'}-light)` }}>
      <Group gap="xs" wrap="nowrap">
        <Text fw={600} size={FS.body}>
          {r.mode === 'new' ? `Restoring into ${r.newName}` : `Replacing ${r.dbname}`}
        </Text>
        <StateBadge state={r.state} />
        <Text size={FS.micro} c="dimmed" lineClamp={1}>from the backup of {absTime(r.backupStartedAt)} · asked by {r.requestedBy}</Text>
        {r.state === 'running' && dba && (
          <Button size="compact-xs" variant="subtle" color="rose" ml="auto" onClick={() => void onCancel()} loading={cancel.isPending}>Stop</Button>
        )}
      </Group>
      {pending ? (
        <Text size={FS.micro} c="dimmed" mt={4}>
          An approver has it. Once they say yes, every session on {r.dbname} ends and the restore starts. <Anchor component={Link} to="/approvals" size={FS.micro}>Approvals</Anchor>
        </Text>
      ) : (
        <>
          <Progress value={100} animated size="sm" mt={6} color="azure" />
          <Text size={FS.micro} ff="monospace" mt={4}>{stageWords(r.stage, r.detail)}</Text>
        </>
      )}
    </Box>
  )
}

function ScheduleLine({ s, database, admin, storage }: { s: BackupSchedule; database: Database; admin: boolean; storage: boolean }) {
  const where = { server: 'on the server', control: 'in the control plane', both: 'on the server and the control plane' }[s.destination]
  return (
    <Group gap="xs" px="sm" py={SP.cozy} wrap="wrap" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
      <Badge size="xs" variant="light" color={s.enabled ? 'teal' : 'slate'} radius="sm">{s.enabled ? 'Scheduled' : 'Not scheduled'}</Badge>
      {s.enabled ? (
        <Text size={FS.meta}>
          <b>{describeSchedule(s)}</b> · keep the last {s.keepLast} · {where}
          {s.testEvery !== 'off' && <> · <b>{describeTests(s)}</b></>}
        </Text>
      ) : (
        <Text size={FS.meta} c="dimmed">Nothing runs on its own{storage ? '' : '; the control plane has no storage, so backups can only stay on the server'}.</Text>
      )}
      {s.enabled && s.nextRunAt && <Text size={FS.micro} c="dimmed">Next: {absTime(s.nextRunAt)}</Text>}
      {s.enabled && s.testEvery === 'off' && (
        <Tooltip label="A backup that pg_restore can read may still not restore. A test restore proves it on this server, then drops the copy." withArrow multiline w={300}>
          <Badge size="xs" color="amber" variant="light" leftSection={<IconFlask size={10} />}>never test-restored on a schedule</Badge>
        </Tooltip>
      )}
      {s.misses >= 3 && (
        <Tooltip label={`The last ${s.misses} scheduled runs were missed or failed. Admins were alerted.`} withArrow>
          <Badge size="xs" color="rose" variant="light" leftSection={<IconAlertTriangle size={10} />}>{s.misses} in a row did not finish</Badge>
        </Tooltip>
      )}
      {admin && (
        <Link to="/databases/$dbId/settings" params={{ dbId: database.id }} search={{ tab: 'backups' }}
          style={{ marginLeft: 'auto', fontSize: 12, color: INK.brand }}>
          Change in Settings
        </Link>
      )}
    </Group>
  )
}

export function describeSchedule(s: BackupSchedule): string {
  const at = `${String(s.atHour).padStart(2, '0')}:${String(s.atMinute).padStart(2, '0')}`
  if (s.every === 'hourly') return `Every hour at :${String(s.atMinute).padStart(2, '0')}`
  if (s.every === 'weekly') return `Every ${['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'][s.weekday]} at ${at}`
  return `Every day at ${at}`
}

const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']

export function describeTests(s: BackupSchedule): string {
  const at = `${String(s.testHour).padStart(2, '0')}:${String(s.testMinute).padStart(2, '0')}`
  switch (s.testEvery) {
    case 'after': return 'test restore after every backup'
    case 'daily': return `test restore every day at ${at}`
    case 'weekly': return `test restore every ${DAYS[s.testWeekday]} at ${at}`
    default: return 'no test restores'
  }
}

/** A duration in words: 850 ms, 42 s, 4 min 12 s, 1 h 3 min. */
export function dur(ms: number): string {
  if (ms < 1000) return `${ms} ms`
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.floor(s / 60)} min${s % 60 ? ` ${s % 60} s` : ''}`
  return `${Math.floor(s / 3600)} h${Math.floor((s % 3600) / 60) ? ` ${Math.floor((s % 3600) / 60)} min` : ''}`
}

const TEST_COLOR: Record<BackupTest['state'], string> = { running: 'azure', passed: 'teal', warned: 'amber', failed: 'rose', skipped: 'slate' }
const TEST_WORD: Record<BackupTest['state'], string> = { running: 'testing', passed: 'passed', warned: 'passed with warnings', failed: 'failed', skipped: 'skipped' }

/** A test restore in progress. */
function TestingCard({ t, database, dba, onOpen }: { t: BackupTest; database: Database; dba: boolean; onOpen: () => void }) {
  const cancel = useCancelBackupTest()
  const [, tick] = useState(0)
  useEffect(() => { const x = setInterval(() => tick((n) => n + 1), 1000); return () => clearInterval(x) }, [])
  const secs = Math.max(0, Math.round((Date.now() - Date.parse(t.startedAt)) / 1000))
  const onCancel = async () => {
    try {
      await cancel.mutateAsync({ id: database.id, testId: t.id })
      notifyOk('Stopping', 'The test restore is being stopped; its copy is dropped.')
    } catch (err) {
      notifyError('Not stopped', err)
    }
  }
  return (
    <Box p="sm" style={{ border: '1px solid var(--mantine-color-azure-4)', borderRadius: 8, background: 'var(--mantine-color-azure-light)' }}>
      <Group gap="xs" wrap="nowrap">
        <IconFlask size={14} style={{ color: 'var(--mantine-color-azure-6)', flexShrink: 0 }} />
        <Text fw={600} size={FS.body}>Test-restoring</Text>
        <Text size={FS.micro} c="dimmed" lineClamp={1}>
          the backup of {absTime(t.backupStartedAt)} into <Code>{t.scratchName || 'a throwaway copy'}</Code> on {database.hostname}
        </Text>
        <Text size={FS.micro} ff="monospace" ml="auto" style={{ whiteSpace: 'nowrap' }}>{dur(secs * 1000)}</Text>
        <Button size="compact-xs" variant="subtle" onClick={onOpen}>Details</Button>
        {dba && <Button size="compact-xs" variant="subtle" color="rose" onClick={() => void onCancel()} loading={cancel.isPending}>Stop</Button>}
      </Group>
      <Progress value={100} animated size="sm" mt={6} color="azure" />
      <Group justify="space-between" mt={4}>
        <Text size={FS.micro} ff="monospace">{stageWords(t.stage, t.detail)}</Text>
        <Text size={FS.micro} c="dimmed">Runs at low priority. A backup that needs the server stops it, and it is tried again later.</Text>
      </Group>
    </Box>
  )
}

/** The newest backup proven to restore, and how long that took. */
function LastGoodCard({ view, onOpen }: { view: BackupsView; onOpen: (id: string) => void }) {
  const g = view.lastGood
  const lastTest = view.tests.find((t) => t.state !== 'running')
  if (!g) {
    if (!lastTest && view.schedule.testEvery === 'off') return null
    return (
      <Group gap="xs" px="sm" py={SP.cozy} wrap="wrap" style={{ border: '1px solid var(--mantine-color-amber-4)', borderRadius: 8 }}>
        <IconAlertTriangle size={14} style={{ color: 'var(--mantine-color-amber-6)' }} />
        <Text size={FS.meta}>No backup of this database has passed a test restore yet.</Text>
        {lastTest && (
          <Anchor size={FS.meta} ml="auto" onClick={() => onOpen(lastTest.id)}>Last test: {TEST_WORD[lastTest.state]} {relTime(lastTest.startedAt)}</Anchor>
        )}
      </Group>
    )
  }
  const failedSince = lastTest && lastTest.id !== g.id && lastTest.state === 'failed' ? lastTest : null
  return (
    <Box px="sm" py={SP.cozy} style={{ border: `1px solid var(--mantine-color-${failedSince ? 'rose' : 'teal'}-4)`, borderRadius: 8 }}>
      <Group gap="xs" wrap="wrap">
        <IconShieldCheck size={16} style={{ color: 'var(--mantine-color-teal-6)' }} />
        <Text size={FS.meta}>
          <b>Last known good:</b> the backup of {absTime(g.backupStartedAt)}, restored in <b>{dur(g.restoreMs)}</b> {relTime(g.finishedAt ?? g.startedAt)}
          {' · '}{g.tables} tables, {g.totalRows.toLocaleString()} rows, {bytes(g.bytes)}
        </Text>
        <Anchor size={FS.meta} ml="auto" onClick={() => onOpen(g.id)}>Report</Anchor>
      </Group>
      {failedSince && (
        <Text size={FS.micro} c="rose" mt={4}>
          The test of {absTime(failedSince.backupStartedAt)} failed {relTime(failedSince.startedAt)}: {failedSince.error || 'see its report'}.{' '}
          <Anchor size={FS.micro} onClick={() => onOpen(failedSince.id)}>Report</Anchor>
        </Text>
      )}
      <Text size={FS.micro} c="dimmed" mt={2}>Retention keeps this backup until a newer one passes.</Text>
    </Box>
  )
}

/** The Tested cell of one backup. */
function TestedCell({ b, test, good, onReport }: { b: DatabaseBackup; test?: BackupTest; good: boolean; onReport: (id: string) => void }) {
  if (b.state !== 'done') return <Text size={FS.meta} c="dimmed">—</Text>
  const readable = b.verified
    ? <Group gap={4} wrap="nowrap"><IconCheck size={12} style={{ color: 'var(--mantine-color-teal-6)' }} /><Text size={FS.meta} c="dimmed">readable</Text></Group>
    : <Text size={FS.meta} c="amber">not checked</Text>
  if (!test) return readable
  let words: string
  if (test.state === 'running') words = 'testing…'
  else if (test.state === 'passed') words = `restored in ${dur(test.restoreMs)}`
  else if (test.state === 'warned') words = `restored in ${dur(test.restoreMs)}, warnings`
  else if (test.state === 'failed') words = 'test failed'
  else words = 'test skipped'
  return (
    <Stack gap={2}>
      <Anchor size={FS.meta} c={TEST_COLOR[test.state]} onClick={() => onReport(test.id)} style={{ whiteSpace: 'nowrap' }}>{words}</Anchor>
      {good && <Badge size="xs" variant="light" color="teal" radius="sm" leftSection={<IconShieldCheck size={10} />}>last known good</Badge>}
      {test.state === 'skipped' && readable}
    </Stack>
  )
}

/** One test restore: what ran, each check, and the rows of each table. */
function TestReportModal({ database, testId, narrow, onClose }: { database: Database; testId: string; narrow: boolean; onClose: () => void }) {
  const { data: t, error } = useQuery(backupTestQuery(database.id, testId))
  const r = t?.report
  const icon = (state: 'ok' | 'warn' | 'fail' | boolean) => {
    const ok = state === true || state === 'ok'
    const c = ok ? 'teal' : state === 'warn' ? 'amber' : 'rose'
    return ok ? <IconCheck size={13} style={{ color: `var(--mantine-color-${c}-6)` }} />
      : state === 'warn' ? <IconAlertTriangle size={13} style={{ color: `var(--mantine-color-${c}-6)` }} />
        : <IconX size={13} style={{ color: `var(--mantine-color-${c}-6)` }} />
  }
  return (
    <Modal opened onClose={onClose} centered size="lg" fullScreen={narrow}
      title={t ? <Group gap="xs"><Text fw={650}>Test restore</Text><Badge size="sm" variant="light" color={TEST_COLOR[t.state]} radius="sm">{TEST_WORD[t.state]}</Badge></Group> : 'Test restore'}>
      {error && <Alert color="rose" variant="light">{(error as Error).message}</Alert>}
      {!t && !error && <Loader size="sm" />}
      {t && r && (
        <Stack gap="sm">
          <Text size={FS.meta} c="dimmed">
            The backup of <b>{absTime(t.backupStartedAt)}</b>, restored on {database.hostname} into <Code>{t.scratchName || '—'}</Code>
            {' · '}{startedBy({ startedBy: t.startedBy } as DatabaseBackup) === 'Scheduled' ? 'scheduled' : `started by ${t.startedBy}`} {relTime(t.startedAt)}
          </Text>
          {t.error && <Alert color={t.state === 'skipped' ? 'slate' : 'rose'} variant="light" p="xs">{t.error}</Alert>}
          {t.state !== 'running' && t.state !== 'skipped' && (
            <Group gap="lg" wrap="wrap">
              <Stat label="Restore took" value={dur(t.restoreMs)} />
              <Stat label="Tables" value={String(t.tables)} />
              <Stat label="Rows" value={t.totalRows.toLocaleString()} />
              <Stat label="Size" value={bytes(t.bytes)} />
            </Group>
          )}
          {(r.checks ?? []).length > 0 && (
            <Box>
              <Text size={FS.micro} c="dimmed" mb={4}>Checks</Text>
              <Table verticalSpacing={4} fz={FS.meta}>
                <Table.Tbody>
                  {(r.checks ?? []).map((c, i) => (
                    <Table.Tr key={i}>
                      <Table.Td w={22}>{icon(c.state)}</Table.Td>
                      <Table.Td>{c.name}{c.kind === 'custom' && <Badge size="xs" variant="outline" color="slate" ml={6}>yours</Badge>}</Table.Td>
                      <Table.Td c="dimmed" ff="monospace" style={{ wordBreak: 'break-word' }}>{c.value}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Box>
          )}
          {(r.steps ?? []).length > 0 && (
            <Box>
              <Text size={FS.micro} c="dimmed" mb={4}>Steps</Text>
              <Table verticalSpacing={2} fz={FS.meta}>
                <Table.Tbody>
                  {(r.steps ?? []).map((st, i) => (
                    <Table.Tr key={i}>
                      <Table.Td w={22}>{icon(st.ok)}</Table.Td>
                      <Table.Td>{st.name}</Table.Td>
                      <Table.Td c="dimmed" style={{ wordBreak: 'break-word' }}>{st.detail}</Table.Td>
                      <Table.Td ff="monospace" ta="right" style={{ whiteSpace: 'nowrap' }}>{dur(st.ms)}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Box>
          )}
          {(r.tables ?? []).length > 0 && (
            <Box>
              <Text size={FS.micro} c="dimmed" mb={4}>
                Rows by table{r.tablesTotal > (r.tables ?? []).length ? `, the ${(r.tables ?? []).length} largest of ${r.tablesTotal}` : ''}
                {r.previousId ? ', against the last passing test' : ''}
              </Text>
              <ScrollArea.Autosize mah={260} type="auto">
                <Table verticalSpacing={2} fz={FS.meta} stickyHeader>
                  <Table.Thead><Table.Tr><Table.Th>Table</Table.Th><Table.Th ta="right">Rows</Table.Th>{r.previousId && <Table.Th ta="right">Before</Table.Th>}</Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {(r.tables ?? []).map((x) => {
                      const shrunk = x.previous !== undefined && x.rows < x.previous / 2
                      return (
                        <Table.Tr key={x.name}>
                          <Table.Td ff="monospace">{x.name}</Table.Td>
                          <Table.Td ff="monospace" ta="right" c={x.rows < 0 ? 'amber' : shrunk ? 'amber' : undefined}>{x.rows < 0 ? 'not counted' : x.rows.toLocaleString()}</Table.Td>
                          {r.previousId && <Table.Td ff="monospace" ta="right" c="dimmed">{x.previous === undefined ? '—' : x.previous.toLocaleString()}</Table.Td>}
                        </Table.Tr>
                      )
                    })}
                  </Table.Tbody>
                </Table>
              </ScrollArea.Autosize>
            </Box>
          )}
          <Text size={FS.micro} c="dimmed">
            {t.state === 'running' ? 'The copy is dropped when the test ends, whatever happens.'
              : t.scratchDropped || !t.scratchName ? 'The copy was dropped. Only these numbers are kept, never rows.'
                : `The copy ${t.scratchName} could not be dropped yet; Argus keeps trying.`}
          </Text>
        </Stack>
      )}
    </Modal>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <Box>
      <Text size={FS.micro} c="dimmed">{label}</Text>
      <Text fw={650} size={FS.body} ff="monospace">{value}</Text>
    </Box>
  )
}

/** The copies a replace kept, with the way back. */
function KeptCopies({ restores, database, dba }: { restores: DatabaseRestore[]; database: Database; dba: boolean }) {
  const undo = useUndoRestore()
  const [undoing, setUndoing] = useState<DatabaseRestore | null>(null)
  const [confirm, setConfirm] = useState('')
  const submit = async () => {
    if (!undoing) return
    try {
      await undo.mutateAsync({ id: database.id, restoreId: undoing.id, confirm })
      notifyOk('Put back', `${database.dbname} is the copy from before the restore again. The restored version is gone.`)
      setUndoing(null); setConfirm('')
    } catch (err) {
      notifyError('Not put back', err)
    }
  }
  return (
    <Box px="sm" py={SP.cozy} style={{ border: '1px solid var(--mantine-color-amber-4)', borderRadius: 8 }}>
      {restores.filter((r) => r.state === 'done' && r.renamedTo && !r.keptDropped).map((r) => (
        <Group key={r.id} gap="xs" wrap="wrap">
          <IconInfoCircle size={14} style={{ color: 'var(--mantine-color-amber-6)' }} />
          <Text size={FS.meta}>
            {database.dbname} was replaced {relTime(r.finishedAt ?? r.createdAt)} from the backup of {absTime(r.backupStartedAt)}.
            The previous database is kept as <Code>{r.renamedTo}</Code> until {absTime(r.keepUntil ?? null)}.
          </Text>
          {dba && <Button size="compact-xs" variant="default" ml="auto" onClick={() => setUndoing(r)}>Put it back</Button>}
        </Group>
      ))}
      <Modal opened={Boolean(undoing)} onClose={() => setUndoing(null)} title={`Put ${database.dbname} back`} centered>
        <Stack gap="sm">
          <Text size={FS.body}>
            The database as it was before the restore, <Code>{undoing?.renamedTo}</Code>, takes the name {database.dbname} again. Every open
            session ends. The restored version is dropped.
          </Text>
          <TextInput label={<>Type <Code>{database.dbname}</Code> to confirm</>} value={confirm} onChange={(e) => setConfirm(e.currentTarget.value)} ff="monospace" />
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setUndoing(null)}>Cancel</Button>
            <Button color="rose" onClick={() => void submit()} loading={undo.isPending} disabled={confirm !== database.dbname}>Put it back</Button>
          </Group>
        </Stack>
      </Modal>
    </Box>
  )
}

function BackupTable({ view, database, level, onRestore, onReport }: {
  view: BackupsView; database: Database; level: DBLevel | ''; onRestore: (b: DatabaseBackup) => void; onReport: (id: string) => void
}) {
  const pin = usePinBackup()
  const del = useDeleteBackup()
  const test = useTestBackup()
  const latestTest = new Map<string, BackupTest>()
  for (const t of view.tests ?? []) if (!latestTest.has(t.backupId)) latestTest.set(t.backupId, t)
  const onTest = async (b: DatabaseBackup) => {
    try {
      await test.mutateAsync({ id: database.id, backupId: b.id })
      notifyOk('Test-restoring', `The backup is being restored into a throwaway copy on ${database.hostname}, checked, and dropped.`)
    } catch (err) {
      notifyError('Not started', err)
    }
  }
  const dba = level === 'dba'
  const canRestore = atLeast(level, 'owner')
  const rows = view.backups.filter((b) => b.state !== 'running')
  const onDelete = async (b: DatabaseBackup) => {
    if (!window.confirm(`Delete the backup of ${absTime(b.startedAt)} (${bytes(b.bytes)}) everywhere it is kept? This is audited and cannot be undone.`)) return
    try {
      await del.mutateAsync({ id: database.id, backupId: b.id })
      notifyOk('Deleted', 'The file is gone from the server and the control plane.')
    } catch (err) {
      notifyError('Not deleted', err)
    }
  }
  const onPin = async (b: DatabaseBackup) => {
    try {
      await pin.mutateAsync({ id: database.id, backupId: b.id, pinned: !b.pinned })
    } catch (err) {
      notifyError(b.pinned ? 'Not unpinned' : 'Not pinned', err)
    }
  }
  if (rows.length === 0) {
    return (
      <Box p="md" ta="center" style={{ border: '1px dashed var(--color-line)', borderRadius: 8 }}>
        <Text size={FS.body} fw={600}>No backups yet</Text>
        <Text size={FS.meta} c="dimmed">
          {atLeast(level, 'owner') ? 'Back up now, or ask an admin to schedule it in Settings.' : 'Nothing has been backed up. An Owner can start one, or an admin can schedule it.'}
        </Text>
      </Box>
    )
  }
  return (
    <Table verticalSpacing="xs" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
      <Table.Thead><Table.Tr>
        <Table.Th>Backup</Table.Th><Table.Th>State</Table.Th><Table.Th>Size</Table.Th><Table.Th>Where</Table.Th><Table.Th>Tested</Table.Th><Table.Th></Table.Th>
      </Table.Tr></Table.Thead>
      <Table.Tbody>
        {rows.map((b) => (
          <Table.Tr key={b.id} className="argus-backup">
            <Table.Td>
              <Group gap={4} wrap="nowrap">
                <Text fw={600} size={FS.body}>{absTime(b.startedAt)}</Text>
                {b.pinned && <Tooltip label="Pinned: retention skips it" withArrow><IconPin size={12} /></Tooltip>}
              </Group>
              <Text size={FS.micro} c="dimmed" lineClamp={1}>{startedBy(b)}{b.reason ? ` · ${b.reason}` : ''}</Text>
              {b.error && <Text size={FS.micro} c="rose" lineClamp={2}>{b.error}</Text>}
            </Table.Td>
            <Table.Td><StateBadge state={b.state} /></Table.Td>
            <Table.Td><Text size={FS.meta}>{b.state === 'done' ? bytes(b.bytes) : '—'}</Text></Table.Td>
            <Table.Td><Text size={FS.meta} c="dimmed">{b.state === 'done' ? whereIs(b) : '—'}</Text></Table.Td>
            <Table.Td>
              <TestedCell b={b} test={latestTest.get(b.id)} good={view.lastGood?.backupId === b.id} onReport={onReport} />
            </Table.Td>
            <Table.Td>
              {b.state === 'done' && (
                <Group gap={4} justify="flex-end" wrap="nowrap">
                  {canRestore && <Button size="compact-xs" variant="default" leftSection={<IconRestore size={12} />} onClick={() => onRestore(b)}>Restore</Button>}
                  {dba && b.onControl && (
                    <Button size="compact-xs" variant="subtle" component="a" href={backupDownloadUrl(database.id, b.id)} leftSection={<IconDownload size={12} />}>Download</Button>
                  )}
                  {dba && (
                    <Menu position="bottom-end" withinPortal>
                      <Menu.Target><ActionIcon variant="subtle" size="sm" aria-label="More"><IconDotsVertical size={14} /></ActionIcon></Menu.Target>
                      <Menu.Dropdown>
                        <Menu.Item leftSection={<IconFlask size={14} />} disabled={Boolean(view.testing) || Boolean(view.running) || !database.tunnelConnected}
                          onClick={() => void onTest(b)}>
                          Test restore: restore a copy, check it, drop it
                        </Menu.Item>
                        <Menu.Item leftSection={b.pinned ? <IconPinnedOff size={14} /> : <IconPin size={14} />} onClick={() => void onPin(b)}>
                          {b.pinned ? 'Unpin' : 'Pin: keep it past retention'}
                        </Menu.Item>
                        <Menu.Item leftSection={<IconTrash size={14} />} color="rose" disabled={b.pinned} onClick={() => void onDelete(b)}>Delete</Menu.Item>
                      </Menu.Dropdown>
                    </Menu>
                  )}
                </Group>
              )}
            </Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  )
}

function StartModal({ database, opened, onClose, view }: { database: Database; opened: boolean; onClose: () => void; view?: BackupsView }) {
  const start = useStartBackup()
  const [reason, setReason] = useState('')
  const [dest, setDest] = useState<BackupDestination | ''>('')
  const dflt = view?.schedule.destination ?? 'server'
  const submit = async () => {
    try {
      await start.mutateAsync({ id: database.id, reason: reason.trim(), destination: (dest || dflt) as BackupDestination })
      notifyOk('Backing up', `pg_dump is running on ${database.hostname}. The list shows its progress.`)
      onClose(); setReason('')
    } catch (err) {
      notifyError('Not started', err)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title={`Back up ${database.dbname} now`} centered>
      <Stack gap="sm">
        <Text size={FS.meta} c="dimmed">
          pg_dump runs on {database.hostname} as the local admin account; the file is encrypted as it is written. Sessions keep running.
        </Text>
        <TextInput label="Reason" placeholder="before the price import" value={reason} onChange={(e) => setReason(e.currentTarget.value)} maxLength={200}
          description="Optional; shown next to the backup and in the audit log." />
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>Keep it</Text>
          <SegmentedControl value={dest || dflt} onChange={(v) => setDest(v as BackupDestination)}
            data={[{ label: 'On the server', value: 'server' }, { label: 'Control plane', value: 'control', disabled: !view?.storage }, { label: 'Both', value: 'both', disabled: !view?.storage }]} />
        </Box>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void submit()} loading={start.isPending}>Back up now</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── Restore ────────────────────────────────────────────────────────────── */

function suggestName(dbname: string): string {
  const d = new Date()
  const base = `${dbname}_${d.getFullYear()}_${String(d.getMonth() + 1).padStart(2, '0')}_${String(d.getDate()).padStart(2, '0')}`
  return base.toLowerCase().replace(/[^a-z0-9_]/g, '_').slice(0, 63)
}

const NAME = /^[a-z_][a-z0-9_]{0,62}$/

export function RestoreModal({ database, b, level, opened, onClose }: {
  database: Database; b: DatabaseBackup; level: DBLevel | ''; opened: boolean; onClose: () => void
}) {
  const restore = useRestoreBackup()
  const { data: cat } = useQuery({ ...backupCatalogQuery(database.id), enabled: opened })
  const dba = level === 'dba'
  const [mode, setMode] = useState<'new' | 'replace'>('new')
  const [name, setName] = useState(() => suggestName(database.dbname))
  const [confirm, setConfirm] = useState('')
  const [why, setWhy] = useState('')
  const nameOk = NAME.test(name) && name !== database.dbname
  const canSubmit = mode === 'new' ? nameOk : dba && confirm === database.dbname && why.trim().length >= 10
  const stamp = new Date().toISOString().slice(0, 16).replace(/[-:]/g, '').replace('T', 'T')
  const submit = async () => {
    try {
      const r = await restore.mutateAsync({ id: database.id, backupId: b.id, mode, newName: name, reason: why.trim(), confirm })
      if (mode === 'new') notifyOk('Restoring', `${name} is being created on ${database.hostname}. It is added to Argus with nobody assigned when it finishes.`)
      else notifyOk('Sent to approvers', `An approver has ${r.state === 'pending' ? '30 minutes to decide' : 'it'}. Nothing changes until they say yes.`)
      onClose()
    } catch (err) {
      notifyError('Not started', err)
    }
  }
  const compatible = cat ? cat.catalog.restoreVersion : null
  return (
    <Modal opened={opened} onClose={onClose} title={`Restore ${database.dbname}`} centered size="lg">
      <Stack gap="sm">
        <Text size={FS.meta} c="dimmed">
          From the backup of <b>{absTime(b.startedAt)}</b> · {bytes(b.bytes)} · {startedBy(b)}{b.reason ? ` · “${b.reason}”` : ''}
          {b.serverVersion ? ` · Postgres ${b.serverVersion}` : ''} · {b.verified ? 'verified' : 'not verified'}
        </Text>
        <Radio.Group value={mode} onChange={(v) => setMode(v as 'new' | 'replace')}>
          <Stack gap="xs">
            <Box p="sm" style={{ border: `1px solid ${mode === 'new' ? 'var(--mantine-color-azure-5)' : 'var(--color-line)'}`, borderRadius: 8 }}>
              <Radio value="new" label={<Text fw={600} size={FS.body}>Into a new database</Text>}
                description="Nothing that exists is touched. The new database is added to Argus with no one assigned until an admin says who." />
              <Group gap="xs" mt="xs" pl={28}>
                <Text size={FS.meta} c="dimmed">Name</Text>
                <TextInput value={name} onChange={(e) => setName(e.currentTarget.value.toLowerCase())} ff="monospace" w={260} disabled={mode !== 'new'}
                  error={mode === 'new' && !nameOk ? (name === database.dbname ? 'that is the existing name' : 'lower-case letters, digits and _') : undefined} aria-label="New database name" />
              </Group>
            </Box>
            <Box p="sm" style={{ border: `1px solid ${mode === 'replace' ? 'var(--mantine-color-azure-5)' : 'var(--color-line)'}`, borderRadius: 8, opacity: dba ? 1 : .6 }}>
              <Radio value="replace" disabled={!dba} label={<Group gap={6}><Text fw={600} size={FS.body}>Replace {database.dbname}</Text>{!dba && <Badge size="xs" variant="light" color="slate" leftSection={<IconLock size={10} />}>DBA</Badge>}</Group>}
                description={<>Every open session on {database.dbname} ends now. The current database is <b>renamed to {database.dbname}_before_restore_{stamp} and kept for 24 hours</b>, so a bad restore can be put back. Needs an approver.</>} />
              {mode === 'replace' && (
                <Stack gap="xs" mt="xs" pl={28}>
                  <TextInput label={<>Type <Code>{database.dbname}</Code> to confirm</>} value={confirm} onChange={(e) => setConfirm(e.currentTarget.value)} ff="monospace" w={260} />
                  <Textarea label="Why (approvers see this)" value={why} onChange={(e) => setWhy(e.currentTarget.value)} autosize minRows={2}
                    placeholder="Price import corrupted the catalog, going back to before the migration" />
                </Stack>
              )}
            </Box>
          </Stack>
        </Radio.Group>
        {compatible && (
          <Alert color="teal" variant="light" icon={<IconCheck size={14} />} p="xs">
            pg_restore {compatible} on {database.hostname} can read this backup{b.serverVersion ? ` (Postgres ${b.serverVersion})` : ''}.
          </Alert>
        )}
        {!database.tunnelConnected && <Alert color="rose" variant="light" p="xs">The agent on {database.hostname} is offline, so nothing can run there.</Alert>}
        <Group justify="space-between">
          <Group gap="xs">
            <Button color={mode === 'replace' ? 'rose' : undefined} onClick={() => void submit()} loading={restore.isPending} disabled={!canSubmit || !database.tunnelConnected}>
              {mode === 'new' ? 'Restore into a new database' : 'Ask an approver to restore'}
            </Button>
            <Button variant="default" onClick={onClose}>Cancel</Button>
          </Group>
          {mode === 'replace' && <Text size={FS.micro} c="dimmed">Approvers · 30 min to decide</Text>}
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── Settings: the schedule ─────────────────────────────────────────────── */

export function BackupSettings({ db }: { db: Database }) {
  const { data } = useQuery(databaseBackupsQuery(db.id))
  const { data: cat, error: catError } = useQuery(backupCatalogQuery(db.id))
  const set = useSetBackupSchedule()
  const [form, setForm] = useState<BackupSchedule | null>(null)
  const saved = data?.schedule
  const s = form ?? saved
  useEffect(() => { if (saved && !form) setForm(saved) }, [saved, form])
  if (!s || !data) return <Loader size="sm" />
  const patch = (p: Partial<BackupSchedule>) => setForm({ ...s, ...p })
  const dirty = JSON.stringify(pick(s)) !== JSON.stringify(pick(saved!))
  const last = data.backups.find((b) => b.state === 'done')
  const save = async () => {
    try {
      await set.mutateAsync({ id: db.id, ...pick(s) })
      setForm(null)
      notifyOk('Saved', s.enabled
        ? `${describeSchedule(s)}, keeping the last ${s.keepLast}${s.testEvery !== 'off' ? `; ${describeTests(s)}` : ''}.`
        : 'Nothing runs on its own. Backups on demand still work.')
    } catch (err) {
      notifyError('Not saved', err)
    }
  }
  const at = `${String(s.atHour).padStart(2, '0')}:${String(s.atMinute).padStart(2, '0')}`
  return (
    <Group align="flex-start" gap="md" wrap="wrap">
      <Stack gap="md" style={{ flex: 1, minWidth: 320 }}>
        <SectionCard title="Schedule" action={<Switch checked={s.enabled} onChange={(e) => patch({ enabled: e.currentTarget.checked })} aria-label="Scheduled backups" />}>
          <Group align="flex-end" gap="md">
            <Box>
              <Text size={FS.micro} c="dimmed" mb={4}>Every</Text>
              <SegmentedControl value={s.every} onChange={(v) => patch({ every: v as BackupSchedule['every'], ...(v === 'hourly' && s.testEvery === 'after' ? { testEvery: 'daily' as const } : {}) })}
                data={[{ label: 'hour', value: 'hourly' }, { label: 'day', value: 'daily' }, { label: 'week', value: 'weekly' }]} disabled={!s.enabled} />
            </Box>
            {s.every === 'weekly' && (
              <Box>
                <Text size={FS.micro} c="dimmed" mb={4}>On</Text>
                <SegmentedControl value={String(s.weekday)} onChange={(v) => patch({ weekday: Number(v) })} disabled={!s.enabled}
                  data={['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'].map((d, i) => ({ label: d, value: String(i) }))} />
              </Box>
            )}
            <TextInput label={s.every === 'hourly' ? 'At minute' : 'At'} w={110} ff="monospace" disabled={!s.enabled}
              value={s.every === 'hourly' ? String(s.atMinute).padStart(2, '0') : at}
              onChange={(e) => {
                const v = e.currentTarget.value
                if (s.every === 'hourly') { const m = Number(v); if (Number.isInteger(m) && m >= 0 && m < 60) patch({ atMinute: m }); return }
                const m = /^(\d{1,2}):(\d{2})$/.exec(v)
                if (m && Number(m[1]) < 24 && Number(m[2]) < 60) patch({ atHour: Number(m[1]), atMinute: Number(m[2]) })
              }} />
            <Text size={FS.micro} c="dimmed" pb={8}>control plane time</Text>
          </Group>
          <Text size={FS.micro} c="dimmed" mt="sm">
            {s.enabled && saved?.nextRunAt && !dirty ? `Next run ${absTime(saved.nextRunAt)}. ` : ''}
            A run missed while the agent is offline happens when it is back, if less than 6 hours late; three misses in a row are flagged on the list.
          </Text>
        </SectionCard>
        <TestRestoreSettings s={s} saved={saved!} dirty={dirty} db={db} patch={patch} />
        <SectionCard title="Keep">
          <Group align="flex-end" gap="md">
            <NumberInput label="Last" value={s.keepLast} onChange={(v) => patch({ keepLast: Number(v) || 1 })} min={1} max={1000} w={120} suffix=" runs" />
            <NumberInput label="And anything newer than" value={s.keepDays} onChange={(v) => patch({ keepDays: Number(v) || 0 })} min={0} max={3650} w={150} suffix=" days"
              description="0 keeps by count only" />
          </Group>
          <Text size={FS.micro} c="dimmed" mt="sm">
            Older backups are deleted after each successful run, each deletion audited. Pinned backups are never deleted. Manual backups count.
          </Text>
        </SectionCard>
        <SectionCard title="Where">
          <Radio.Group value={s.destination} onChange={(v) => patch({ destination: v as BackupDestination })}>
            <Stack gap="xs">
              <Radio value="server" label="On the server" description={`${cat?.catalog.dir ?? 'the agent’s backup directory'} on ${db.hostname}, root only. Fastest to restore; lost with the server.`} />
              <Radio value="control" label="Control plane storage" disabled={!data.storage}
                description={data.storage ? 'Streamed to the same storage as recordings. Survives the server; a restore streams it back.' : 'The control plane has no storage for backups: configure object storage or a recordings directory first.'} />
              <Radio value="both" label="Both" disabled={!data.storage} description="Written on the server first, then uploaded. The usual production choice." />
            </Stack>
          </Radio.Group>
        </SectionCard>
        <Group>
          <Button onClick={() => void save()} disabled={!dirty} loading={set.isPending}>Save</Button>
          <Button variant="default" onClick={() => setForm(null)} disabled={!dirty}>Discard</Button>
        </Group>
      </Stack>
      <Stack gap="md" w={{ base: '100%', md: 320 }}>
        <SectionCard title="Encryption">
          <Text size={FS.meta} c="dimmed">
            Every backup is encrypted on the server before it is written, to this organization's backup key. The server and the storage hold only
            ciphertext; the key is derived from the control plane's secret and is nowhere else.
          </Text>
          {cat?.keyId && <Text size={FS.micro} ff="monospace" mt="xs">key {cat.keyId} · XChaCha20-Poly1305</Text>}
          <Text size={FS.micro} c="dimmed" mt="xs">To open a backup without Argus: <Code>argus-verify --secret … file.argusdump</Code>. See docs/lockout.md.</Text>
        </SectionCard>
        <SectionCard title={`On ${db.hostname}`}>
          {catError && <Text size={FS.meta} c="rose">{(catError as Error).message}</Text>}
          {cat && (
            <Table verticalSpacing={2} fz={FS.meta}>
              <Table.Tbody>
                <Table.Tr><Table.Td c="dimmed">pg_dump</Table.Td><Table.Td ff="monospace">{cat.catalog.dumpVersion} · {cat.catalog.compression}</Table.Td></Table.Tr>
                <Table.Tr><Table.Td c="dimmed">Postgres</Table.Td><Table.Td ff="monospace">{cat.catalog.serverVersion}</Table.Td></Table.Tr>
                <Table.Tr><Table.Td c="dimmed">Free space</Table.Td><Table.Td ff="monospace">{bytes(cat.catalog.freeBytes)} under {cat.catalog.dir}</Table.Td></Table.Tr>
                {last && <Table.Tr><Table.Td c="dimmed">Last backup</Table.Td><Table.Td ff="monospace">{bytes(last.bytes)}{last.finishedAt ? ` · ${Math.round((Date.parse(last.finishedAt) - Date.parse(last.startedAt)) / 1000)} s` : ''}</Table.Td></Table.Tr>}
                <Table.Tr><Table.Td c="dimmed">Runs as</Table.Td><Table.Td ff="monospace">{db.adminUser} · peer auth</Table.Td></Table.Tr>
              </Table.Tbody>
            </Table>
          )}
          {!cat && !catError && <Loader size="xs" />}
          <Text size={FS.micro} c="dimmed" mt="xs">A dump refuses to start with less free space than the last size plus 20 %.</Text>
        </SectionCard>
      </Stack>
    </Group>
  )
}

function pick(s: BackupSchedule) {
  return {
    enabled: s.enabled, every: s.every, atHour: s.atHour, atMinute: s.atMinute, weekday: s.weekday, destination: s.destination, keepLast: s.keepLast, keepDays: s.keepDays,
    testEvery: s.testEvery, testHour: s.testHour, testMinute: s.testMinute, testWeekday: s.testWeekday,
    testChecks: (s.testChecks ?? []).map((c) => ({ name: c.name, sql: c.sql })),
  }
}

const MAX_CHECKS = 20

/** When to test-restore, and the admin's own checks on the restored copy. */
function TestRestoreSettings({ s, saved, dirty, db, patch }: {
  s: BackupSchedule; saved: BackupSchedule; dirty: boolean; db: Database; patch: (p: Partial<BackupSchedule>) => void
}) {
  const tryChecks = useTryBackupChecks()
  const [tried, setTried] = useState<CheckResult[] | null>(null)
  const checks = s.testChecks ?? []
  const setChecks = (c: TestCheck[]) => { setTried(null); patch({ testChecks: c }) }
  const off = s.testEvery === 'off'
  const at = `${String(s.testHour).padStart(2, '0')}:${String(s.testMinute).padStart(2, '0')}`
  const onTry = async () => {
    try {
      const r = await tryChecks.mutateAsync({ id: db.id, checks: checks.filter((c) => c.sql.trim()) })
      setTried(r.checks)
    } catch (err) {
      notifyError('Checks not run', err)
    }
  }
  return (
    <SectionCard title="Test restores">
      <Text size={FS.meta} c="dimmed" mb="sm">
        Restores a backup into a throwaway database on {db.hostname}, counts every table, checks the indexes and your checks below, times it, and
        drops it. Runs at low priority, one at a time, and only if the server has room. A failure alerts admins.
      </Text>
      <Group align="flex-end" gap="md">
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>Test</Text>
          <SegmentedControl value={s.testEvery} onChange={(v) => patch({ testEvery: v as BackupSchedule['testEvery'] })}
            data={[
              { label: 'off', value: 'off' },
              { label: 'after every backup', value: 'after', disabled: s.every === 'hourly' },
              { label: 'daily', value: 'daily' },
              { label: 'weekly', value: 'weekly' },
            ]} />
        </Box>
        {s.testEvery === 'weekly' && (
          <Box>
            <Text size={FS.micro} c="dimmed" mb={4}>On</Text>
            <SegmentedControl value={String(s.testWeekday)} onChange={(v) => patch({ testWeekday: Number(v) })}
              data={['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'].map((d, i) => ({ label: d, value: String(i) }))} />
          </Box>
        )}
        {(s.testEvery === 'daily' || s.testEvery === 'weekly') && (
          <TextInput key={`${s.testEvery}-${saved.testHour}-${saved.testMinute}`} label="At" w={110} ff="monospace" defaultValue={at}
            onChange={(e) => {
              const m = /^(\d{1,2}):(\d{2})$/.exec(e.currentTarget.value)
              if (m && Number(m[1]) < 24 && Number(m[2]) < 60) patch({ testHour: Number(m[1]), testMinute: Number(m[2]) })
            }} />
        )}
      </Group>
      <Text size={FS.micro} c="dimmed" mt="sm">
        {s.every === 'hourly' && 'After every backup is not offered with hourly backups; test daily or weekly instead. '}
        {s.testEvery === 'after' && 'The newest backup is tested a minute after each scheduled run. '}
        {!off && !dirty && saved.testNextAt ? `Next test ${absTime(saved.testNextAt)}. ` : ''}
        {!off && 'The newest backup not tested yet is the one tested. If none passes for twice the interval and a day more, admins are alerted.'}
        {off && 'Off: backups are only checked as readable by pg_restore, which does not prove they restore.'}
      </Text>

      <Box mt="md">
        <Group justify="space-between" mb={4}>
          <Text size={FS.micro} c="dimmed">Your checks · each one SELECT, run read only on the restored copy; true or a non-zero number passes</Text>
        </Group>
        <Stack gap="xs">
          {checks.map((c, i) => {
            const r = tried?.find((x) => x.name === c.name)
            return (
              <Box key={i} p="xs" style={{ border: '1px solid var(--color-line)', borderRadius: 6 }}>
                <Group gap="xs" wrap="nowrap" align="flex-start">
                  <Stack gap={6} style={{ flex: 1, minWidth: 0 }}>
                    <TextInput size="xs" placeholder="Orders this year" value={c.name} maxLength={80} aria-label="Check name"
                      onChange={(e) => setChecks(checks.map((x, j) => (j === i ? { ...x, name: e.currentTarget.value } : x)))} />
                    <Textarea size="xs" ff="monospace" autosize minRows={1} maxLength={2000} aria-label="Check SQL"
                      placeholder="SELECT count(*) > 0 FROM orders WHERE created_at > now() - interval '1 year'" value={c.sql}
                      onChange={(e) => setChecks(checks.map((x, j) => (j === i ? { ...x, sql: e.currentTarget.value } : x)))} />
                    {r && (
                      <Group gap={4} wrap="nowrap">
                        {r.ok ? <IconCheck size={12} style={{ color: 'var(--mantine-color-teal-6)' }} /> : <IconX size={12} style={{ color: 'var(--mantine-color-rose-6)' }} />}
                        <Text size={FS.micro} c={r.ok ? 'teal' : 'rose'} ff="monospace" lineClamp={2}>
                          {r.error ? r.error : `${r.value ?? ''} on the live database now`}
                        </Text>
                      </Group>
                    )}
                  </Stack>
                  <ActionIcon variant="subtle" color="slate" aria-label="Remove check" onClick={() => setChecks(checks.filter((_, j) => j !== i))}>
                    <IconTrash size={14} />
                  </ActionIcon>
                </Group>
              </Box>
            )
          })}
        </Stack>
        <Group gap="xs" mt="xs">
          <Button size="compact-xs" variant="default" leftSection={<IconPlus size={12} />} disabled={checks.length >= MAX_CHECKS}
            onClick={() => setChecks([...checks, { name: '', sql: '' }])}>
            Add a check
          </Button>
          {checks.length > 0 && (
            <Button size="compact-xs" variant="subtle" onClick={() => void onTry()} loading={tryChecks.isPending}
              disabled={!db.tunnelConnected || checks.some((c) => !c.name.trim() || !c.sql.trim())}>
              Try them on the live database
            </Button>
          )}
          <Text size={FS.micro} c="dimmed">Trying is read only and audited.</Text>
        </Group>
      </Box>
    </SectionCard>
  )
}

/** The databases list's column. */
export function LastBackupCell({ b }: { b?: DatabaseBackup }) {
  if (!b) return <Text size={FS.meta} c="dimmed">never</Text>
  if (b.state === 'running') return <Text size={FS.meta} c="azure">running</Text>
  if (b.state === 'failed') return <Tooltip label={b.error} withArrow multiline w={320}><Text size={FS.meta} c="rose">failed {relTime(b.startedAt)}</Text></Tooltip>
  return <Text size={FS.meta} c="dimmed">{relTime(b.startedAt)}{b.bytes ? ` · ${bytes(b.bytes)}` : ''}</Text>
}
