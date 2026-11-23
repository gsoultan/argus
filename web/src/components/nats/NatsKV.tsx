import {
  ActionIcon, Badge, Box, Button, Card, Code, Group, Modal, NumberInput, SegmentedControl, SimpleGrid, Stack, Table, Text, TextInput, Textarea,
} from '@mantine/core'
import { IconPlus, IconRefresh, IconSearch, IconTrash } from '@tabler/icons-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import type { StoreAnswer } from '~/components/useStoreSession'
import { ConfirmName } from '~/components/nats/NatsForms'
import type { Ask, Req } from '~/components/nats/NatsWorkspace'
import {
  ago, bucketProblems, fmtBytes, fmtCount, items, showData, validKey,
  type KVSpec, type NatsKVBucket, type NatsKVEntry, type NatsKVKey,
} from '~/lib/nats'
import { notifyError } from '~/lib/notify'
import { FS } from '~/theme'

/*
The account's key-value buckets. A bucket is a stream, and each key a
subject in it, so the keys and their values are read with the stream's own
get: nothing is watched and no consumer is made. Read sees them; Write puts
and deletes keys; Manage makes and removes buckets. Reading a value and
every change are recorded.
*/

const MONO = { input: { fontFamily: 'var(--font-mono)' } }

export function KeyValue({ ask, act, can, narrow }: {
  ask: Ask; act: (r: Req, ok: string) => Promise<StoreAnswer>; can: (op: string) => boolean; narrow: boolean
}) {
  const [buckets, setBuckets] = useState<NatsKVBucket[] | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  const [making, setMaking] = useState(false)
  const load = useCallback(async () => {
    const a = await ask({ op: 'kv_buckets' })
    if (a.type === 'result') { setBuckets(items<NatsKVBucket>(a.data)); setErr(null) } else setErr(a.error ?? 'could not list buckets')
  }, [ask])
  useEffect(() => { void load() }, [load])
  useEffect(() => {
    if (buckets && !open && buckets.length > 0 && !narrow) setOpen(buckets[0]!.bucket)
  }, [buckets, open, narrow])
  const bucket = buckets?.find((b) => b.bucket === open)

  const list = (
    <Card withBorder radius="md" padding={0}>
      <Group justify="space-between" p="sm" pb={6}>
        <Text fw={600} size={FS.body}>Buckets</Text>
        <Group gap={4}>
          <ActionIcon size="sm" variant="subtle" aria-label="Refresh buckets" onClick={() => void load()}><IconRefresh size={14} /></ActionIcon>
          {can('kv_create') && <Button size="compact-xs" leftSection={<IconPlus size={12} />} onClick={() => setMaking(true)}>New bucket</Button>}
        </Group>
      </Group>
      {err && <Text size={FS.meta} c="rose" px="sm">{err}</Text>}
      {buckets === null && !err && <Text size={FS.meta} c="dimmed" p="sm">Reading buckets…</Text>}
      {buckets?.length === 0 && <Text size={FS.meta} c="dimmed" p="sm">No key-value buckets in this account.</Text>}
      <Stack gap={0}>
        {(buckets ?? []).map((b) => (
          <Box key={b.bucket} px="sm" py={8} onClick={() => setOpen(b.bucket)} role="button" aria-pressed={open === b.bucket}
            style={{ cursor: 'pointer', borderTop: '1px solid var(--color-line)', background: open === b.bucket ? 'var(--mantine-color-azure-light)' : undefined }}>
            <Text ff="monospace" size={FS.meta} fw={500}>{b.bucket}</Text>
            <Text size={FS.micro} c="dimmed">{fmtCount(b.values)} values · {fmtBytes(b.bytes)} · history {b.history}{b.ttlS ? ` · kept ${fmtTTL(b.ttlS)}` : ''} · {b.storage} R{b.replicas}</Text>
          </Box>
        ))}
      </Stack>
    </Card>
  )
  return (
    <>
      {narrow && open && bucket
        ? <Bucket key={open} b={bucket} ask={ask} act={act} can={can} narrow={narrow} onBack={() => setOpen(null)} onGone={() => { setOpen(null); void load() }} />
        : (
          <Box style={{ display: 'grid', gridTemplateColumns: narrow ? '1fr' : 'minmax(240px, 300px) 1fr', gap: 'var(--mantine-spacing-md)', alignItems: 'start' }}>
            {list}
            {!narrow && (bucket
              ? <Bucket key={open} b={bucket} ask={ask} act={act} can={can} narrow={narrow} onGone={() => { setOpen(null); void load() }} />
              : <Text size={FS.meta} c="dimmed">Pick a bucket to see its keys.</Text>)}
          </Box>
        )}
      {making && buckets && <NewBucket buckets={buckets} onClose={() => setMaking(false)}
        onSave={async (spec) => {
          const a = await act({ op: 'kv_create', nats: { kvSpec: spec } }, `Made bucket ${spec.bucket}.`)
          if (a.type === 'result') { setMaking(false); await load(); setOpen(spec.bucket) }
        }} />}
    </>
  )
}

