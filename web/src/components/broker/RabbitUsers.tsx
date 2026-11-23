import {
  ActionIcon, Alert, Badge, Box, Button, Card, Checkbox, Code, CopyButton, Drawer, Group, Modal, NumberInput, SegmentedControl,
  SimpleGrid, Stack, Table, Text, TextInput, UnstyledButton,
} from '@mantine/core'
import { IconAlertTriangle, IconCheck, IconCopy, IconPlus, IconSearch, IconUserPlus, IconX } from '@tabler/icons-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { ChoiceCards, ConfirmTyped, MONO, PasswordChoice, Section, Step, madeText, useUserEvents } from '~/components/brokerusers/parts'
import { dataOf, useRedisChange, type ChangeAnswer } from '~/components/redis/useRedisChange'
import type { StoreRequest, StoreSession } from '~/components/useStoreSession'
import { madeBy, type BrokerUserEvent } from '~/lib/kafkaUsers'
import { rabbitPasswordHash } from '~/lib/rabbitHash'
import {
  limitWords, matching, patternProblem, permLines, planPerms, rabbitSettings, rabbitUserFindings, rabbitUserNameProblem,
  type RabbitLimits, type RabbitLoginResult, type RabbitPerms, type RabbitPlan, type RabbitTopicPerm, type RabbitUser, type RabbitUsersData,
} from '~/lib/rabbitUsers'
import { FS } from '~/theme'
import type { Database } from '~/types/domain'

/*
The users that applications sign in to RabbitMQ with on this vhost: what each
can do here in words with its three patterns under them, its connections now,
who made it; and, at Admin (RabbitMQ shows users only to its
administrators), making one, changing its permissions, tags, limits and
password, removing its access to the vhost, and deleting it where it has
none elsewhere.

Only RabbitMQ's password_hash leaves the browser; the sign-in check right
after carries the password once. Argus does not keep it, so it is shown once.
*/

type Made = { user: string; password: string }

const TAGS = [{ value: '', label: 'None (an app)' }, { value: 'monitoring', label: 'Monitoring' }, { value: 'management', label: 'Management' }]

function Perms({ p }: { p: RabbitPerms }) {
  const lines = permLines(p)
  return (
    <Stack gap={2}>
      {lines.length === 0 && <Text size={FS.micro} c="dimmed">Nothing on this vhost</Text>}
      {lines.map((l) => <Text key={l.what} size={FS.micro}><Text span c="dimmed" size={FS.micro}>{l.what}</Text> {l.words}</Text>)}
      <Text size={FS.micro} ff="monospace" c="dimmed">{p.configure || '""'} · {p.write || '""'} · {p.read || '""'}</Text>
    </Stack>
  )
}

function outcome(a: ChangeAnswer): { ok: boolean; text: string } {
  if (a.notApproved) return { ok: false, text: 'Nothing changed: it was not approved.' }
  return a.type === 'result' ? { ok: true, text: '' } : { ok: false, text: a.error ?? 'failed' }
}

