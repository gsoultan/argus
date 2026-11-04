import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Checkbox, Code, Group, Loader, NumberInput, PasswordInput, ScrollArea, Select,
  Switch, Table, Tabs, Text, Textarea, TextInput, Tooltip,
} from '@mantine/core'
import { IconRefresh } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { notifyOk } from '~/lib/notify'
import {
  AdminRefused, NeedsDBA, PanelFrame, type Admin, type AdminFailure, type AdminOp, type AdminPanelProps,
  type ConnInfo,
} from '~/components/DatabaseAdmin'
import { usePreview } from '~/components/DatabaseExtensions'

/*
Logical replication: what this database publishes and what it subscribes to.

The subscription forms take the publisher's password. It goes, inside one
request, to the agent and its helper, which puts it in the statement Postgres
runs; the statement that is previewed here, recorded and audited has it as
[redacted], and the list never carries it. Argus keeps no copy: Postgres holds
the connection string in pg_subscription as it does for every subscription.
*/

export interface PGPublication {
  name: string
  owner: string
  allTables: boolean
  tables?: string[]
  publish: string[]
  viaRoot?: boolean
  setUpBy?: string
}

export interface PGSubscription {
  name: string
  enabled: boolean
  publisher: string
  conninfo: string
  publications: string[]
  slot?: string
  owner: string
  setUpBy?: string
  workerPid?: number
  receivedLsn?: string
  latestEndLsn?: string
  lastMessageAt?: string
  latestEndAt?: string
  applyLagS?: number | null
  tablesReady: number
  tablesSyncing: number
  tablesWaiting: number
  applyErrors?: number | null
  syncErrors?: number | null
  syncCommit?: string
}

type Form =
  | { mode: 'new-publication' }
  | { mode: 'publication-tables'; pub: PGPublication }
  | { mode: 'drop-publication'; pub: PGPublication }
  | { mode: 'new-subscription' }
  | { mode: 'subscription-connection'; sub: PGSubscription }
  | { mode: 'refresh-subscription'; sub: PGSubscription }
  | { mode: 'drop-subscription'; sub: PGSubscription }

const REFRESH_MS = 10_000

export function fmtLag(s: number | null | undefined): string {
  if (s === null || s === undefined) return '—'
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.floor(s / 60)} min`
  return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`
}

export function subscriptionStatus(s: PGSubscription): { label: string; color: string } {
  if (!s.enabled) return { label: 'disabled', color: 'slate' }
  if (!s.workerPid) return { label: 'enabled, no worker', color: 'amber' }
  if ((s.applyLagS ?? 0) > 300) return { label: 'streaming, behind', color: 'amber' }
  return { label: 'streaming', color: 'teal' }
}