function fmtTTL(s: number): string {
  if (s % 86_400 === 0) return `${s / 86_400} day${s === 86_400 ? '' : 's'}`
  if (s % 3600 === 0) return `${s / 3600} h`
  if (s % 60 === 0) return `${s / 60} min`
  return `${s} s`
}

function Bucket({ b, ask, act, can, narrow, onBack, onGone }: {
  b: NatsKVBucket; ask: Ask; act: (r: Req, ok: string) => Promise<StoreAnswer>; can: (op: string) => boolean; narrow: boolean
  onBack?: () => void; onGone: () => void
}) {
  const [filter, setFilter] = useState('')
  const [keys, setKeys] = useState<NatsKVKey[] | null>(null)
  const [total, setTotal] = useState(0)
  const [showDeleted, setShowDeleted] = useState(false)
  const [key, setKey] = useState<string | null>(null)
  const [putting, setPutting] = useState<{ key: string; value: string } | null>(null)
  const [removing, setRemoving] = useState(false)
  const f = filter.trim()
  const filterOk = !f || validKey(f, true)
  const load = useCallback(async () => {
    const a = await ask({ op: 'kv_keys', nats: { bucket: b.bucket, ...(f && filterOk ? { key: f } : {}) } })
    if (a.type === 'result') { setKeys(items<NatsKVKey>(a.data)); setTotal(a.total ?? 0) } else notifyError('Could not list keys', new Error(a.error ?? 'failed'))
  }, [ask, b.bucket, f, filterOk])
  useEffect(() => { void load() }, [b.bucket]) // eslint-disable-line react-hooks/exhaustive-deps
  const rows = useMemo(() => (keys ?? []).filter((k) => showDeleted || k.op === 'put'), [keys, showDeleted])
  const deleted = (keys ?? []).filter((k) => k.op !== 'put').length
  return (
    <Stack gap="sm">
      <Group gap="xs" wrap="wrap">
        {onBack && <Button size="xs" variant="subtle" onClick={onBack}>All buckets</Button>}
        <Text ff="monospace" fw={650} size={FS.body}>{b.bucket}</Text>
        {b.description && <Text size={FS.micro} c="dimmed">{b.description}</Text>}
        <Group gap={6} ml="auto">
          {can('kv_put') && <Button size="xs" leftSection={<IconPlus size={12} />} onClick={() => setPutting({ key: '', value: '' })}>Put a key</Button>}
          {can('kv_delete') && <Button size="xs" variant="default" color="rose" leftSection={<IconTrash size={12} />} onClick={() => setRemoving(true)}>Remove bucket…</Button>}
        </Group>
      </Group>
      <Group gap="xs" wrap="wrap">
        <TextInput size="xs" placeholder="Keys, e.g. eu.* or users.>" leftSection={<IconSearch size={12} />} value={filter} styles={MONO}
          onChange={(e) => setFilter(e.currentTarget.value)} onKeyDown={(e) => { if (e.key === 'Enter' && filterOk) void load() }} aria-label="Filter keys"
          w={narrow ? '100%' : 260} error={!filterOk ? 'not a key filter' : undefined} />
        <Button size="xs" variant="default" onClick={() => void load()} disabled={!filterOk}>Find</Button>
        {deleted > 0 && <SegmentedControl size="xs" value={showDeleted ? 'all' : 'live'} onChange={(v) => setShowDeleted(v === 'all')}
          data={[{ label: 'Live keys', value: 'live' }, { label: `With ${deleted} deleted`, value: 'all' }]} />}
      </Group>
      <Card withBorder radius="md" padding={0}>
        <Table.ScrollContainer minWidth={520}>
          <Table verticalSpacing={6} fz="sm" highlightOnHover>
            <Table.Thead><Table.Tr><Table.Th>Key</Table.Th><Table.Th ta="right">Revision</Table.Th><Table.Th ta="right">Size</Table.Th><Table.Th ta="right">Kept</Table.Th><Table.Th>Changed</Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {keys === null && <Table.Tr><Table.Td colSpan={5}><Text size={FS.meta} c="dimmed" p="sm">Reading keys…</Text></Table.Td></Table.Tr>}
              {rows.map((k) => (
                <Table.Tr key={k.key} onClick={() => setKey(k.key)} style={{ cursor: 'pointer' }}>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <Text ff="monospace" size={FS.meta} td={k.op !== 'put' ? 'line-through' : undefined}>{k.key}</Text>
                      {k.op !== 'put' && <Badge size="xs" variant="light" color="slate" radius="sm">deleted</Badge>}
                    </Group>
                  </Table.Td>
                  <Table.Td ta="right"><Text size={FS.micro} ff="monospace">#{k.revision.toLocaleString()}</Text></Table.Td>
                  <Table.Td ta="right">{k.op === 'put' ? fmtBytes(k.size) : '–'}</Table.Td>
                  <Table.Td ta="right">{k.kept}</Table.Td>
                  <Table.Td><Text size={FS.micro} c="dimmed">{k.time ? `${ago(k.time)} ago` : ''}</Text></Table.Td>
                </Table.Tr>
              ))}
              {keys !== null && rows.length === 0 && <Table.Tr><Table.Td colSpan={5}><Text size={FS.meta} c="dimmed" p="sm">{f ? `No key matches ${f}.` : 'No keys in this bucket.'}</Text></Table.Td></Table.Tr>}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Card>
      <Text size={FS.micro} c="dimmed">
        {total > rows.length && keys && keys.length < total ? `The first ${keys.length} of ${fmtCount(total)} keys, in order; narrow them with a filter. ` : ''}
        Read with the bucket&rsquo;s own get, so nothing is watched. A deleted key stays listed until the bucket is compacted.
      </Text>
      {key && <KeyDrawer bucket={b.bucket} k={key} ask={ask} act={act} can={can} narrow={narrow} onClose={() => setKey(null)}
        onEdit={(value) => { setKey(null); setPutting({ key, value }) }} onChanged={() => void load()} />}
      {putting && <PutKey bucket={b.bucket} initial={putting} maxValue={b.maxValueSize} onClose={() => setPutting(null)}
        onPut={async (k, v) => {
          const a = await act({ op: 'kv_put', nats: { bucket: b.bucket, key: k }, payload: v }, `Put ${k} in ${b.bucket}.`)
          if (a.type === 'result') { setPutting(null); void load() }
        }} />}
      <ConfirmName opened={removing} name={b.bucket} what={`Remove bucket ${b.bucket} and all ${fmtCount(b.values)} values in it. Apps using it get errors until it is made again.`}
        onCancel={() => setRemoving(false)}
        onConfirm={(typed) => {
          setRemoving(false)
          void act({ op: 'kv_delete', nats: { bucket: b.bucket }, confirm: typed }, `Removed bucket ${b.bucket}.`).then((a) => { if (a.type === 'result') onGone() })
        }} />
    </Stack>
  )
}

