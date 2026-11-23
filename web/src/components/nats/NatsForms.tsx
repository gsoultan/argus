import {
  ActionIcon, Alert, Badge, Box, Button, Card, Checkbox, Code, Collapse, Drawer, Group, Modal, NumberInput, SegmentedControl, Select, SimpleGrid, Stack,
  Tabs, Text, TextInput, Textarea, UnstyledButton,
} from '@mantine/core'
import { IconAlertTriangle, IconChevronDown, IconChevronRight, IconPlus, IconX } from '@tabler/icons-react'
import { useEffect, useMemo, useState } from 'react'
import type { StoreAnswer } from '~/components/useStoreSession'
import type { Ask, Req } from '~/components/nats/NatsWorkspace'
import { Message } from '~/components/nats/NatsStream'
import {
  ago, appSubject, consumerFilters, consumerKind, consumerProblems, fmtCount, parseBackoff, specOf, streamProblems, streamsFor,
  type ConsumerSpec, type NatsConsumer, type NatsMessage, type NatsStream, type StreamSpec,
} from '~/lib/nats'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS } from '~/theme'

const MONO = { input: { fontFamily: 'var(--font-mono)' } }

/**
 * Asks for the stream's name before something that wipes it. The agent
 * refuses the command without it, so this is the console asking first.
 */
