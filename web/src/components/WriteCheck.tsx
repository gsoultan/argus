import type { ImpactPreview } from '~/lib/changeApproval'
import { useEffect, useRef, useState } from 'react'
import { Badge, Box, Button, Group, Loader, Modal, Stack, Text, TextInput } from '@mantine/core'
import { IconAlertTriangle, IconArchive, IconCheck, IconTrash, IconX } from '@tabler/icons-react'
import { useQuery } from '@tanstack/react-query'
import { FS } from '~/theme'
import { GUARD_DROP_DB, GUARD_NO_WHERE, type PGGuard, type SQLClass } from '~/lib/sqlClass'
import { databaseBackupsQuery, useStartBackup } from '~/lib/queries'
import type { DatabaseBackup } from '~/types/domain'

/*
Check before it runs.

Opened by the workspace before an UPDATE, DELETE, MERGE or INSERT … SELECT,
with what the agent learned by running the statement and rolling it back,
and when the agent stops a statement before it ran (no WHERE, TRUNCATE, DROP,
DROP DATABASE) with why and what would let it through.

The typed phrase is the agent's, not ours: it compares it. What the person
types is a pause, not a permission; their level's grants are the permission.

What a DELETE, DROP TABLE, DROP SCHEMA or TRUNCATE removes goes to the
database's trash (see DatabaseTrash), so the dialog says how long it is kept.
A DELETE of more than 10,000 rows cannot keep a copy, so the agent refuses
it, and the dialog says so before it is sent. Before a drop or an empty, a
backup is taken first when the newest one is more than a day old.
*/

/** The most rows a DELETE may remove while the trash keeps a copy. */
export const KEPT_ROWS_MAX = 10000

/** A backup older than this is taken again before a drop. */
const FRESH_MS = 24 * 3600 * 1000

/** Whether what a guarded statement removes goes to the trash. */
export function goesToTrash(g: PGGuard): boolean {
  return g.verb === 'truncate' || (g.verb === 'drop' && (g.object === 'table' || g.object === 'schema'))
}

export interface PGCheckTable { name: string; inserted?: number; updated?: number; deleted?: number; estimate?: number }
export interface PGCheck { class: SQLClass; rows: number; tag?: string; tables?: PGCheckTable[]; guard?: PGGuard; preview?: ImpactPreview }

/** What a DROP or TRUNCATE would take with it, read as the person's role. */
export interface DropFacts { rows?: number; bytes?: number; dependents: string[]; tables?: number; error?: string }

export type WriteCheckState =
  | { kind: 'checking'; sql: string; tab: string }
  | { kind: 'checked'; sql: string; tab: string; check: PGCheck }
  | { kind: 'guard'; sql: string; tab: string; guard: PGGuard; facts?: DropFacts | null }
  | { kind: 'failed'; sql: string; tab: string; error: string; code?: string }

const n = (v: number) => v.toLocaleString()
const verbed: Record<string, string> = { update: 'changed', delete: 'deleted', merge: 'changed', insert: 'inserted' }

function bytes(b: number): string {
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++ }
  return `${b < 10 && i ? b.toFixed(1) : Math.round(b)} ${u[i]}`
}

