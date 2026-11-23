import {
  ActionIcon, Alert, Badge, Box, Button, Card, Code, Group, NumberInput, SegmentedControl, Select, SimpleGrid, Stack, Table, Tabs, Text, TextInput,
} from '@mantine/core'
import { IconArrowLeft, IconPlayerPlay, IconRefresh, IconSend, IconTrash } from '@tabler/icons-react'
import { useCallback, useEffect, useState } from 'react'
import type { StoreAnswer } from '~/components/useStoreSession'
import { ConfirmName, PurgeModal } from '~/components/nats/NatsForms'
import type { Ask, Req } from '~/components/nats/NatsWorkspace'
import {
  ago, consumerFilters, consumerKind, fmtBytes, fmtCount, fmtLimit, fmtNanos, items, showData, validSubject,
  type NatsConsumer, type NatsMessage, type NatsStream,
} from '~/lib/nats'
import { notifyError } from '~/lib/notify'
import { FS, INK } from '~/theme'

/*
One stream: its facts and limits on the left, with Edit, Purge and Delete;
on the right its messages, read by sequence so no consumer is made and no
app's place moves, its consumers, the subjects it holds, its settings and
its replicas.
*/

type DetailTab = 'messages' | 'consumers' | 'subjects' | 'settings' | 'replicas'