export function ReplicationPanel({ admin, level, narrow, onBack, onAskForAccess }: AdminPanelProps) {
  const [tab, setTab] = useState<string | null>('publications')
  const [pubs, setPubs] = useState<PGPublication[] | null>(null)
  const [subs, setSubs] = useState<PGSubscription[] | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [asOf, setAsOf] = useState<Date | null>(null)
  const [auto, setAuto] = useState(true)
  const [busy, setBusy] = useState(false)
  const [form, setForm] = useState<Form | null>(null)
  const [picked, setPicked] = useState<string | null>(null)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const refresh = useCallback(async () => {
    setBusy(true)
    const [p, s] = await Promise.all([
      admin<PGPublication[]>({ kind: 'publication.list' }),
      admin<PGSubscription[]>({ kind: 'subscription.list' }),
    ])
    if (!alive.current) return
    const err = p.error ?? s.error
    if (err) setFailure(err)
    else { setFailure(null); setPubs(p.data ?? []); setSubs(s.data ?? []) }
    setAsOf(new Date())
    setBusy(false)
  }, [admin])
  useEffect(() => { if (level === 'dba') void refresh() }, [refresh, level])
  useEffect(() => {
    if (!auto || level !== 'dba' || form) return
    const t = setInterval(() => void refresh(), REFRESH_MS)
    return () => clearInterval(t)
  }, [auto, refresh, level, form])

  if (level !== 'dba') {
    return (
      <PanelFrame title="Replication" narrow={narrow} onBack={onBack} blurb="What this database publishes and what it subscribes to.">
        <NeedsDBA level={level} what="Publications and subscriptions" onAskForAccess={onAskForAccess} />
      </PanelFrame>
    )
  }

  const act = async (op: AdminOp, done: string) => {
    const r = await admin(op)
    if (r.error) { setFailure(r.error); return }
    setFailure(null)
    notifyOk(done, r.tag ?? 'Done')
    void refresh()
  }
  const closeForm = () => { setForm(null); void refresh() }
  const newButton = tab === 'publications'
    ? <Button size="xs" onClick={() => setForm({ mode: 'new-publication' })}>New publication</Button>
    : <Button size="xs" onClick={() => setForm({ mode: 'new-subscription' })}>New subscription</Button>

  return (
    <PanelFrame title="Replication" narrow={narrow} onBack={onBack}
      blurb="What this database publishes and what it subscribes to. Logical replication only; every change runs through the server’s postgres account and is recorded."
      right={!form ? (
        <Group gap="xs" wrap="nowrap">
          <Switch size="xs" label={narrow ? '' : 'Every 10 s'} checked={auto} onChange={(e) => setAuto(e.currentTarget.checked)} aria-label="Refresh automatically" />
          <ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>
          {newButton}
        </Group>
      ) : undefined}>
      {form ? (
        <ReplicationForm form={form} admin={admin} pubs={pubs ?? []} onDone={closeForm} onCancel={() => setForm(null)} />
      ) : (
        <>
          <Tabs value={tab} onChange={setTab} px="md">
            <Tabs.List>
              <Tabs.Tab value="publications" fz={FS.meta}>Publications{pubs ? ` · ${pubs.length}` : ''}</Tabs.Tab>
              <Tabs.Tab value="subscriptions" fz={FS.meta}>Subscriptions{subs ? ` · ${subs.length}` : ''}</Tabs.Tab>
              {asOf && <Text size={FS.micro} c="dimmed" ml="auto" style={{ alignSelf: 'center' }}>as of {asOf.toLocaleTimeString()}</Text>}
            </Tabs.List>
          </Tabs>
          {failure && <Box p="md"><AdminRefused error={failure} /></Box>}
          {!pubs || !subs ? (!failure && <Loader size={16} m="md" />) : (
            <ScrollArea style={{ flex: 1 }} type="auto">
              {tab === 'publications' && (
                <>
                  <Table verticalSpacing={4} fz={12} striped highlightOnHover>
                    <Table.Thead>
                      <Table.Tr><Table.Th>Publication</Table.Th><Table.Th>Tables</Table.Th>{!narrow && <Table.Th>Publishes</Table.Th>}{!narrow && <Table.Th>Set up by</Table.Th>}<Table.Th></Table.Th></Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {pubs.map((p) => (
                        <Table.Tr key={p.name} onClick={() => setPicked(picked === 'p:' + p.name ? null : 'p:' + p.name)} style={{ cursor: 'pointer' }}>
                          <Table.Td><Text size={FS.meta} ff="monospace" fw={600}>{p.name}</Text></Table.Td>
                          <Table.Td>
                            <Text size={FS.meta} fw={600}>{p.allTables ? 'All tables' : `${p.tables?.length ?? 0} table${p.tables?.length === 1 ? '' : 's'}`}</Text>
                            <Text size={FS.micro} c="dimmed" ff="monospace" lineClamp={1}>{p.allTables ? 'every table in this database, now and later' : p.tables?.join(', ')}</Text>
                          </Table.Td>
                          {!narrow && <Table.Td><Text size={FS.meta} ff="monospace">{p.publish.join(', ')}</Text></Table.Td>}
                          {!narrow && <Table.Td><Text size={FS.micro} c="dimmed">{p.setUpBy ?? `${p.owner} · not through Argus`}</Text></Table.Td>}
                          <Table.Td ta="right">
                            <Group gap={4} justify="flex-end" wrap="nowrap" onClick={(e) => e.stopPropagation()}>
                              {!p.allTables && <Button size="compact-xs" variant="default" onClick={() => setForm({ mode: 'publication-tables', pub: p })}>Change tables</Button>}
                              <Button size="compact-xs" variant="subtle" color="rose" onClick={() => setForm({ mode: 'drop-publication', pub: p })}>Drop</Button>
                            </Group>
                          </Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                  {pubs.length === 0 && <Text size={FS.meta} c="dimmed" p="md">This database publishes nothing yet.</Text>}
                  <Text size={FS.micro} c="dimmed" p="md">
                    A publication set up outside Argus is listed the same way and can be changed here; the change is recorded. Publishing all tables needs superuser, which the helper has.
                  </Text>
                </>
              )}
              {tab === 'subscriptions' && (
                <>
                  <Table verticalSpacing={4} fz={12} striped highlightOnHover>
                    <Table.Thead>
                      <Table.Tr><Table.Th>Subscription</Table.Th><Table.Th>Publisher</Table.Th>{!narrow && <Table.Th>Publications</Table.Th>}<Table.Th>Status</Table.Th><Table.Th>Tables</Table.Th>{!narrow && <Table.Th>Errors</Table.Th>}<Table.Th></Table.Th></Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {subs.map((s) => {
                        const st = subscriptionStatus(s)
                        const errs = (s.applyErrors ?? 0) + (s.syncErrors ?? 0)
                        return (
                          <Table.Tr key={s.name}>
                            <Table.Td>
                              <Text size={FS.meta} ff="monospace" fw={600}>{s.name}</Text>
                              <Text size={FS.micro} c="dimmed">{s.slot ? `slot ${s.slot}` : 'no slot'}{s.setUpBy ? ` · ${s.setUpBy}` : ` · ${s.owner} · not through Argus`}</Text>
                            </Table.Td>
                            <Table.Td>
                              <Text size={FS.meta} ff="monospace">{s.publisher || '—'}</Text>
                              <Tooltip label={s.conninfo} withArrow multiline maw={420}>
                                <Text size={FS.micro} c="dimmed">{s.conninfo.includes('password=[redacted]') ? 'password not shown' : 'no password in the connection'}</Text>
                              </Tooltip>
                            </Table.Td>
                            {!narrow && <Table.Td><Text size={FS.meta} ff="monospace">{s.publications.join(', ')}</Text></Table.Td>}
                            <Table.Td>
                              <Badge size="xs" variant="light" color={st.color} radius="sm">{st.label}</Badge>
                              <Text size={FS.micro} c="dimmed">
                                {s.enabled && <>lag <b style={{ color: (s.applyLagS ?? 0) > 300 ? INK.denied : undefined }}>{fmtLag(s.applyLagS)}</b></>}
                                {s.workerPid ? ` · worker ${s.workerPid}` : ''}
                                {s.lastMessageAt ? ` · last message ${new Date(s.lastMessageAt).toLocaleTimeString()}` : ''}
                              </Text>
                            </Table.Td>
                            <Table.Td>
                              <Text size={FS.meta}><b>{s.tablesReady}</b> ready{s.tablesSyncing ? <> · <b style={{ color: INK.pending }}>{s.tablesSyncing}</b> copying</> : null}{s.tablesWaiting ? <> · {s.tablesWaiting} waiting</> : null}</Text>
                            </Table.Td>
                            {!narrow && (
                              <Table.Td>
                                {s.applyErrors === null || s.applyErrors === undefined ? <Text size={FS.micro} c="dimmed">—</Text>
                                  : errs ? <Text size={FS.meta} c="rose">{s.applyErrors} apply · {s.syncErrors} sync</Text>
                                    : <Text size={FS.micro} c="dimmed">none</Text>}
                              </Table.Td>
                            )}
                            <Table.Td ta="right">
                              <select defaultValue="" aria-label={`Actions for ${s.name}`}
                                onChange={(e) => {
                                  const v = e.currentTarget.value
                                  e.currentTarget.value = ''
                                  if (v === 'enable') void act({ kind: 'subscription.enable', name: s.name }, `${s.name} enabled`)
                                  if (v === 'disable') void act({ kind: 'subscription.disable', name: s.name }, `${s.name} disabled`)
                                  if (v === 'refresh') setForm({ mode: 'refresh-subscription', sub: s })
                                  if (v === 'connection') setForm({ mode: 'subscription-connection', sub: s })
                                  if (v === 'drop') setForm({ mode: 'drop-subscription', sub: s })
                                }}
                                style={{ fontSize: 12, padding: '2px 6px', borderRadius: 6, border: '1px solid var(--color-line)', background: 'var(--color-surface)', color: 'inherit', maxWidth: 160 }}>
                                <option value="" disabled>Actions…</option>
                                {s.enabled ? <option value="disable">Disable</option> : <option value="enable">Enable</option>}
                                <option value="refresh" disabled={!s.enabled}>Refresh publication</option>
                                <option value="connection">Change connection…</option>
                                <option value="drop">Drop…</option>
                              </select>
                            </Table.Td>
                          </Table.Tr>
                        )
                      })}
                    </Table.Tbody>
                  </Table>
                  {subs.length === 0 && <Text size={FS.meta} c="dimmed" p="md">This database subscribes to nothing yet.</Text>}
                  <Text size={FS.micro} c="dimmed" p="md">
                    Status is the server’s: pg_stat_subscription and the state of each table. Lag is now minus the last applied transaction’s commit time. Error counts come from pg_stat_subscription_stats (Postgres 15 and later); the message itself is in the server log.
                    The publisher’s password is never shown: the list strips it before anything leaves the server.
                  </Text>
                </>
              )}
            </ScrollArea>
          )}
        </>
      )}
    </PanelFrame>
  )
}

/* ── Forms ─────────────────────────────────────────────────────────────── */

function ConnFields({ conn, setConn, note }: { conn: ConnInfo; setConn: (c: ConnInfo) => void; note?: string }) {
  return (
    <>
      <Group grow mt="sm" align="flex-start">
        <TextInput size="xs" label="Publisher host" value={conn.host} onChange={(e) => setConn({ ...conn, host: e.currentTarget.value })} placeholder="eu-db-01.internal" style={{ flex: 3 }} />
        <NumberInput size="xs" label="Port" value={conn.port ?? 5432} min={1} max={65535} onChange={(v) => setConn({ ...conn, port: typeof v === 'number' ? v : 5432 })} style={{ flex: 1 }} />
        <TextInput size="xs" label="Database" value={conn.dbname} onChange={(e) => setConn({ ...conn, dbname: e.currentTarget.value })} style={{ flex: 2 }} />
      </Group>
      <Group grow mt="xs" align="flex-start">
        <TextInput size="xs" label="User" value={conn.user} onChange={(e) => setConn({ ...conn, user: e.currentTarget.value })} autoComplete="off" />
        <PasswordInput size="xs" label="Password" value={conn.password ?? ''} onChange={(e) => setConn({ ...conn, password: e.currentTarget.value })} autoComplete="new-password" />
        <Select size="xs" label="SSL" data={['require', 'verify-ca', 'verify-full', 'prefer', 'disable']} value={conn.sslmode ?? 'require'} onChange={(v) => setConn({ ...conn, sslmode: v ?? 'require' })} />
      </Group>
      <Text size={FS.micro} c="dimmed" mt={6}>
        {note ?? 'Postgres stores this connection string on the server, as it does for every subscription; Argus does not keep a copy and will not show it again. Leave the password empty to use a .pgpass entry or a client certificate on the server.'}
      </Text>
    </>
  )
}

function ReplicationForm({ form, admin, pubs, onDone, onCancel }: {
  form: Form; admin: Admin; pubs: PGPublication[]; onDone: () => void; onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [allTables, setAllTables] = useState(false)
  const [tables, setTables] = useState(form.mode === 'publication-tables' ? (form.pub.tables ?? []).join('\n') : '')
  const [publish, setPublish] = useState<string[]>(['insert', 'update', 'delete', 'truncate'])
  const [conn, setConn] = useState<ConnInfo>({ host: '', port: 5432, dbname: '', user: '', password: '', sslmode: 'require' })
  const [publications, setPublications] = useState('')
  const [copyData, setCopyData] = useState(true)
  const [createSlot, setCreateSlot] = useState(true)
  const [enabled, setEnabled] = useState(true)
  const [publisherGone, setPublisherGone] = useState(false)
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<AdminFailure | null>(null)

  const tableList = tables.split(/[\n,]/).map((t) => t.trim()).filter(Boolean)
  const pubList = publications.split(/[\n,]/).map((t) => t.trim()).filter(Boolean)
  let op: AdminOp | null = null
  let title = ''
  let verb = ''
  let confirmName = ''
  let danger = false
  switch (form.mode) {
    case 'new-publication':
      title = 'New publication'; verb = 'Create publication'
      op = name && (allTables || tableList.length) ? { kind: 'publication.create', name, allTables, tables: allTables ? undefined : tableList, publish } : null
      break
    case 'publication-tables':
      title = `Change the tables of ${form.pub.name}`; verb = 'Set tables'
      op = tableList.length ? { kind: 'publication.tables', name: form.pub.name, tables: tableList } : null
      break
    case 'drop-publication':
      title = `Drop ${form.pub.name}`; verb = 'Drop publication'; confirmName = form.pub.name; danger = true
      op = { kind: 'publication.drop', name: form.pub.name }
      break
    case 'new-subscription':
      title = 'New subscription'; verb = 'Create subscription'
      op = name && conn.host && conn.dbname && conn.user && pubList.length
        ? { kind: 'subscription.create', name, conn, publications: pubList, copyData, createSlot, enabled }
        : null
      break
    case 'subscription-connection':
      title = `Change the connection of ${form.sub.name}`; verb = 'Change connection'
      op = conn.host && conn.dbname && conn.user ? { kind: 'subscription.connection', name: form.sub.name, conn } : null
      break
    case 'refresh-subscription':
      title = `Refresh ${form.sub.name}`; verb = 'Refresh publication'
      op = { kind: 'subscription.refresh', name: form.sub.name, copyData }
      break
    case 'drop-subscription':
      title = `Drop ${form.sub.name}`; verb = 'Drop subscription'; confirmName = form.sub.name; danger = true
      op = { kind: 'subscription.drop', name: form.sub.name, publisherGone }
      break
  }
  const preview = usePreview(admin, op)
  const canRun = !busy && !!op && !!preview.sql && (!confirmName || typed === confirmName)

  const run = async () => {
    if (!op) return
    setBusy(true)
    const r = await admin(op)
    setBusy(false)
    if (r.error) { setFailure(r.error); return }
    notifyOk(title, r.tag ?? 'Done')
    onDone()
  }
  const toggle = (o: string) => setPublish((p) => (p.includes(o) ? p.filter((x) => x !== o) : [...p, o]))

  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={720}>
        <Text fw={650} size={FS.body}>{title}</Text>
        {form.mode === 'new-publication' && (
          <>
            <TextInput size="xs" mt="sm" label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} placeholder="orders_pub" autoComplete="off" />
            <Checkbox size="xs" mt="sm" checked={allTables} onChange={(e) => setAllTables(e.currentTarget.checked)}
              label="All tables: every table in this database, now and later (FOR ALL TABLES)" />
            {!allTables && (
              <Textarea size="xs" mt="sm" label="Tables, one per line, as schema.table" value={tables} onChange={(e) => setTables(e.currentTarget.value)} autosize minRows={3} placeholder={'public.orders\npublic.order_items'} />
            )}
            <Group mt="sm" gap="md">
              <Text size={FS.meta}>Publish:</Text>
              {['insert', 'update', 'delete', 'truncate'].map((o) => (
                <Checkbox key={o} size="xs" label={o} checked={publish.includes(o)} onChange={() => toggle(o)} />
              ))}
            </Group>
          </>
        )}
        {form.mode === 'publication-tables' && (
          <Textarea size="xs" mt="sm" label="Tables, one per line, as schema.table. This replaces the list." value={tables} onChange={(e) => setTables(e.currentTarget.value)} autosize minRows={3} />
        )}
        {form.mode === 'drop-publication' && (
          <Text size={FS.meta} c="dimmed" mt="xs">Subscribers of this publication on other servers keep failing until someone drops their subscriptions there.</Text>
        )}
        {form.mode === 'new-subscription' && (
          <>
            <TextInput size="xs" mt="sm" label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} placeholder="orders_from_eu" autoComplete="off" />
            <ConnFields conn={conn} setConn={setConn} />
            <TextInput size="xs" mt="sm" label="Publications on the publisher, comma-separated" value={publications} onChange={(e) => setPublications(e.currentTarget.value)} placeholder="orders_pub" autoComplete="off" />
            <Checkbox size="xs" mt="sm" checked={copyData} onChange={(e) => setCopyData(e.currentTarget.checked)} label="Copy existing rows first (copy_data). Off when the tables are already in sync." />
            <Checkbox size="xs" mt="xs" checked={createSlot} onChange={(e) => setCreateSlot(e.currentTarget.checked)} label="Create the replication slot on the publisher (create_slot)" />
            <Checkbox size="xs" mt="xs" checked={enabled} onChange={(e) => setEnabled(e.currentTarget.checked)} label="Start it now (enabled)" />
          </>
        )}
        {form.mode === 'subscription-connection' && (
          <ConnFields conn={conn} setConn={setConn} note="Enter the whole connection again, including the password; the previous one is not shown and is replaced." />
        )}
        {form.mode === 'refresh-subscription' && (
          <>
            <Text size={FS.meta} c="dimmed" mt="xs">Picks up tables added to the publication since. The subscription must be enabled.</Text>
            <Checkbox size="xs" mt="sm" checked={copyData} onChange={(e) => setCopyData(e.currentTarget.checked)} label="Copy the new tables’ existing rows (copy_data)" />
          </>
        )}
        {form.mode === 'drop-subscription' && (
          <Checkbox size="xs" mt="sm" checked={publisherGone} onChange={(e) => setPublisherGone(e.currentTarget.checked)}
            label="The publisher is gone: don’t try to remove its slot. The slot on the publisher is then yours to drop by hand, or it keeps WAL there." />
        )}
        {confirmName && (
          <TextInput size="xs" mt="sm" label={`Type ${confirmName} to confirm`} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} autoComplete="off" />
        )}
        <Text size={FS.micro} c="dimmed" mt="md">Recorded as</Text>
        <Code block mt={4} fz={12}>{preview.sql || (preview.error ? `— ${preview.error.error}` : 'Fill the form to see the statement.')}</Code>
        {(form.mode === 'new-subscription' || form.mode === 'subscription-connection') && (
          <Text size={FS.micro} c="dimmed" mt={4}>
            The password travels inside this one request to the agent on the server, which hands it to the helper. It is not written to the recording, the audit log, or Argus’s database.
            If the subscription cannot connect, check the publisher has wal_level = logical, a free replication slot, and a pg_hba.conf line letting this user in from this server.
          </Text>
        )}
        {pubs.length > 0 && form.mode === 'new-subscription' && (
          <Text size={FS.micro} c="dimmed" mt={4}>Publications must exist on the publisher, not here.</Text>
        )}
        {failure && <Box mt="sm"><AdminRefused error={failure} /></Box>}
        <Group mt="md" gap="xs">
          <Button size="xs" color={danger ? 'rose' : undefined} onClick={() => void run()} disabled={!canRun} loading={busy}>{verb}</Button>
          <Button size="xs" variant="default" onClick={onCancel}>Cancel</Button>
        </Group>
      </Box>
    </ScrollArea>
  )
}
