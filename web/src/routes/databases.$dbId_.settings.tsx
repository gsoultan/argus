import {
  ActionIcon, Alert, Badge, Box, Button, Checkbox, Code, CopyButton, Group, Modal, NumberInput, SegmentedControl, Select,
  Stack, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconCheck, IconCopy, IconInfoCircle, IconPlus, IconTrash, IconUser, IconUsersGroup } from '@tabler/icons-react'
import { useState } from 'react'
import { PageBody, PageHeader, SectionCard } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { notifyError, notifyOk } from '~/lib/notify'
import { isConfigured } from '~/lib/live'
import {
  databasePoolerSetupQuery, databaseQuery, dbAssignmentsQuery, meQuery, teamsQuery, useDeleteDatabase, useRemoveDBAssignment,
  useSetDBAssignment, useUpdateDatabase, usersQuery,
} from '~/lib/queries'
import { guard } from '~/lib/guard'
import { FS } from '~/theme'
import { engineOf } from '~/lib/engines'
import type { DBLabel, DBLevel, DBPolicy, Database } from '~/types/domain'

/**
 * Who can connect to one database, and its safety settings. Admin only.
 *
 * Levels are plain words; Argus turns them into Postgres roles. The default
 * when giving Write or above on a production database is "needs approval
 * each time", which is the one security step most people ever meet.
 */
export const Route = createFileRoute('/databases/$dbId_/settings')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  component: DatabaseSettings,
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databaseQuery(params.dbId)).catch(() => undefined) : undefined,
    ]),
})

const LEVEL_NAMES: { value: DBLevel; label: string }[] = [
  { value: 'read', label: 'Read' }, { value: 'write', label: 'Write' }, { value: 'owner', label: 'Owner' }, { value: 'dba', label: 'DBA' },
]

/** The levels, with what each one means on this engine. */
function levelsFor(db: Database): { value: DBLevel; label: string; blurb: string }[] {
  const info = engineOf(db.engine)
  return LEVEL_NAMES.map((l) => ({ ...l, blurb: info.levels[l.value] }))
}