export function StreamDetail({ name, streams, consumers, ask, act, can, narrow, onBack, onConsumer, onPublish, onEdit, onNewConsumer }: {
  name: string; streams: NatsStream[]; consumers: NatsConsumer[]; ask: Ask; act: (r: Req, ok: string) => Promise<StoreAnswer>
  can: (op: string) => boolean; narrow: boolean; onBack: () => void; onConsumer: (c: string) => void; onPublish: (subject: string) => void
  onEdit: (s: NatsStream) => void; onNewConsumer: () => void
}) {
  const [detail, setDetail] = useState<{ info: NatsStream; subjects: Record<string, number> | null } | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [tab, setTab] = useState<DetailTab>('messages')
  const [purging, setPurging] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const listed = streams.find((s) => s.config.name === name)

  const load = useCallback(async () => {
    const a = await ask({ op: 'stream', nats: { stream: name } })
    if (a.type === 'result') { setDetail(a.data as { info: NatsStream; subjects: Record<string, number> | null }); setErr(null) } else setErr(a.error ?? 'could not read the stream')
  }, [ask, name])
  useEffect(() => { void load() }, [load, listed?.state.last_seq])

  const s = detail?.info ?? listed
  if (!s) {
    return (
      <Stack gap="sm">
        <Group gap="xs"><ActionIcon variant="subtle" color="slate" onClick={onBack} aria-label="Back to streams"><IconArrowLeft size={16} /></ActionIcon>
          <Text ff="monospace" fw={650}>{name}</Text></Group>
        <Text size={FS.meta} c={err ? 'rose' : 'dimmed'}>{err ?? 'Reading the stream…'}</Text>
      </Stack>
    )
  }
  const c = s.config
  const subjects = Object.entries(detail?.subjects ?? {}).sort((a, b) => b[1] - a[1])
  const peers = s.cluster?.replicas ?? []
  return (
    <Stack gap="md">
      <Group gap="xs" wrap="wrap">
        <ActionIcon variant="subtle" color="slate" onClick={onBack} aria-label="Back to streams"><IconArrowLeft size={16} /></ActionIcon>
        <Text ff="monospace" fw={650} size={FS.body}>{name}</Text>
        <Text size={FS.micro} c="dimmed">{c.storage} · R{c.num_replicas || 1} · {c.retention === 'workqueue' ? 'work queue' : c.retention === 'interest' ? 'kept until read' : `kept ${fmtNanos(c.max_age)}`}</Text>
        <Group gap={6} ml="auto">
          <Button size="xs" variant="default" leftSection={<IconRefresh size={12} />} onClick={() => void load()}>Refresh</Button>
          {can('publish') && <Button size="xs" variant="light" leftSection={<IconSend size={12} />} onClick={() => onPublish((c.subjects ?? []).find((x) => !/[*>]/.test(x)) ?? '')}>Publish here</Button>}
        </Group>
      </Group>
      {err && <Alert color="rose" variant="light">{err}</Alert>}
      <Box style={{ display: 'grid', gridTemplateColumns: narrow ? '1fr' : 'minmax(260px, 300px) 1fr', gap: 'var(--mantine-spacing-md)', alignItems: 'start' }}>
        <Card withBorder radius="md" padding="sm">
          <Stack gap={6}>
            <Fact k="Subjects" v={<Text size={FS.meta} ff="monospace" ta="right">{(c.subjects ?? []).join(' ') || (c.mirror ? `mirror of ${c.mirror.name}` : '–')}</Text>} />
            <Fact k="Messages" v={fmtCount(s.state.messages)} />
            <Fact k="Size" v={fmtBytes(s.state.bytes)} />
            <Fact k="First / last" v={<Text size={FS.meta} ff="monospace">#{s.state.first_seq.toLocaleString()} / #{s.state.last_seq.toLocaleString()}</Text>} />
            <Fact k="Last message" v={s.state.messages ? `${ago(s.state.last_ts)} ago` : 'empty'} />
            <Fact k="Dedupe window" v={fmtNanos(c.duplicate_window, 'off')} />
            {s.cluster?.leader && <Fact k="Leader" v={s.cluster.leader} />}
            {peers.map((p) => (
              <Fact key={p.name} k={p.name} v={<Badge size="sm" variant="light" radius="sm" color={p.offline ? 'rose' : p.current ? 'teal' : 'amber'}>
                {p.offline ? 'offline' : p.current ? 'current' : `${fmtCount(p.lag)} behind`}</Badge>} />
            ))}
            {s.cluster && c.num_replicas > 1 && !s.cluster.leader && <Text size={FS.micro} c="rose">No leader: nothing can be stored or read.</Text>}
          </Stack>
          {(can('stream_update') || can('stream_purge') || can('stream_delete')) && (
            <Group gap={6} mt="md">
              {can('stream_update') && <Button size="xs" variant="default" onClick={() => onEdit(s)} disabled={c.sealed}>Edit limits</Button>}
              {can('stream_purge') && <Button size="xs" variant="default" onClick={() => setPurging(true)} disabled={c.deny_purge}>Purge…</Button>}
              {can('stream_delete') && <Button size="xs" variant="default" color="rose" leftSection={<IconTrash size={12} />} onClick={() => setDeleting(true)}>Delete…</Button>}
            </Group>
          )}
          {c.deny_purge && <Text size={FS.micro} c="dimmed" mt={4}>This stream refuses purges.</Text>}
        </Card>
        <Box style={{ minWidth: 0 }}>
          <Tabs value={tab} onChange={(v) => v && setTab(v as DetailTab)} styles={{ list: { flexWrap: 'nowrap', overflowX: 'auto' } }}>
            <Tabs.List>
              <Tabs.Tab value="messages">Messages</Tabs.Tab>
              <Tabs.Tab value="consumers">Consumers {consumers.length}</Tabs.Tab>
              <Tabs.Tab value="subjects">Subjects {s.state.num_subjects ?? subjects.length}</Tabs.Tab>
              <Tabs.Tab value="settings">Settings</Tabs.Tab>
              {peers.length > 0 && <Tabs.Tab value="replicas">Replicas</Tabs.Tab>}
            </Tabs.List>
          </Tabs>
          <Box mt="sm">
            {tab === 'messages' && <Messages stream={s} ask={ask} act={act} canDelete={can('msg_delete') && !c.deny_delete} canTail={can('tail')} narrow={narrow} />}
            {tab === 'consumers' && (
              <Stack gap="sm">
                {can('consumer_create') && <Button size="xs" style={{ alignSelf: 'flex-end' }} onClick={onNewConsumer}>New consumer</Button>}
                <Card withBorder radius="md" padding={0}>
                  <Table.ScrollContainer minWidth={560}>
                    <Table verticalSpacing={6} fz="sm" highlightOnHover>
                      <Table.Thead><Table.Tr><Table.Th>Consumer</Table.Th><Table.Th>Reads</Table.Th><Table.Th>Kind</Table.Th><Table.Th ta="right">Waiting</Table.Th><Table.Th ta="right">Unacked</Table.Th><Table.Th>At</Table.Th></Table.Tr></Table.Thead>
                      <Table.Tbody>
                        {consumers.map((x) => (
                          <Table.Tr key={x.name} onClick={() => onConsumer(x.name)} style={{ cursor: 'pointer' }}>
                            <Table.Td><Text ff="monospace" size={FS.meta}>{x.name}</Text></Table.Td>
                            <Table.Td><Text ff="monospace" size={FS.micro}>{consumerFilters(x.config).join(' ') || 'everything'}</Text></Table.Td>
                            <Table.Td><Text size={FS.micro}>{x.paused ? 'paused' : consumerKind(x.config)}</Text></Table.Td>
                            <Table.Td ta="right" fw={x.num_pending ? 600 : undefined}>{fmtCount(x.num_pending)}</Table.Td>
                            <Table.Td ta="right">{fmtCount(x.num_ack_pending)}</Table.Td>
                            <Table.Td><Text size={FS.micro} ff="monospace">#{x.delivered.stream_seq.toLocaleString()}</Text></Table.Td>
                          </Table.Tr>
                        ))}
                        {consumers.length === 0 && <Table.Tr><Table.Td colSpan={6}><Text size={FS.meta} c="dimmed" p="sm">No consumer reads this stream.</Text></Table.Td></Table.Tr>}
                      </Table.Tbody>
                    </Table>
                  </Table.ScrollContainer>
                </Card>
              </Stack>
            )}
            {tab === 'subjects' && (
              <Card withBorder radius="md" padding={0}>
                <Table verticalSpacing={6} fz="sm">
                  <Table.Thead><Table.Tr><Table.Th>Subject</Table.Th><Table.Th ta="right">Messages</Table.Th></Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {subjects.slice(0, 500).map(([k, v]) => (
                      <Table.Tr key={k}><Table.Td><Text ff="monospace" size={FS.meta}>{k}</Text></Table.Td><Table.Td ta="right">{fmtCount(v)}</Table.Td></Table.Tr>
                    ))}
                    {subjects.length === 0 && <Table.Tr><Table.Td colSpan={2}><Text size={FS.meta} c="dimmed" p="sm">No messages, so no subjects.</Text></Table.Td></Table.Tr>}
                  </Table.Tbody>
                </Table>
                {subjects.length > 500 && <Text size={FS.micro} c="dimmed" p="xs">The 500 with the most messages of {subjects.length}.</Text>}
              </Card>
            )}
            {tab === 'settings' && (
              <Card withBorder radius="md" padding="sm">
                <SimpleGrid cols={{ base: 1, sm: 2 }} spacing={6}>
                  <Fact k="Keeps" v={c.retention === 'workqueue' ? 'until a consumer acks (work queue)' : c.retention === 'interest' ? 'until every consumer has read it' : 'until a limit'} />
                  <Fact k="When full" v={c.discard === 'new' ? 'refuses new messages' : 'drops the oldest'} />
                  <Fact k="At most for" v={fmtNanos(c.max_age)} />
                  <Fact k="At most messages" v={fmtLimit(c.max_msgs)} />
                  <Fact k="At most size" v={fmtLimit(c.max_bytes, fmtBytes)} />
                  <Fact k="Per subject" v={fmtLimit(c.max_msgs_per_subject)} />
                  <Fact k="Stored on" v={c.storage === 'memory' ? 'memory' : 'disk'} />
                  <Fact k="Copies" v={String(c.num_replicas || 1)} />
                  <Fact k="Message delete" v={c.deny_delete ? 'refused' : 'allowed'} />
                  <Fact k="Purge" v={c.deny_purge ? 'refused' : 'allowed'} />
                  <Fact k="Created" v={new Date(s.created).toLocaleString()} />
                  {c.description && <Fact k="Description" v={c.description} />}
                </SimpleGrid>
              </Card>
            )}
            {tab === 'replicas' && (
              <Card withBorder radius="md" padding={0}>
                <Table verticalSpacing={6} fz="sm">
                  <Table.Thead><Table.Tr><Table.Th>Server</Table.Th><Table.Th>Role</Table.Th><Table.Th>State</Table.Th><Table.Th ta="right">Behind</Table.Th></Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {s.cluster?.leader && <Table.Tr><Table.Td>{s.cluster.leader}</Table.Td><Table.Td>leader</Table.Td><Table.Td><Badge size="sm" variant="light" color="teal" radius="sm">current</Badge></Table.Td><Table.Td ta="right">0</Table.Td></Table.Tr>}
                    {peers.map((p) => (
                      <Table.Tr key={p.name}><Table.Td>{p.name}</Table.Td><Table.Td>replica</Table.Td>
                        <Table.Td><Badge size="sm" variant="light" radius="sm" color={p.offline ? 'rose' : p.current ? 'teal' : 'amber'}>{p.offline ? 'offline' : p.current ? 'current' : 'catching up'}</Badge></Table.Td>
                        <Table.Td ta="right">{fmtCount(p.lag ?? 0)}</Table.Td></Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Card>
            )}
          </Box>
        </Box>
      </Box>
      {purging && <PurgeModal stream={s} onClose={() => setPurging(false)}
        onPurge={async (nats, confirm) => {
          const a = await act({ op: 'stream_purge', nats: { stream: name, ...nats }, ...(confirm ? { confirm } : {}) }, `Purged ${name}.`)
          if (a.type === 'result') { setPurging(false); void load() }
        }} />}
      <ConfirmName opened={deleting} name={name} what={`Delete stream ${name} and every message in it. Its ${s.state.consumer_count} consumer${s.state.consumer_count === 1 ? '' : 's'} go with it.`}
        onCancel={() => setDeleting(false)}
        onConfirm={(typed) => {
          setDeleting(false)
          void act({ op: 'stream_delete', nats: { stream: name }, confirm: typed }, `Deleted stream ${name}.`).then((a) => { if (a.type === 'result') onBack() })
        }} />
    </Stack>
  )
}

function Fact({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <Group justify="space-between" gap="xs" wrap="nowrap" align="flex-start">
      <Text size={FS.meta} c="dimmed" style={{ flexShrink: 0 }}>{k}</Text>
      {typeof v === 'string' ? <Text size={FS.meta} ta="right">{v}</Text> : v}
    </Group>
  )
}

function Messages({ stream, ask, act, canDelete, canTail, narrow }: {
  stream: NatsStream; ask: Ask; act: (r: Req, ok: string) => Promise<StoreAnswer>; canDelete: boolean; canTail: boolean; narrow: boolean
}) {
  const [from, setFrom] = useState<'newest' | 'seq' | 'time'>('newest')
  const [seq, setSeq] = useState<number | string>('')
  const [time, setTime] = useState('')
  const [subject, setSubject] = useState('')
  const [count, setCount] = useState<string>('20')
  const [msgs, setMsgs] = useState<NatsMessage[] | null>(null)
  const [more, setMore] = useState(false)
  const [reading, setReading] = useState(false)
  const [tailing, setTailing] = useState(false)
  const name = stream.config.name
  const filterOk = !subject.trim() || validSubject(subject.trim())

  const read = async () => {
    const nats: Record<string, unknown> = { stream: name, from }
    if (subject.trim()) nats.subject = subject.trim()
    if (from === 'seq') nats.seq = Number(seq) || stream.state.first_seq
    if (from === 'time') {
      const t = Date.parse(time)
      if (!Number.isFinite(t)) { notifyError('Say a time', new Error('the time to start at is not a time')); return }
      nats.time = new Date(t).toISOString().replace(/\.\d{3}Z$/, 'Z')
    }
    setReading(true)
    const a = await ask({ op: 'get', nats, count: Number(count) })
    setReading(false)
    if (a.type === 'result') { setMsgs(items<NatsMessage>(a.data)); setMore(Boolean(a.truncated)) } else notifyError('Could not read messages', new Error(a.error ?? 'failed'))
  }
  // Read the newest when the stream opens, as the picture shows.
  useEffect(() => { if (stream.state.messages > 0) void read() }, [name]) // eslint-disable-line react-hooks/exhaustive-deps

  const tail = async () => {
    const subj = subject.trim() || (stream.config.subjects ?? [])[0] || ''
    if (!subj || subj.startsWith('$')) { notifyError('Say a subject', new Error('say the subject to listen on')); return }
    setTailing(true)
    const a = await ask({ op: 'tail', nats: { subject: subj, seconds: 10 }, count: 100 })
    setTailing(false)
    if (a.type === 'result') { setMsgs(items<NatsMessage>(a.data)); setMore(false) } else notifyError('Could not listen', new Error(a.error ?? 'failed'))
  }

  return (
    <Stack gap="sm">
      <Group gap="xs" align="flex-end" wrap="wrap">
        <SegmentedControl size="xs" value={from} onChange={(v) => setFrom(v as typeof from)}
          data={[{ label: 'Newest', value: 'newest' }, { label: 'From sequence', value: 'seq' }, { label: 'From time', value: 'time' }]} />
        {from === 'seq' && <NumberInput size="xs" placeholder={`#${stream.state.first_seq}`} value={seq} onChange={setSeq} min={1} w={130} aria-label="Start at sequence" />}
        {from === 'time' && <TextInput size="xs" type="datetime-local" value={time} onChange={(e) => setTime(e.currentTarget.value)} w={200} aria-label="Start at time" />}
        <TextInput size="xs" placeholder="Subject, e.g. orders.eu.*" value={subject} onChange={(e) => setSubject(e.currentTarget.value)} aria-label="Subject filter"
          styles={{ input: { fontFamily: 'var(--font-mono)' } }} w={narrow ? '100%' : 220} error={!filterOk ? 'not a subject' : undefined} />
        <Select size="xs" value={count} onChange={(v) => setCount(v ?? '20')} data={['10', '20', '50', '100', '200']} w={80} aria-label="How many" allowDeselect={false} />
        <Button size="xs" onClick={() => void read()} loading={reading} disabled={!filterOk}>Read</Button>
        {canTail && <Button size="xs" variant="default" ml="auto" leftSection={<IconPlayerPlay size={12} />} onClick={() => void tail()} loading={tailing}>Listen 10 s</Button>}
      </Group>
      {msgs && (
        <Stack gap={8}>
          {msgs.length === 0 && <Text size={FS.meta} c="dimmed">No messages{subject.trim() ? ` on ${subject.trim()}` : ''}.</Text>}
          {msgs.map((m, i) => <Message key={m.seq ?? `t${i}`} m={m} narrow={narrow}
            onDelete={canDelete && m.seq ? () => void act({ op: 'msg_delete', nats: { stream: name, seq: m.seq } }, `Deleted message #${m.seq} of ${name}.`)
              .then((a) => { if (a.type === 'result') setMsgs((l) => (l ?? []).filter((x) => x.seq !== m.seq)) }) : undefined} />)}
          {more && <Text size={FS.micro} c="dimmed">There are more; read again from a later sequence to see them.</Text>}
        </Stack>
      )}
      <Text size={FS.micro} c="dimmed">
        Read with a direct get by sequence: no consumer is made and nothing is acked, so no app&rsquo;s place moves. Listen subscribes for 10 seconds and shows what is published, stored or not. Both are recorded.
      </Text>
    </Stack>
  )
}

export function Message({ m, narrow, onDelete }: { m: NatsMessage; narrow: boolean; onDelete?: () => void }) {
  const v = showData(m)
  const headers = Object.entries(m.headers ?? {})
  return (
    <Box style={{ border: '1px solid var(--color-line)', borderRadius: 8 }} p="xs">
      <Group gap="xs" wrap="wrap" mb={4}>
        {m.seq ? <Text size={FS.micro} ff="monospace" fw={650}>#{m.seq.toLocaleString()}</Text> : null}
        <Text size={FS.micro} ff="monospace" c={INK.brand}>{m.subject}</Text>
        {m.time && <Text size={FS.micro} c="dimmed">{ago(m.time)} ago</Text>}
        <Text size={FS.micro} c="dimmed" ml={narrow ? undefined : 'auto'} truncate maw={360}>
          {headers.map(([k, vs]) => `${k}: ${vs.join(', ')}`).join(' · ')}
        </Text>
        {onDelete && <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Delete message ${m.seq}`} onClick={onDelete}><IconTrash size={12} /></ActionIcon>}
      </Group>
      <Code block style={{ maxHeight: 260, overflow: 'auto', fontSize: 12 }}>{v.text}</Code>
      {(v.kind === 'base64' || m.cut) && (
        <Text size={FS.micro} c="dimmed" mt={2}>{v.kind === 'base64' ? 'Not text, shown as base64. ' : ''}{m.cut ? `Cut at 64 KB of ${fmtBytes(m.size)}.` : ''}</Text>
      )}
    </Box>
  )
}
