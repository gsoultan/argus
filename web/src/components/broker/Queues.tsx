import { Autocomplete, Badge, Box, Button, Card, Checkbox, Group, Modal, NumberInput, SegmentedControl, SimpleGrid, Stack, Switch, Table, Text, TextInput } from '@mantine/core'
import { IconPlus, IconSearch } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import {
  fmtCount, fmtRate, growing, noConsumers, queueTags, rate, sortQueues, systemQueue, type Queue,
} from '~/lib/rabbit'
import { FS } from '~/theme'
import { MONO, useBroker } from './BrokerWorkspace'
import { Sparkline } from './charts'

export function TypeTag({ type }: { type?: string }) {
  if (!type) return null
  const color = type === 'quorum' ? 'sky' : type === 'stream' ? 'amber' : 'slate'
  return <Badge size="xs" variant="light" color={color} radius="sm" style={{ textTransform: 'none' }}>{type}</Badge>
}

export function StateBadge({ q }: { q: Queue }) {
  if (q.state === 'flow') return <Badge size="sm" variant="light" color="amber" radius="sm">Flow control</Badge>
  if (q.state && q.state !== 'running') return <Badge size="sm" variant="light" color="rose" radius="sm">{q.state}</Badge>
  const busy = rate(q.message_stats?.publish_details) + rate(q.message_stats?.deliver_get_details) > 0 || (q.messages_unacknowledged ?? 0) > 0
  return <Badge size="sm" variant="light" color={busy ? 'teal' : 'slate'} radius="sm">{busy ? 'Running' : 'Idle'}</Badge>
}

type Filter = 'all' | 'none' | 'growing' | 'dl' | 'flow'

/** A queue's rate history is not in the list answer, so the sparkline is what this page has seen since it opened. */
const history = new Map<string, { in: number[]; out: number[] }>()
function remember(q: Queue) {
  const h = history.get(q.name) ?? { in: [], out: [] }
  const last = h.in.length
  h.in = [...h.in, rate(q.message_stats?.publish_details)].slice(-24)
  h.out = [...h.out, rate(q.message_stats?.deliver_get_details)].slice(-24)
  if (last < 24 || history.size < 2000) history.set(q.name, h)
  return h
}