export function RabbitUsers({ s, database, names, narrow }: {
  s: StoreSession
  database: Database
  /** The queues and exchanges the other tabs loaded, to say what a pattern matches. */
  names: { queues: string[]; exchanges: string[] }
  narrow: boolean
}) {
  const vhost = database.dbname
  const { change, dialog } = useRedisChange(s, database)
  const events = useUserEvents(database)
  const [data, setData] = useState<RabbitUsersData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [q, setQ] = useState('')
  const [creating, setCreating] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const [made, setMade] = useState<Made | null>(null)
  const [showSessions, setShowSessions] = useState(false)

  const load = useCallback(async () => {
    const a = await s.ask({ op: 'users' })
    const d = dataOf<RabbitUsersData>(a)
    if (!d) setErr(a.error ?? 'could not list the users')
    else { setErr(null); setData(d) }
    void events.refetch()
  }, [s]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => { if (s.status === 'ready') void load() }, [s.status, load])

  const madeMap = useMemo(() => madeBy(events.data?.events ?? []), [events.data])
  const rows = useMemo(() => (data?.users ?? []).filter((u) => !q.trim() || u.name.toLowerCase().includes(q.trim().toLowerCase())), [data, q])
  const findings = useMemo(() => rabbitUserFindings(data?.users ?? []), [data])

  if (!data) return err ? <Alert color="rose" variant="light">{err}</Alert> : <Text size={FS.meta} c="dimmed">Reading the users…</Text>
  const user = open ? data.users.find((u) => u.name === open) ?? null : null
  return (
    <Stack gap="md">
      {findings.length > 0 && (
        <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} title={`${findings.length} finding${findings.length === 1 ? '' : 's'}`}>
          <Stack gap={2}>{findings.map((f) => (
            <Group key={f.user} gap="xs"><Text size={FS.meta}>{f.text}</Text><Button size="compact-xs" variant="light" color="amber" onClick={() => setOpen(f.user)}>Open {f.user}</Button></Group>
          ))}</Stack>
        </Alert>
      )}
      <Card withBorder radius="md" padding={0}>
        <Group p="md" gap="sm" align="flex-start" wrap={narrow ? 'wrap' : 'nowrap'} style={{ borderBottom: '1px solid var(--color-line)' }}>
          <Box style={{ flex: 1, minWidth: 220 }}>
            <Text fw={600} size={FS.body}>Users of apps on {vhost}</Text>
            <Text size={FS.micro} c="dimmed">
              The users with permissions on this vhost, what they can do here, and who made them.
              {data.notShown > 0 && ` ${data.notShown} other user${data.notShown === 1 ? ' has' : 's have'} no permissions here and ${data.notShown === 1 ? 'is' : 'are'} not shown.`}
            </Text>
          </Box>
          <TextInput size="xs" placeholder="Search users" leftSection={<IconSearch size={12} />} value={q} onChange={(e) => setQ(e.currentTarget.value)}
            aria-label="Search users" w={narrow ? '100%' : 220} />
          <Button size="xs" leftSection={<IconUserPlus size={14} />} onClick={() => setCreating(true)}>New user</Button>
        </Group>
        {narrow ? (
          <Stack gap={0}>
            {rows.map((u) => (
              <UnstyledButton key={u.name} onClick={() => setOpen(u.name)} p="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
                <Group gap={6} mb={4}><Text ff="monospace" fw={600} size={FS.meta}>{u.name}</Text><TagBadges tags={u.tags} /></Group>
                <Perms p={u.perms} />
                <Text size={FS.micro} c="dimmed" mt={4}>{u.connections} connection{u.connections === 1 ? '' : 's'} · {madeText(madeMap.get(u.name))}{u.elsewhere?.length ? ` · on ${u.elsewhere.length} other vhost${u.elsewhere.length === 1 ? '' : 's'}` : ''}</Text>
              </UnstyledButton>
            ))}
          </Stack>
        ) : (
          <Table.ScrollContainer minWidth={820}>
            <Table verticalSpacing={8} fz="sm" highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>User</Table.Th><Table.Th>Tags</Table.Th><Table.Th>Can do on {vhost}</Table.Th>
                  <Table.Th>Connections</Table.Th><Table.Th>Other vhosts</Table.Th><Table.Th>Made</Table.Th><Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {rows.map((u) => (
                  <Table.Tr key={u.name}>
                    <Table.Td><Text ff="monospace" fw={600} size={FS.meta}>{u.name}</Text></Table.Td>
                    <Table.Td><TagBadges tags={u.tags} /></Table.Td>
                    <Table.Td maw={360}><Perms p={u.perms} /></Table.Td>
                    <Table.Td><Text size={FS.micro}>{u.connections}</Text></Table.Td>
                    <Table.Td>{u.elsewhere?.length ? <Badge size="sm" variant="light" color="slate" radius="sm" style={{ textTransform: 'none' }}>{u.elsewhere.length}</Badge> : <Text size={FS.micro} c="dimmed">–</Text>}</Table.Td>
                    <Table.Td><Text size={FS.micro} c={madeMap.get(u.name) ? undefined : 'dimmed'}>{madeText(madeMap.get(u.name))}</Text></Table.Td>
                    <Table.Td ta="right"><Button size="compact-xs" variant="default" onClick={() => setOpen(u.name)}>Open</Button></Table.Td>
                  </Table.Tr>
                ))}
                {rows.length === 0 && (
                  <Table.Tr><Table.Td colSpan={7}><Text size={FS.meta} c="dimmed" p="sm">{data.users.length ? 'No user matches.' : `No app user has permissions on ${vhost} yet.`}</Text></Table.Td></Table.Tr>
                )}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
        <Group gap="xs" p="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
          <Checkbox size="xs" checked={showSessions} onChange={(e) => setShowSessions(e.currentTarget.checked)}
            label={`Show Argus's session users · ${data.sessions.length} now`} />
          <Text size={FS.micro} c="dimmed">Made for each person's session and removed when it closes; never changed here.</Text>
        </Group>
        {showSessions && <Text size={FS.micro} ff="monospace" px="sm" pb="sm">{data.sessions.join(', ') || 'None open.'}</Text>}
      </Card>

      {creating && (
        <NewRabbitUser vhost={vhost} names={names} existing={data.users.map((u) => u.name)} narrow={narrow} change={change}
          onClose={() => setCreating(false)} onMade={(m) => { setCreating(false); setMade(m); void load() }} />
      )}
      {made && <MadeRabbitUser s={s} database={database} data={data} made={made} onClose={() => setMade(null)} />}
      {user && (
        <RabbitUserDrawer vhost={vhost} user={user} narrow={narrow} change={change}
          events={(events.data?.events ?? []).filter((e) => e.user === user.name)}
          onClose={() => setOpen(null)} onChanged={() => void load()} onPassword={(m) => setMade(m)} />
      )}
      {dialog}
    </Stack>
  )
}

function TagBadges({ tags }: { tags: string[] }) {
  if (tags.length === 0) return <Text size={FS.micro} c="dimmed">none (app)</Text>
  return <Group gap={4}>{tags.map((t) => <Badge key={t} size="xs" variant="light" color={t === 'administrator' ? 'rose' : 'slate'} radius="sm" style={{ textTransform: 'none' }}>{t}</Badge>)}</Group>
}

const WHAT: { value: RabbitPlan['what']; label: string; hint: string }[] = [
  { value: 'publish', label: 'Publish', hint: 'Send to exchanges' },
  { value: 'consume', label: 'Consume', hint: 'Read queues' },
  { value: 'both', label: 'Publish and consume', hint: 'Both' },
  { value: 'custom', label: 'Custom', hint: 'Write the patterns' },
]

function limitsOf(l: { conns: number | string; chans: number | string }): RabbitLimits {
  return { maxConnections: Number(l.conns) > 0 ? Math.round(Number(l.conns)) : 0, maxChannels: Number(l.chans) > 0 ? Math.round(Number(l.chans)) : 0 }
}

function Matches({ label, pattern, names }: { label: string; pattern: string; names: string[] }) {
  const m = matching(pattern, names)
  return <Text size={FS.micro} c="dimmed">{label}: {m.length === 0 ? 'none now' : `${m.slice(0, 4).join(', ')}${m.length > 4 ? ` and ${m.length - 4} more` : ''}`}</Text>
}

function PatternFields({ perms, onChange, disabled }: { perms: RabbitPerms; onChange: (p: RabbitPerms) => void; disabled?: boolean }) {
  return (
    <SimpleGrid cols={3} spacing="xs">
      {(['configure', 'write', 'read'] as const).map((k) => (
        <TextInput key={k} size="xs" label={k === 'configure' ? 'Configure' : k === 'write' ? 'Write' : 'Read'} styles={MONO} value={perms[k]} disabled={disabled}
          error={patternProblem(perms[k]) ?? undefined} onChange={(e) => { const v = e.currentTarget.value; onChange({ ...perms, [k]: v }) }} />
      ))}
    </SimpleGrid>
  )
}

function NewRabbitUser({ vhost, names, existing, narrow, change, onClose, onMade }: {
  vhost: string; names: { queues: string[]; exchanges: string[] }; existing: string[]; narrow: boolean
  change: (r: StoreRequest) => Promise<ChangeAnswer>
  onClose: () => void; onMade: (m: Made) => void
}) {
  const [name, setName] = useState('')
  const [tag, setTag] = useState('')
  const [password, setPassword] = useState('')
  const [plan, setPlan] = useState<RabbitPlan>({ what: 'both', exchange: '', queue: '', declares: true, custom: { configure: '.*', write: '.*', read: '.*' } })
  const [limits, setLimits] = useState<{ conns: number | string; chans: number | string }>({ conns: '', chans: '' })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const perms = planPerms(plan)
  const nameErr = name ? rabbitUserNameProblem(name, existing) : null
  const permErr = [perms.configure, perms.write, perms.read].map(patternProblem).find(Boolean) ?? null
  const lim = limitsOf(limits)
  const anyLimit = !!(lim.maxConnections || lim.maxChannels)
  const ok = !!name && !nameErr && !permErr && password.length >= 12
  const pub = plan.what === 'publish' || plan.what === 'both'
  const con = plan.what === 'consume' || plan.what === 'both'

  const create = async () => {
    setBusy(true); setErr(null)
    const passwordHash = await rabbitPasswordHash(password)
    const a = await change({ op: 'user_set', brokerUser: { name, create: true, passwordHash, tags: tag ? [tag] : [], perms, ...(anyLimit ? { limits: lim } : {}) } })
    setBusy(false)
    const o = outcome(a)
    if (!o.ok) { setErr(o.text); return }
    onMade({ user: name, password })
  }

  const preview = [
    '# through the management API, as the agent',
    `PUT /api/users/${name || '<name>'}`,
    `  password_hash [redacted], tags ${tag || 'none'}`,
    `PUT /api/permissions/${vhost}/${name || '<name>'}`,
    `  configure ${perms.configure}`, `  write     ${perms.write}`, `  read      ${perms.read}`,
    ...(anyLimit ? [`PUT /api/user-limits/${name || '<name>'}`, `  ${limitWords(lim)}`] : []),
  ].join('\n')

  return (
    <Modal opened onClose={onClose} title={<Text fw={650}>New RabbitMQ user on {vhost}</Text>} size={narrow ? '100%' : 1000} fullScreen={narrow}>
      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="lg" style={{ gridTemplateColumns: narrow ? undefined : 'minmax(0,1fr) 340px' }}>
        <Stack gap="sm">
          <SimpleGrid cols={2} spacing="sm">
            <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value.trim())} styles={MONO} placeholder="invoices-app" data-autofocus error={nameErr ?? undefined} />
            <Stack gap={4}>
              <Text size={FS.meta} fw={500}>Tags</Text>
              <SegmentedControl size="xs" value={tag} onChange={setTag} data={TAGS} />
            </Stack>
          </SimpleGrid>
          <Text size={FS.micro} c="dimmed" mt={-6}>An app needs no tag. Monitoring and Management let it sign in to the management UI; administrator and policymaker are never given from here.</Text>
          <Stack gap={4}>
            <Text size={FS.meta} fw={500}>Password</Text>
            <PasswordChoice value={password} onChange={setPassword} />
          </Stack>
          <Stack gap={6}>
            <Text size={FS.meta} fw={500}>What it does</Text>
            <ChoiceCards value={plan.what} onChange={(what) => setPlan((p) => ({ ...p, what, custom: what === 'custom' ? planPerms(p) : p.custom }))} options={WHAT} />
            <Box p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 10, background: 'var(--mantine-color-default-hover)' }}>
              <Stack gap="xs">
                {plan.what !== 'custom' && (
                  <>
                    <SimpleGrid cols={2} spacing="sm">
                      <TextInput size="xs" label="Publishes to exchanges starting with" styles={MONO} value={plan.exchange} disabled={!pub && !(con && plan.declares)} placeholder="any exchange"
                        onChange={(e) => { const v = e.currentTarget.value.trim(); setPlan((p) => ({ ...p, exchange: v })) }} />
                      <TextInput size="xs" label="Reads queues starting with" styles={MONO} value={plan.queue} disabled={!con} placeholder="any queue"
                        onChange={(e) => { const v = e.currentTarget.value.trim(); setPlan((p) => ({ ...p, queue: v })) }} />
                    </SimpleGrid>
                    <Checkbox size="xs" label="Declares and binds its own queues, with the same start" checked={plan.declares} disabled={!con}
                      onChange={(e) => { const v = e.currentTarget.checked; setPlan((p) => ({ ...p, declares: v })) }} />
                    <Text size={FS.micro} c="dimmed">Leave a field empty for every exchange or queue on {vhost}. The patterns it gets are below; editing one makes it Custom.</Text>
                  </>
                )}
                <PatternFields perms={perms} onChange={(custom) => setPlan((p) => ({ ...p, what: 'custom', custom }))} />
                <Matches label="Writes to" pattern={perms.write} names={[...names.exchanges, ...names.queues]} />
                <Matches label="Reads" pattern={perms.read} names={[...names.queues, ...names.exchanges]} />
              </Stack>
            </Box>
          </Stack>
          <Stack gap={4}>
            <Text size={FS.meta} fw={500}>Limits <Text span size={FS.micro} c="dimmed">· optional, for this user on the whole server</Text></Text>
            <SimpleGrid cols={2} spacing="sm">
              <NumberInput size="sm" placeholder="No limit" aria-label="Most connections" rightSection={<Text size={FS.micro} c="dimmed" pr={6}>connections</Text>} rightSectionWidth={90}
                value={limits.conns} min={0} allowDecimal={false} onChange={(v) => setLimits((l) => ({ ...l, conns: v }))} />
              <NumberInput size="sm" placeholder="No limit" aria-label="Most channels" rightSection={<Text size={FS.micro} c="dimmed" pr={6}>channels</Text>} rightSectionWidth={90}
                value={limits.chans} min={0} allowDecimal={false} onChange={(v) => setLimits((l) => ({ ...l, chans: v }))} />
            </SimpleGrid>
          </Stack>
        </Stack>
        <Stack gap="xs" p="md" style={{ background: 'var(--mantine-color-default-hover)', borderRadius: 10 }}>
          <Text size={FS.meta} fw={600}>Runs on the server</Text>
          <Code block style={{ fontSize: 11.5, whiteSpace: 'pre-wrap' }}>{preview}</Code>
          <Text size={FS.meta} fw={600} mt="xs">Then</Text>
          <Text size={FS.micro}>• {name || 'The user'} opens an AMQP connection to {vhost} with the new password.</Text>
          <Text size={FS.micro}>• Connection details, shown once.</Text>
          <Alert color="sky" variant="light" p="xs" mt="xs">
            <Text size={FS.micro}>The password is hashed in your browser. RabbitMQ gets only the hash; the agent and Argus never see the password.</Text>
          </Alert>
        </Stack>
      </SimpleGrid>
      {err && <Alert color="rose" variant="light" mt="sm">{err}</Alert>}
      <Group mt="md" gap="xs">
        <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>Recorded in the session and in Activity as broker.user.created.</Text>
        <Button variant="default" onClick={onClose}>Cancel</Button>
        <Button onClick={() => void create()} loading={busy} disabled={!ok}>Create {name || 'user'}</Button>
      </Group>
    </Modal>
  )
}

