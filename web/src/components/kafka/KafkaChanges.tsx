import {
  Alert, Badge, Button, Card, Drawer, Group, Modal, NumberInput, SegmentedControl, Select, SimpleGrid, Stack, Table, Text, TextInput,
} from '@mantine/core'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { ChoiceCards, MONO } from '~/components/brokerusers/parts'
import { dataOf, type ChangeAnswer } from '~/components/redis/useRedisChange'
import type { StoreAnswer, StoreRequest } from '~/components/useStoreSession'
import { fmtCount } from '~/lib/kafka'
import {
  BYTE_UNITS, MS_UNITS, behindAfter, fromSettingValue, planCount, settingWords, toSettingValue,
  type KafkaChange, type KafkaConfig, type KafkaDryRun, type KafkaGroupDetail, type KafkaSetting,
} from '~/lib/kafkaChanges'
import { FS } from '~/theme'

/*
The Kafka topic and group changes beside creating and deleting topics: a
topic's settings, more partitions, deleting the oldest messages, and moving
or deleting a consumer group. Each needs Manage; the agent records it, and
on an entry whose changes need approval it waits for one (change() asks).

Deleting messages and moving a group show, per partition, what happens
before anything runs: a dry run works out the exact offsets, and those are
what is sent, recorded and approved.
*/

type Ask = (r: StoreRequest) => Promise<StoreAnswer>
type Change = (r: StoreRequest) => Promise<ChangeAnswer>

function outcome(a: ChangeAnswer): string | null {
  if (a.notApproved) return 'Nothing changed: it was not approved.'
  return a.type === 'result' ? null : a.error ?? 'failed'
}

function req(op: string, name: string | undefined, kafkaChange: KafkaChange, extra: Record<string, unknown> = {}): StoreRequest {
  return { op, ...(name ? { name } : {}), kafkaChange, ...extra }
}

/** Every setting of a topic, the ones Argus changes first, in words. */
export function SettingsCard({ topic, ask, canEdit, onEdit, version }: { topic: string; ask: Ask; canEdit: boolean; onEdit: (c: KafkaConfig[], s: KafkaSetting[]) => void; version: number }) {
  const [data, setData] = useState<{ configs: KafkaConfig[]; settings: KafkaSetting[] } | null>(null)
  const [all, setAll] = useState(false)
  useEffect(() => {
    void ask(req('topic_configs', topic, {})).then((a) => setData(dataOf(a)))
  }, [ask, topic, version])
  if (!data) return <Card withBorder radius="md" padding="sm"><Text size={FS.meta} c="dimmed">Reading the settings…</Text></Card>
  const by = new Map(data.configs.map((c) => [c.key, c]))
  return (
    <Card withBorder radius="md" padding="sm">
      <Group mb={6}>
        <Text fw={600} size={FS.body} style={{ flex: 1 }}>Settings</Text>
        {canEdit && <Button size="compact-xs" variant="light" onClick={() => onEdit(data.configs, data.settings)}>Edit settings</Button>}
      </Group>
      <Stack gap={4}>
        {data.settings.map((s) => {
          const c = by.get(s.key)
          return (
            <Group key={s.key} gap="xs" wrap="nowrap" align="flex-start">
              <Stack gap={0} style={{ flex: 1, minWidth: 0 }}>
                <Text size={FS.meta}>{s.name}</Text>
                <Text size={FS.micro} ff="monospace" c="dimmed">{s.key}</Text>
              </Stack>
              <Text size={FS.meta} fw={c && !c.default ? 600 : undefined}>{c ? settingWords(s, c.value) : '–'}</Text>
              <Badge size="xs" variant="light" color={c && !c.default ? 'azure' : 'slate'} radius="sm" style={{ textTransform: 'none' }} w={78}>
                {c && !c.default ? 'this topic' : 'default'}
              </Badge>
            </Group>
          )
        })}
      </Stack>
      <Button size="compact-xs" variant="subtle" mt="xs" onClick={() => setAll((v) => !v)}>{all ? 'Hide' : 'All'} {data.configs.length} settings</Button>
      {all && (
        <Stack gap={1} mt={4} mah={260} style={{ overflow: 'auto' }}>
          {data.configs.map((c) => (
            <Text key={c.key} size={FS.micro} ff="monospace"><Text span c="dimmed" ff="monospace" size={FS.micro}>{c.key}</Text> = {c.sensitive ? '[hidden]' : c.value}{c.default ? '' : ' (this topic)'}</Text>
          ))}
        </Stack>
      )}
    </Card>
  )
}