function DatabaseSettings() {
  const { dbId } = Route.useParams()
  const { data, error } = useQuery(databaseQuery(dbId))
  const { data: assignments } = useQuery(dbAssignmentsQuery(dbId))
  const { data: teams } = useQuery(teamsQuery())
  const { data: users } = useQuery(usersQuery())
  const remove = useRemoveDBAssignment()
  const del = useDeleteDatabase()
  const navigate = useNavigate()
  const [adding, add] = useDisclosure(false)
  const [tab, setTab] = useState<'who' | 'safety' | 'connection'>('who')

  if (error) {
    return (
      <Box><PageHeader title="Database" crumbs={[{ label: 'Databases', to: '/databases' }]} />
        <PageBody><Alert color="rose" variant="light">{(error as Error).message}</Alert></PageBody></Box>
    )
  }
  if (!data) return null
  const db = data.database
  const info = engineOf(db.engine)
  const LEVELS = levelsFor(db)
  const title = db.engine === 'redis' ? `Redis · db ${db.dbname}` : db.engine === 'rabbitmq' ? `RabbitMQ · ${db.dbname}` : db.dbname
  const names = new Map((users ?? []).map((u) => [u.email, u.displayName]))

  const onRemove = async (id: string, label: string) => {
    try {
      await remove.mutateAsync({ id: dbId, assignmentId: id })
      notifyOk('Removed', db.engine === 'postgres'
        ? `${label} can no longer open ${db.dbname}. Their Postgres role is locked at their next session end.`
        : `${label} can no longer open ${title}. A session already open keeps its user until it ends.`)
    } catch (err) {
      notifyError('Not removed', err)
    }
  }
  const onDelete = async () => {
    if (!window.confirm(`Remove ${title} from Argus? Sessions and recordings are kept; nobody can open it from here any more.`)) return
    try {
      await del.mutateAsync(dbId)
      notifyOk('Removed', `${title} is no longer in Argus.`)
      void navigate({ to: '/databases' })
    } catch (err) {
      notifyError('Not removed', err)
    }
  }

  return (
    <Box>
      <PageHeader
        title={title}
        crumbs={[{ label: 'Databases', to: '/databases' }]}
        status={<Badge variant="light" color={{ production: 'rose', staging: 'amber', development: 'teal' }[db.label]} radius="sm">{db.label}</Badge>}
        description={db.engine === 'postgres'
          ? `On ${db.hostname}. Who can open this database and what they can do; Argus keeps a Postgres role for each person to match.`
          : `${info.label} on ${db.hostname}. Who can open it and what they can do; each person gets their own ${info.label} user for the length of their session.`}
        actions={
          <Group gap="xs">
            <ButtonLink to="/databases/$dbId" params={{ dbId }} variant="default" size="sm">Open</ButtonLink>
            <Button size="sm" leftSection={<IconPlus size={16} />} onClick={add.open}>Give access</Button>
          </Group>
        }
      />
      <PageBody>
        <Stack gap="md">
          <SegmentedControl value={tab} onChange={(v) => setTab(v as 'who' | 'safety' | 'connection')}
            data={[{ label: 'Who can connect', value: 'who' }, { label: 'Safety', value: 'safety' }, { label: 'Connection', value: 'connection' }]} w="fit-content" />

          {tab === 'who' && (
            <Stack gap="md">
              <SectionCard title="Teams and people" description="Admins can always open it at DBA, and that is audited like everyone else.">
                {(assignments?.length ?? 0) === 0 ? (
                  <Text size={FS.meta} c="dimmed">Only admins so far. Give a team Read to start.</Text>
                ) : (
                  <Table verticalSpacing="xs">
                    <Table.Thead><Table.Tr>
                      <Table.Th></Table.Th><Table.Th>Team or person</Table.Th><Table.Th>Access</Table.Th>
                      <Table.Th>Approval</Table.Th><Table.Th></Table.Th>
                    </Table.Tr></Table.Thead>
                    <Table.Tbody>
                      {assignments?.map((g) => (
                        <Table.Tr key={g.id}>
                          <Table.Td w={40}>{g.teamId ? <IconUsersGroup size={16} /> : <IconUser size={16} />}</Table.Td>
                          <Table.Td>
                            <Text fw={600} size={FS.body}>{g.teamId ? g.teamName : names.get(g.userEmail ?? '') ?? g.userEmail}</Text>
                            <Text size={FS.micro} c="dimmed">{g.teamId ? 'Team' : g.userEmail}</Text>
                          </Table.Td>
                          <Table.Td><Badge variant="light" color={g.level === 'read' ? 'slate' : 'azure'} radius="sm">{g.level}</Badge></Table.Td>
                          <Table.Td>
                            {g.needsApproval
                              ? <Badge variant="light" color="amber" radius="sm">Needs approval each time</Badge>
                              : <Text size={FS.meta} c="dimmed">Opens straight away</Text>}
                          </Table.Td>
                          <Table.Td ta="right">
                            <Tooltip label="Remove" withArrow>
                              <ActionIcon variant="subtle" color="rose" aria-label="Remove"
                                onClick={() => void onRemove(g.id, g.teamName || g.userEmail || 'they')}>
                                <IconTrash size={16} />
                              </ActionIcon>
                            </Tooltip>
                          </Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                )}
              </SectionCard>
              <SectionCard title="What each level means">
                <Table verticalSpacing="xs">
                  <Table.Tbody>
                    {LEVELS.map((l) => (
                      <Table.Tr key={l.value}>
                        <Table.Td w={90}><Badge variant="light" color={l.value === 'read' ? 'slate' : 'azure'} radius="sm">{l.label}</Badge></Table.Td>
                        <Table.Td><Text size={FS.meta} c="dimmed">{l.blurb}</Text></Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </SectionCard>
            </Stack>
          )}

          {tab === 'safety' && <Safety db={db} />}
          {tab === 'connection' && (db.engine === 'postgres' ? <Connection db={db} /> : <StoreConnection db={db} />)}

          <SectionCard title="Remove from Argus" description={db.engine === 'postgres'
            ? 'Sessions and recordings are kept. Nothing is changed in Postgres; locked roles stay locked.'
            : `Sessions and recordings are kept. Nothing is changed in ${info.label}.`}>
            <Button color="rose" variant="light" onClick={() => void onDelete()} loading={del.isPending}>Remove {title}</Button>
          </SectionCard>
        </Stack>
        <GiveAccess dbId={dbId} db={db} levels={LEVELS} opened={adding} onClose={add.close}
          teams={(teams ?? []).map((t) => ({ value: t.id, label: t.name }))}
          people={(users ?? []).map((u) => ({ value: u.email, label: `${u.displayName} · ${u.email}` }))} />
      </PageBody>
    </Box>
  )
}

/**
 * Where sessions go. Postgres itself is fixed when the database is added; a
 * PgBouncer in front of it is optional, and the statements an admin runs once
 * so it can authenticate Argus roles come from the control plane, so what is
 * shown is exactly what the agent also makes sure of before each session.
 */
function Connection({ db }: { db: Database }) {
  const update = useUpdateDatabase()
  const [enabled, setEnabled] = useState(db.poolerPort > 0)
  const [host, setHost] = useState(db.poolerHost || '127.0.0.1')
  const [port, setPort] = useState<number | string>(db.poolerPort || 6432)
  const { data: setup } = useQuery({ ...databasePoolerSetupQuery(db.id), enabled: enabled })
  const wantPort = enabled ? Number(port) || 0 : 0
  const wantHost = enabled ? host.trim() : ''
  const dirty = wantPort !== db.poolerPort || (enabled && wantHost !== db.poolerHost)
  const save = async () => {
    try {
      await update.mutateAsync({ id: db.id, poolerHost: wantHost, poolerPort: wantPort })
      notifyOk('Saved', wantPort > 0
        ? `New sessions on ${db.dbname} go through PgBouncer at ${wantHost}:${wantPort}. If it refuses them, Argus falls back to Postgres itself and says so.`
        : `New sessions on ${db.dbname} go straight to Postgres.`)
    } catch (err) {
      notifyError('Not saved', err)
    }
  }
  const copy = (text: string) => (
    <CopyButton value={text}>{({ copied, copy }) => (
      <Tooltip label={copied ? 'Copied' : 'Copy'} withArrow>
        <ActionIcon variant="subtle" size="sm" onClick={copy} aria-label="Copy">{copied ? <IconCheck size={14} /> : <IconCopy size={14} />}</ActionIcon>
      </Tooltip>
    )}</CopyButton>
  )
  const block = (text: string) => (
    <Group align="flex-start" wrap="nowrap" gap={4}>
      <Code block fz="xs" style={{ whiteSpace: 'pre-wrap', flex: 1, minWidth: 0 }}>{text}</Code>
      {copy(text)}
    </Group>
  )
  return (
    <Stack gap="md">
      <SectionCard title="Postgres" description="The agent on the server opens it here. Provisioning of roles always goes this way, as the postgres account over the local socket.">
        <Text size={FS.body} ff="monospace">{db.host}:{db.port}/{db.dbname}</Text>
      </SectionCard>
      <SectionCard title="PgBouncer" description="If applications on this server connect through PgBouncer, sessions from Argus can too. Transaction pooling is fine: every statement carries its own role and limits.">
        <Stack gap="sm">
          <Checkbox label="Sessions go through PgBouncer" checked={enabled} onChange={(e) => setEnabled(e.currentTarget.checked)} />
          {enabled && (
            <Group align="flex-end" gap="sm">
              <TextInput label="Address" description="On the server: a loopback address or a socket directory." value={host}
                onChange={(e) => setHost(e.currentTarget.value)} w={260} aria-label="PgBouncer address" />
              <NumberInput label="Port" value={port} onChange={setPort} min={1} max={65535} w={120} aria-label="PgBouncer port" />
            </Group>
          )}
          <Group><Button onClick={() => void save()} disabled={!dirty} loading={update.isPending}>Save</Button></Group>
        </Stack>
      </SectionCard>
      {enabled && (
        <SectionCard title="Set up PgBouncer once" description="PgBouncer authenticates each Argus role with an auth_query. Argus provides a lookup that answers only for roles it made, so PgBouncer learns nothing about anyone else's password. Nothing about your applications' users changes.">
          {!setup ? <Text size={FS.meta} c="dimmed">Loading…</Text> : (
            <Stack gap="sm">
              <Box>
                <Text size={FS.body} fw={600}>1. In Postgres, as the postgres account, in database {db.dbname}</Text>
                <Text size={FS.micro} c="dimmed" mb={4}>The agent also runs these before every session through PgBouncer; running them yourself first lets step 2 work before anyone connects.</Text>
                {block(setup.sql.map((q: string) => q.trimEnd() + ';').join('\n\n'))}
              </Box>
              <Box>
                <Text size={FS.body} fw={600}>2. Let PgBouncer's auth_user use the lookup</Text>
                <Text size={FS.micro} c="dimmed" mb={4}>Replace the placeholder with the role PgBouncer already uses as auth_user.</Text>
                {block(setup.grant)}
              </Box>
              <Box>
                <Text size={FS.body} fw={600}>3. In pgbouncer.ini</Text>
                <Text size={FS.micro} c="dimmed" mb={4}>
                  If PgBouncer already has an auth_query that reads pg_shadow, keep it: it finds Argus roles too. Otherwise point auth_query at the lookup. Clients must use scram-sha-256; a userlist.txt alone cannot know one-time passwords. Then reload PgBouncer.
                </Text>
                {block(setup.ini)}
              </Box>
            </Stack>
          )}
        </SectionCard>
      )}
    </Stack>
  )
}

/**
 * Where a Redis or a RabbitMQ is, and how the agent signs in as its admin
 * there. Nothing here is a credential: for Redis it is the path of the file
 * on the server that holds one.
 */
function StoreConnection({ db }: { db: Database }) {
  const update = useUpdateDatabase()
  const [host, setHost] = useState(db.host)
  const [port, setPort] = useState<number | string>(db.port)
  const [conf, setConf] = useState(db.configPath ?? '')
  const dirty = host.trim() !== db.host || Number(port) !== db.port || (db.engine === 'redis' && conf.trim() !== (db.configPath ?? ''))
  const save = async () => {
    try {
      await update.mutateAsync({ id: db.id, host: host.trim(), port: Number(port) || db.port,
        ...(db.engine === 'redis' ? { configPath: conf.trim() } : {}) })
      notifyOk('Saved', 'New sessions go to the new address; open ones keep theirs.')
    } catch (err) {
      notifyError('Not saved', err)
    }
  }
  return (
    <Stack gap="md">
      <SectionCard title={db.engine === 'redis' ? 'Redis' : 'RabbitMQ management API'}
        description={db.engine === 'redis'
          ? 'On the server itself: a loopback address or the path of its unix socket. The agent refuses anything else.'
          : 'On the server itself, on loopback. The agent refuses anything else.'}>
        <Stack gap="sm">
          <Group align="flex-end" gap="sm">
            <TextInput label="Address" value={host} onChange={(e) => setHost(e.currentTarget.value)} w={280} aria-label="Address" />
            <NumberInput label="Port" value={port} onChange={setPort} min={1} max={65535} w={120} aria-label="Port" />
          </Group>
          {db.engine === 'redis' && (
            <TextInput label="redis.conf" value={conf} onChange={(e) => setConf(e.currentTarget.value)} w={420}
              description="The agent reads requirepass from this file on the server to sign in as admin, only to give and take each person’s ACL user. If the default user has no password, nothing is read." />
          )}
          <Group><Button onClick={() => void save()} disabled={!dirty} loading={update.isPending}>Save</Button></Group>
        </Stack>
      </SectionCard>
      <SectionCard title="How people sign in">
        <Text size={FS.meta} c="dimmed">
          {db.engine === 'redis'
            ? 'Each person gets a Redis ACL user named argus-<login>-<level> with a one-time password for their session, and it is deleted when their last session closes. Redis 6 or later. Argus never touches a user whose name does not start with argus-.'
            : 'The agent runs rabbitmqctl on the server to add a user named argus-<login>-<level> with its tag and permissions on this vhost, the password on standard input, and deletes it when the last session closes. Argus never touches a user whose name does not start with argus-.'}
        </Text>
      </SectionCard>
    </Stack>
  )
}

function GiveAccess({ dbId, db, levels: LEVELS, opened, onClose, teams, people }: {
  dbId: string; db: Database; levels: { value: DBLevel; label: string; blurb: string }[]; opened: boolean; onClose: () => void
  teams: { value: string; label: string }[]; people: { value: string; label: string }[]
}) {
  const [kind, setKind] = useState<'team' | 'person'>('team')
  const [who, setWho] = useState<string | null>(null)
  const [level, setLevel] = useState<DBLevel>('read')
  const [touched, setTouched] = useState(false)
  const [approval, setApproval] = useState(false)
  const set = useSetDBAssignment()
  // Production defaults to approval for write and above, unless the admin
  // decided otherwise for this row.
  const effectiveApproval = touched ? approval : db.label === 'production' && level !== 'read'
  const submit = async () => {
    if (!who) return
    try {
      await set.mutateAsync({ id: dbId, level, needsApproval: effectiveApproval,
        ...(kind === 'team' ? { teamId: who } : { userEmail: who }) })
      notifyOk('Access given', `${level[0]!.toUpperCase() + level.slice(1)} on ${db.dbname}${effectiveApproval ? ', with approval each time' : ''}.`)
      onClose()
      setWho(null); setTouched(false); setLevel('read')
    } catch (err) {
      notifyError('Not given', err)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title={`Give access to ${db.dbname}`} centered>
      <Stack gap="sm">
        <SegmentedControl value={kind} onChange={(v) => { setKind(v as 'team' | 'person'); setWho(null) }}
          data={[{ label: 'A team', value: 'team' }, { label: 'A person', value: 'person' }]} />
        <Select label={kind === 'team' ? 'Team' : 'Person'} data={kind === 'team' ? teams : people} value={who}
          onChange={setWho} searchable placeholder={kind === 'team' ? 'Pick a team' : 'Pick a person'} />
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>Access</Text>
          <SegmentedControl value={level} onChange={(v) => setLevel(v as DBLevel)}
            data={LEVELS.map((l) => ({ label: l.label, value: l.value }))} />
          <Text size={FS.micro} c="dimmed" mt={4}>{LEVELS.find((l) => l.value === level)?.blurb}</Text>
        </Box>
        <Checkbox label="Needs approval each time" checked={effectiveApproval}
          onChange={(e) => { setTouched(true); setApproval(e.currentTarget.checked) }}
          description="Every session at this level waits for an approver first, and lasts as long as they allow." />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void submit()} loading={set.isPending} disabled={!who}>Give access</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function Safety({ db }: { db: Database }) {
  const update = useUpdateDatabase()
  const [label, setLabel] = useState<DBLabel>(db.label)
  const [policy, setPolicy] = useState<DBPolicy>(db.policy ?? {})
  const dirty = label !== db.label || JSON.stringify(policy) !== JSON.stringify(db.policy ?? {})
  const save = async () => {
    try {
      await update.mutateAsync({ id: db.id, label, policy })
      notifyOk('Saved', 'The new limits apply to the next session; running ones keep theirs.')
    } catch (err) {
      notifyError('Not saved', err)
    }
  }
  const num = (k: keyof DBPolicy, label: string, hint: string, def: number, unit: string) => (
    <Group justify="space-between" wrap="nowrap" key={k}>
      <Box><Text size={FS.body} fw={600}>{label}</Text><Text size={FS.micro} c="dimmed">{hint}</Text></Box>
      <NumberInput w={140} value={policy[k] || ''} placeholder={`${def.toLocaleString()} ${unit}`} min={0} suffix={` ${unit}`}
        onChange={(v) => setPolicy({ ...policy, [k]: typeof v === 'number' ? v : 0 })} aria-label={label} />
    </Group>
  )
  return (
    <Stack gap="md">
      <Alert icon={<IconInfoCircle size={16} />} color="azure" variant="light">
        Every value has a safe default. A new database is safe the moment it is added; change these only if this
        one needs something different.
      </Alert>
      <SectionCard title="Label" description="Production defaults to approval for Write and above when you give access.">
        <SegmentedControl value={label} onChange={(v) => setLabel(v as DBLabel)}
          data={[{ label: 'Production', value: 'production' }, { label: 'Staging', value: 'staging' }, { label: 'Development', value: 'development' }]} />
      </SectionCard>
      <SectionCard title="Limits">
        {db.engine === 'postgres' ? (
          <Stack gap="sm">
            {num('statementTimeoutS', 'Stop a statement after', 'A runaway query ends on its own.', 60, 's')}
            {num('idleTxTimeoutS', 'End a transaction left idle after', 'So a forgotten BEGIN does not hold locks.', 120, 's')}
            {num('maxRows', 'Rows a person can see per statement', 'They can always narrow with LIMIT.', 100000, 'rows')}
            {num('idleTimeoutS', 'End idle sessions after', '', 1800, 's')}
            {num('maxSessionS', 'Longest session', 'The role’s one-time password expires with it.', 28800, 's')}
          </Stack>
        ) : (
          <Stack gap="sm">
            {num('statementTimeoutS', db.engine === 'redis' ? 'Stop waiting for a command after' : 'Stop waiting for RabbitMQ after', 'The session then starts a fresh connection.', 60, 's')}
            {num('maxRows', db.engine === 'redis' ? 'Items shown per value or reply' : 'Queues or messages shown at once', 'Larger answers are cut and say so.', 10000, 'items')}
            {db.engine === 'redis' && num('keysLimit', 'Run KEYS only on dbs up to', 'KEYS stops Redis while it walks every key; past this, people use the key browser, which uses SCAN.', 10000, 'keys')}
            {num('idleTimeoutS', 'End idle sessions after', '', 1800, 's')}
            {num('maxSessionS', 'Longest session', 'The person’s user is removed when it ends.', 28800, 's')}
          </Stack>
        )}
      </SectionCard>
      {db.engine !== 'postgres' ? (
        <SectionCard title="Recording">
          <Group justify="space-between"><Box><Text size={FS.body} fw={600}>Commands</Text><Text size={FS.micro} c="dimmed">{db.engine === 'redis'
            ? 'Every command, and every value read in the key browser, is recorded before it runs, in a hash chain the verifier checks. Commands the agent refused are recorded too.'
            : 'Every peek, publish (with its payload), purge and change is recorded before it is sent, in a hash chain the verifier checks.'}</Text></Box>
            <Badge variant="light" color="teal" radius="sm">Always on</Badge></Group>
          <Group justify="space-between" mt="sm"><Box><Text size={FS.body} fw={600}>Replies</Text><Text size={FS.micro} c="dimmed">Never recorded. A SHA-256 of each reply is kept, so a replay can prove what came back without holding it.</Text></Box>
            <Badge variant="light" color="slate" radius="sm">Hash only</Badge></Group>
        </SectionCard>
      ) : (
      <SectionCard title="Recording">
        <Group justify="space-between"><Box><Text size={FS.body} fw={600}>Statements</Text><Text size={FS.micro} c="dimmed">Always recorded, before they run, in a hash chain the verifier checks.</Text></Box>
          <Badge variant="light" color="teal" radius="sm">Always on</Badge></Group>
        <Group justify="space-between" mt="sm"><Box><Text size={FS.body} fw={600}>Query results</Text><Text size={FS.micro} c="dimmed">Never recorded, so customer data stays out of recordings. The row count and a SHA-256 of each result are kept, so a replay can prove what a statement returned without holding it.</Text></Box>
          <Badge variant="light" color="slate" radius="sm">Rows and hash only</Badge></Group>
      </SectionCard>
      )}
      <Group><Button onClick={() => void save()} disabled={!dirty} loading={update.isPending}>Save</Button></Group>
    </Stack>
  )
}