export function Queues() {
  const { queues, dlq, openQueue, narrow, can, loaded } = useBroker()
  const [filter, setFilter] = useState<Filter>('all')
  const [type, setType] = useState('all')
  const [q, setQ] = useState('')
  const [hideSystem, setHideSystem] = useState(true)
  const [creating, setCreating] = useState(false)

  const counts = useMemo(() => ({
    all: queues.length,
    none: queues.filter((x) => noConsumers(x) && !dlq.has(x.name)).length,
    growing: queues.filter(growing).length,
    dl: queues.filter((x) => dlq.has(x.name)).length,
    flow: queues.filter((x) => x.state === 'flow').length,
  }), [queues, dlq])

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return sortQueues(queues, dlq).filter((x) =>
      (!needle || x.name.toLowerCase().includes(needle)) &&
      (!hideSystem || !systemQueue(x.name)) &&
      (type === 'all' || x.type === type) &&
      (filter === 'all' || (filter === 'none' && noConsumers(x) && !dlq.has(x.name)) || (filter === 'growing' && growing(x)) ||
        (filter === 'dl' && dlq.has(x.name)) || (filter === 'flow' && x.state === 'flow')))
  }, [queues, dlq, q, hideSystem, type, filter])

  const hist = new Map(queues.map((x) => [x.name, remember(x)]))

  const chips: [Filter, string][] = [['all', 'All'], ['none', 'No consumers'], ['growing', 'Growing'], ['dl', 'Dead-letter'], ['flow', 'Flow control']]
  return (
    <Stack gap="sm">
      <Group gap="xs" wrap="wrap">
        <Group gap={6} wrap={narrow ? 'nowrap' : 'wrap'} style={narrow ? { overflowX: 'auto', maxWidth: '100%' } : undefined}
          role="group" aria-label="Show queues">
          {chips.filter(([k]) => k === 'all' || counts[k] > 0).map(([k, label]) => (
            <Button key={k} size="compact-sm" radius="xl" fw={500} variant={filter === k ? 'light' : 'default'} aria-pressed={filter === k}
              onClick={() => setFilter(k)} style={{ flexShrink: 0 }}>{label} {counts[k]}</Button>
          ))}
        </Group>
        <Group gap="xs" ml={narrow ? undefined : 'auto'} wrap="wrap">
          <TextInput size="xs" placeholder="Filter by name" leftSection={<IconSearch size={12} />} value={q}
            onChange={(e) => setQ(e.currentTarget.value)} aria-label="Filter queues by name" w={narrow ? '100%' : 200} />
          {!narrow && (
            <SegmentedControl size="xs" value={type} onChange={setType}
              data={[{ label: 'All types', value: 'all' }, { label: 'Quorum', value: 'quorum' }, { label: 'Classic', value: 'classic' }, { label: 'Stream', value: 'stream' }]} />
          )}
          <Switch size="xs" label="Hide amq.* queues" checked={hideSystem} onChange={(e) => setHideSystem(e.currentTarget.checked)} />
          {can('declare_queue') && <Button size="xs" leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New queue</Button>}
        </Group>
      </Group>

      {!loaded ? <Text size={FS.meta} c="dimmed">Reading the broker…</Text> : narrow ? (
        <Stack gap={8}>
          {rows.map((x) => {
            const warn = (noConsumers(x) && !dlq.has(x.name)) || growing(x) || (dlq.has(x.name) && (x.messages_ready ?? 0) > 0)
            return (
              <Card key={x.name} withBorder radius="md" padding="sm" onClick={() => openQueue(x.name)}
                style={{ cursor: 'pointer', borderLeft: warn ? '3px solid var(--mantine-color-amber-6)' : undefined }}>
                <Group gap={6} wrap="nowrap">
                  <Text ff="monospace" fw={600} size={FS.meta} truncate style={{ flex: 1 }}>{x.name}</Text>
                  <TypeTag type={x.type} />
                  <Sparkline values={hist.get(x.name)?.in ?? []} width={52} />
                </Group>
                <Group gap="md" mt={6}>
                  <Text size={FS.micro}><b>{fmtCount(x.messages_ready)}</b> <Text span c="dimmed" size={FS.micro}>ready</Text></Text>
                  <Text size={FS.micro} c={(x.consumers ?? 0) === 0 && (x.messages_ready ?? 0) > 0 ? 'rose' : undefined}>
                    <b>{x.consumers ?? 0}</b> <Text span c="dimmed" size={FS.micro}>consumers</Text>
                  </Text>
                  <Text size={FS.micro} c="dimmed" ml="auto">{fmtRate(rate(x.message_stats?.publish_details))} in</Text>
                </Group>
              </Card>
            )
          })}
        </Stack>
      ) : (
        <Card withBorder radius="md" padding={0}>
          <Table.ScrollContainer minWidth={900}>
            <Table verticalSpacing={8} fz="sm" highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Queue</Table.Th><Table.Th>Type</Table.Th><Table.Th>State</Table.Th>
                  <Table.Th ta="right">Ready</Table.Th><Table.Th ta="right">Unacked</Table.Th><Table.Th ta="right">Consumers</Table.Th>
                  <Table.Th ta="right">Incoming</Table.Th><Table.Th ta="right">Delivered</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {rows.map((x) => {
                  const warn = (noConsumers(x) && !dlq.has(x.name)) || growing(x)
                  const h = hist.get(x.name)
                  return (
                    <Table.Tr key={x.name} onClick={() => openQueue(x.name)} style={{ cursor: 'pointer', boxShadow: warn ? 'inset 3px 0 0 var(--mantine-color-amber-6)' : undefined }}>
                      <Table.Td>
                        <Group gap={6} wrap="wrap">
                          <Text span ff="monospace" fw={500} size={FS.meta}>{x.name}</Text>
                          {queueTags(x, dlq).map((t) => (
                            <Badge key={t.label} size="xs" variant="light" color={t.label === 'dead-letter' ? 'amber' : 'slate'} radius="sm" style={{ textTransform: 'none' }}>{t.label}</Badge>
                          ))}
                        </Group>
                      </Table.Td>
                      <Table.Td><TypeTag type={x.type} /></Table.Td>
                      <Table.Td><StateBadge q={x} /></Table.Td>
                      <Table.Td ta="right">{fmtCount(x.messages_ready)}</Table.Td>
                      <Table.Td ta="right">{x.type === 'stream' ? '—' : fmtCount(x.messages_unacknowledged)}</Table.Td>
                      <Table.Td ta="right" c={(x.consumers ?? 0) === 0 && (x.messages_ready ?? 0) > 0 ? 'rose' : undefined} fw={(x.consumers ?? 0) === 0 && (x.messages_ready ?? 0) > 0 ? 600 : undefined}>{x.consumers ?? 0}</Table.Td>
                      <Table.Td><Group gap={8} justify="flex-end" wrap="nowrap">
                        <Sparkline values={h?.in ?? []} />
                        <Text ff="monospace" size={FS.meta} w={64} ta="right">{fmtRate(rate(x.message_stats?.publish_details))}</Text>
                      </Group></Table.Td>
                      <Table.Td><Group gap={8} justify="flex-end" wrap="nowrap">
                        <Sparkline values={h?.out ?? []} color="var(--mantine-color-teal-6)" />
                        <Text ff="monospace" size={FS.meta} w={64} ta="right">{fmtRate(rate(x.message_stats?.deliver_get_details))}</Text>
                      </Group></Table.Td>
                    </Table.Tr>
                  )
                })}
                {rows.length === 0 && (
                  <Table.Tr><Table.Td colSpan={8}><Text size={FS.meta} c="dimmed" p="sm">{queues.length ? 'No queue matches.' : 'No queues in this vhost.'}</Text></Table.Td></Table.Tr>
                )}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Card>
      )}
      <Text size={FS.micro} c="dimmed">
        Rates are RabbitMQ's, refreshed every 5 seconds; the trend lines are what this page has seen since it opened. What needs attention is listed first.
      </Text>
      <NewQueue opened={creating} onClose={() => setCreating(false)} />
    </Stack>
  )
}