function KeyDrawer({ bucket, k, ask, act, can, narrow, onClose, onEdit, onChanged }: {
  bucket: string; k: string; ask: Ask; act: (r: Req, ok: string) => Promise<StoreAnswer>; can: (op: string) => boolean; narrow: boolean
  onClose: () => void; onEdit: (value: string) => void; onChanged: () => void
}) {
  const [entries, setEntries] = useState<NatsKVEntry[] | null>(null)
  const [err, setErr] = useState<string | null>(null)
  useEffect(() => {
    void ask({ op: 'kv_get', nats: { bucket, key: k } }).then((a) => {
      if (a.type === 'result') setEntries((a.data as { entries?: NatsKVEntry[] }).entries ?? [])
      else setErr(a.error ?? 'could not read the key')
    })
  }, [ask, bucket, k])
  const latest = entries?.[0]
  return (
    <Modal opened onClose={onClose} size={narrow ? '100%' : 'lg'} fullScreen={narrow} title={<Text fw={650} ff="monospace">{bucket} › {k}</Text>}>
      <Stack gap="sm">
        {err && <Text size={FS.meta} c="rose">{err}</Text>}
        {entries === null && !err && <Text size={FS.meta} c="dimmed">Reading…</Text>}
        {entries?.map((e, i) => {
          const v = showData(e.value)
          return (
            <Box key={e.revision} style={{ border: '1px solid var(--color-line)', borderRadius: 8, opacity: i === 0 ? 1 : 0.8 }} p="xs">
              <Group gap="xs" mb={4}>
                <Text size={FS.micro} ff="monospace" fw={650}>#{e.revision.toLocaleString()}</Text>
                {i === 0 && <Badge size="xs" variant="light" color={e.op === 'put' ? 'teal' : 'slate'} radius="sm">{e.op === 'put' ? 'current' : 'deleted'}</Badge>}
                {e.op !== 'put' && i > 0 && <Badge size="xs" variant="light" color="slate" radius="sm">{e.op}</Badge>}
                {e.value.time && <Text size={FS.micro} c="dimmed" ml="auto">{ago(e.value.time)} ago</Text>}
              </Group>
              {e.op === 'put' && <Code block style={{ maxHeight: 220, overflow: 'auto', fontSize: 12 }}>{v.text}</Code>}
              {v.kind === 'base64' && <Text size={FS.micro} c="dimmed">Not text, shown as base64.</Text>}
            </Box>
          )
        })}
        <Text size={FS.micro} c="dimmed">The bucket keeps what its history allows, newest first. Reading is recorded.</Text>
        <Group justify="flex-end">
          {can('kv_del') && latest?.op === 'put' && (
            <Button variant="default" color="rose" onClick={() => void act({ op: 'kv_del', nats: { bucket, key: k } }, `Deleted ${k} from ${bucket}.`)
              .then((a) => { if (a.type === 'result') { onChanged(); onClose() } })}>Delete key</Button>
          )}
          {can('kv_put') && <Button onClick={() => onEdit(latest?.op === 'put' && !latest.value.b64 && !latest.value.cut ? latest.value.data : '')}>Put a new value</Button>}
        </Group>
      </Stack>
    </Modal>
  )
}