function TopicPermEditor({ rows, onChange, disabled }: { rows: RabbitTopicPerm[]; onChange: (r: RabbitTopicPerm[]) => void; disabled?: boolean }) {
  const [row, setRow] = useState<RabbitTopicPerm>({ exchange: 'amq.topic', write: '.*', read: '.*' })
  const dup = rows.some((r) => r.exchange === row.exchange)
  return (
    <Stack gap={6}>
      {rows.length === 0 && <Text size={FS.micro} c="dimmed">None: routing keys are not limited on any topic exchange.</Text>}
      {rows.map((r) => (
        <Group key={r.exchange} gap={6} wrap="nowrap">
          <Text size={FS.micro} ff="monospace" style={{ flex: 1 }} truncate>{r.exchange} · write {r.write} · read {r.read}</Text>
          {!disabled && <ActionIcon size="sm" variant="subtle" color="slate" aria-label={`Remove topic permission on ${r.exchange}`} onClick={() => onChange(rows.filter((x) => x.exchange !== r.exchange))}><IconX size={12} /></ActionIcon>}
        </Group>
      ))}
      {!disabled && (
        <Group gap={6} align="flex-end" wrap="wrap">
          <TextInput size="xs" label="Topic exchange" styles={MONO} value={row.exchange} onChange={(e) => { const v = e.currentTarget.value.trim(); setRow((r) => ({ ...r, exchange: v })) }} style={{ flex: 1, minWidth: 120 }} />
          <TextInput size="xs" label="Write keys" styles={MONO} value={row.write} w={110} onChange={(e) => { const v = e.currentTarget.value; setRow((r) => ({ ...r, write: v })) }} />
          <TextInput size="xs" label="Read keys" styles={MONO} value={row.read} w={110} onChange={(e) => { const v = e.currentTarget.value; setRow((r) => ({ ...r, read: v })) }} />
          <Button size="xs" variant="default" leftSection={<IconPlus size={12} />} disabled={!row.exchange || dup || !!patternProblem(row.write) || !!patternProblem(row.read)}
            onClick={() => onChange([...rows, row])}>Add</Button>
        </Group>
      )}
    </Stack>
  )
}

