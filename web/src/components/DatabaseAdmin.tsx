import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Group, Loader, Menu, ScrollArea, Switch, Table, Text, Tooltip,
} from '@mantine/core'
import { IconArrowLeft, IconDotsVertical, IconLock, IconRefresh } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
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
  cols: { name: string; type: string }[]
  rows: Cell[][]
  error?: { error?: string; code?: string }
}
export type Collect = (sql: string, maxRows?: number) => Promise<Collected>

export interface AdminPanelProps {
  collect: Collect
  level: DBLevel | ''
  /** The level roles of this database, from the session's ready answer. */
  levels: Partial<Record<DBLevel, string>>
  /** The person's own role, to mark their own session. */
  self: string
  narrow: boolean
  onBack: () => void
  onAskForAccess?: () => void
}

const REFRESH_MS = 10_000

/** A role or identifier that is safe to put in a statement as it is. */
const SAFE = /^[a-z_][a-z0-9_]{0,62}$/
const ident = (n: string) => (SAFE.test(n) ? n : `"${n.replace(/"/g, '""')}"`)
const literal = (s: string) => `'${s.replace(/'/g, "''")}'`

function PanelHeader({ title, blurb, narrow, onBack, right }: {
  title: string; blurb: string; narrow: boolean; onBack: () => void; right?: React.ReactNode
}) {
  return (
    <Group gap="xs" px="md" py={SP.cozy} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
      <ActionIcon variant="subtle" onClick={onBack} aria-label="Back to the query editor"><IconArrowLeft size={16} /></ActionIcon>
      <Box style={{ minWidth: 0, flex: 1 }}>
        <Text fw={650} size={FS.body}>{title}</Text>
        {!narrow && <Text size={FS.micro} c="dimmed" lineClamp={1}>{blurb}</Text>}
      </Box>
      {right}
    </Group>
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
      <Group gap="md" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }} wrap="wrap">
        <Text size={FS.meta}><b>{rows.length}</b> connections</Text>
        <Text size={FS.meta}><b>{active}</b> active</Text>
        <Text size={FS.meta} c={idleTx ? 'amber' : undefined}><b>{idleTx}</b> idle in transaction</Text>
        <Text size={FS.meta} c={blocked ? 'rose' : undefined}><b>{blocked}</b> waiting on a lock</Text>
        {asOf && <Text size={FS.micro} c="dimmed" ml="auto">as of {asOf.toLocaleTimeString()}</Text>}
      </Group>
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
                    <Table.Td style={{ whiteSpace: 'nowrap', color: r[4]?.startsWith('idle in transaction') ? 'var(--mantine-color-amber-7)' : undefined }}>{r[4]}</Table.Td>
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

const LEVEL_ORDER: DBLevel[] = ['read', 'write', 'owner', 'dba']