export function WriteCheckDialog({ state, label, onRun, onClose, onBackups, step, databaseId, canBackup }: {
  state: WriteCheckState | null
  /** The database, to take a backup before a drop. */
  databaseId?: string
  /** The person's level may start a backup. */
  canBackup?: boolean
  /** Where the statement is in a script being run, if it is in one. */
  step?: { n: number; total: number }
  /** production, staging or development. */
  label: string
  onRun: (confirm?: string) => void
  onClose: () => void
  /** Opens the database's Backups panel. */
  onBackups?: () => void
}) {
  const [typed, setTyped] = useState('')
  useEffect(() => { setTyped('') }, [state?.sql, state?.kind])
  if (!state) return null

  const guard = state.kind === 'guard' ? state.guard : state.kind === 'checked' ? state.check.guard : undefined
  const needsPhrase = Boolean(guard?.confirm) && guard?.code !== GUARD_DROP_DB
  const phraseOK = !needsPhrase || typed === guard?.confirm
  const labelColor = { production: 'rose', staging: 'amber', development: 'teal' }[label] ?? 'slate'

  const title = state.kind === 'guard'
    ? guard?.code === GUARD_DROP_DB ? 'Not from a session'
      : guard?.verb === 'truncate' ? `Empty ${guard.targets?.join(', ')}?`
        : guard?.verb === 'drop' ? `Drop ${guard.object} ${guard.targets?.join(', ')}?`
          : 'Change every row?'
    : 'Check before it runs'

  let body: React.ReactNode = null
  let action: React.ReactNode = null
  if (state.kind === 'checking') {
    body = (
      <Group gap="sm" py="md">
        <Loader size="sm" />
        <Text size={FS.body}>Counting what it would change, by running it and rolling it back…</Text>
      </Group>
    )
  } else if (state.kind === 'failed') {
    body = (
      <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
        <Text size={FS.body} fw={600}>Postgres refused it while checking: {state.error}{state.code ? ` (${state.code})` : ''}</Text>
        <Text size={FS.meta} mt={4}>Nothing was changed. Fix the statement and run it again.</Text>
      </Box>
    )
  } else if (state.kind === 'checked') {
    const c = state.check
    const verb = c.class.verb
    const main = c.tables?.[0]
    const others = (c.tables ?? []).slice(1)
    const share = main?.estimate && main.estimate > 0 ? Math.min(100, (c.rows / main.estimate) * 100) : null
    body = (
      <Stack gap="sm">
        <Group gap={28} align="flex-end">
          <Box>
            <Text fz={30} fw={700} lh={1.1} c={guard ? 'rose' : undefined}>{n(c.rows)}</Text>
            <Text size={FS.meta} c="dimmed">{c.rows === 1 ? 'row' : 'rows'} would be {verbed[verb] ?? 'changed'}</Text>
          </Box>
          {share !== null && main && (
            <Box>
              <Text fz={30} fw={700} lh={1.1} c="dimmed">{share < 0.1 ? '<0.1' : share.toFixed(share < 10 ? 1 : 0)}%</Text>
              <Text size={FS.meta} c="dimmed">of about {n(main.estimate!)} in {main.name}</Text>
            </Box>
          )}
        </Group>
        {others.length > 0 && (
          <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-amber-light)' }}>
            <Group gap={6} mb={4}><IconAlertTriangle size={14} /><Text size={FS.meta} fw={600}>It reaches other tables too</Text></Group>
            {others.map((t) => (
              <Text key={t.name} size={FS.meta} ff="monospace">
                {t.name}: {[t.deleted ? `${n(t.deleted)} deleted` : '', t.updated ? `${n(t.updated)} changed` : '', t.inserted ? `${n(t.inserted)} inserted` : '']
                  .filter(Boolean).join(', ')}
              </Text>
            ))}
            <Text size={FS.micro} c="dimmed" mt={4}>By foreign keys with ON DELETE/UPDATE, or by triggers.</Text>
          </Box>
        )}
        {verb === 'delete' && c.rows > KEPT_ROWS_MAX && (
          <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
            <Text size={FS.meta} fw={600}>Too many rows to keep a copy, so Argus will not run it.</Text>
            <Text size={FS.meta}>A DELETE keeps the rows it removes, up to {n(KEPT_ROWS_MAX)}. Delete in smaller batches, or empty the table, which keeps it whole in the trash.</Text>
          </Box>
        )}
        {verb === 'delete' && c.rows > 0 && c.rows <= KEPT_ROWS_MAX && (
          <Group gap={6} wrap="nowrap" align="flex-start">
            <IconTrash size={14} style={{ flexShrink: 0, marginTop: 2 }} />
            <Text size={FS.meta}>The deleted rows are kept for 30 days. Undo, under the result or in Trash, puts them back.</Text>
          </Group>
        )}
        {guard?.code === GUARD_NO_WHERE && (
          <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
            <Text size={FS.meta} fw={600}>{guard.reason}</Text>
            <Text size={FS.meta}>Add a WHERE, or type the phrase below if every row is really meant.</Text>
          </Box>
        )}
        <Text size={FS.meta} c="dimmed">
          Counted by running it inside a transaction that was then rolled back. Nothing has changed yet; sequences it used have moved on.
        </Text>
      </Stack>
    )
    action = (
      <Button color={guard ? 'rose' : undefined} disabled={!phraseOK || (verb === 'delete' && c.rows > KEPT_ROWS_MAX)}
        onClick={() => onRun(needsPhrase ? typed : undefined)}>
        {c.rows === 0 ? 'Run it anyway' : `Run it: ${n(c.rows)} ${c.rows === 1 ? 'row' : 'rows'}`}
      </Button>
    )
  } else if (state.kind === 'guard' && guard) {
    const f = state.facts
    body = (
      <Stack gap="sm">
        <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
          <Group gap={6} wrap="nowrap" align="flex-start">
            <IconX size={16} style={{ flexShrink: 0, marginTop: 2 }} />
            <Text size={FS.meta} fw={600}>{guard.reason} It was stopped before it ran.</Text>
          </Group>
        </Box>
        {guard.code !== GUARD_DROP_DB && f === undefined && <Group gap="xs"><Loader size={12} /><Text size={FS.meta} c="dimmed">Looking at what depends on it…</Text></Group>}
        {f && !f.error && (
          <Group gap={28} align="flex-end">
            {f.rows !== undefined && <Box><Text fz={26} fw={700} lh={1.1}>{n(f.rows)}</Text><Text size={FS.meta} c="dimmed">rows (estimate)</Text></Box>}
            {f.bytes !== undefined && <Box><Text fz={26} fw={700} lh={1.1} c="dimmed">{bytes(f.bytes)}</Text><Text size={FS.meta} c="dimmed">with indexes</Text></Box>}
            {f.tables !== undefined && <Box><Text fz={26} fw={700} lh={1.1}>{n(f.tables)}</Text><Text size={FS.meta} c="dimmed">tables in it</Text></Box>}
            <Box><Text fz={26} fw={700} lh={1.1} c={f.dependents.length ? 'rose' : 'dimmed'}>{f.dependents.length}</Text><Text size={FS.meta} c="dimmed">depend on it</Text></Box>
          </Group>
        )}
        {f && f.dependents.length > 0 && (
          <Box style={{ border: '1px solid var(--color-line)', borderRadius: 8, maxHeight: 160, overflow: 'auto' }} p={8}>
            {f.dependents.map((d) => <Text key={d} size={FS.meta} ff="monospace">{d}</Text>)}
            <Text size={FS.micro} c="dimmed" mt={4}>
              {goesToTrash(guard) && guard.verb === 'drop' && guard.object === 'table'
                ? 'While these depend on it, Argus will not drop it: dropping them with it could not be put back. Drop them first.'
                : guard.cascade ? 'CASCADE drops these too.' : 'Without CASCADE, Postgres will refuse while these exist.'}
            </Text>
          </Box>
        )}
        {goesToTrash(guard) && (
          <Group gap={8} wrap="nowrap" align="flex-start">
            <IconTrash size={16} style={{ flexShrink: 0, marginTop: 2 }} />
            <Text size={FS.meta}>
              {guard.verb === 'truncate'
                ? 'A copy of its rows goes to the trash for 7 days (up to 1 GB), and Trash puts them back.'
                : `The ${guard.object} goes to the trash for 7 days, out of everyone's reach, and Trash puts it back.`}
            </Text>
          </Group>
        )}
        {guard.code !== GUARD_DROP_DB && !goesToTrash(guard) && (
          <Group gap={8} wrap="nowrap" align="flex-start">
            <IconAlertTriangle size={16} color="var(--mantine-color-amber-6)" style={{ flexShrink: 0, marginTop: 2 }} />
            <Text size={FS.meta}>
              This cannot be undone from Argus. Check that a recent backup exists first
              {onBackups && <> (<Text span c="var(--color-brand)" style={{ cursor: 'pointer' }} onClick={onBackups}>Backups</Text>)</>}.
            </Text>
          </Group>
        )}
      </Stack>
    )
    if (guard.code !== GUARD_DROP_DB) {
      const label = guard.verb === 'truncate' ? 'Empty it' : guard.verb === 'drop' ? 'Drop it' : 'Run it on every row'
      action = goesToTrash(guard) && databaseId && canBackup
        ? <BackupFirst databaseId={databaseId} label={label} disabled={!phraseOK} what={state.sql} onGo={() => onRun(typed)} />
        : <Button color="rose" disabled={!phraseOK} onClick={() => onRun(typed)}>{label}</Button>
    }
  }

  return (
    <Modal opened onClose={onClose} size={560} title={<Group gap={8}><Text fw={650}>{title}</Text><Badge variant="light" color={labelColor} radius="sm">{label}</Badge></Group>}>
      <Stack gap="md">
        <Box p={8} style={{ background: 'var(--color-void)', borderRadius: 6, maxHeight: 96, overflow: 'auto' }}>
          <Text size={FS.meta} ff="monospace" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{state.sql.length > 600 ? state.sql.slice(0, 600) + '…' : state.sql}</Text>
        </Box>
        {step && (
          <Text size={FS.meta} c="dimmed">
            Statement {step.n} of {step.total} in your script. The ones before it have run; going back stops the script here.
          </Text>
        )}
        {body}
        {needsPhrase && state.kind !== 'checking' && (
          <TextInput
            label={<>Type <Text span ff="monospace" c="rose">{guard?.confirm}</Text> to go ahead</>}
            value={typed} onChange={(e) => setTyped(e.currentTarget.value)}
            styles={{ input: { fontFamily: 'var(--font-mono)' } }}
            rightSection={phraseOK && typed ? <IconCheck size={14} color="var(--mantine-color-teal-6)" /> : null}
            autoComplete="off" spellCheck={false} data-autofocus
          />
        )}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onClose}>{step ? 'Stop the script' : action ? 'Back to the editor' : 'Close'}</Button>
          {action}
        </Group>
      </Stack>
    </Modal>
  )
}