function NewQueue({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const { act, database, openQueue, exchanges } = useBroker()
  const [name, setName] = useState('')
  const [type, setType] = useState<'classic' | 'quorum' | 'stream'>('classic')
  const [durable, setDurable] = useState(true)
  const [maxLength, setMaxLength] = useState<number | string>('')
  const [ttlS, setTtlS] = useState<number | string>('')
  const [dlx, setDlx] = useState('')
  const [deliveryLimit, setDeliveryLimit] = useState<number | string>('')
  const [keepDays, setKeepDays] = useState<number | string>(7)
  const [maxGB, setMaxGB] = useState<number | string>('')
  const [segmentMB, setSegmentMB] = useState<number | string>('')
  const [bindTo, setBindTo] = useState('')
  const [bindKey, setBindKey] = useState('')
  const [busy, setBusy] = useState(false)
  const n = name.trim()
  const num = (v: number | string) => (Number(v) > 0 ? Number(v) : undefined)
  const queue = {
    type,
    ...(type !== 'stream' ? { maxLength: num(maxLength), messageTtlMs: num(ttlS) ? Math.round(Number(ttlS) * 1000) : undefined, deadLetterExchange: dlx.trim() || undefined } : {}),
    ...(type === 'quorum' ? { deliveryLimit: num(deliveryLimit) } : {}),
    ...(type === 'stream' ? {
      maxAgeS: num(keepDays) ? Math.round(Number(keepDays) * 86400) : undefined,
      maxLengthBytes: num(maxGB) ? Math.round(Number(maxGB) * 1024 ** 3) : undefined,
      segmentBytes: num(segmentMB) ? Math.round(Number(segmentMB) * 1024 ** 2) : undefined,
    } : {}),
    ...(bindTo.trim() ? { bindExchange: bindTo.trim(), bindRoutingKey: bindKey.trim() } : {}),
  }
  const create = async () => {
    setBusy(true)
    try {
      const a = await act({ op: 'declare_queue', name: n, durable: type !== 'classic' || durable, queue },
        `Created ${type === 'classic' ? 'queue' : type === 'quorum' ? 'quorum queue' : 'stream'} ${n} in ${database.dbname}${bindTo.trim() ? `, bound to ${bindTo.trim()}` : ''}.`)
      if (a.type === 'result') { onClose(); setName(''); setBindTo(''); setBindKey(''); openQueue(n) }
    } finally {
      setBusy(false)
    }
  }
  const exNames = exchanges.map((e) => e.name).filter((e) => e && !e.startsWith('amq.'))
  return (
    <Modal opened={opened} onClose={onClose} title="New queue" centered size="lg">
      <Stack gap="sm">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} data-autofocus styles={MONO} />
        <Stack gap={4}>
          <Text size={FS.meta} fw={500}>Type</Text>
          <SegmentedControl value={type} onChange={(v) => setType(v as typeof type)} fullWidth
            data={[{ label: 'Classic', value: 'classic' }, { label: 'Quorum', value: 'quorum' }, { label: 'Stream', value: 'stream' }]} />
          <Text size={FS.micro} c="dimmed">
            {type === 'classic' ? 'One copy, on one node. Fine for work that can be redone.'
              : type === 'quorum' ? 'Kept on a majority of nodes, so a node going down loses nothing. Always durable.'
              : 'An append-only log: consumers read from any point, and messages stay until they age out or the size is reached. Always durable; read it with a consumer group from your application, or browse it here.'}
          </Text>
        </Stack>
        {type === 'classic' && (
          <Checkbox label="Durable: survives a broker restart" checked={durable} onChange={(e) => setDurable(e.currentTarget.checked)} />
        )}
        {type !== 'stream' ? (
          <SimpleGrid cols={{ base: 1, sm: type === 'quorum' ? 4 : 3 }}>
            <NumberInput label="Most messages" value={maxLength} onChange={setMaxLength} min={1} placeholder="No limit" />
            <NumberInput label="Message TTL (s)" value={ttlS} onChange={setTtlS} min={1} placeholder="For ever" />
            <TextInput label="Dead-letter exchange" value={dlx} onChange={(e) => setDlx(e.currentTarget.value)} placeholder="None" styles={MONO} />
            {type === 'quorum' && <NumberInput label="Delivery limit" value={deliveryLimit} onChange={setDeliveryLimit} min={1} placeholder="Default" />}
          </SimpleGrid>
        ) : (
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <NumberInput label="Keep for (days)" value={keepDays} onChange={setKeepDays} min={0} decimalScale={3} placeholder="For ever" />
            <NumberInput label="Largest size (GB)" value={maxGB} onChange={setMaxGB} min={0} decimalScale={2} placeholder="No limit" />
            <NumberInput label="Segment size (MB)" value={segmentMB} onChange={setSegmentMB} min={1} placeholder="500 (default)" />
          </SimpleGrid>
        )}
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <Autocomplete label="Bind to exchange" value={bindTo} onChange={setBindTo} data={exNames} placeholder="Optional" styles={MONO} />
          <TextInput label="Routing key" value={bindKey} onChange={(e) => setBindKey(e.currentTarget.value)} disabled={!bindTo.trim()} styles={MONO} />
        </SimpleGrid>
        <Box><Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button disabled={!n} loading={busy} onClick={() => void create()}>Create</Button></Group></Box>
      </Stack>
    </Modal>
  )
}