export function ConfirmName({ opened, name, what, onCancel, onConfirm }: {
  opened: boolean; name: string; what: string; onCancel: () => void; onConfirm: (typed: string) => void
}) {
  const [typed, setTyped] = useState('')
  useEffect(() => { if (opened) setTyped('') }, [opened])
  return (
    <Modal opened={opened} onClose={onCancel} centered
      title={<Group gap={6}><IconAlertTriangle size={16} color="var(--mantine-color-rose-6)" /><Text fw={650}>Confirm</Text></Group>}>
      <Stack gap="sm">
        <Text size={FS.body}>{what}</Text>
        <Text size={FS.meta} c="dimmed">This cannot be undone. It is recorded with your name either way. Type <b>{name}</b> to go ahead.</Text>
        <TextInput value={typed} onChange={(e) => setTyped(e.currentTarget.value)} placeholder={name} aria-label="Stream name"
          data-autofocus spellCheck={false} autoCapitalize="off" styles={MONO} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onCancel}>Cancel</Button>
          <Button color="rose" disabled={typed !== name} onClick={() => onConfirm(typed)}>Do it</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/** Purge all of a stream, one subject of it, everything below a sequence, or all but the newest. */
export function PurgeModal({ stream, onClose, onPurge }: {
  stream: NatsStream; onClose: () => void; onPurge: (nats: Record<string, unknown>, confirm?: string) => Promise<void>
}) {
  const name = stream.config.name
  const [how, setHow] = useState<'all' | 'subject' | 'below' | 'keep'>('subject')
  const [subject, setSubject] = useState('')
  const [seq, setSeq] = useState<number | string>('')
  const [keep, setKeep] = useState<number | string>(1000)
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const nats: Record<string, unknown> = how === 'subject' ? { subject: subject.trim() } : how === 'below' ? { seq: Number(seq) } : how === 'keep' ? { keep: Number(keep) } : {}
  const ok = how === 'all' ? typed === name : how === 'subject' ? subject.trim() !== '' : how === 'below' ? Number(seq) > 0 : Number(keep) > 0
  return (
    <Modal opened onClose={onClose} centered size="lg" title={<Text fw={650}>Purge {name}</Text>}>
      <Stack gap="sm">
        <SegmentedControl value={how} onChange={(v) => setHow(v as typeof how)} fullWidth
          data={[{ label: 'One subject', value: 'subject' }, { label: 'Below a sequence', value: 'below' }, { label: 'Keep the newest', value: 'keep' }, { label: 'Everything', value: 'all' }]} />
        {how === 'subject' && <TextInput label="Subject" value={subject} onChange={(e) => setSubject(e.currentTarget.value)} styles={MONO} placeholder="orders.eu.created"
          description="Every message on this subject (wildcards allowed) goes." />}
        {how === 'below' && <NumberInput label="Below sequence" value={seq} onChange={setSeq} min={1} placeholder={`#${stream.state.last_seq}`}
          description={`Messages before this one go. The stream holds #${stream.state.first_seq.toLocaleString()} to #${stream.state.last_seq.toLocaleString()}.`} />}
        {how === 'keep' && <NumberInput label="Keep the newest" value={keep} onChange={setKeep} min={1} description="All but this many of the newest messages go." />}
        {how === 'all' && (
          <>
            <Alert color="rose" variant="light">Every one of the {fmtCount(stream.state.messages)} messages goes. Consumers stay, with nothing to read.</Alert>
            <TextInput label={<>Type <b>{name}</b> to purge everything</>} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} styles={MONO} aria-label="Stream name" />
          </>
        )}
        <Text size={FS.micro} c="dimmed">Recorded with your name. Messages purged cannot be brought back.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" disabled={!ok} loading={busy}
            onClick={async () => { setBusy(true); await onPurge(nats, how === 'all' ? typed : undefined); setBusy(false) }}>Purge</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

const KEEPS = [
  { value: 'limits', title: 'Until a limit', body: 'Kept for an age, a count or a size, then the oldest go. Any number of consumers read it.' },
  { value: 'interest', title: 'Until all have read it', body: 'Kept while a consumer has yet to ack it. With no consumers, nothing is kept.' },
  { value: 'workqueue', title: 'Work queue', body: 'Each message goes to one consumer and is gone once acked.' },
] as const

/** A new stream, or one to change. NATS keeps a stream's name, retention and storage once made. */
export function StreamForm({ edit, streams, narrow, onClose, onSave }: {
  edit?: NatsStream; streams: NatsStream[]; narrow: boolean; onClose: () => void; onSave: (spec: StreamSpec) => Promise<void>
}) {
  const [spec, setSpec] = useState<StreamSpec>(() => edit ? specOf(edit.config) : {
    name: '', subjects: [''], retention: 'limits', storage: 'file', replicas: 1, maxAgeS: 7 * 86_400, discard: 'old', duplicatesS: 120, allowDelete: true,
  })
  const [advanced, setAdvanced] = useState(false)
  const [saving, setSaving] = useState(false)
  const set = (p: Partial<StreamSpec>) => setSpec((s) => ({ ...s, ...p }))
  const clean: StreamSpec = { ...spec, subjects: spec.subjects.map((x) => x.trim()).filter(Boolean) }
  const problems = streamProblems(clean, streams)
  const sent = JSON.stringify(Object.fromEntries(Object.entries(clean).filter(([, v]) => v !== '' && v !== 0 && v !== false && v !== undefined)), null, 2)
  const days = spec.maxAgeS ? +(spec.maxAgeS / 86_400).toFixed(3) : ''
  return (
    <Modal opened onClose={onClose} size={narrow ? '100%' : 980} fullScreen={narrow} title={<Text fw={650}>{edit ? `Change ${edit.config.name}` : 'New stream'}</Text>}>
      <Box style={{ display: 'grid', gridTemplateColumns: narrow ? '1fr' : '1fr 300px', gap: 'var(--mantine-spacing-lg)' }}>
        <Stack gap="sm">
          <TextInput label="Name" value={spec.name} onChange={(e) => set({ name: e.currentTarget.value })} styles={MONO} disabled={Boolean(edit)} data-autofocus
            description={edit ? 'A stream keeps its name.' : 'Letters, digits, - and _. Usually upper case, like ORDERS.'} />
          <Box>
            <Text size={FS.meta} fw={500}>Subjects</Text>
            <Text size={FS.micro} c="dimmed" mb={4}>What it stores. * is one word, &gt; the rest: orders.&gt; takes orders.eu.created.</Text>
            <Stack gap={6}>
              {spec.subjects.map((x, i) => (
                <Group key={i} gap="xs" wrap="nowrap">
                  <TextInput size="xs" value={x} placeholder="orders.>" styles={MONO} style={{ flex: 1 }} aria-label={`Subject ${i + 1}`}
                    onChange={(e) => { const v = e.currentTarget.value; set({ subjects: spec.subjects.map((y, j) => (j === i ? v : y)) }) }} />
                  {spec.subjects.length > 1 && <ActionIcon variant="subtle" color="slate" aria-label="Remove subject" onClick={() => set({ subjects: spec.subjects.filter((_, j) => j !== i) })}><IconX size={14} /></ActionIcon>}
                </Group>
              ))}
              <Button size="compact-xs" variant="subtle" leftSection={<IconPlus size={12} />} style={{ alignSelf: 'flex-start' }} onClick={() => set({ subjects: [...spec.subjects, ''] })}>Add subject</Button>
            </Stack>
          </Box>
          <Box>
            <Text size={FS.meta} fw={500} mb={4}>Keeps messages</Text>
            <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="xs">
              {KEEPS.map((k) => (
                <UnstyledButton key={k.value} disabled={Boolean(edit)} onClick={() => set({ retention: k.value })} aria-pressed={spec.retention === k.value}
                  style={{ border: `1px solid ${spec.retention === k.value ? 'var(--mantine-color-azure-6)' : 'var(--color-line)'}`, borderRadius: 8, padding: 10, opacity: edit && spec.retention !== k.value ? 0.5 : 1 }}>
                  <Text size={FS.meta} fw={600}>{k.title}</Text>
                  <Text size={FS.micro} c="dimmed">{k.body}</Text>
                </UnstyledButton>
              ))}
            </SimpleGrid>
            {edit && <Text size={FS.micro} c="dimmed" mt={4}>NATS keeps how a stream keeps messages and where it stores them once it is made.</Text>}
          </Box>
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
            <NumberInput label="For at most (days)" value={days} onChange={(v) => set({ maxAgeS: v === '' ? 0 : Math.round(Number(v) * 86_400) })} min={0} decimalScale={3} placeholder="for ever" />
            <NumberInput label="Up to messages" value={spec.maxMsgs || ''} onChange={(v) => set({ maxMsgs: Number(v) || 0 })} min={0} placeholder="no limit" thousandSeparator="," />
            <NumberInput label="Up to size (MB)" value={spec.maxBytes ? Math.round(spec.maxBytes / 1048576) : ''} onChange={(v) => set({ maxBytes: Number(v) ? Number(v) * 1048576 : 0 })} min={0} placeholder="no limit" />
            <NumberInput label="Per subject, keep the newest" value={spec.maxPerSubject || ''} onChange={(v) => set({ maxPerSubject: Number(v) || 0 })} min={0} placeholder="all" />
          </SimpleGrid>
          <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
            <Box>
              <Text size={FS.meta} fw={500} mb={4}>When full</Text>
              <SegmentedControl size="xs" fullWidth value={spec.discard} onChange={(v) => set({ discard: v as 'old' | 'new' })}
                data={[{ label: 'Drop oldest', value: 'old' }, { label: 'Refuse new', value: 'new' }]} />
            </Box>
            <Box>
              <Text size={FS.meta} fw={500} mb={4}>Stored on</Text>
              <SegmentedControl size="xs" fullWidth value={spec.storage} onChange={(v) => set({ storage: v as 'file' | 'memory' })} disabled={Boolean(edit)}
                data={[{ label: 'Disk', value: 'file' }, { label: 'Memory', value: 'memory' }]} />
            </Box>
            <Box>
              <Text size={FS.meta} fw={500} mb={4}>Copies</Text>
              <SegmentedControl size="xs" fullWidth value={String(spec.replicas ?? 1)} onChange={(v) => set({ replicas: Number(v) })}
                data={['1', '3', '5']} />
            </Box>
          </SimpleGrid>
          <NumberInput label="Ignore repeats for (seconds)" value={spec.duplicatesS || ''} onChange={(v) => set({ duplicatesS: Number(v) || 0 })} min={0} placeholder="NATS default, 2 min"
            description="A message with a Nats-Msg-Id seen within this window is stored once." w={narrow ? undefined : 320} />
          <UnstyledButton onClick={() => setAdvanced((a) => !a)}>
            <Group gap={4}>{advanced ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}<Text size={FS.meta} fw={500}>Advanced</Text></Group>
          </UnstyledButton>
          <Collapse expanded={advanced}>
            <Stack gap="xs">
              <TextInput label="Description" value={spec.description ?? ''} onChange={(e) => set({ description: e.currentTarget.value })} />
              <Checkbox label="Allow deleting one message" checked={spec.allowDelete ?? true} onChange={(e) => set({ allowDelete: e.currentTarget.checked })} />
              <Checkbox label="Refuse purges" checked={spec.denyPurge ?? false} onChange={(e) => set({ denyPurge: e.currentTarget.checked })} />
              <Text size={FS.micro} c="dimmed">Mirrors, sources and compression are set with the nats CLI for now; Argus keeps them when it changes a stream.</Text>
            </Stack>
          </Collapse>
        </Stack>
        <Stack gap="xs">
          <Text size={FS.meta} fw={500}>What is sent</Text>
          <Code block style={{ fontSize: 11.5, maxHeight: 360, overflow: 'auto' }}>{sent}</Code>
          {problems.length > 0 && (
            <Stack gap={4}>{problems.map((p) => <Text key={p} size={FS.micro} c="rose">{p}</Text>)}</Stack>
          )}
          <Text size={FS.micro} c="dimmed">Recorded with the full settings sent.</Text>
        </Stack>
      </Box>
      <Group justify="flex-end" mt="md">
        <Button variant="default" onClick={onClose}>Cancel</Button>
        <Button disabled={problems.length > 0} loading={saving} onClick={async () => { setSaving(true); await onSave(clean); setSaving(false) }}>
          {edit ? 'Save changes' : 'Create stream'}
        </Button>
      </Group>
    </Modal>
  )
}

const STARTS = [
  { value: 'all', title: 'The first message' },
  { value: 'new', title: 'New messages only' },
  { value: 'last_per_subject', title: 'Newest of each subject' },
  { value: 'by_start_sequence', title: 'A sequence' },
  { value: 'by_start_time', title: 'A time' },
] as const

/** Code an app uses to read a pull consumer, by language. */
export function consumerSnippets(stream: string, name: string): Record<string, string> {
  return {
    Go: `js, _ := jetstream.New(nc)\ncons, _ := js.Consumer(ctx, "${stream}", "${name}")\ncons.Consume(func(m jetstream.Msg) {\n\t// handle m.Data()\n\tm.Ack()\n})`,
    Node: `const js = jetstream(nc)\nconst c = await js.consumers.get("${stream}", "${name}")\nfor await (const m of await c.consume()) {\n  // handle m.data\n  m.ack()\n}`,
    Python: `js = nc.jetstream()\nsub = await js.pull_subscribe_bind("${name}", stream="${stream}")\nfor m in await sub.fetch(10):\n    # handle m.data\n    await m.ack()`,
    Java: `StreamContext sc = nc.getStreamContext("${stream}");\nConsumerContext cc = sc.getConsumerContext("${name}");\ncc.consume(m -> {\n    // handle m.getData()\n    m.ack();\n});`,
  }
}

export function ConsumerForm({ streams, initialStream, narrow, onClose, onSave }: {
  streams: NatsStream[]; initialStream?: string; narrow: boolean; onClose: () => void; onSave: (stream: string, spec: ConsumerSpec) => Promise<boolean>
}) {
  const [stream, setStream] = useState(initialStream ?? streams[0]?.config.name ?? '')
  const [name, setName] = useState('')
  const [push, setPush] = useState(false)
  const [filters, setFilters] = useState('')
  const [deliver, setDeliver] = useState<NonNullable<ConsumerSpec['deliver']>>('all')
  const [startSeq, setStartSeq] = useState<number | string>('')
  const [startTime, setStartTime] = useState('')
  const [ack, setAck] = useState<'explicit' | 'all' | 'none'>('explicit')
  const [ackWait, setAckWait] = useState<number | string>(30)
  const [maxDeliver, setMaxDeliver] = useState<number | string>('')
  const [backoff, setBackoff] = useState('')
  const [maxAckPending, setMaxAckPending] = useState<number | string>('')
  const [deliverSubject, setDeliverSubject] = useState('')
  const [deliverGroup, setDeliverGroup] = useState('')
  const [made, setMade] = useState(false)
  const [saving, setSaving] = useState(false)
  const st = streams.find((s) => s.config.name === stream)
  const back = parseBackoff(backoff)
  const startIso = Number.isFinite(Date.parse(startTime)) ? new Date(Date.parse(startTime)).toISOString().replace(/\.\d{3}Z$/, 'Z') : startTime
  const spec: ConsumerSpec = {
    name,
    filterSubjects: filters.split(/[,\s]+/).filter(Boolean),
    deliver,
    ...(deliver === 'by_start_sequence' ? { startSeq: Number(startSeq) || 0 } : {}),
    ...(deliver === 'by_start_time' ? { startTime: startIso } : {}),
    ack,
    ackWaitS: Number(ackWait) || 0,
    maxDeliver: Number(maxDeliver) || 0,
    backoffS: back ?? [],
    maxAckPending: Number(maxAckPending) || 0,
    ...(push ? { deliverSubject: deliverSubject.trim(), deliverGroup: deliverGroup.trim() } : {}),
  }
  const problems = [...consumerProblems(spec, st), ...(back === null ? ['Back-off is seconds between tries, like 5, 30, 120.'] : []),
    ...(push && !deliverSubject.trim() ? ['A push consumer needs the subject it sends to.'] : [])]
  const snippets = consumerSnippets(stream, name)
  return (
    <Modal opened onClose={onClose} size={narrow ? '100%' : 760} fullScreen={narrow} title={<Text fw={650}>{made ? `Consumer ${name} is ready` : 'New consumer'}</Text>}>
      {made ? (
        <Stack gap="sm">
          <Text size={FS.meta}>Apps read it by its stream and name. With a user that may read {stream}:</Text>
          <Tabs defaultValue="Go">
            <Tabs.List>{Object.keys(snippets).map((k) => <Tabs.Tab key={k} value={k}>{k}</Tabs.Tab>)}</Tabs.List>
            {Object.entries(snippets).map(([k, v]) => <Tabs.Panel key={k} value={k} pt="xs"><Code block style={{ fontSize: 12 }}>{v}</Code></Tabs.Panel>)}
          </Tabs>
          <Group justify="flex-end"><Button onClick={onClose}>Done</Button></Group>
        </Stack>
      ) : (
        <Stack gap="sm">
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
            <Select label="Stream" value={stream} onChange={(v) => setStream(v ?? '')} data={streams.map((s) => s.config.name)} searchable allowDeselect={false} />
            <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} styles={MONO} placeholder="invoicer" data-autofocus
              description="Apps find it by this name, and it keeps its place while they are away." />
          </SimpleGrid>
          <Box>
            <Text size={FS.meta} fw={500} mb={4}>How apps get messages</Text>
            <SegmentedControl fullWidth value={push ? 'push' : 'pull'} onChange={(v) => setPush(v === 'push')}
              data={[{ label: 'Pull: apps ask for batches (usual)', value: 'pull' }, { label: 'Push: NATS sends to a subject', value: 'push' }]} />
          </Box>
          {push && (
            <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
              <TextInput label="Sends to subject" value={deliverSubject} onChange={(e) => setDeliverSubject(e.currentTarget.value)} styles={MONO} placeholder="deliver.invoicer" />
              <TextInput label="Queue group" value={deliverGroup} onChange={(e) => setDeliverGroup(e.currentTarget.value)} styles={MONO} placeholder="optional"
                description="Apps in one group share the messages." />
            </SimpleGrid>
          )}
          <TextInput label="Only subjects" value={filters} onChange={(e) => setFilters(e.currentTarget.value)} styles={MONO} placeholder={(st?.config.subjects ?? []).join(' ') || 'everything in the stream'}
            description="Leave empty to read everything the stream holds. Several are split by spaces." />
          <Box>
            <Text size={FS.meta} fw={500} mb={4}>Starts at</Text>
            <SegmentedControl fullWidth size="xs" value={deliver} onChange={(v) => setDeliver(v as typeof deliver)} data={STARTS.map((s) => ({ label: s.title, value: s.value }))} />
            {deliver === 'by_start_sequence' && <NumberInput mt="xs" value={startSeq} onChange={setStartSeq} min={1} placeholder={st ? `#${st.state.first_seq}` : 'sequence'} w={200} aria-label="Start sequence" />}
            {deliver === 'by_start_time' && <TextInput mt="xs" type="datetime-local" value={startTime} onChange={(e) => setStartTime(e.currentTarget.value)} w={240} aria-label="Start time" />}
          </Box>
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
            <Box>
              <Text size={FS.meta} fw={500} mb={4}>Acks</Text>
              <SegmentedControl fullWidth size="xs" value={ack} onChange={(v) => setAck(v as typeof ack)}
                data={[{ label: 'Each message', value: 'explicit' }, { label: 'All up to one', value: 'all' }, { label: 'None', value: 'none' }]} />
            </Box>
            <NumberInput label="Wait for an ack (seconds)" value={ackWait} onChange={setAckWait} min={1} disabled={ack === 'none'} description="Then the message is sent again." />
            <NumberInput label="Tries at most" value={maxDeliver} onChange={setMaxDeliver} min={0} placeholder="no limit" disabled={ack === 'none'} />
            <TextInput label="Back-off (seconds)" value={backoff} onChange={(e) => setBackoff(e.currentTarget.value)} placeholder="5, 30, 120" disabled={ack === 'none'}
              description="Wait between tries; instead of the ack wait." />
            <NumberInput label="Most unacked at once" value={maxAckPending} onChange={setMaxAckPending} min={0} placeholder="NATS default, 1000" />
          </SimpleGrid>
          {problems.length > 0 && <Stack gap={2}>{problems.map((p) => <Text key={p} size={FS.micro} c="rose">{p}</Text>)}</Stack>}
          <Text size={FS.micro} c="dimmed">Recorded with the full settings sent.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button disabled={problems.length > 0 || !stream} loading={saving}
              onClick={async () => { setSaving(true); const ok = await onSave(stream, spec); setSaving(false); if (ok) setMade(true) }}>Create consumer</Button>
          </Group>
        </Stack>
      )}
    </Modal>
  )
}

/** Where a consumer is, in words. */
export function consumerSays(c: NatsConsumer): string {
  if (c.paused) return `Paused${c.pause_remaining ? ` for another ${Math.round(c.pause_remaining / 6e10)} min` : ''}; ${fmtCount(c.num_pending)} messages wait.`
  if (c.num_pending === 0 && c.num_ack_pending === 0) return 'Caught up: it has had every message and every one is acked.'
  const idle = c.delivered.last_active ? `${ago(c.delivered.last_active)}` : 'ever'
  const who = c.num_waiting > 0 ? `${c.num_waiting} pull request${c.num_waiting === 1 ? ' is' : 's are'} waiting` : c.push_bound ? 'an app is subscribed' : `nothing has pulled for ${idle}`
  return `${fmtCount(c.num_pending)} messages wait and ${fmtCount(c.num_ack_pending)} are out unacked; ${who}.`
}

export function ConsumerDrawer({ c, narrow, act, can, onClose, onStream }: {
  c: NatsConsumer; narrow: boolean; act: (r: Req, ok: string) => Promise<StoreAnswer>; can: (op: string) => boolean
  onClose: () => void; onStream: () => void
}) {
  const [moveTo, setMoveTo] = useState<'first' | 'newest' | 'seq' | 'time'>('seq')
  const [seq, setSeq] = useState<number | string>('')
  const [time, setTime] = useState('')
  const [until, setUntil] = useState('')
  const [deleting, setDeleting] = useState(false)
  const nats = { stream: c.stream_name, consumer: c.name }
  const iso = (t: string) => new Date(Date.parse(t)).toISOString().replace(/\.\d{3}Z$/, 'Z')
  const durable = Boolean(c.config.durable_name)
  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 520}
      title={<Group gap={6}><Text fw={650} ff="monospace">{c.name}</Text><Text size={FS.micro} c="dimmed">on</Text>
        <Text size={FS.meta} ff="monospace" style={{ cursor: 'pointer', textDecoration: 'underline' }} onClick={onStream}>{c.stream_name}</Text></Group>}>
      <Stack gap="md">
        <Text size={FS.body}>{consumerSays(c)}</Text>
        <SimpleGrid cols={2} spacing={6}>
          <Fact k="Reads" v={consumerFilters(c.config).join(' ') || 'everything'} mono />
          <Fact k="Kind" v={`${consumerKind(c.config)} · ack ${c.config.ack_policy}`} />
          <Fact k="Delivered up to" v={`#${c.delivered.stream_seq.toLocaleString()}`} mono />
          <Fact k="Acked up to" v={`#${c.ack_floor.stream_seq.toLocaleString()}`} mono />
          <Fact k="Waiting" v={fmtCount(c.num_pending)} />
          <Fact k="Unacked" v={fmtCount(c.num_ack_pending)} />
          <Fact k="Redelivered" v={fmtCount(c.num_redelivered)} />
          <Fact k="Tries at most" v={c.config.max_deliver && c.config.max_deliver > 0 ? String(c.config.max_deliver) : 'no limit'} />
        </SimpleGrid>
        {can('consumer_move') && (
          <Card withBorder radius="md" padding="sm">
            <Text fw={600} size={FS.meta}>Move where it reads from</Text>
            <Text size={FS.micro} c="dimmed" mb="xs">NATS cannot move a consumer, so Argus makes it again with the same name and settings and the new start. Apps see the change on their next pull.</Text>
            <SegmentedControl size="xs" fullWidth value={moveTo} onChange={(v) => setMoveTo(v as typeof moveTo)}
              data={[{ label: 'A sequence', value: 'seq' }, { label: 'A time', value: 'time' }, { label: 'The first', value: 'first' }, { label: 'Newest', value: 'newest' }]} />
            <Group gap="xs" mt="xs" align="flex-end">
              {moveTo === 'seq' && <NumberInput size="xs" value={seq} onChange={setSeq} min={1} placeholder="sequence" w={160} aria-label="Move to sequence" />}
              {moveTo === 'time' && <TextInput size="xs" type="datetime-local" value={time} onChange={(e) => setTime(e.currentTarget.value)} w={220} aria-label="Move to time" />}
              <Button size="xs" variant="default" disabled={!durable || (moveTo === 'seq' && !(Number(seq) > 0)) || (moveTo === 'time' && !Number.isFinite(Date.parse(time)))}
                onClick={() => void act({ op: 'consumer_move', nats: { ...nats, from: moveTo, ...(moveTo === 'seq' ? { seq: Number(seq) } : {}), ...(moveTo === 'time' ? { time: iso(time) } : {}) } },
                  `Moved ${c.name}.`)}>Move</Button>
            </Group>
            {!durable && <Text size={FS.micro} c="dimmed" mt={4}>An ephemeral consumer goes with its app, so it cannot be moved.</Text>}
          </Card>
        )}
        {can('consumer_pause') && (
          <Card withBorder radius="md" padding="sm">
            <Text fw={600} size={FS.meta}>{c.paused ? 'Paused' : 'Pause'}</Text>
            <Text size={FS.micro} c="dimmed" mb="xs">A paused consumer sends nothing until the time, then carries on where it was (NATS 2.11 and later).</Text>
            <Group gap="xs" align="flex-end">
              {c.paused
                ? <Button size="xs" variant="default" onClick={() => void act({ op: 'consumer_pause', nats }, `Resumed ${c.name}.`)}>Resume now</Button>
                : (
                  <>
                    <TextInput size="xs" type="datetime-local" value={until} onChange={(e) => setUntil(e.currentTarget.value)} w={220} aria-label="Pause until" />
                    <Button size="xs" variant="default" disabled={!(Date.parse(until) > Date.now())}
                      onClick={() => void act({ op: 'consumer_pause', nats: { ...nats, time: iso(until) } }, `Paused ${c.name}.`)}>Pause until</Button>
                  </>
                )}
            </Group>
          </Card>
        )}
        {can('consumer_delete') && (
          <Group>
            <Button size="xs" variant="default" color="rose" onClick={() => setDeleting(true)}>Delete consumer…</Button>
          </Group>
        )}
      </Stack>
      <Modal opened={deleting} onClose={() => setDeleting(false)} centered title={<Text fw={650}>Delete {c.name}?</Text>}>
        <Stack gap="sm">
          <Text size={FS.meta}>Apps reading it stop getting messages, and its place in {c.stream_name} is lost. The messages stay in the stream. Recorded.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setDeleting(false)}>Cancel</Button>
            <Button color="rose" onClick={() => {
              setDeleting(false)
              void act({ op: 'consumer_delete', nats }, `Deleted consumer ${c.name}.`).then((a) => { if (a.type === 'result') onClose() })
            }}>Delete</Button>
          </Group>
        </Stack>
      </Modal>
    </Drawer>
  )
}