export function RolesPanel({ collect, level, levels, narrow, onBack, onAskForAccess }: AdminPanelProps) {
  const [data, setData] = useState<Collected | null>(null)
  const [busy, setBusy] = useState(false)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const levelNames = LEVEL_ORDER.map((l) => levels[l]).filter((n): n is string => !!n && SAFE.test(n))
  const inLevels = levelNames.length ? levelNames.map(literal).join(', ') : "''"
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await collect(`SELECT r.rolname, r.rolcanlogin::text, r.rolsuper::text,
  coalesce(to_char(r.rolvaliduntil AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI') || ' UTC', ''),
  coalesce(shobj_description(r.oid, 'pg_authid'), ''),
  coalesce((SELECT string_agg(g.rolname, ',' ORDER BY g.rolname) FROM pg_auth_members am JOIN pg_roles g ON g.oid = am.roleid
            WHERE am.member = r.oid AND g.rolname IN (${inLevels})), ''),
  (SELECT count(*) FROM pg_stat_activity a WHERE a.usename = r.rolname AND a.datname = current_database())::text,
  r.rolcreaterole::text
FROM pg_roles r
WHERE r.rolname NOT LIKE 'pg\\_%'
ORDER BY r.rolsuper DESC, r.rolname
LIMIT 1000`, 1000)
    if (!alive.current) return
    setData(r)
    setBusy(false)
  }, [collect, inLevels])
  useEffect(() => { if (level === 'dba') void refresh() }, [refresh, level])

  if (level !== 'dba') {
    return (
      <Box style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
        <PanelHeader title="Roles & access" narrow={narrow} onBack={onBack} blurb="Who holds which level in Postgres, and what applications' own roles may do." />
        <Box p="md">
          <Group gap="xs"><IconLock size={16} /><Text fw={600} size={FS.body}>Needs DBA access</Text></Group>
          <Text size={FS.meta} c="dimmed" mt={4}>
            Your level here is {level ? level.toUpperCase() : 'not known yet'}. Reading pg_roles is open to everyone, but giving or taking a level
            and ending another role's sessions are DBA work, so this panel opens at DBA only.
          </Text>
          {onAskForAccess && <Button mt="sm" size="xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>Ask for access</Button>}
        </Box>
      </Box>
    )
  }

  const run = async (sql: string, confirmText?: string) => {
    if (confirmText && !window.confirm(confirmText)) return
    const r = await collect(sql, 1)
    if (r.error) window.alert(`Postgres refused this: ${r.error.error ?? ''}`)
    void refresh()
  }
  const kindOf = (comment: string): { label: string; color: string; owner?: string } => {
    if (comment.startsWith('argus-managed owner=')) return { label: 'Argus person', color: 'azure', owner: comment.slice('argus-managed owner='.length) }
    if (comment.startsWith('argus-managed level')) return { label: 'Argus level', color: 'slate' }
    if (comment.startsWith('argus-managed')) return { label: 'Argus', color: 'slate' }
    return { label: 'Application', color: 'teal' }
  }
  const rows = data?.rows ?? []

  return (
    <Box style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
      <PanelHeader title="Roles & access" narrow={narrow} onBack={onBack}
        blurb="Every role on this server, what it holds here, and how many connections it has to this database. Argus's own roles are managed from the database's settings; an application's role can be given a level or have it taken back."
        right={<ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>} />
      <Group gap="xs" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }} wrap="wrap">
        <Text size={FS.micro} c="dimmed">Levels here:</Text>
        {LEVEL_ORDER.map((l) => levels[l] && <Badge key={l} size="xs" variant="light" color="slate" radius="sm" ff="monospace" tt="none">{levels[l]}</Badge>)}
      </Group>
      {data?.error ? <Refused error={data.error} /> : !data ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Table verticalSpacing={4} fz={12} striped highlightOnHover>
            <Table.Thead style={{ position: 'sticky', top: 0, background: 'var(--color-raised)', zIndex: 1 }}>
              <Table.Tr>
                <Table.Th>Role</Table.Th><Table.Th>Kind</Table.Th><Table.Th>Can log in</Table.Th>
                {!narrow && <Table.Th>Valid until</Table.Th>}
                <Table.Th>Level here</Table.Th><Table.Th>Connections</Table.Th><Table.Th></Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((r) => {
                const name = r[0] ?? ''
                const kind = kindOf(r[4] ?? '')
                const held = (r[5] ?? '').split(',').filter(Boolean)
                const heldLevels = LEVEL_ORDER.filter((l) => levels[l] && held.includes(levels[l]!))
                const app = kind.label === 'Application' && r[2] !== 'true'
                const conns = Number(r[6] ?? 0)
                return (
                  <Table.Tr key={name}>
                    <Table.Td>
                      <Text size={FS.meta} ff="monospace">{name}</Text>
                      {kind.owner && <Text size={FS.micro} c="dimmed">{kind.owner}</Text>}
                    </Table.Td>
                    <Table.Td>
                      <Badge size="xs" variant="light" color={kind.color} radius="sm">{r[2] === 'true' ? 'Superuser' : kind.label}</Badge>
                      {r[7] === 'true' && r[2] !== 'true' && <Badge size="xs" variant="outline" color="slate" radius="sm" ml={4}>createrole</Badge>}
                    </Table.Td>
                    <Table.Td>{r[1] === 'true' ? 'yes' : <span style={{ color: 'var(--mantine-color-dimmed)' }}>no</span>}</Table.Td>
                    {!narrow && <Table.Td style={{ whiteSpace: 'nowrap' }}>{r[3]}</Table.Td>}
                    <Table.Td>
                      <Group gap={4}>
                        {heldLevels.map((l) => <Badge key={l} size="xs" variant="light" color={l === 'read' ? 'slate' : 'azure'} radius="sm">{l}</Badge>)}
                        {kind.label === 'Argus level' && <Text size={FS.micro} c="dimmed">is a level</Text>}
                      </Group>
                    </Table.Td>
                    <Table.Td>{conns || <span style={{ color: 'var(--mantine-color-dimmed)' }}>0</span>}</Table.Td>
                    <Table.Td ta="right">
                      {(app || conns > 0) && r[2] !== 'true' && (
                        <Menu position="bottom-end" withArrow shadow="md">
                          <Menu.Target>
                            <ActionIcon variant="subtle" size="sm" aria-label={`Actions for ${name}`}><IconDotsVertical size={14} /></ActionIcon>
                          </Menu.Target>
                          <Menu.Dropdown>
                            {app && (
                              <>
                                <Menu.Label>Give a level on this database</Menu.Label>
                                {(['read', 'write', 'owner'] as DBLevel[]).filter((l) => levels[l] && !heldLevels.includes(l)).map((l) => (
                                  <Menu.Item key={l} onClick={() => void run(`GRANT ${ident(levels[l]!)} TO ${ident(name)}`)}>
                                    {l[0]!.toUpperCase() + l.slice(1)}
                                  </Menu.Item>
                                ))}
                                {heldLevels.length > 0 && <Menu.Label>Take back</Menu.Label>}
                                {heldLevels.map((l) => (
                                  <Menu.Item key={'r' + l} color="rose" onClick={() => void run(`REVOKE ${ident(levels[l]!)} FROM ${ident(name)}`,
                                    `Take ${l} on this database from ${name}? Its own grants are untouched; only the Argus level goes.`)}>
                                    {l[0]!.toUpperCase() + l.slice(1)}
                                  </Menu.Item>
                                ))}
                              </>
                            )}
                            {conns > 0 && (
                              <>
                                <Menu.Divider />
                                <Menu.Item color="rose" onClick={() => void run(
                                  `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename = ${literal(name)} AND datname = current_database() AND pid <> pg_backend_pid()`,
                                  `End all ${conns} connection(s) of ${name} to this database? Their transactions are rolled back.`)}>
                                  End its {conns} connection{conns === 1 ? '' : 's'}
                                </Menu.Item>
                              </>
                            )}
                          </Menu.Dropdown>
                        </Menu>
                      )}
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
          <Text size={FS.micro} c="dimmed" p="md">
            People who open this database through Argus are given and taken levels from its settings, not here; their roles are locked between sessions.
            Giving an application role a level adds to what it already has and takes nothing away.
          </Text>
        </ScrollArea>
      )}
    </Box>
  )
}