/** One row of the settings drawer: keep, change, or reset to the default. */
function SettingRow({ s, c, value, onChange }: { s: KafkaSetting; c?: KafkaConfig; value: string | null; onChange: (v: string | null) => void }) {
  const cur = value ?? c?.value ?? ''
  const split = fromSettingValue(s.kind, cur)
  const [unit, setUnit] = useState(split.unit || (s.kind === 'ms' ? 'days' : 'MB'))
  const changed = value !== null
  const units = s.kind === 'ms' ? MS_UNITS : s.kind === 'bytes' ? BYTE_UNITS : []
  const amount = s.kind === 'ms' || s.kind === 'bytes' ? (Number(cur) === -1 ? '' : fromSettingValue(s.kind, cur).unit === unit ? fromSettingValue(s.kind, cur).amount : Number(cur) / Number(toSettingValue(s.kind, 1, unit))) : Number(cur)
  return (
    <Group gap="xs" wrap="wrap" align="flex-end" py={6} style={{ borderBottom: '1px solid var(--color-line)' }}>
      <Stack gap={0} style={{ flex: 1, minWidth: 180 }}>
        <Text size={FS.meta} fw={500}>{s.name}</Text>
        <Text size={FS.micro} ff="monospace" c="dimmed">{s.key} · {c && !c.default ? 'set on this topic' : 'the cluster default'}{changed ? ' · changed' : ''}</Text>
      </Stack>
      {s.kind === 'choice' ? (
        <Select size="xs" w={170} data={s.choices ?? []} value={cur} allowDeselect={false} aria-label={s.name} onChange={(v) => v && onChange(v)} />
      ) : s.kind === 'int' ? (
        <NumberInput size="xs" w={110} value={amount} min={1} allowDecimal={false} aria-label={s.name} onChange={(v) => onChange(String(v))} />
      ) : (
        <Group gap={4} wrap="nowrap">
          <NumberInput size="xs" w={100} value={amount} min={0} placeholder="no limit" aria-label={s.name}
            onChange={(v) => onChange(v === '' ? '-1' : toSettingValue(s.kind, Number(v), unit))} />
          <Select size="xs" w={96} data={[...units]} value={unit} allowDeselect={false} aria-label={`${s.name} unit`}
            onChange={(u) => { if (!u) return; setUnit(u); if (cur !== '' && Number(cur) !== -1 && changed) onChange(toSettingValue(s.kind, Number(amount), u)) }} />
        </Group>
      )}
      {c && !c.default && <Button size="compact-xs" variant="subtle" color="slate" onClick={() => onChange('')}>Reset</Button>}
      {changed && <Button size="compact-xs" variant="subtle" onClick={() => onChange(null)}>Keep</Button>}
    </Group>
  )
}

export function SettingsDrawer({ topic, configs, settings, change, narrow, onClose, onDone }: {
  topic: string; configs: KafkaConfig[]; settings: KafkaSetting[]; change: Change; narrow: boolean; onClose: () => void; onDone: () => void
}) {
  // null keeps, '' resets to the default, anything else sets.
  const [edits, setEdits] = useState<Record<string, string | null>>({})
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const by = new Map(configs.map((c) => [c.key, c]))
  const set: Record<string, string> = {}
  const reset: string[] = []
  for (const [k, v] of Object.entries(edits)) {
    if (v === null) continue
    if (v === '') reset.push(k)
    else if (v !== by.get(k)?.value || by.get(k)?.default) set[k] = v
  }
  const n = Object.keys(set).length + reset.length
  const shorter = set['retention.ms'] && by.get('retention.ms') && Number(set['retention.ms']) !== -1 &&
    (Number(by.get('retention.ms')!.value) === -1 || Number(set['retention.ms']) < Number(by.get('retention.ms')!.value))
  const save = async () => {
    setBusy(true); setErr(null)
    const e = outcome(await change(req('alter_configs', topic, { set, reset })))
    setBusy(false)
    if (e) setErr(e); else onDone()
  }
  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 620} title={<Text fw={650}>Settings of <Text span ff="monospace">{topic}</Text></Text>}>
      <Stack gap="sm">
        <Text size={FS.micro} c="dimmed" mt={-8}>Only what you change is sent (Kafka's incremental alter), so other settings stay as they are. Replication settings stay read only.</Text>
        <Stack gap={0}>
          {settings.map((s) => (
            <SettingRow key={s.key} s={s} c={by.get(s.key)} value={edits[s.key] ?? null}
              onChange={(v) => setEdits((e) => ({ ...e, [s.key]: v }))} />
          ))}
        </Stack>
        {shorter && <Alert color="amber" variant="light"><Text size={FS.micro}>Keeping messages for less time deletes the older ones at Kafka's next clean-up, for every reader.</Text></Alert>}
        {err && <Alert color="rose" variant="light">{err}</Alert>}
        <Group justify="flex-end">
          <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>{n === 0 ? 'Nothing changed yet.' : `${n} setting${n === 1 ? '' : 's'} to change. Recorded.`}</Text>
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void save()} disabled={n === 0} loading={busy}>Save</Button>
        </Group>
      </Stack>
    </Drawer>
  )
}