function RabbitUserDrawer({ vhost, user, narrow, change, events, onClose, onChanged, onPassword }: {
  vhost: string; user: RabbitUser; narrow: boolean
  change: (r: StoreRequest) => Promise<ChangeAnswer>
  events: BrokerUserEvent[]
  onClose: () => void; onChanged: () => void; onPassword: (m: Made) => void
}) {
  const manage = !user.locked
  const hidden = (user.elsewhere?.length ?? 0) > 0
  const [perms, setPerms] = useState<RabbitPerms>(user.perms)
  const [topics, setTopics] = useState<RabbitTopicPerm[]>(user.topicPerms)
  const [tag, setTag] = useState(user.tags.find((t) => t === 'monitoring' || t === 'management') ?? '')
  const [limits, setLimits] = useState<{ conns: number | string; chans: number | string }>({ conns: user.limits.maxConnections || '', chans: user.limits.maxChannels || '' })
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [renewing, setRenewing] = useState(false)
  const [removing, setRemoving] = useState<'scope' | 'user' | null>(null)
  useEffect(() => { setPerms(user.perms); setTopics(user.topicPerms) }, [user])
  const permsChanged = perms.configure !== user.perms.configure || perms.write !== user.perms.write || perms.read !== user.perms.read
  const topicsChanged = JSON.stringify(topics) !== JSON.stringify(user.topicPerms)
  const tagsChanged = (tag ? [tag] : []).join() !== user.tags.join()
  const lim = limitsOf(limits)
  const limitsChanged = (lim.maxConnections ?? 0) !== (user.limits.maxConnections ?? 0) || (lim.maxChannels ?? 0) !== (user.limits.maxChannels ?? 0)
  const permErr = [perms.configure, perms.write, perms.read].map(patternProblem).find(Boolean)
  const dirty = permsChanged || topicsChanged || (!hidden && (tagsChanged || limitsChanged))

  const run = async (what: string, req: StoreRequest): Promise<boolean> => {
    setBusy(true); setNote(null)
    const o = outcome(await change(req))
    setBusy(false)
    setNote({ ok: o.ok, text: o.ok ? `${what}.` : o.text })
    if (o.ok) onChanged()
    return o.ok
  }
  const save = () => void run('Saved', { op: 'user_set', brokerUser: {
    name: user.name, ...(permsChanged ? { perms } : {}), ...(topicsChanged ? { topicPerms: topics } : {}),
    ...(!hidden && tagsChanged ? { tags: tag ? [tag] : [] } : {}), ...(!hidden && limitsChanged ? { limits: lim } : {}),
  } })
  const made = events.find((e) => e.action === 'broker.user.created')

  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 600}
      title={<Group gap={8}><Text ff="monospace" fw={650}>{user.name}</Text><TagBadges tags={user.tags} /></Group>}>
      <Stack gap="md">
        <Text size={FS.micro} c="dimmed" mt={-8}>{user.connections} connection{user.connections === 1 ? '' : 's'} on {vhost} now · {made ? `made in Argus by ${made.by}` : 'made outside Argus'}</Text>
        {user.locked && <Alert color="slate" variant="light"><Text size={FS.micro}>{user.name} is {user.locked}.</Text></Alert>}
        {user.defaultPassword && <Alert color="amber" variant="light"><Text size={FS.micro}>{user.name} still signs in with its default password.</Text></Alert>}
        {note && <Alert color={note.ok ? 'teal' : 'rose'} variant="light" withCloseButton onClose={() => setNote(null)}>{note.text}</Alert>}
        <Section title={`Permissions on ${vhost}`} hint="regular expressions on queue and exchange names">
          <PatternFields perms={perms} onChange={setPerms} disabled={!manage} />
          <Stack gap={2}>{permLines(perms).map((l) => <Text key={l.what} size={FS.micro}><Text span c="dimmed" size={FS.micro}>{l.what}</Text> {l.words}</Text>)}</Stack>
        </Section>
        <Section title="Topic permissions" hint="routing keys on topic exchanges">
          <TopicPermEditor rows={topics} onChange={setTopics} disabled={!manage} />
        </Section>
        {hidden && (
          <Box p="xs" style={{ border: '1px dashed var(--color-line)', borderRadius: 8 }}>
            <Text size={FS.micro}>Also has permissions on <b>{user.elsewhere!.join(', ')}</b>. Its tags, limits and password, and deleting it, change from here once it has none there.</Text>
          </Box>
        )}
        <Section title="Tags">
          <SegmentedControl size="xs" value={tag} onChange={setTag} data={TAGS} disabled={!manage || hidden} />
        </Section>
        <Section title="Limits" hint="for this user on the whole server">
          <SimpleGrid cols={2} spacing="xs">
            <NumberInput size="xs" label="Most connections" placeholder="No limit" value={limits.conns} min={0} allowDecimal={false} disabled={!manage || hidden}
              onChange={(v) => setLimits((l) => ({ ...l, conns: v }))} />
            <NumberInput size="xs" label="Most channels" placeholder="No limit" value={limits.chans} min={0} allowDecimal={false} disabled={!manage || hidden}
              onChange={(v) => setLimits((l) => ({ ...l, chans: v }))} />
          </SimpleGrid>
        </Section>
        {manage && !hidden && (
          <Section title="Password">
            <Group gap="xs" wrap="nowrap" align="flex-start">
              <Button size="xs" variant="default" onClick={() => setRenewing(true)}>Give it a new password</Button>
              <Text size={FS.micro} c="dimmed">The app needs the new one before it reconnects; the old one stops working at once.</Text>
            </Group>
          </Section>
        )}
        <Section title="Recent changes">
          {events.length === 0 ? <Text size={FS.micro} c="dimmed">None made through Argus.</Text> : (
            <Stack gap={4}>{events.slice(0, 8).map((e, i) => (
              <Text key={i} size={FS.micro}><Text span c="dimmed" size={FS.micro}>{new Date(e.at).toLocaleString()} · {e.by}</Text> {e.action.replace('broker.user.', '')}: {e.text.split(';')[0]}</Text>
            ))}</Stack>
          )}
        </Section>
        {manage && (
          <Group gap="xs" pt="sm" style={{ borderTop: '1px solid var(--color-line)' }} wrap="wrap">
            <Button size="xs" variant="outline" color="rose" onClick={() => setRemoving('scope')}>Remove from {vhost}</Button>
            {!hidden && <Button size="xs" variant="subtle" color="rose" onClick={() => setRemoving('user')}>Delete user</Button>}
            <Box style={{ flex: 1 }} />
            <Button size="xs" onClick={save} loading={busy} disabled={!dirty || !!permErr}>Save</Button>
          </Group>
        )}
      </Stack>
      {renewing && (
        <RenewPassword name={user.name} onClose={() => setRenewing(false)} onRenew={async (password) => {
          const ok = await run('New password set', { op: 'user_set', brokerUser: { name: user.name, passwordHash: await rabbitPasswordHash(password) } })
          if (ok) { setRenewing(false); onPassword({ user: user.name, password }) }
        }} />
      )}
      <ConfirmTyped opened={removing !== null} name={user.name}
        title={removing === 'user' ? `Delete ${user.name}` : `Remove ${user.name} from ${vhost}`}
        text={removing === 'user'
          ? `Deletes the user, its permissions and its limits. RabbitMQ closes its ${user.connections} connection${user.connections === 1 ? '' : 's'}. This cannot be undone.`
          : `Deletes its permissions and topic permissions on ${vhost}. The user stays${hidden ? ', with its access elsewhere' : ''}.`}
        action={removing === 'user' ? 'Delete user' : 'Remove access'}
        onCancel={() => setRemoving(null)}
        onConfirm={async () => {
          const op = removing === 'user' ? 'user_del' : 'user_scope_del'
          const ok = await run(removing === 'user' ? `${user.name} was deleted` : `Removed its access to ${vhost}`, { op, brokerUser: { name: user.name } })
          setRemoving(null)
          if (ok) onClose()
        }} />
    </Drawer>
  )
}

