import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Group, Loader, ScrollArea, Switch, Table, Text, Tooltip,
} from '@mantine/core'
import { IconArrowLeft, IconLock, IconRefresh } from '@tabler/icons-react'
import { FS, INK, SP } from '~/theme'
import { SummaryTiles } from '~/components/DatabaseShell'
import type { DBLevel } from '~/types/domain'

/*
The two administration panels of the query workspace. Both are ordinary
recorded statements run as the person's role through the same session as the
editor, so what a DBA looks at and does here is in the session recording,
statement by statement, like everything else. Postgres decides what each
level may see: pg_stat_activity shows other people's statements only to a
role with pg_read_all_stats, which the DBA level holds, and only the DBA
level can signal a backend or change who holds a level.
*/

type Cell = string | null
export interface Collected {
  cols: { name: string; type: string; table?: number; attnum?: number }[]
  rows: Cell[][]
  error?: { error?: string; code?: string }
  /** Postgres's command tag, when it finished: "INSERT 0 500". */
  tag?: string
  /** A DELETE, DROP or TRUNCATE kept what it removed in the trash. */
  trashed?: boolean
}
export type Collect = (sql: string, maxRows?: number, params?: (string | null)[]) => Promise<Collected>

/*
Administrative operations (extensions, publications, subscriptions) are not
statements run as the person's role: they go to the agent's pg-admin helper,
which runs them as the server's postgres account from a fixed list, and only
for a session whose signed grant is dba. The helper redacts any password
before the statement is recorded, audited or echoed. See DatabaseExtensions
and DatabaseReplication, and plans/postgres-extensions-replication/design.md.
*/
export interface ConnInfo {
  host: string
  port?: number
  dbname: string
  user: string
  password?: string
  sslmode?: string
}
export interface AdminOp {
  kind: string
  preview?: boolean
  name?: string
  schema?: string
  version?: string
  cascade?: boolean
  allTables?: boolean
  tables?: string[]
  publish?: string[]
  conn?: ConnInfo
  publications?: string[]
  copyData?: boolean
  createSlot?: boolean
  enabled?: boolean
  connect?: boolean
  slotName?: string
  viaRoot?: boolean
  refresh?: boolean
  syncCommit?: string
  streaming?: string
  binary?: boolean
  disableOnError?: boolean
  publisherGone?: boolean
  /** subscription.skip: the publisher transaction to skip, where it finished. */
  lsn?: string
  /** network.set: who may connect, and how (see DatabaseNetwork). */
  reach?: string
  addresses?: string[]
  sources?: string[]
  confirm?: string
  restart?: boolean
  /** Roles and privileges (see DatabaseRoles and internal/tunnel/pgroles.go). */
  role?: {
    name: string; rename?: string; login?: boolean; superuser?: boolean; createdb?: boolean; createrole?: boolean
    inherit?: boolean; replication?: boolean; bypassrls?: boolean; connLimit?: number; validUntil?: string
    password?: string; noPassword?: boolean; comment?: string; memberOf?: string[]; reassignTo?: string; dropOwned?: boolean
  }
  member?: { role: string; member: string; admin?: boolean }
  grant?: {
    grantee: string; on: string; schema?: string; objects?: string[]; privileges: string[]; withGrant?: boolean
    schemaUsage?: boolean; future?: boolean; forRoles?: string[]; cascade?: boolean
  }
  /** SQL Server users and roles (see DatabaseMSSQLAccess and internal/tunnel/mssqlaccess.go). */
  principal?: {
    name: string; rename?: string; auth?: 'sql' | 'login' | 'none'; login?: string; password?: string
    defaultSchema?: string; enabled?: boolean; memberOf?: string[]; dropLogin?: boolean
  }
  msGrant?: {
    grantee?: string; on?: 'database' | 'schema' | 'object'; schema?: string; objects?: string[]; privileges?: string[]
    withGrant?: boolean; cascade?: boolean
  }
  /** SQL Server's DBA tools (see DatabaseMSSQLDBA and internal/tunnel/mssqldba.go). */
  msdba?: {
    session?: number; schema?: string; table?: string; index?: string; online?: boolean; fullScan?: boolean; sample?: number
    physicalOnly?: boolean; job?: string; enabled?: boolean; sort?: string; setting?: string; value?: string
  }
  /** SQL Server replication (see DatabaseMSSQLRepl and internal/tunnel/mssqlrepl.go). */
  msrepl?: {
    group?: string; force?: boolean; enabled?: boolean; publication?: string; kind?: 'transactional' | 'snapshot'
    schema?: string; table?: string; article?: string; subscriber?: string; subscriberDb?: string
    login?: string; password?: string; workingDir?: string
  }
}
export interface AdminFailure { error?: string; code?: string; detail?: string; hint?: string }
export interface AdminResult<T = unknown> { data?: T; tag?: string; ms?: number; error?: AdminFailure }
export type Admin = <T = unknown>(op: AdminOp) => Promise<AdminResult<T>>