function PutKey({ bucket, initial, maxValue, onClose, onPut }: {
  bucket: string; initial: { key: string; value: string }; maxValue: number; onClose: () => void; onPut: (k: string, v: string) => Promise<void>
}) {
  const [k, setK] = useState(initial.key)
  const [v, setV] = useState(initial.value)
  const [busy, setBusy] = useState(false)
  const keyOk = validKey(k.trim())
  const size = new TextEncoder().encode(v).length
  const tooBig = maxValue > 0 && size > maxValue
  return (
    <Modal opened onClose={onClose} size="lg" title={<Text fw={650}>Put a key in {bucket}</Text>}>
      <Stack gap="sm">
        <TextInput label="Key" value={k} onChange={(e) => setK(e.currentTarget.value)} styles={MONO} placeholder="users.c1" data-autofocus={!initial.key}
          disabled={Boolean(initial.key)} error={k && !keyOk ? 'Letters, digits and -/_=. split by dots' : undefined} />
        <Textarea label="Value" value={v} onChange={(e) => setV(e.currentTarget.value)} autosize minRows={6} maxRows={16} data-autofocus={Boolean(initial.key)}
          styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12.5 } }}
          error={tooBig ? `The bucket takes values up to ${fmtBytes(maxValue)}` : undefined} />
        <Text size={FS.micro} c="dimmed">A new revision; apps watching the key get it at once. The value is kept in the recording.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!keyOk || tooBig} loading={busy} onClick={async () => { setBusy(true); await onPut(k.trim(), v); setBusy(false) }}>Put</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function NewBucket({ buckets, onClose, onSave }: { buckets: NatsKVBucket[]; onClose: () => void; onSave: (k: KVSpec) => Promise<void> }) {
  const [spec, setSpec] = useState<KVSpec>({ bucket: '', history: 1, storage: 'file', replicas: 1 })
  const [days, setDays] = useState<number | string>('')
  const [busy, setBusy] = useState(false)
  const set = (p: Partial<KVSpec>) => setSpec((s) => ({ ...s, ...p }))
  const out: KVSpec = { ...spec, ttlS: days === '' ? 0 : Math.round(Number(days) * 86_400) }
  const problems = bucketProblems(out, buckets)
  return (
    <Modal opened onClose={onClose} size="lg" title={<Text fw={650}>New bucket</Text>}>
      <Stack gap="sm">
        <TextInput label="Name" value={spec.bucket} onChange={(e) => set({ bucket: e.currentTarget.value })} styles={MONO} placeholder="carts" data-autofocus
          description="Letters, digits, - and _. Apps open it by this name." />
        <TextInput label="Description" value={spec.description ?? ''} onChange={(e) => set({ description: e.currentTarget.value })} />
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <NumberInput label="Values kept per key" value={spec.history} onChange={(v) => set({ history: Number(v) || 1 })} min={1} max={64}
            description="1 keeps only the latest." />
          <NumberInput label="Keep values for (days)" value={days} onChange={setDays} min={0} decimalScale={3} placeholder="for ever" />
          <NumberInput label="Up to size (MB)" value={spec.maxBytes ? spec.maxBytes / 1048576 : ''} onChange={(v) => set({ maxBytes: Number(v) ? Number(v) * 1048576 : 0 })} min={0} placeholder="no limit" />
          <NumberInput label="Largest value (KB)" value={spec.maxValueSize ? spec.maxValueSize / 1024 : ''} onChange={(v) => set({ maxValueSize: Number(v) ? Number(v) * 1024 : 0 })} min={0} placeholder="no limit" />
        </SimpleGrid>
        <SimpleGrid cols={2} spacing="sm">
          <Box>
            <Text size={FS.meta} fw={500} mb={4}>Stored on</Text>
            <SegmentedControl size="xs" fullWidth value={spec.storage} onChange={(v) => set({ storage: v as 'file' | 'memory' })}
              data={[{ label: 'Disk', value: 'file' }, { label: 'Memory', value: 'memory' }]} />
          </Box>
          <Box>
            <Text size={FS.meta} fw={500} mb={4}>Copies</Text>
            <SegmentedControl size="xs" fullWidth value={String(spec.replicas ?? 1)} onChange={(v) => set({ replicas: Number(v) })} data={['1', '3', '5']} />
          </Box>
        </SimpleGrid>
        {problems.map((p) => <Text key={p} size={FS.micro} c="rose">{p}</Text>)}
        <Text size={FS.micro} c="dimmed">Recorded with the settings sent.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={problems.length > 0} loading={busy} onClick={async () => { setBusy(true); await onSave(out); setBusy(false) }}>Make bucket</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
