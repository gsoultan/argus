import { useState } from 'react'
import {
  Badge, Box, Button, Group, Loader, PasswordInput, ScrollArea, Select, Switch, Table, Tabs, Text, TextInput,
} from '@mantine/core'
import { FS, INK } from '~/theme'
import { PanelFrame, type AdminOp, type AdminPanelProps } from '~/components/DatabaseAdmin'
import { ConfirmOp, Failed, RefreshButton, stickyHead, useRead, when, type Pending } from '~/components/DatabaseMSSQLDBA'

/*
SQL Server replication for one database, what SSMS spreads over Always On High
Availability, Replication and log shipping: the availability group the
database is in and how far behind each replica is; its transactional and
snapshot publications, their articles, push subscriptions and agents; and the
log shipping it takes part in.

Each read and change is a fixed operation the agent runs as the admin login
(internal/tunnel/mssqlrepl.go). Every change opens a confirmation with the
agent's own preview; passwords show there only as placeholders, filled in by
the agent when it runs. Setting a server up as its distributor, failing over,
removing the database from its group and ending a publication always wait for
approval.
*/

interface Replica {
  server: string; local?: boolean; role: string; mode: string; failover: string; health: string; connected: string
  syncState: string; suspended?: string; sendKb: number; redoKb: number; lastCommit?: string; lagS?: number
}
interface AgentRun { status: string; message?: string; at?: string; latencyMs?: number }
interface Publication {
  name: string; kind: string; active: boolean
  articles: { name: string; schema: string; table: string }[]
  subscriptions: { subscriber: string; db: string; status: string; agent?: AgentRun }[]
  snapshot?: AgentRun
}
interface LogShipping {
  role: 'primary' | 'secondary'
  lastBackup?: string; lastBackupFile?: string; backupAlertMin?: number; secondaries?: string[]
  primary?: string; lastCopied?: string; lastRestored?: string; restoreLagMin?: number; restoreAlertMin?: number
}
export interface MSReplication {
  server: string; edition: string; hadrEnabled: boolean; recovery: string
  distributor: boolean; distribution?: string; replDir?: string; published: boolean; subscribed: boolean
  ag?: { group: string; clusterType: string; localRole: string; replicas: Replica[] }
  groups?: { name: string; clusterType: string }[]
  publications?: Publication[]
  tables?: { schema: string; name: string; pk: boolean }[]
  databases?: string[]
  logShipping?: LogShipping
}

type Repl = NonNullable<AdminOp['msrepl']>
const op = (kind: string, msrepl?: Repl): AdminOp => ({ kind, msrepl })

const AGENT_COLOR: Record<string, string> = { succeeded: 'teal', idle: 'teal', 'in progress': 'sky', started: 'sky', retrying: 'amber', failed: 'rose' }
const lower = (s: string) => s.toLowerCase().replace(/_/g, ' ')
const kb = (n: number) => (n < 1024 ? `${n} KB` : `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} MB`)
/** How far behind a replica is, in words. */
export function lagWords(r: Pick<Replica, 'lagS' | 'role'>): string {
  if (r.role === 'PRIMARY') return ''
  if (r.lagS === undefined) return '—'
  if (r.lagS < 1) return 'up to date'
  if (r.lagS < 120) return `${r.lagS} s behind`
  return `${Math.round(r.lagS / 60)} min behind`
}

function Agent({ run }: { run?: AgentRun }) {
  if (!run) return <Text span size={FS.meta} c="dimmed">no run yet</Text>
  return (
    <span title={run.message}>
      <Badge size="xs" variant="light" color={AGENT_COLOR[run.status] ?? 'slate'}>{run.status}</Badge>
      {run.latencyMs ? <Text span size={FS.micro} c="dimmed" ml={4}>{run.latencyMs} ms latency</Text> : null}
      {run.at && <Text span size={FS.micro} c="dimmed" ml={4}>{when(run.at)}</Text>}
    </span>
  )
}