export interface AdminPanelProps {
  collect: Collect
  admin: Admin
  level: DBLevel | ''
  /** The level roles of this database, from the session's ready answer. */
  levels: Partial<Record<DBLevel, string>>
  /** The person's own role, to mark their own session. */
  self: string
  narrow: boolean
  onBack?: () => void
  onAskForAccess?: () => void
  /** The server the database runs on, for the network panel. */
  assetId?: string
  /** Puts a statement in a new editor tab, without running it. */
  onOpenSql?: (sql: string, name: string) => void
}

const REFRESH_MS = 10_000

/** A role or identifier that is safe to put in a statement as it is. */
export const SAFE = /^[a-z_][a-z0-9_]{0,62}$/
export const ident = (n: string) => (SAFE.test(n) ? n : `"${n.replace(/"/g, '""')}"`)
export const literal = (s: string) => `'${s.replace(/'/g, "''")}'`

function PanelHeader({ title, blurb, narrow, onBack, right }: {
  title: string; blurb: string; narrow: boolean; onBack?: () => void; right?: React.ReactNode
}) {
  return (
    <Group gap="xs" px="md" py={SP.cozy} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
      {onBack && <ActionIcon variant="subtle" onClick={onBack} aria-label="Back to the query editor"><IconArrowLeft size={16} /></ActionIcon>}
      <Box style={{ minWidth: 0, flex: 1 }}>
        <Text fw={650} size="md" style={{ letterSpacing: '-0.01em' }}>{title}</Text>
        {!narrow && <Text size={FS.meta} c="dimmed" mt={2} lineClamp={2} maw={760}>{blurb}</Text>}
      </Box>
      {right}
    </Group>
  )
}

export function PanelFrame({ title, blurb, narrow, onBack, right, children }: {
  title: string; blurb: string; narrow: boolean; onBack?: () => void; right?: React.ReactNode; children: React.ReactNode
}) {
  return (
    <Box style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
      <PanelHeader title={title} blurb={blurb} narrow={narrow} onBack={onBack} right={right} />
      {children}
    </Box>
  )
}

