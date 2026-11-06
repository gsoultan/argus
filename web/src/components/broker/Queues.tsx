import { Badge, Box, Button, Card, Checkbox, Chip, Group, Modal, SegmentedControl, Stack, Switch, Table, Text, TextInput } from '@mantine/core'
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
        <Chip.Group value={filter} onChange={(v) => setFilter(v as Filter)}>
          <Group gap={6} wrap={narrow ? 'nowrap' : 'wrap'} style={narrow ? { overflowX: 'auto', maxWidth: '100%' } : undefined}>
            {chips.filter(([k]) => k === 'all' || counts[k] > 0).map(([k, label]) => (
              <Chip key={k} value={k} size="sm" variant="outline">{label} {counts[k]}</Chip>
            ))}
          </Group>
        </Chip.Group>
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
  const { act, database, openQueue } = useBroker()
  const [name, setName] = useState('')
  const [durable, setDurable] = useState(true)
  const create = async () => {
    const n = name.trim()
    const a = await act({ op: 'declare_queue', name: n, durable }, `Created queue ${n} in ${database.dbname}.`)
    if (a.type === 'result') { onClose(); setName(''); openQueue(n) }
  }
  return (
    <Modal opened={opened} onClose={onClose} title="New queue" centered>
      <Stack gap="sm">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} data-autofocus styles={MONO} />
        <Checkbox label="Durable: survives a broker restart" checked={durable} onChange={(e) => setDurable(e.currentTarget.checked)} />
        <Text size={FS.micro} c="dimmed">A classic queue. Quorum queues and arguments are set by your applications or by policy.</Text>
        <Box><Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button disabled={!name.trim()} onClick={() => void create()}>Create</Button></Group></Box>
      </Stack>
    </Modal>
  )
}
