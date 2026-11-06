import {
  Alert, Badge, Box, Button, Card, Group, Loader, SegmentedControl, Select, Stack, Table, Tabs, Text, UnstyledButton,
} from '@mantine/core'
import { IconArrowBackUp, IconArrowLeft, IconCopy } from '@tabler/icons-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  cameFrom, fmtCount, fmtRate, items, prettyPayload, queueTags, rate, samples, xDeath, type Binding, type Consumer, type Peeked, type Queue,
} from '~/lib/rabbit'
import { FS } from '~/theme'
import { useBroker } from './BrokerWorkspace'
import { Sparkline } from './charts'
import { TypeTag } from './Queues'

/*
One queue: its numbers, its messages, who consumes it and what it is bound
to. On a dead-letter queue, "Send them back" moves the messages to another
queue -- where they died, by default -- once whatever killed them is fixed.
*/

export function QueueDetail({ name }: { name: string }) {
  const { ask, can, openQueue, dlq, narrow, queues, confirm } = useBroker()
  const [q, setQ] = useState<Queue | null>(null)
  const [bindings, setBindings] = useState<Binding[]>([])
  const [err, setErr] = useState<string | null>(null)
  const [tab, setTab] = useState<string>('messages')
  const [msgs, setMsgs] = useState<Peeked[] | null>(null)
  const [count, setCount] = useState('10')
  const [peeking, setPeeking] = useState(false)
  const [view, setView] = useState<'pretty' | 'raw' | 'headers'>('pretty')

  const load = useCallback(async () => {
    const a = await ask({ op: 'queue', name })
    if (a.type !== 'result') { setErr(a.error ?? 'could not read the queue'); return }
    const d = a.data as { queue?: Queue; bindings?: Binding[] }
    setErr(null); setQ(d.queue ?? null); setBindings(items<Binding>(d.bindings))
  }, [ask, name])
  useEffect(() => {
    void load()
    const t = window.setInterval(() => { if (document.visibilityState === 'visible') void load() }, 5000)
    return () => window.clearInterval(t)
  }, [load])

  const peek = async () => {
    setPeeking(true)
    const a = await ask({ op: 'peek', name, count: Number(count) })
    setPeeking(false)
    if (a.type !== 'result') { notifyError('Could not peek', new Error(a.error ?? 'failed')); return }
    setMsgs(Array.isArray(a.data) ? (a.data as Peeked[]) : [])
    void load()
  }

  const from = useMemo(() => (msgs ? cameFrom(msgs) : null), [msgs])
  const back = (
    <UnstyledButton onClick={() => openQueue(null)}>
      <Group gap={4}><IconArrowLeft size={14} /><Text size={FS.meta} c="azure">Queues</Text></Group>
    </UnstyledButton>
  )
  if (err) return <Stack gap="sm">{back}<Alert color="rose" variant="light">{err}</Alert></Stack>
  if (!q) return <Stack gap="sm">{back}<Loader size="sm" /></Stack>

  const isDL = dlq.has(q.name)
  const ready = q.messages_ready ?? 0
  const stat = (label: string, v: string, tone?: string) => (
    <Box><Text size={FS.micro} c="dimmed">{label}</Text><Text fw={650} fz={17} c={tone}>{v}</Text></Box>
  )
  const consumers = q.consumer_details ?? []

  const side = (
    <Stack gap="md">
      {can('move') && (
        <MoveCard from={q.name} ready={ready} suggest={from ?? dlq.get(q.name)?.split(', ')[0] ?? ''} queues={queues} onMoved={() => { setMsgs(null); void load() }} />
      )}
      <Card withBorder radius="md" padding="md">
        <Text fw={600} size={FS.body} mb={6}>Arguments</Text>
        {Object.keys(q.arguments ?? {}).length === 0 ? <Text size={FS.micro} c="dimmed">None.</Text> : (
          <Stack gap={2}>
            {Object.entries(q.arguments ?? {}).map(([k, v]) => (
              <Group key={k} gap="xs" wrap="nowrap"><Text size={FS.micro} c="dimmed" ff="monospace">{k}</Text><Text size={FS.micro} ff="monospace" ml="auto" ta="right" style={{ wordBreak: 'break-all' }}>{String(v)}</Text></Group>
            ))}
          </Stack>
        )}
        <Group gap="xs" mt="xs">
          <Text size={FS.micro} c="dimmed">durable</Text><Text size={FS.micro} ml="auto">{q.durable ? 'yes' : 'no'}</Text>
        </Group>
        {q.policy && <Group gap="xs"><Text size={FS.micro} c="dimmed">policy</Text><Text size={FS.micro} ml="auto" ff="monospace">{q.policy}</Text></Group>}
      </Card>
      {(can('purge') || can('delete_queue')) && (
        <Card withBorder radius="md" padding="md">
          <Text fw={600} size={FS.body} c="rose">Danger</Text>
          <Group gap="xs" mt="xs">
            {can('purge') && (
              <Button size="xs" variant="light" color="rose" disabled={ready === 0}
                onClick={() => confirm({ op: 'purge', req: { name }, what: `Purge every ready message (${fmtCount(ready)}) from ${name}.`, then: () => setMsgs(null) })}>
                Purge {fmtCount(ready)} ready
              </Button>
            )}
            {can('delete_queue') && (
              <Button size="xs" variant="subtle" color="rose"
                onClick={() => confirm({ op: 'delete_queue', req: { name }, what: `Delete queue ${name} and its ${fmtCount(q.messages)} messages.`, then: () => openQueue(null) })}>
                Delete queue
              </Button>
            )}
          </Group>
          <Text size={FS.micro} c="dimmed" mt={6}>Both ask you to type the server's name.</Text>
        </Card>
      )}
    </Stack>
  )

  return (
    <Stack gap="sm">
      {back}
      <Group gap="sm" wrap="wrap" align="flex-end">
        <Box style={{ minWidth: 0 }}>
          <Text ff="monospace" fw={650} fz={17} style={{ wordBreak: 'break-all' }}>{q.name}</Text>
          <Group gap={4} mt={4}>
            <TypeTag type={q.type} />
            {isDL && <Badge size="xs" variant="light" color="amber" radius="sm" style={{ textTransform: 'none' }}>dead-letter queue{dlq.get(q.name) ? ` of ${dlq.get(q.name)}` : ''}</Badge>}
            {queueTags(q, dlq).filter((t) => t.label !== 'dead-letter').map((t) => (
              <Badge key={t.label} size="xs" variant="light" color="slate" radius="sm" style={{ textTransform: 'none' }}>{t.label}</Badge>
            ))}
          </Group>
        </Box>
        <Group gap="lg" ml={narrow ? undefined : 'auto'} wrap="wrap">
          {stat('Ready', fmtCount(ready))}
          {q.type !== 'stream' && stat('Unacked', fmtCount(q.messages_unacknowledged))}
          {stat('Consumers', String(q.consumers ?? 0), (q.consumers ?? 0) === 0 && ready > 0 ? 'rose' : undefined)}
          {stat('Incoming', fmtRate(rate(q.message_stats?.publish_details)))}
          {stat('Delivered', fmtRate(rate(q.message_stats?.deliver_get_details)))}
          {!narrow && (
            <Box>
              <Sparkline values={samples(q.messages_ready_details)} color="var(--mantine-color-amber-6)" width={140} height={36} label="Ready messages over 10 minutes" />
              <Text size={FS.micro} c="dimmed">ready, 10 min</Text>
            </Box>
          )}
        </Group>
      </Group>

      <Tabs value={tab} onChange={(v) => setTab(v ?? 'messages')}>
        <Tabs.List style={{ flexWrap: 'nowrap', overflowX: 'auto' }}>
          <Tabs.Tab value="messages">Messages</Tabs.Tab>
          <Tabs.Tab value="consumers">Consumers {consumers.length}</Tabs.Tab>
          <Tabs.Tab value="bindings">Bindings {bindings.length}</Tabs.Tab>
        </Tabs.List>
      </Tabs>

      <Box style={{ display: 'grid', gap: 'var(--mantine-spacing-md)' }} className="argus-broker-grid">
        <Box style={{ minWidth: 0 }}>
          {tab === 'messages' && (
            <Stack gap="sm">
              <Group gap="xs" wrap="wrap">
                <Select size="xs" w={130} value={count} onChange={(v) => setCount(v ?? '10')} aria-label="How many to peek" allowDeselect={false}
                  data={['1', '10', '50', '100'].map((n) => ({ value: n, label: `${n} message${n === '1' ? '' : 's'}` }))} />
                <Button size="xs" onClick={() => void peek()} loading={peeking} disabled={!can('peek') || q.type === 'stream'}>Peek</Button>
                <SegmentedControl size="xs" value={view} onChange={(v) => setView(v as typeof view)}
                  data={[{ label: 'Pretty', value: 'pretty' }, { label: 'Raw', value: 'raw' }, { label: 'Headers', value: 'headers' }]} />
              </Group>
              <Text size={FS.micro} c="dimmed">
                {q.type === 'stream'
                  ? 'A stream is read by offset, not taken; peeking it from here is not supported.'
                  : 'Peek puts every message back. RabbitMQ marks them redelivered for whoever consumes them next, and the order of the queue can change.'}
              </Text>
              {msgs && msgs.length === 0 && <Text size={FS.meta} c="dimmed">The queue had no ready messages.</Text>}
              {msgs?.map((m, i) => <MessageCard key={i} m={m} i={i} view={view} />)}
            </Stack>
          )}
          {tab === 'consumers' && <ConsumerTable consumers={consumers} />}
          {tab === 'bindings' && (
            <Card withBorder radius="md" padding={0}>
              <Table verticalSpacing={6} fz="sm">
                <Table.Thead><Table.Tr><Table.Th>From exchange</Table.Th><Table.Th>Routing key</Table.Th><Table.Th>Arguments</Table.Th></Table.Tr></Table.Thead>
                <Table.Tbody>
                  {bindings.map((b, i) => (
                    <Table.Tr key={i}>
                      <Table.Td ff="monospace">{b.source || '(default)'}</Table.Td>
                      <Table.Td ff="monospace">{b.routing_key || <Text span c="dimmed" size={FS.meta}>(none)</Text>}</Table.Td>
                      <Table.Td ff="monospace" fz={FS.micro}>{Object.keys(b.arguments ?? {}).length ? JSON.stringify(b.arguments) : ''}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Card>
          )}
        </Box>
        {side}
      </Box>
    </Stack>
  )
}

function MessageCard({ m, i, view }: { m: Peeked; i: number; view: 'pretty' | 'raw' | 'headers' }) {
  const deaths = xDeath(m.properties)
  const p = prettyPayload(m)
  const props = Object.entries(m.properties ?? {}).filter(([k]) => k !== 'headers')
  const headers = Object.entries(m.properties?.headers ?? {}).filter(([k]) => k !== 'x-death')
  const copy = () => { void navigator.clipboard?.writeText(m.payload).then(() => notifyOk('Copied', 'The payload is on your clipboard.')) }
  return (
    <Card withBorder radius="md" padding={0}>
      <Group gap="xs" px="sm" py={6} wrap="wrap" style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--mantine-color-default-hover)' }}>
        <Text size={FS.micro} fw={700}>#{i + 1}</Text>
        <Text size={FS.micro} c="dimmed">routing key <Text span ff="monospace" size={FS.micro} c="var(--mantine-color-text)">{m.routing_key || '(none)'}</Text></Text>
        {m.exchange !== undefined && <Text size={FS.micro} c="dimmed">from <Text span ff="monospace" size={FS.micro}>{m.exchange || '(default)'}</Text></Text>}
        {m.payload_encoding === 'base64' && <Badge size="xs" variant="light" color="slate">binary, shown as base64</Badge>}
        <Group gap={4} ml="auto">
          {m.redelivered && <Badge size="xs" variant="light" color="amber" style={{ textTransform: 'none' }}>redelivered</Badge>}
          <Button size="compact-xs" variant="subtle" leftSection={<IconCopy size={12} />} onClick={copy}>Copy</Button>
        </Group>
      </Group>
      {(deaths.length > 0 || props.length > 0) && view !== 'raw' && (
        <Group gap={6} px="sm" pt={8} wrap="wrap">
          {deaths.map((d, j) => (
            <Badge key={`d${j}`} size="sm" variant="light" color="amber" radius="sm" style={{ textTransform: 'none', fontFamily: 'var(--font-mono)' }}>
              {d.reason} × {d.count} in {d.queue}
            </Badge>
          ))}
          {view === 'pretty' && props.map(([k, v]) => (
            <Badge key={k} size="sm" variant="light" color="slate" radius="sm" style={{ textTransform: 'none', fontFamily: 'var(--font-mono)', fontWeight: 400 }}>
              {k} {String(v)}
            </Badge>
          ))}
        </Group>
      )}
      {view === 'headers' ? (
        <Box px="sm" py={8}>
          {headers.length === 0 && deaths.length === 0 && <Text size={FS.micro} c="dimmed">No headers.</Text>}
          {headers.map(([k, v]) => (
            <Group key={k} gap="xs" wrap="nowrap"><Text size={FS.micro} ff="monospace" c="dimmed">{k}</Text><Text size={FS.micro} ff="monospace">{typeof v === 'object' ? JSON.stringify(v) : String(v)}</Text></Group>
          ))}
          {props.map(([k, v]) => (
            <Group key={k} gap="xs" wrap="nowrap"><Text size={FS.micro} ff="monospace" c="dimmed">{k}</Text><Text size={FS.micro} ff="monospace">{String(v)}</Text></Group>
          ))}
        </Box>
      ) : (
        <pre className="argus-broker-pre">{view === 'pretty' ? p.text : m.payload}</pre>
      )}
    </Card>
  )
}

function ConsumerTable({ consumers }: { consumers: Consumer[] }) {
  if (consumers.length === 0) return <Text size={FS.meta} c="dimmed">Nobody is consuming this queue.</Text>
  return (
    <Card withBorder radius="md" padding={0}>
      <Table.ScrollContainer minWidth={560}>
        <Table verticalSpacing={6} fz="sm">
          <Table.Thead><Table.Tr><Table.Th>Application</Table.Th><Table.Th>Consumer tag</Table.Th><Table.Th>Ack</Table.Th><Table.Th ta="right">Prefetch</Table.Th><Table.Th>Active</Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {consumers.map((c, i) => (
              <Table.Tr key={i}>
                <Table.Td><Text size={FS.meta} ff="monospace">{c.channel_details?.connection_name ?? c.channel_details?.name ?? '—'}</Text>
                  {c.channel_details?.user && <Text size={FS.micro} c="dimmed">{c.channel_details.user}{c.channel_details.peer_host ? ` · ${c.channel_details.peer_host}` : ''}</Text>}</Table.Td>
                <Table.Td ff="monospace" fz={FS.micro}>{c.consumer_tag}</Table.Td>
                <Table.Td>{c.ack_required ? 'manual' : 'auto'}</Table.Td>
                <Table.Td ta="right">{c.prefetch_count ? c.prefetch_count : 'unlimited'}</Table.Td>
                <Table.Td>{c.active === false ? <Badge size="xs" variant="light" color="slate">waiting</Badge> : <Badge size="xs" variant="light" color="teal">active</Badge>}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </Card>
  )
}

function MoveCard({ from, ready, suggest, queues, onMoved }: {
  from: string; ready: number; suggest: string; queues: Queue[]; onMoved: () => void
}) {
  const { ask, reload } = useBroker()
  const [to, setTo] = useState<string | null>(suggest && suggest !== from ? suggest : null)
  const [how, setHow] = useState('all')
  const [moving, setMoving] = useState(false)
  useEffect(() => { if (!to && suggest && suggest !== from) setTo(suggest) }, [suggest, from, to])
  const n = how === 'all' ? ready : Math.min(Number(how), ready)
  const move = async () => {
    if (!to) return
    setMoving(true)
    const a = await ask({ op: 'move', name: from, target: to, count: how === 'all' ? 0 : Number(how) })
    setMoving(false)
    if (a.type !== 'result') { notifyError('Not moved', new Error(a.error ?? 'failed')); return }
    const r = a.data as { moved: number; stopped?: string }
    if (r.stopped && r.stopped !== 'no more ready messages') notifyError(`Moved ${r.moved.toLocaleString()}, then stopped`, new Error(r.stopped))
    else notifyOk('Moved', `${r.moved.toLocaleString()} message${r.moved === 1 ? '' : 's'} from ${from} to ${to}. It is in the session's recording.`)
    onMoved()
    void reload()
  }
  return (
    <Card withBorder radius="md" padding="md">
      <Group gap={6}><IconArrowBackUp size={16} /><Text fw={600} size={FS.body}>Send them back</Text></Group>
      <Text size={FS.micro} c="dimmed" mt={4} mb="sm">
        Move ready messages to another queue, for example back to where they died once the cause is fixed. Each is taken,
        published to the target, and put back here if the target does not take it.
      </Text>
      <Select size="xs" label="To" searchable value={to} onChange={setTo} placeholder="Pick a queue"
        data={queues.filter((q) => q.name !== from).map((q) => ({ value: q.name, label: q.name === suggest ? `${q.name} (where they died)` : q.name }))} />
      <Text size={FS.micro} fw={500} mt="xs" mb={4}>How many</Text>
      <SegmentedControl size="xs" value={how} onChange={setHow}
        data={[{ label: `All ${fmtCount(ready)}`, value: 'all' }, { label: 'First 10', value: '10' }, { label: 'First 100', value: '100' }]} />
      <Button fullWidth mt="sm" size="xs" disabled={!to || ready === 0} loading={moving} onClick={() => void move()}>
        Move {fmtCount(n)} message{n === 1 ? '' : 's'}
      </Button>
      {ready > 10_000 && <Text size={FS.micro} c="dimmed" mt={4}>Up to 10,000 at a time.</Text>}
    </Card>
  )
}