/** The one-line gate below DBA: visible, labelled, with the way to ask. */
export function NeedsDBA({ level, what, account = 'postgres account', onAskForAccess }: { level: DBLevel | ''; what: string; account?: string; onAskForAccess?: () => void }) {
  return (
    <Box p="md">
      <Group gap="xs"><IconLock size={16} /><Text fw={600} size={FS.body}>Needs DBA access</Text></Group>
      <Text size={FS.meta} c="dimmed" mt={4}>
        Your level here is {level ? level.toUpperCase() : 'not known yet'}. {what} run through the server’s own {account},
        so this panel opens at DBA only; on a production database that is one approval away.
      </Text>
      {onAskForAccess && <Button mt="sm" size="xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>Ask for access</Button>}
    </Box>
  )
}

/** Postgres's refusal of an administrative operation, with its next step. */
export function AdminRefused({ error, who = 'Postgres' }: { error: AdminFailure; who?: string }) {
  return (
    <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
      <Text size={FS.body} fw={600}>
        {who} refused this: {error.error}
        {error.code && <Text span size={FS.micro} ff="monospace" ml={6}>({error.code})</Text>}
      </Text>
      {error.detail && <Text size={FS.meta} mt={4} style={{ whiteSpace: 'pre-wrap' }}>{error.detail}</Text>}
      {error.hint && <Text size={FS.meta} mt={4}>{error.hint}</Text>}
      <Text size={FS.meta} mt={4}>Nothing was changed.</Text>
    </Box>
  )
}

function Refused({ error }: { error: { error?: string; code?: string } }) {
  return (
    <Box p="md">
      <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
        <Text size={FS.body} fw={600}>Postgres refused this: {error.error}</Text>
        {error.code === '42501' && <Text size={FS.meta} mt={4}>Your level does not allow it. Nothing was changed.</Text>}
      </Box>
    </Box>
  )
}

/* ── Activity & locks ───────────────────────────────────────────────────── */

const ACTIVITY_SQL = `SELECT a.pid::text, coalesce(a.usename, ''), coalesce(a.application_name, ''),
  coalesce(a.client_addr::text, 'local'), coalesce(a.state, ''),
  coalesce(a.wait_event_type || ': ' || a.wait_event, ''),
  CASE WHEN a.state = 'active' THEN to_char(now() - a.query_start, 'HH24:MI:SS') ELSE '' END,
  CASE WHEN a.xact_start IS NOT NULL THEN to_char(now() - a.xact_start, 'HH24:MI:SS') ELSE '' END,
  array_to_string(pg_blocking_pids(a.pid), ', '),
  left(coalesce(a.query, ''), 400),
  (a.pid = pg_backend_pid())::text
FROM pg_stat_activity a
WHERE a.datname = current_database() AND a.backend_type = 'client backend'
ORDER BY cardinality(pg_blocking_pids(a.pid)) > 0 DESC, a.state = 'active' DESC, a.query_start DESC NULLS LAST
LIMIT 500`

export function ActivityPanel({ collect, level, narrow, onBack }: AdminPanelProps) {
  const [data, setData] = useState<Collected | null>(null)
  const [asOf, setAsOf] = useState<Date | null>(null)
  const [auto, setAuto] = useState(true)
  const [busy, setBusy] = useState(false)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await collect(ACTIVITY_SQL, 500)
    if (!alive.current) return
    setData(r)
    setAsOf(new Date())
    setBusy(false)
  }, [collect])
  useEffect(() => { void refresh() }, [refresh])
  useEffect(() => {
    if (!auto) return
    const t = setInterval(() => void refresh(), REFRESH_MS)
    return () => clearInterval(t)
  }, [auto, refresh])

  const dba = level === 'dba'
  const act = async (fn: 'pg_cancel_backend' | 'pg_terminate_backend', pid: string) => {
    if (fn === 'pg_terminate_backend' && !window.confirm(`End backend ${pid}? Its transaction is rolled back and its connection closed.`)) return
    const r = await collect(`SELECT ${fn}(${Number(pid)})`, 1)
    if (r.error) window.alert(`Postgres refused this: ${r.error.error ?? ''}`)
    void refresh()
  }

  const rows = data?.rows ?? []
  const blocked = rows.filter((r) => r[8]).length
  const active = rows.filter((r) => r[4] === 'active').length
  const idleTx = rows.filter((r) => r[4]?.startsWith('idle in transaction')).length

  return (
    <Box style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
      <PanelHeader title="Activity & locks" narrow={narrow} onBack={onBack}
        blurb={dba
          ? 'Every connection to this database, who is waiting on whom, and the statement each one runs. Cancel stops a statement; End closes the connection.'
          : 'Connections to this database. Postgres shows other people’s statements only to a DBA; cancelling or ending one needs DBA access too.'}
        right={(
          <Group gap="xs" wrap="nowrap">
            <Switch size="xs" label={narrow ? '' : 'Every 10 s'} checked={auto} onChange={(e) => setAuto(e.currentTarget.checked)} aria-label="Refresh automatically" />
            <ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>
          </Group>
        )} />
      <Box px="md" py="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <SummaryTiles narrow={narrow} tiles={[
          { label: 'Connections', value: String(rows.length), note: 'now' },
          { label: 'Active', value: String(active), note: 'running a statement' },
          { label: 'Idle in transaction', value: String(idleTx), note: idleTx ? 'holding locks open' : undefined, tone: idleTx ? 'pending' : undefined },
          { label: 'Waiting on a lock', value: String(blocked), note: blocked ? 'see Blocked by' : undefined, tone: blocked ? 'denied' : undefined },
        ]} />
        {asOf && <Text size={FS.micro} c="dimmed" mt={6} ta="right">as of {asOf.toLocaleTimeString()}</Text>}
      </Box>
      {data?.error ? <Refused error={data.error} /> : !data ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Table verticalSpacing={4} fz={12} style={{ fontFamily: 'var(--font-mono)' }} striped highlightOnHover>
            <Table.Thead style={{ position: 'sticky', top: 0, background: 'var(--color-raised)', zIndex: 1 }}>
              <Table.Tr>
                <Table.Th>PID</Table.Th><Table.Th>Role</Table.Th>
                {!narrow && <Table.Th>Application</Table.Th>}
                <Table.Th>State</Table.Th>
                {!narrow && <Table.Th>Waiting on</Table.Th>}
                <Table.Th>Running</Table.Th>
                <Table.Th>Blocked by</Table.Th>
                <Table.Th>Statement</Table.Th>
                <Table.Th></Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((r) => {
                const me = r[10] === 'true'
                const blockedBy = r[8] ?? ''
                return (
                  <Table.Tr key={r[0]} style={{ background: blockedBy ? 'var(--mantine-color-rose-light)' : undefined }}>
                    <Table.Td>{r[0]}{me && <Badge size="xs" variant="light" ml={4}>you</Badge>}</Table.Td>
                    <Table.Td>{r[1]}</Table.Td>
                    {!narrow && <Table.Td style={{ maxWidth: 160, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={`${r[2]} from ${r[3]}`}>{r[2]}</Table.Td>}
                    <Table.Td style={{ whiteSpace: 'nowrap', color: r[4]?.startsWith('idle in transaction') ? INK.pending : undefined }}>{r[4]}</Table.Td>
                    {!narrow && <Table.Td style={{ whiteSpace: 'nowrap' }}>{r[5]}</Table.Td>}
                    <Table.Td style={{ whiteSpace: 'nowrap' }} title={r[7] ? `in a transaction for ${r[7]}` : undefined}>{r[6]}{r[7] && !r[6] ? <span style={{ color: 'var(--mantine-color-dimmed)' }}>tx {r[7]}</span> : ''}</Table.Td>
                    <Table.Td style={{ whiteSpace: 'nowrap', fontWeight: blockedBy ? 600 : undefined }}>{blockedBy}</Table.Td>
                    <Table.Td style={{ maxWidth: narrow ? 160 : 520, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={r[9] ?? ''}>
                      {r[9] || <span style={{ color: 'var(--mantine-color-dimmed)' }}>{dba ? '' : 'not shown to your level'}</span>}
                    </Table.Td>
                    <Table.Td>
                      {!me && (
                        <Tooltip label={dba ? 'Cancel the statement, or end the connection' : 'Needs DBA access'} withArrow>
                          <Group gap={4} wrap="nowrap">
                            <Button size="compact-xs" variant="default" disabled={!dba || r[4] !== 'active'} onClick={() => void act('pg_cancel_backend', r[0]!)}>Cancel</Button>
                            <Button size="compact-xs" variant="light" color="rose" disabled={!dba} onClick={() => void act('pg_terminate_backend', r[0]!)}>End</Button>
                          </Group>
                        </Tooltip>
                      )}
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
          {rows.length === 0 && <Text size={FS.meta} c="dimmed" p="md">No other connection to this database right now.</Text>}
        </ScrollArea>
      )}
    </Box>
  )
}

/* ── Roles & access ─────────────────────────────────────────────────────── */

/**
 * Roles & access below DBA: labelled, not hidden, with the way to ask. At DBA
 * the workspace shows RolesManager (DatabaseRoles) instead, which creates,
 * changes and drops roles and grants and revokes through the helper.
 */
export function RolesPanel({ level, narrow, onBack, onAskForAccess }: AdminPanelProps) {
  return (
    <PanelFrame title="Roles & access" narrow={narrow} onBack={onBack}
      blurb="Every role on this server, what each can do here, and granting and revoking privileges.">
      <NeedsDBA level={level} what="Creating roles and granting privileges" onAskForAccess={onAskForAccess} />
    </PanelFrame>
  )
}