function Fact({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <Box>
      <Text size={FS.micro} c="dimmed">{k}</Text>
      <Text size={FS.meta} ff={mono ? 'monospace' : undefined}>{v}</Text>
    </Box>
  )
}

export function PublishDrawer({ initial, streams, narrow, ask, canStream, onClose, onDone }: {
  initial: string; streams: NatsStream[]; narrow: boolean; ask: Ask; canStream: boolean; onClose: () => void; onDone: () => void
}) {
  const [subject, setSubject] = useState(initial)
  const [payload, setPayload] = useState('')
  const [headers, setHeaders] = useState<{ k: string; v: string }[]>([])
  const [mode, setMode] = useState<'core' | 'stream' | 'request'>('core')
  const [sending, setSending] = useState(false)
  const [reply, setReply] = useState<NatsMessage | null>(null)
  const subj = subject.trim()
  const ok = appSubject(subj)
  const storedBy = useMemo(() => (ok ? streamsFor(subj, streams) : []), [ok, subj, streams])
  const json = useMemo(() => {
    const t = payload.trim()
    if (!t || !(t.startsWith('{') || t.startsWith('['))) return null
    try { JSON.parse(t); return true } catch { return false }
  }, [payload])
  const send = async () => {
    setSending(true)
    setReply(null)
    const hdrs = Object.fromEntries(headers.filter((h) => h.k.trim()).map((h) => [h.k.trim(), h.v]))
    const a = await ask({ op: 'publish', nats: { subject: subj, mode }, payload, headers: hdrs })
    setSending(false)
    if (a.type !== 'result') { notifyError('Not published', new Error(a.error ?? 'failed')); return }
    const d = a.data as { stream?: string; seq?: number; duplicate?: boolean; reply?: NatsMessage }
    if (mode === 'request' && d.reply) { setReply(d.reply); return }
    notifyOk('Published', mode === 'stream'
      ? `${d.duplicate ? 'Already stored' : 'Stored'} in ${d.stream} as #${d.seq}. It is in the session’s recording.`
      : `Sent on ${subj}. It is in the session’s recording.`)
    onDone()
    onClose()
  }
  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 540} title={<Text fw={650}>Publish a message</Text>}>
      <Stack gap="sm">
        <TextInput label="Subject" value={subject} onChange={(e) => setSubject(e.currentTarget.value)} styles={MONO} placeholder="orders.eu.created" data-autofocus
          error={subj && !ok ? 'A subject an app uses: no wildcards, nothing starting with $ or _INBOX.' : undefined}
          description={ok ? (storedBy.length ? `Stored by ${storedBy.join(', ')}.` : 'No stream stores it: only apps listening now get it.') : undefined} />
        <Box>
          <Text size={FS.meta} fw={500} mb={4}>Send as</Text>
          <SegmentedControl fullWidth size="xs" value={mode} onChange={(v) => setMode(v as typeof mode)}
            data={[{ label: 'Publish', value: 'core' }, { label: 'Publish and wait for the stream', value: 'stream', disabled: !canStream || storedBy.length === 0 }, { label: 'Request a reply', value: 'request' }]} />
        </Box>
        <Box>
          <Group gap="xs" mb={4}>
            <Text size={FS.meta} fw={500}>Data</Text>
            {json !== null && <Text size={FS.micro} c={json ? 'teal' : 'rose'} ml="auto">{json ? 'valid JSON' : 'not valid JSON'}</Text>}
          </Group>
          <Textarea value={payload} onChange={(e) => setPayload(e.currentTarget.value)} autosize minRows={6} maxRows={16}
            styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12.5 } }} placeholder='{"order_id": "ord-1"}' aria-label="Data" />
        </Box>
        <Text size={FS.meta} fw={500}>Headers</Text>
        {headers.map((h, i) => (
          <Group key={i} gap="xs" wrap="nowrap">
            <TextInput size="xs" styles={MONO} placeholder="name" value={h.k} style={{ flex: 1 }} aria-label="Header name"
              onChange={(e) => { const v = e.currentTarget.value; setHeaders((hs) => hs.map((x, j) => (j === i ? { ...x, k: v } : x))) }} />
            <TextInput size="xs" styles={MONO} placeholder="value" value={h.v} style={{ flex: 1 }} aria-label="Header value"
              onChange={(e) => { const v = e.currentTarget.value; setHeaders((hs) => hs.map((x, j) => (j === i ? { ...x, v } : x))) }} />
            <ActionIcon variant="subtle" color="slate" aria-label="Remove header" onClick={() => setHeaders((hs) => hs.filter((_, j) => j !== i))}><IconX size={14} /></ActionIcon>
          </Group>
        ))}
        <Group gap="xs">
          <Button size="compact-xs" variant="subtle" leftSection={<IconPlus size={12} />} onClick={() => setHeaders((hs) => [...hs, { k: '', v: '' }])} disabled={headers.length >= 64}>Add header</Button>
          {!headers.some((h) => h.k === 'Nats-Msg-Id') && (
            <Button size="compact-xs" variant="subtle" onClick={() => setHeaders((hs) => [...hs, { k: 'Nats-Msg-Id', v: crypto.randomUUID() }])}>Add Nats-Msg-Id</Button>
          )}
        </Group>
        {reply && (
          <Box>
            <Group gap={6} mb={4}><Text size={FS.meta} fw={500}>Reply</Text><Badge size="xs" variant="light" color="teal" radius="sm">answered</Badge></Group>
            <Message m={reply} narrow={narrow} />
          </Box>
        )}
        <Group mt="md" gap="xs">
          <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>The subject, headers and data are kept in the recording.</Text>
          <Button variant="default" onClick={onClose}>{reply ? 'Close' : 'Cancel'}</Button>
          <Button onClick={() => void send()} loading={sending} disabled={!ok}>{mode === 'request' ? 'Send request' : 'Publish'}</Button>
        </Group>
      </Stack>
    </Drawer>
  )
}