const ageText = (ms: number) => {
  const h = Math.round(ms / 3600_000)
  return h < 1 ? 'less than an hour ago' : h < 48 ? `${h} hours ago` : `${Math.round(h / 24)} days ago`
}

/**
 * The Drop or Empty button, which first takes a backup when the newest one
 * is more than a day old, and runs the statement once it is done.
 */
function BackupFirst({ databaseId, label, disabled, what, onGo }: {
  databaseId: string
  label: string
  disabled: boolean
  what: string
  onGo: () => void
}) {
  const { data, isPending, isError, fetchStatus } = useQuery(databaseBackupsQuery(databaseId))
  const start = useStartBackup()
  const [waiting, setWaiting] = useState<string | null>(null)
  const [failed, setFailed] = useState<string | null>(null)
  const went = useRef(false)
  const newest: DatabaseBackup | undefined = data?.backups.filter((b) => b.state === 'done')
    .sort((a, b) => (b.finishedAt ?? b.startedAt).localeCompare(a.finishedAt ?? a.startedAt))[0]
  const age = newest ? Date.now() - new Date(newest.finishedAt ?? newest.startedAt).getTime() : Infinity
  const fresh = age < FRESH_MS
  // The backup this dialog started, as the polled list sees it.
  const mine = waiting ? (data?.running?.id === waiting ? data.running : data?.backups.find((b) => b.id === waiting)) : undefined
  useEffect(() => {
    if (!waiting || !mine || went.current) return
    if (mine.state === 'done') { went.current = true; onGo() }
    else if (mine.state === 'failed' || mine.state === 'skipped') { setFailed(mine.error || 'the backup did not finish'); setWaiting(null) }
  }, [waiting, mine, onGo])

  if (isPending && fetchStatus !== 'idle') return <Button color="rose" disabled loading>{label}</Button>
  // No backups to look at (none set up, or no way to ask): the trash still keeps it.
  if (isError || !data) return <Button color="rose" disabled={disabled} onClick={onGo}>{label}</Button>
  return (
    <Group gap="xs" wrap="nowrap" align="center">
      <Text size={FS.micro} c="dimmed" maw={230} ta="right">
        {waiting ? <><Loader size={10} mr={4} />Backing up first{mine?.stage ? `: ${mine.stage}` : '…'}</>
          : failed ? `The backup failed: ${failed}.`
            : fresh ? <><IconArchive size={11} /> Last backup {ageText(age)}.</>
              : newest ? `The last backup is from ${ageText(age)}, so one is taken first.` : 'There is no backup yet, so one is taken first.'}
      </Text>
      {failed
        ? <Button color="rose" variant="light" disabled={disabled} onClick={onGo}>{label} without it</Button>
        : <Button color="rose" disabled={disabled || Boolean(waiting) || Boolean(data.running && !waiting)} loading={Boolean(waiting)}
            title={data.running && !waiting ? 'A backup is already running' : undefined}
            onClick={async () => {
              if (fresh) { onGo(); return }
              try {
                const b = await start.mutateAsync({ id: databaseId, reason: `Before: ${what.slice(0, 120)}` })
                setWaiting(b.id)
              } catch (err) {
                setFailed(err instanceof Error ? err.message : String(err))
              }
            }}>
            {fresh ? label : `Back up, then ${label.toLowerCase()}`}
          </Button>}
    </Group>
  )
}

/** SQL that reads what a DROP or TRUNCATE target holds and what depends on it. */
export function dropFactsSQL(guard: PGGuard): { size?: string; deps?: string; tables?: string } {
  const t = guard.targets?.[0]
  if (!t) return {}
  const lit = `'${t.replace(/'/g, "''")}'`
  if (guard.object === 'schema') {
    return {
      tables: `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = ${lit} AND c.relkind IN ('r','p')`,
    }
  }
  return {
    size: `SELECT greatest(c.reltuples, 0)::int8, pg_total_relation_size(c.oid) FROM pg_class c WHERE c.oid = to_regclass(${lit})`,
    deps: `SELECT DISTINCT pg_describe_object(d.classid, d.objid, d.objsubid) FROM pg_depend d
WHERE d.refobjid = to_regclass(${lit}) AND d.deptype = 'n'
ORDER BY 1 LIMIT 50`,
  }
}