export function AddPartitions({ topic, current, change, onClose, onDone }: { topic: string; current: number; change: Change; onClose: () => void; onDone: () => void }) {
  const [add, setAdd] = useState<number | string>(1)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const n = Number(add) || 0
  const go = async () => {
    setBusy(true); setErr(null)
    const e = outcome(await change(req('add_partitions', topic, { add: n })))
    setBusy(false)
    if (e) setErr(e); else onDone()
  }
  return (
    <Modal opened onClose={onClose} title={<Text fw={650}>Add partitions to {topic}</Text>} size="md">
      <Stack gap="sm">
        <Group gap="sm" align="flex-end">
          <NumberInput label="Partitions to add" value={add} onChange={setAdd} min={1} max={1000 - current} allowDecimal={false} w={160} data-autofocus />
          <Text size={FS.meta} pb={8}>{current} now, {current + n} after</Text>
        </Group>
        <Alert color="amber" variant="light">
          <Text size={FS.micro}>Messages with a key go to a partition chosen from the key and the number of partitions, so from now on a key's new messages may land on a different partition than its old ones: ordering per key starts again. Partitions are never removed.</Text>
        </Alert>
        {err && <Alert color="rose" variant="light">{err}</Alert>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void go()} disabled={n < 1} loading={busy}>Add {n} partition{n === 1 ? '' : 's'}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function PlanTable({ plan, kind }: { plan: KafkaDryRun['plan']; kind: 'delete' | 'reset' }) {
  const rows = plan ?? []
  if (rows.length === 0) return <Text size={FS.meta} c="dimmed">Nothing would change.</Text>
  return (
    <Table.ScrollContainer minWidth={420}>
      <Table fz="sm" verticalSpacing={4}>
        <Table.Thead>
          <Table.Tr>
            {kind === 'reset' && <Table.Th>Topic</Table.Th>}
            <Table.Th>Partition</Table.Th>
            <Table.Th ta="right">{kind === 'delete' ? 'Starts at now' : 'Now'}</Table.Th>
            <Table.Th ta="right">{kind === 'delete' ? 'Starts at after' : 'After'}</Table.Th>
            <Table.Th ta="right">{kind === 'delete' ? 'Deleted' : 'Change'}</Table.Th>
            {kind === 'reset' && <Table.Th ta="right">Behind after</Table.Th>}
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {rows.map((p) => (
            <Table.Tr key={`${p.topic}:${p.partition}`}>
              {kind === 'reset' && <Table.Td ff="monospace">{p.topic}</Table.Td>}
              <Table.Td>{p.partition}</Table.Td>
              <Table.Td ta="right" ff="monospace">{p.now < 0 ? 'none' : p.now.toLocaleString()}</Table.Td>
              <Table.Td ta="right" ff="monospace">{p.after.toLocaleString()}</Table.Td>
              <Table.Td ta="right" ff="monospace">{kind === 'delete' ? fmtCount(p.after - p.now) : `${p.after - Math.max(p.now, p.start) >= 0 ? '+' : ''}${(p.after - Math.max(p.now, p.start)).toLocaleString()}`}</Table.Td>
              {kind === 'reset' && <Table.Td ta="right" ff="monospace">{fmtCount(behindAfter(p))}</Table.Td>}
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  )
}

const DELETE_WAYS: { value: 'time' | 'offset' | 'all'; label: string; hint: string }[] = [
  { value: 'time', label: 'Older than a time', hint: 'On every partition, or one' },
  { value: 'offset', label: 'Before an offset', hint: 'On one partition' },
  { value: 'all', label: 'All of them', hint: 'Empties the topic, keeps it' },
]

/** A date and time input's value now, minus a day, as the field takes it. */
function localInput(ms: number): string {
  const d = new Date(ms - new Date().getTimezoneOffset() * 60_000)
  return d.toISOString().slice(0, 16)
}

export function DeleteMessages({ topic, partitions, hostname, ask, change, onClose, onDone }: {
  topic: string; partitions: number; hostname: string; ask: Ask; change: Change; onClose: () => void; onDone: () => void
}) {
  const [way, setWay] = useState<'time' | 'offset' | 'all'>('time')
  const [when, setWhen] = useState(localInput(Date.now() - 86_400_000))
  const [partition, setPartition] = useState<string>(way === 'offset' ? '0' : '')
  const [offset, setOffset] = useState<number | string>('')
  const [dry, setDry] = useState<KafkaDryRun | null>(null)
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const spec = useMemo((): KafkaChange | null => {
    const p = partition === '' ? undefined : Number(partition)
    if (way === 'time') { const at = new Date(when).getTime(); return Number.isFinite(at) ? { to: 'time', at, ...(p !== undefined ? { partition: p } : {}) } : null }
    if (way === 'offset') return p !== undefined && offset !== '' ? { to: 'offset', partition: p, offset: Number(offset) } : null
    return { to: 'all', ...(p !== undefined ? { partition: p } : {}) }
  }, [way, when, partition, offset])
  useEffect(() => {
    setDry(null)
    if (!spec) return
    let live = true
    const t = setTimeout(() => {
      void ask(req('delete_records', topic, { ...spec, dryRun: true })).then((a) => {
        if (!live) return
        if (a.type === 'result') { setDry(dataOf<KafkaDryRun>(a)); setErr(null) } else setErr(a.error ?? 'could not work out the offsets')
      })
    }, 250)
    return () => { live = false; clearTimeout(t) }
  }, [ask, topic, spec])
  const gone = planCount(dry?.plan, 'delete')
  const unread = Object.entries(dry?.unread ?? {}).filter(([, n]) => n > 0)
  const go = async () => {
    if (!dry) return
    setBusy(true); setErr(null)
    const e = outcome(await change(req('delete_records', topic, { offsets: dry.offsets }, { confirm: typed })))
    setBusy(false)
    if (e) setErr(e); else onDone()
  }
  const partOpts = [...(way === 'offset' ? [] : [{ value: '', label: 'Every partition' }]), ...Array.from({ length: partitions }, (_, i) => ({ value: String(i), label: `Partition ${i}` }))]
  return (
    <Modal opened onClose={onClose} title={<Text fw={650}>Delete messages from {topic}</Text>} size={760}>
      <Stack gap="sm">
        <ChoiceCards value={way} onChange={(w) => { setWay(w); if (w === 'offset' && partition === '') setPartition('0') }} options={DELETE_WAYS} />
        <Group gap="sm" align="flex-end" wrap="wrap">
          {way === 'time' && <TextInput type="datetime-local" label="Older than" value={when} onChange={(e) => setWhen(e.currentTarget.value)} w={220} />}
          <Select label="On" data={partOpts} value={partition} onChange={(v) => setPartition(v ?? '')} allowDeselect={false} w={170} />
          {way === 'offset' && <NumberInput label="Before offset" value={offset} onChange={setOffset} min={0} allowDecimal={false} w={160} />}
        </Group>
        <PlanTable plan={dry?.plan ?? null} kind="delete" />
        {dry && gone > 0 && (
          <Alert color="rose" variant="light">
            <Text size={FS.micro}>{fmtCount(gone)} message{gone === 1 ? '' : 's'} will be deleted for every reader. This cannot be undone.
              {unread.length > 0 && ` Not read yet by ${unread.map(([g, n]) => `${g} (${fmtCount(n)})`).join(', ')}.`}</Text>
          </Alert>
        )}
        <TextInput label={`Type the server's name, ${hostname}, to confirm`} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} styles={MONO} />
        {err && <Alert color="rose" variant="light">{err}</Alert>}
        <Group justify="flex-end">
          <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>Recorded with the exact offsets above.</Text>
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" onClick={() => void go()} disabled={!dry || gone === 0 || typed !== hostname} loading={busy}>Delete {fmtCount(gone)} messages</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

const RESET_WAYS = [
  { value: 'earliest', label: 'Earliest' }, { value: 'latest', label: 'Latest' }, { value: 'time', label: 'From a time' },
  { value: 'shift', label: 'By a number' }, { value: 'offset', label: 'An offset' },
]

/** One consumer group: where it is on each partition, and moving or deleting it once it is empty. */
export function GroupDrawer({ group, topics, ask, change, canChange, narrow, onClose, onDone }: {
  group: string; topics: string[]; ask: Ask; change: Change; canChange: boolean; narrow: boolean; onClose: () => void; onDone: () => void
}) {
  const [d, setD] = useState<KafkaGroupDetail | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [to, setTo] = useState<NonNullable<KafkaChange['to']>>('earliest')
  const [when, setWhen] = useState(localInput(Date.now() - 3_600_000))
  const [by, setBy] = useState<number | string>(-100)
  const [offset, setOffset] = useState<number | string>(0)
  const [topic, setTopic] = useState('')
  const [partition, setPartition] = useState('0')
  const [dry, setDry] = useState<KafkaDryRun | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [note, setNote] = useState<string | null>(null)
  const [deleting, setDeleting] = useState(false)

  const load = useCallback(async () => {
    const a = await ask(req('group', undefined, { group }))
    if (a.type === 'result') { setD(dataOf<KafkaGroupDetail>(a)); setErr(null) } else setErr(a.error ?? 'could not read the group')
  }, [ask, group])
  useEffect(() => { void load() }, [load])
  const empty = d?.state === 'Empty' || d?.state === 'Dead'
  const groupTopics = [...new Set((d?.offsets ?? []).map((o) => o.topic))]
  const spec = useMemo((): KafkaChange | null => {
    const base: KafkaChange = { group, ...(topic ? { topics: [topic] } : {}) }
    if (to === 'time') { const at = new Date(when).getTime(); return Number.isFinite(at) ? { ...base, to, at } : null }
    if (to === 'shift') return Number(by) ? { ...base, to, by: Number(by) } : null
    if (to === 'offset') return topic ? { ...base, to, offset: Number(offset), partition: Number(partition) } : null
    return { ...base, to }
  }, [group, topic, to, when, by, offset, partition])
  useEffect(() => {
    setDry(null)
    if (!spec || !canChange || !empty) return
    let live = true
    const t = setTimeout(() => {
      void ask(req('group_reset', topic || undefined, { ...spec, dryRun: true })).then((a) => {
        if (!live) return
        if (a.type === 'result') { setDry(dataOf<KafkaDryRun>(a)); setErr(null) } else setErr(a.error ?? 'could not work out the offsets')
      })
    }, 250)
    return () => { live = false; clearTimeout(t) }
  }, [ask, spec, canChange, empty, topic])

  const run = async (what: string, r: StoreRequest) => {
    setBusy(what); setNote(null)
    const e = outcome(await change(r))
    setBusy(null)
    if (e) { setNote(e); return false }
    onDone()
    return true
  }
  const total = (d?.offsets ?? []).reduce((n, o) => n + Math.max(0, o.end - Math.max(o.now, o.start)), 0)
  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 680}
      title={<Group gap={8}><Text ff="monospace" fw={650}>{group}</Text>{d && <Badge size="sm" variant="light" color={empty ? 'slate' : 'teal'} style={{ textTransform: 'none' }}>{d.state}</Badge>}</Group>}>
      {!d ? (err ? <Alert color="rose" variant="light">{err}</Alert> : <Text size={FS.meta} c="dimmed">Reading the group…</Text>) : (
        <Stack gap="md">
          <Text size={FS.micro} c="dimmed" mt={-8}>{d.members.length} member{d.members.length === 1 ? '' : 's'} · {fmtCount(total)} behind on this entry's topics{d.outside ? ` · offsets on ${d.outside} other topic${d.outside === 1 ? '' : 's'}` : ''}</Text>
          {d.members.length > 0 && (
            <Card withBorder radius="md" padding="sm">
              <Text fw={600} size={FS.meta} mb={4}>Members</Text>
              {d.members.map((m) => <Text key={m.id} size={FS.micro} ff="monospace">{m.client} on {m.host}{m.topics?.length ? ` · ${m.topics.join(', ')}` : ''}</Text>)}
            </Card>
          )}
          <Card withBorder radius="md" padding={0}>
            <Text fw={600} size={FS.meta} p="sm" pb={0}>Where it is</Text>
            <Table.ScrollContainer minWidth={420}>
              <Table fz="sm" verticalSpacing={4}>
                <Table.Thead><Table.Tr><Table.Th>Topic</Table.Th><Table.Th>Partition</Table.Th><Table.Th ta="right">Committed</Table.Th><Table.Th ta="right">End</Table.Th><Table.Th ta="right">Behind</Table.Th></Table.Tr></Table.Thead>
                <Table.Tbody>
                  {d.offsets.map((o) => (
                    <Table.Tr key={`${o.topic}:${o.partition}`}>
                      <Table.Td ff="monospace">{o.topic}</Table.Td><Table.Td>{o.partition}</Table.Td>
                      <Table.Td ta="right" ff="monospace">{o.now < 0 ? 'none' : o.now.toLocaleString()}</Table.Td>
                      <Table.Td ta="right" ff="monospace">{o.end.toLocaleString()}</Table.Td>
                      <Table.Td ta="right" ff="monospace">{fmtCount(Math.max(0, o.end - Math.max(o.now, o.start)))}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          </Card>
          {canChange && !empty && (
            <Alert color="amber" variant="light"><Text size={FS.micro}>Kafka moves or deletes a group only once its consumers stop. Stop {d.members.map((m) => m.client || m.id).join(', ')}, then open it again.</Text></Alert>
          )}
          {canChange && empty && (
            <Card withBorder radius="md" padding="sm">
              <Text fw={600} size={FS.meta} mb={6}>Move the group</Text>
              <Stack gap="xs">
                <SegmentedControl size="xs" value={to} onChange={(v) => setTo(v as typeof to)} data={RESET_WAYS} fullWidth />
                <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="xs">
                  <Select size="xs" label="On" value={topic} onChange={(v) => setTopic(v ?? '')} allowDeselect={false}
                    data={[...(to === 'offset' ? [] : [{ value: '', label: 'All its topics here' }]), ...[...new Set([...groupTopics, ...topics])].map((t) => ({ value: t, label: t }))]} />
                  {to === 'time' && <TextInput size="xs" type="datetime-local" label="From" value={when} onChange={(e) => setWhen(e.currentTarget.value)} />}
                  {to === 'shift' && <NumberInput size="xs" label="By (back when below 0)" value={by} onChange={setBy} allowDecimal={false} />}
                  {to === 'offset' && <NumberInput size="xs" label="Partition" value={Number(partition)} onChange={(v) => setPartition(String(v))} min={0} allowDecimal={false} />}
                  {to === 'offset' && <NumberInput size="xs" label="Offset" value={offset} onChange={setOffset} min={0} allowDecimal={false} />}
                </SimpleGrid>
                <PlanTable plan={dry?.plan ?? null} kind="reset" />
                {note && <Alert color="rose" variant="light">{note}</Alert>}
                <Group justify="flex-end">
                  <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>{dry ? `${planCount(dry.plan, 'reset') < 0 ? 'Reads again' : 'Skips'} ${fmtCount(Math.abs(planCount(dry.plan, 'reset')))} messages. Recorded with the exact offsets.` : ''}</Text>
                  <Button size="xs" onClick={() => dry && void run('move', req('group_reset', topic || undefined, { group, offsets: dry.offsets }))}
                    disabled={!dry || (dry.plan ?? []).length === 0} loading={busy === 'move'}>Move {group}</Button>
                </Group>
              </Stack>
            </Card>
          )}
          {canChange && empty && (
            <Group pt="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
              {deleting ? (
                <>
                  <Text size={FS.micro} style={{ flex: 1 }}>Deletes {group} and every offset it committed. Its consumers start from their reset policy next time.</Text>
                  <Button size="xs" variant="default" onClick={() => setDeleting(false)}>Cancel</Button>
                  <Button size="xs" color="rose" loading={busy === 'delete'} onClick={() => void run('delete', req('delete_group', undefined, { group })).then((ok) => ok && onClose())}>Delete group</Button>
                </>
              ) : <Button size="xs" variant="subtle" color="rose" onClick={() => setDeleting(true)}>Delete group</Button>}
            </Group>
          )}
        </Stack>
      )}
    </Drawer>
  )
}
