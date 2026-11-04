import { useEffect, useState } from 'react'
import { Badge, Box, Button, Group, Loader, Modal, Stack, Text, TextInput } from '@mantine/core'
import { IconAlertTriangle, IconCheck, IconX } from '@tabler/icons-react'
import { FS } from '~/theme'
import { GUARD_DROP_DB, GUARD_NO_WHERE, type PGGuard, type SQLClass } from '~/lib/sqlClass'

/*
Check before it runs.

Opened by the workspace before an UPDATE, DELETE, MERGE or INSERT … SELECT,
with what the agent learned by running the statement and rolling it back,
and when the agent stops a statement before it ran (no WHERE, TRUNCATE, DROP,
DROP DATABASE) with why and what would let it through.

The typed phrase is the agent's, not ours: it compares it. What the person
types is a pause, not a permission; their level's grants are the permission.
*/

export interface PGCheckTable { name: string; inserted?: number; updated?: number; deleted?: number; estimate?: number }
export interface PGCheck { class: SQLClass; rows: number; tag?: string; tables?: PGCheckTable[]; guard?: PGGuard }

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

export function WriteCheckDialog({ state, label, onRun, onClose, onBackups }: {
  state: WriteCheckState | null
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
      <Button color={guard ? 'rose' : undefined} disabled={!phraseOK} onClick={() => onRun(needsPhrase ? typed : undefined)}>
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
              {guard.cascade ? 'CASCADE drops these too.' : 'Without CASCADE, Postgres will refuse while these exist.'}
            </Text>
          </Box>
        )}
        {guard.code !== GUARD_DROP_DB && (
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
      action = (
        <Button color="rose" disabled={!phraseOK} onClick={() => onRun(typed)}>
          {guard.verb === 'truncate' ? 'Empty it' : guard.verb === 'drop' ? 'Drop it' : 'Run it on every row'}
        </Button>
      )
    }
  }

  return (
    <Modal opened onClose={onClose} size={560} title={<Group gap={8}><Text fw={650}>{title}</Text><Badge variant="light" color={labelColor} radius="sm">{label}</Badge></Group>}>
      <Stack gap="md">
        <Box p={8} style={{ background: 'var(--color-void)', borderRadius: 6, maxHeight: 96, overflow: 'auto' }}>
          <Text size={FS.meta} ff="monospace" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{state.sql.length > 600 ? state.sql.slice(0, 600) + '…' : state.sql}</Text>
        </Box>
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
          <Button variant="default" onClick={onClose}>{action ? 'Back to the editor' : 'Close'}</Button>
          {action}
        </Group>
      </Stack>
    </Modal>
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