function RenewPassword({ name, onClose, onRenew }: { name: string; onClose: () => void; onRenew: (pw: string) => Promise<void> }) {
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  return (
    <Modal opened onClose={onClose} title={`A new password for ${name}`} size="lg">
      <Stack gap="sm">
        <PasswordChoice value={password} onChange={setPassword} />
        <Alert color="amber" variant="light"><Text size={FS.micro}>The old password stops working as soon as this is set. Put the new one in the app first, or be ready to.</Text></Alert>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} disabled={password.length < 12} onClick={async () => { setBusy(true); await onRenew(password); setBusy(false) }}>Set the new password</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function MadeRabbitUser({ s, database, data, made, onClose }: { s: StoreSession; database: Database; data: RabbitUsersData; made: Made; onClose: () => void }) {
  const [login, setLogin] = useState<RabbitLoginResult | null>(null)
  const [kind, setKind] = useState<'url' | 'spring' | 'env'>('url')
  const [copied, setCopied] = useState(false)
  const [leaving, setLeaving] = useState(false)
  useEffect(() => {
    let live = true
    void s.ask({ op: 'user_login', password: made.password, brokerUser: { name: made.user } }).then((a) => {
      if (live) setLogin(dataOf<RabbitLoginResult>(a) ?? { user: made.user, address: '', ok: false, detail: a.error ?? 'the check did not run' })
    })
    return () => { live = false }
  }, []) // eslint-disable-line react-hooks/exhaustive-deps
  const tls = !!data.tlsPort
  const details = { host: database.hostname, port: data.tlsPort || data.amqpPort || 5672, tls, vhost: database.dbname, user: made.user, password: made.password }
  const settings = rabbitSettings(details, kind)
  const close = () => { if (!copied && !leaving) { setLeaving(true); return } onClose() }
  const rows: [string, string][] = [['Host', details.host], ['Port', String(details.port)], ['Vhost', details.vhost], ['User', details.user], ['Password', details.password]]
  return (
    <Modal opened onClose={close} title={<Text fw={650}>{made.user}: connection details</Text>} size={900} closeOnClickOutside={false}>
      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="lg">
        <Stack gap="xs">
          <Step ok={login ? login.ok : null} text={login ? (login.ok ? `Opens ${database.dbname} on ${login.address}` : `Did not sign in on ${login.address || 'the server'}: ${login.detail ?? ''}`) : 'Signing in as the new user…'} />
          <Text size={FS.micro} c="dimmed">Apps on other machines reach {details.host}:{details.port} only if the Network tab lets them.</Text>
          <Text size={FS.meta} fw={600} mt="xs">Client settings</Text>
          <SegmentedControl size="xs" value={kind} onChange={(v) => setKind(v as typeof kind)} data={[{ value: 'url', label: 'URL' }, { value: 'spring', label: 'Spring' }, { value: 'env', label: '.env' }]} />
          <Code block style={{ fontSize: 11.5, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{settings}</Code>
        </Stack>
        <Stack gap="xs">
          <Group><Text size={FS.meta} fw={600}>Connection details</Text><Badge color="rose" variant="light" ml="auto">shown once</Badge></Group>
          {rows.map(([k, v]) => (
            <Group key={k} gap={6} wrap="nowrap">
              <Text size={FS.micro} c="dimmed" w={80}>{k}</Text>
              <Code style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{v}</Code>
              <CopyButton value={v}>{({ copied: c, copy }) => <ActionIcon variant="default" aria-label={`Copy ${k}`} onClick={() => { copy(); if (k === 'Password') setCopied(true) }}>{c ? <IconCheck size={14} /> : <IconCopy size={14} />}</ActionIcon>}</CopyButton>
            </Group>
          ))}
          <Group gap="xs" mt="xs">
            <CopyButton value={settings}>{({ copy }) => <Button size="xs" variant="default" onClick={() => { copy(); setCopied(true) }}>Copy settings</Button>}</CopyButton>
          </Group>
          <Text size={FS.micro} c="dimmed">Argus does not keep the password; if it is lost, give {made.user} a new one from its page.</Text>
        </Stack>
      </SimpleGrid>
      <Group mt="md" justify="flex-end" gap="xs">
        {leaving && !copied && <Text size={FS.micro} c="amber" style={{ flex: 1 }}>The password will not be shown again. Close anyway?</Text>}
        <Button onClick={close}>{leaving && !copied ? 'Close without copying' : 'I\'ve saved it, close'}</Button>
      </Group>
    </Modal>
  )
}