export function MSSQLReplication({ admin, narrow, onBack }: AdminPanelProps) {
  const { data, failure, busy, refresh } = useRead<MSReplication>(admin, { kind: 'ms.replication' }, 30_000)
  const [pending, setPending] = useState<Pending | null>(null)
  const [tab, setTab] = useState<string | null>('ag')
  return (
    <PanelFrame title="Replication" narrow={narrow} onBack={onBack} right={<RefreshButton busy={busy} onClick={() => void refresh()} />}
      blurb="Copies of this database on other servers: its availability group and each replica's lag, its publications and subscribers, and its log shipping.">
      {failure ? <Failed failure={failure} /> : !data ? <Loader size={16} m="md" /> : (
        <Tabs value={tab} onChange={setTab} style={{ flex: 1, display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <Tabs.List px="md">
            <Tabs.Tab value="ag">Availability group</Tabs.Tab>
            <Tabs.Tab value="pubs">Publications</Tabs.Tab>
            <Tabs.Tab value="ls">Log shipping</Tabs.Tab>
          </Tabs.List>
          <ScrollArea style={{ flex: 1 }} type="auto">
            <Box p="md">
              {tab === 'ag' && <AvailabilityGroup data={data} narrow={narrow} ask={setPending} />}
              {tab === 'pubs' && <Publications data={data} narrow={narrow} ask={setPending} />}
              {tab === 'ls' && <LogShippingView data={data} ask={setPending} />}
            </Box>
          </ScrollArea>
        </Tabs>
      )}
      <ConfirmOp admin={admin} pending={pending} onClose={() => setPending(null)} onDone={() => { setPending(null); void refresh() }} />
    </PanelFrame>
  )
}

type Ask = (p: Pending) => void

/* ── Availability group ──────────────────────────────────────────────────── */

function AvailabilityGroup({ data, narrow, ask }: { data: MSReplication; narrow: boolean; ask: Ask }) {
  const [group, setGroup] = useState<string | null>(null)
  const [force, setForce] = useState(false)
  const ag = data.ag
  if (!data.hadrEnabled) {
    return <Text size={FS.body}>Always On availability groups are not enabled on {data.server}. A server administrator turns them on in SQL Server Configuration Manager (or with <code>mssql-conf set hadr.hadrenabled 1</code> on Linux) and restarts the service.</Text>
  }
  if (!ag) {
    const groups = data.groups ?? []
    const simple = data.recovery === 'SIMPLE'
    return (
      <>
        <Text size={FS.body}>This database is not in an availability group.</Text>
        {groups.length === 0 ? (
          <Text size={FS.meta} c="dimmed" mt={4}>{data.server} is not the primary of any availability group. Create the group and its replicas first; the database can then be added here.</Text>
        ) : (
          <>
            {simple && <Text size={FS.meta} c={INK.pending} mt={4}>It uses the simple recovery model; a database in a group needs full recovery and a full backup first (Settings, Backups).</Text>}
            <Group mt="md" gap="sm" align="end">
              <Select size="xs" label="Availability group" data={groups.map((g) => g.name)} value={group} onChange={setGroup} w={240} />
              <Button size="xs" disabled={!group || simple} onClick={() => ask({
                op: op('ms.ag.add', { group: group! }), title: `Add to ${group}`, verb: 'Add', done: 'Added to the group',
                words: 'The database joins the group on this primary. Each secondary then needs it seeded: automatic seeding copies it; otherwise restore it there WITH NORECOVERY and join it.',
              })}>Add…</Button>
            </Group>
          </>
        )}
      </>
    )
  }
  const local = ag.replicas.find((r) => r.local)
  const primary = ag.localRole === 'PRIMARY'
  const suspended = Boolean(local?.suspended)
  const noCluster = ag.clusterType.toLowerCase() === 'none'
  return (
    <>
      <Text size={FS.body}>
        In <b>{ag.group}</b> <Text span c="dimmed">({lower(ag.clusterType)} cluster)</Text>; this server is the <b>{lower(ag.localRole)}</b>.
      </Text>
      <Table verticalSpacing={4} fz={12} mt="sm" striped>
        <Table.Thead style={stickyHead}><Table.Tr>
          <Table.Th>Replica</Table.Th><Table.Th>Role</Table.Th>{!narrow && <Table.Th>Commit</Table.Th>}<Table.Th>State</Table.Th>
          {!narrow && <Table.Th>To send</Table.Th>}{!narrow && <Table.Th>To redo</Table.Th>}<Table.Th>Lag</Table.Th>
        </Table.Tr></Table.Thead>
        <Table.Tbody>
          {ag.replicas.map((r) => (
            <Table.Tr key={r.server}>
              <Table.Td>{r.server}{r.local && <Badge size="xs" variant="outline" ml={4}>this server</Badge>}</Table.Td>
              <Table.Td>{lower(r.role)}</Table.Td>
              {!narrow && <Table.Td>{lower(r.mode)}, {lower(r.failover)} failover</Table.Td>}
              <Table.Td>
                <Badge size="xs" variant="light" color={r.health === 'HEALTHY' ? 'teal' : r.health === 'PARTIALLY_HEALTHY' ? 'amber' : 'rose'}>{lower(r.syncState || r.health || 'unknown')}</Badge>
                {r.suspended && <Badge size="xs" variant="outline" color="amber" ml={4}>{lower(r.suspended)}</Badge>}
                {r.connected && r.connected !== 'CONNECTED' && <Badge size="xs" variant="outline" color="rose" ml={4}>{lower(r.connected)}</Badge>}
              </Table.Td>
              {!narrow && <Table.Td>{r.role === 'PRIMARY' ? '' : kb(r.sendKb)}</Table.Td>}
              {!narrow && <Table.Td>{r.role === 'PRIMARY' ? '' : kb(r.redoKb)}</Table.Td>}
              <Table.Td style={{ color: (r.lagS ?? 0) >= 60 ? INK.pending : undefined }}>{lagWords(r)}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      <Group mt="md" gap="xs" wrap="wrap">
        {suspended
          ? <Button size="xs" onClick={() => ask({ op: op('ms.ag.resume'), title: 'Resume data movement', verb: 'Resume', done: 'Resumed',
            words: 'This replica starts sending (or applying) the database’s log again and catches up.' })}>Resume…</Button>
          : <Button size="xs" variant="default" onClick={() => ask({ op: op('ms.ag.suspend'), title: 'Suspend data movement', verb: 'Suspend', done: 'Suspended',
            words: primary ? 'The primary stops sending log to every secondary. The log cannot be truncated while it is suspended, so it grows until data movement resumes.'
              : 'This secondary stops applying log; it falls behind, and the primary’s log grows until it resumes.' })}>Suspend…</Button>}
        {!primary && <>
          <Switch size="xs" label="Allow data loss" checked={force || noCluster} disabled={noCluster} onChange={(e) => setForce(e.currentTarget.checked)}
            title={noCluster ? 'A group with no cluster fails over only by force.' : undefined} />
          <Button size="xs" color="rose" variant="light" onClick={() => ask({
            op: op('ms.ag.failover', { group: ag.group, force: force || noCluster || undefined }), title: `Fail ${ag.group} over to ${data.server}`,
            verb: 'Fail over', danger: true, done: 'Failed over',
            words: force || noCluster
              ? 'This replica becomes the primary at once, even if it is behind: transactions it has not received are lost. The old primary’s databases are suspended until they are resumed.'
              : 'This synchronized secondary becomes the primary with no data lost; applications reconnect through the listener.',
          })}>Fail over…</Button>
        </>}
        {primary && <Button size="xs" color="rose" variant="subtle" onClick={() => ask({
          op: op('ms.ag.remove', { group: ag.group }), title: `Remove from ${ag.group}`, verb: 'Remove', danger: true, done: 'Removed from the group',
          words: 'The database leaves the group. It stays here as an ordinary database; its copies on the secondaries are left restoring and no longer follow it.',
        })}>Remove from group…</Button>}
      </Group>
    </>
  )
}

/* ── Publications ────────────────────────────────────────────────────────── */

function Publications({ data, narrow, ask }: { data: MSReplication; narrow: boolean; ask: Ask }) {
  const [dir, setDir] = useState(data.replDir ?? '')
  const [name, setName] = useState('')
  const [kind, setKind] = useState<'transactional' | 'snapshot'>('transactional')
  if (!data.distributor) {
    return (
      <>
        <Text size={FS.body}>{data.server} has no distributor, so nothing on it can be published.</Text>
        <Text size={FS.meta} c="dimmed" mt={4}>Argus can make this server its own distributor: a distribution database here, and a snapshot folder the SQL Server Agent jobs write to. It changes the whole server and waits for approval.</Text>
        <Group mt="md" gap="sm" align="end">
          <TextInput size="xs" label="Snapshot folder on the server" value={dir} onChange={(e) => setDir(e.currentTarget.value)} w={360} placeholder="/var/opt/mssql/ReplData" />
          <Button size="xs" disabled={!dir.trim()} onClick={() => ask({
            op: op('ms.repl.distributor', { workingDir: dir.trim() }), title: `Make ${data.server} a distributor`, verb: 'Set up', danger: true, done: 'Distributor set up',
            words: 'Creates the distribution database and registers this server as its own publisher. The agents sign in with the admin login; its password goes in at run time and is not recorded.',
          })}>Set up…</Button>
        </Group>
      </>
    )
  }
  const pubs = data.publications ?? []
  if (!data.published) {
    return (
      <>
        <Text size={FS.body}>Publishing is off for this database.</Text>
        <Button size="xs" mt="sm" onClick={() => ask({
          op: op('ms.repl.publish', { enabled: true }), title: 'Turn publishing on', verb: 'Turn on', done: 'Publishing on',
          words: 'Lets this database have transactional and snapshot publications. Nothing is published until a publication and its articles are added.',
        })}>Turn publishing on…</Button>
      </>
    )
  }
  const ok = /^[A-Za-z_][A-Za-z0-9_ -]{0,127}$/.test(name.trim())
  return (
    <>
      {pubs.length === 0 && <Text size={FS.body}>No publications yet.</Text>}
      {pubs.map((p) => <PublicationCard key={p.name} p={p} data={data} narrow={narrow} ask={ask} />)}
      <Text size={FS.body} fw={600} mt="lg">New publication</Text>
      <Group mt={4} gap="sm" align="end" wrap="wrap">
        <TextInput size="xs" label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} w={220} error={name && !ok ? 'Letters, digits, spaces, - and _' : undefined} />
        <Select size="xs" label="Kind" data={[{ value: 'transactional', label: 'Transactional' }, { value: 'snapshot', label: 'Snapshot' }]}
          value={kind} onChange={(v) => setKind(v === 'snapshot' ? 'snapshot' : 'transactional')} allowDeselect={false} w={160} />
        <Button size="xs" disabled={!ok} onClick={() => ask({
          op: op('ms.repl.pub.add', { publication: name.trim(), kind }), title: `Add publication ${name.trim()}`, verb: 'Add', done: 'Publication added',
          words: kind === 'transactional'
            ? 'A transactional publication sends each committed change to its subscribers within seconds, after a first snapshot. Add the tables to it next.'
            : 'A snapshot publication copies its tables whole each time its snapshot runs; nothing in between.',
        })}>Add…</Button>
      </Group>
      {pubs.length === 0 && (
        <Button size="xs" mt="lg" variant="subtle" color="rose" onClick={() => ask({
          op: op('ms.repl.publish', { enabled: false }), title: 'Turn publishing off', verb: 'Turn off', danger: true, done: 'Publishing off',
          words: 'This database stops being a publisher; its Log Reader Agent stops.',
        })}>Turn publishing off…</Button>
      )}
    </>
  )
}

function PublicationCard({ p, data, narrow, ask }: { p: Publication; data: MSReplication; narrow: boolean; ask: Ask }) {
  const [table, setTable] = useState<string | null>(null)
  const [server, setServer] = useState('')
  const [db, setDb] = useState('')
  const [login, setLogin] = useState('')
  const [pw, setPw] = useState('')
  const published = new Set(p.articles.map((a) => `${a.schema}.${a.table}`))
  const transactional = p.kind === 'transactional'
  const tables = (data.tables ?? []).filter((t) => !published.has(`${t.schema}.${t.name}`))
    .map((t) => ({ value: `${t.schema}.${t.name}`, label: `${t.schema}.${t.name}${transactional && !t.pk ? ' (no primary key)' : ''}`, disabled: transactional && !t.pk }))
  const remote = server.trim() !== ''
  const canSub = db.trim() !== '' && (!remote || (login.trim() !== '' && pw !== ''))
  const pub = p.name
  return (
    <Box mb="lg" pb="md" style={{ borderBottom: '1px solid var(--color-line)' }}>
      <Group gap="xs">
        <Text size={FS.body} fw={600}>{p.name}</Text>
        <Badge size="xs" variant="light">{p.kind}</Badge>
        {!p.active && <Badge size="xs" variant="outline" color="amber">inactive</Badge>}
        <Text size={FS.meta} c="dimmed">Snapshot:</Text><Agent run={p.snapshot} />
      </Group>
      <Group gap="xs" mt="xs">
        <Button size="compact-xs" variant="default" disabled={p.articles.length === 0} onClick={() => ask({
          op: op('ms.repl.snapshot', { publication: pub }), title: `Run the snapshot of ${pub}`, verb: 'Run', done: 'Snapshot started',
          words: 'The Snapshot Agent writes every article’s schema and rows to the snapshot folder; subscriptions waiting for one then apply it.',
        })}>Run snapshot…</Button>
        <Button size="compact-xs" variant="subtle" color="rose" onClick={() => ask({
          op: op('ms.repl.pub.drop', { publication: pub }), title: `End publication ${pub}`, verb: 'End publication', danger: true, done: 'Publication ended',
          words: 'Drops its subscriptions and the publication. Subscribers keep the tables they have, but nothing more reaches them.',
        })}>End publication…</Button>
      </Group>

      <Text size={FS.meta} fw={600} mt="sm">Articles</Text>
      {p.articles.length === 0 && <Text size={FS.meta} c="dimmed">None; add the tables to publish.</Text>}
      {p.articles.map((a) => (
        <Group key={a.name} gap={6}>
          <Text size={FS.meta}>{a.schema}.{a.table}{a.name !== a.table && <Text span c="dimmed"> as {a.name}</Text>}</Text>
          <Button size="compact-xs" variant="subtle" color="rose" onClick={() => ask({
            op: op('ms.repl.article.drop', { publication: pub, article: a.name }), title: `Stop publishing ${a.schema}.${a.table}`, verb: 'Drop article', danger: true, done: 'Article dropped',
            words: 'Its changes stop reaching subscribers; their copy of the table stays. The publication needs a new snapshot.',
          })}>Drop</Button>
        </Group>
      ))}
      <Group gap="sm" align="end" mt={4}>
        <Select size="xs" placeholder="Add a table" data={tables} value={table} onChange={setTable} searchable w={280} nothingFoundMessage="No table" aria-label="Table to publish" />
        <Button size="xs" variant="default" disabled={!table} onClick={() => {
          const dot = table!.indexOf('.')
          const schema = table!.slice(0, dot); const t = table!.slice(dot + 1)
          ask({
            op: op('ms.repl.article.add', { publication: pub, schema, table: t }), title: `Publish ${table}`, verb: 'Add article', done: 'Article added',
            words: 'The table joins the publication. Existing subscriptions get it with the next snapshot.',
          })
        }}>Add…</Button>
      </Group>

      <Text size={FS.meta} fw={600} mt="sm">Subscriptions</Text>
      {p.subscriptions.length === 0 ? <Text size={FS.meta} c="dimmed">None.</Text> : (
        <Table verticalSpacing={3} fz={12}>
          <Table.Thead><Table.Tr><Table.Th>Subscriber</Table.Th><Table.Th>Database</Table.Th>{!narrow && <Table.Th>Status</Table.Th>}<Table.Th>Agent</Table.Th><Table.Th></Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {p.subscriptions.map((s) => {
              const here = s.subscriber.toLowerCase() === data.server.toLowerCase()
              return (
                <Table.Tr key={`${s.subscriber}/${s.db}`}>
                  <Table.Td>{s.subscriber}{here && <Text span c="dimmed"> (this server)</Text>}</Table.Td>
                  <Table.Td>{s.db}</Table.Td>
                  {!narrow && <Table.Td>{s.status}</Table.Td>}
                  <Table.Td><Agent run={s.agent} /></Table.Td>
                  <Table.Td><Button size="compact-xs" variant="subtle" color="rose" onClick={() => ask({
                    op: op('ms.repl.sub.drop', { publication: pub, subscriber: here ? undefined : s.subscriber, subscriberDb: s.db }),
                    title: `Drop the subscription of ${s.subscriber}.${s.db}`, verb: 'Drop subscription', danger: true, done: 'Subscription dropped',
                    words: 'Its Distribution Agent stops; the subscriber keeps the rows it has.',
                  })}>Drop</Button></Table.Td>
                </Table.Tr>
              )
            })}
          </Table.Tbody>
        </Table>
      )}
      <Group gap="sm" align="end" mt={4} wrap="wrap">
        <TextInput size="xs" label="Subscriber server" placeholder="this server" value={server} onChange={(e) => setServer(e.currentTarget.value)} w={200} />
        {remote
          ? <TextInput size="xs" label="Database there" value={db} onChange={(e) => setDb(e.currentTarget.value)} w={160} />
          : <Select size="xs" label="Database there" data={data.databases ?? []} value={db || null} onChange={(v) => setDb(v ?? '')} searchable w={180} />}
        {remote && <>
          <TextInput size="xs" label="Login there" value={login} onChange={(e) => setLogin(e.currentTarget.value)} w={140} />
          <PasswordInput size="xs" label="Its password" value={pw} onChange={(e) => setPw(e.currentTarget.value)} w={160} />
        </>}
        <Button size="xs" variant="default" disabled={!canSub} onClick={() => ask({
          op: op('ms.repl.sub.add', remote
            ? { publication: pub, subscriber: server.trim(), subscriberDb: db.trim(), login: login.trim(), password: pw }
            : { publication: pub, subscriberDb: db.trim() }),
          title: `Subscribe ${remote ? server.trim() : 'this server'}.${db.trim()}`, verb: 'Subscribe', done: 'Subscription added',
          words: `A push subscription: the Distribution Agent runs here and delivers to ${remote ? 'that server as that login' : 'this server as the admin login'}. The database there must exist; the next snapshot creates the tables.${remote ? ' The password is sent to the agent only, not recorded.' : ''}`,
        })}>Subscribe…</Button>
      </Group>
    </Box>
  )
}

/* ── Log shipping ────────────────────────────────────────────────────────── */

function LogShippingView({ data, ask }: { data: MSReplication; ask: Ask }) {
  const ls = data.logShipping
  if (!ls) {
    return <Text size={FS.body}>This database takes no part in log shipping. It is set up from SQL Server Management Studio (database properties, Transaction Log Shipping); once it is, its backup, copy and restore jobs show and run here.</Text>
  }
  const job = (kind: 'ms.ls.backup' | 'ms.ls.copy' | 'ms.ls.restore', what: string, words: string) => (
    <Button size="xs" variant="default" onClick={() => ask({ op: op(kind), title: `Run the ${what} job`, verb: 'Run', done: `${what[0]!.toUpperCase()}${what.slice(1)} job started`, words })}>Run {what} now…</Button>
  )
  if (ls.role === 'primary') {
    const late = ls.lastBackup && ls.backupAlertMin ? (Date.now() - new Date(ls.lastBackup).getTime()) / 60_000 > ls.backupAlertMin : !ls.lastBackup
    return (
      <>
        <Text size={FS.body}>This database is a log shipping <b>primary</b>, shipping to {(ls.secondaries ?? []).join(', ') || 'no secondary yet'}.</Text>
        <Text size={FS.meta} mt={4} c={late ? INK.pending : undefined}>Last log backup: {ls.lastBackup ? when(ls.lastBackup) : 'none yet'}{ls.backupAlertMin ? ` (alert after ${ls.backupAlertMin} min)` : ''}</Text>
        {ls.lastBackupFile && <Text size={FS.micro} c="dimmed" style={{ wordBreak: 'break-all' }}>{ls.lastBackupFile}</Text>}
        <Group mt="md">{job('ms.ls.backup', 'backup', 'SQL Server Agent runs the log shipping backup job now, besides its schedule.')}</Group>
      </>
    )
  }
  const late = (ls.restoreLagMin ?? 0) > (ls.restoreAlertMin || Infinity)
  return (
    <>
      <Text size={FS.body}>This database is a log shipping <b>secondary</b> of {ls.primary}.</Text>
      <Text size={FS.meta} mt={4}>Last copied: {when(ls.lastCopied)}</Text>
      <Text size={FS.meta} c={late ? INK.pending : undefined}>Last restored: {when(ls.lastRestored)}{ls.restoreLagMin !== undefined ? `, ${ls.restoreLagMin} min behind` : ''}{ls.restoreAlertMin ? ` (alert after ${ls.restoreAlertMin} min)` : ''}</Text>
      <Group mt="md" gap="xs">
        {job('ms.ls.copy', 'copy', 'SQL Server Agent copies the primary’s new log backups here now.')}
        {job('ms.ls.restore', 'restore', 'SQL Server Agent restores the copied log backups now; sessions reading a standby database are disconnected while it does.')}
      </Group>
    </>
  )
}
