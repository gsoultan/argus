import { useCallback, useEffect, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, Checkbox, Drawer, Group, Loader, ScrollArea, SegmentedControl, Select, Stack,
  Table, Text, TextInput, Textarea, UnstyledButton,
} from '@mantine/core'
import { useDisclosure, useMediaQuery } from '@mantine/hooks'
import { IconInfoCircle, IconMenu2, IconMessages, IconRefresh, IconSearch } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import type { DBAccess, DBLevel, Database } from '~/types/domain'
import { ConfirmByName, FatalBanner, StoreHeader } from '~/components/StoreChrome'
import { useStoreSession, type StoreAnswer } from '~/components/useStoreSession'
import { notifyError, notifyOk } from '~/lib/notify'

/*
One RabbitMQ vhost in the browser: queues and exchanges on the left, a
queue's detail with peek, purge and publish on the right.

What the person can do comes from the agent's answer (`ops`), which is the
fixed list for their level; RabbitMQ's own permissions are under it. Peek
puts each message back, and RabbitMQ marks it redelivered for whoever
consumes it next -- the page says so, because a consumer that treats
redelivered as "retry" will notice.
*/

export interface RabbitWorkspaceProps {
  database: Database
  access: DBAccess
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>
  endpoint: string
  onAskForAccess?: () => void
}

interface Queue {
  name: string; type?: string; durable?: boolean; messages?: number; messages_ready?: number
  messages_unacknowledged?: number; consumers?: number; state?: string
}
interface Exchange { name: string; type?: string; durable?: boolean; internal?: boolean }
interface Binding { source: string; destination: string; routing_key: string }
interface Message { payload: string; payload_encoding?: string; routing_key?: string; exchange?: string; redelivered?: boolean; properties?: Record<string, unknown>; message_count?: number }
interface Connection { name: string; user?: string; peer_host?: string; peer_port?: number; state?: string; channels?: number; client_properties?: { product?: string; connection_name?: string } }

function items<T>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[]
  if (data && typeof data === 'object' && Array.isArray((data as { items?: unknown }).items)) return (data as { items: T[] }).items
  return []
}

type Confirm = { op: 'purge' | 'delete_queue' | 'close_connection'; name: string; what: string } | null

export function RabbitWorkspace({ database, access, getTicket, endpoint, onAskForAccess }: RabbitWorkspaceProps) {
  const narrow = useMediaQuery('(max-width: 48em)') ?? false
  const [listOpen, listDrawer] = useDisclosure(false)
  const s = useStoreSession(database, getTicket, endpoint)
  const level = s.ready?.level ?? access.level
  const can = (op: string) => s.ready?.ops?.includes(op) ?? false

  const [tab, setTab] = useState<'queues' | 'exchanges' | 'connections'>('queues')
  const [filter, setFilter] = useState('')
  const [queues, setQueues] = useState<Queue[]>([])
  const [exchanges, setExchanges] = useState<Exchange[]>([])
  const [conns, setConns] = useState<Connection[]>([])
  const [loading, setLoading] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [selected, setSelected] = useState<string | null>(null)
  const [confirm, setConfirm] = useState<Confirm>(null)

  const load = useCallback(async () => {
    setLoading(true)
    const [q, e] = await Promise.all([
      s.ask({ op: 'queues', pattern: filter, count: 500 }),
      s.ask({ op: 'exchanges', pattern: filter, count: 500 }),
    ])
    setLoading(false)
    if (q.type !== 'result') { setErr(q.error ?? 'could not list queues'); return }
    setErr(e.type === 'result' ? null : e.error ?? null)
    setQueues(items<Queue>(q.data))
    setExchanges(items<Exchange>(e.data))
    if (s.ready?.ops?.includes('connections')) {
      const c = await s.ask({ op: 'connections' })
      if (c.type === 'result') setConns(items<Connection>(c.data))
    }
  }, [s, filter])

  useEffect(() => {
    if (s.status === 'ready' && s.ready) void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.status, s.ready])

  const doConfirmed = async (typed: string) => {
    const c = confirm
    setConfirm(null)
    if (!c) return
    const a = await s.ask({ op: c.op, name: c.name, confirm: typed })
    if (a.type === 'result') {
      notifyOk('Done', `${c.what} It is in the session's recording.`)
      if (c.op === 'delete_queue') setSelected(null)
      void load()
    } else {
      notifyError('Not done', new Error(a.error ?? 'failed'))
    }
  }

  const visibleQueues = queues
  const list = (
    <Stack gap={SP.cozy} p="xs" h="100%">
      <Group gap={6} wrap="nowrap">
        <TextInput size="xs" value={filter} onChange={(e) => setFilter(e.currentTarget.value)} style={{ flex: 1 }}
          leftSection={<IconSearch size={12} />} placeholder="Filter by name" aria-label="Filter"
          onKeyDown={(e) => { if (e.key === 'Enter') void load() }} />
        <ActionIcon variant="default" onClick={() => void load()} aria-label="Refresh" loading={loading}><IconRefresh size={14} /></ActionIcon>
      </Group>
      <SegmentedControl size="xs" value={tab} onChange={(v) => setTab(v as typeof tab)} fullWidth
        data={[{ label: `Queues ${queues.length}`, value: 'queues' }, { label: `Exchanges ${exchanges.length}`, value: 'exchanges' },
          ...(can('connections') ? [{ label: 'Connections', value: 'connections' }] : [])]} />
      {err && <Text size={FS.micro} c="rose">{err}</Text>}
      <ScrollArea style={{ flex: 1 }} type="auto">
        <Stack gap={0}>
          {tab === 'queues' && visibleQueues.map((q) => (
            <UnstyledButton key={q.name} px={6} py={5} onClick={() => { setSelected(q.name); if (narrow) listDrawer.close() }}
              style={{ borderRadius: 6, background: selected === q.name ? 'var(--mantine-color-azure-light)' : undefined }}>
              <Group gap={6} wrap="nowrap">
                <Text size={FS.meta} ff="monospace" truncate style={{ flex: 1 }} title={q.name}>{q.name}</Text>
                <Text size={FS.micro} c="dimmed">{(q.messages ?? 0).toLocaleString()} msgs</Text>
              </Group>
            </UnstyledButton>
          ))}
          {tab === 'exchanges' && exchanges.map((x) => (
            <Group key={x.name || '(default)'} gap={6} px={6} py={5} wrap="nowrap">
              <Text size={FS.meta} ff="monospace" truncate style={{ flex: 1 }}>{x.name || '(default)'}</Text>
              <Badge size="xs" variant="outline" color="slate" radius="sm">{x.type}</Badge>
            </Group>
          ))}
          {tab === 'connections' && conns.map((c) => (
            <Group key={c.name} gap={6} px={6} py={5} wrap="nowrap">
              <Box style={{ flex: 1, minWidth: 0 }}>
                <Text size={FS.meta} ff="monospace" truncate>{c.client_properties?.connection_name || c.name}</Text>
                <Text size={FS.micro} c="dimmed">{c.user} · {c.peer_host}{c.channels !== undefined ? ` · ${c.channels} channels` : ''}</Text>
              </Box>
              {can('close_connection') && (
                <Button size="compact-xs" variant="subtle" color="rose"
                  onClick={() => setConfirm({ op: 'close_connection', name: c.name, what: `Closed connection ${c.name}.` })}>Close</Button>
              )}
            </Group>
          ))}
          {tab === 'queues' && queues.length === 0 && !loading && <Text size={FS.meta} c="dimmed" p={6}>No queues{filter ? ' match' : ' in this vhost'}.</Text>}
        </Stack>
      </ScrollArea>
      {can('declare_queue') && <NewQueue onCreate={async (name, durable) => {
        const a = await s.ask({ op: 'declare_queue', name, durable })
        if (a.type === 'result') { notifyOk('Queue created', `${name} in ${database.dbname}.`); void load(); setSelected(name) }
        else notifyError('Not created', new Error(a.error ?? 'failed'))
      }} />}
    </Stack>
  )

  return (
    <Box style={{ display: 'flex', height: narrow ? 'auto' : 'calc(100dvh - 120px)', minHeight: 480 }}>
      {narrow ? (
        <Drawer opened={listOpen} onClose={listDrawer.close} size="85%" padding="xs" title="Queues and exchanges">{list}</Drawer>
      ) : (
        <Box w={300} style={{ flexShrink: 0, borderRight: '1px solid var(--color-line)', overflow: 'hidden' }}>{list}</Box>
      )}
      <Box style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <StoreHeader database={database} access={access} level={level} status={s.status} narrow={narrow}
          onAskForAccess={onAskForAccess} icon={<IconMessages size={16} color="var(--color-brand)" />}
          title={`RabbitMQ · ${database.dbname}`}
          detail={`${database.hostname}${s.ready?.version ? ` · RabbitMQ ${s.ready.version}` : ''}${s.ready?.user ? ` · as ${s.ready.user}` : ''}`}
          left={narrow ? <ActionIcon variant="subtle" onClick={listDrawer.open} aria-label="Open queues"><IconMenu2 size={18} /></ActionIcon> : undefined} />
        <FatalBanner fatal={s.fatal} />
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Stack gap="md" p="md">
            {selected
              ? <QueueDetail key={selected} name={selected} ask={s.ask} can={can} onConfirm={setConfirm} />
              : <Text size={FS.meta} c="dimmed">Pick a queue to see its messages and bindings.</Text>}
            {can('publish') && <Publish exchanges={exchanges} queues={queues} ask={s.ask} defaultQueue={selected} onSent={() => void load()} />}
          </Stack>
        </ScrollArea>
      </Box>
      <ConfirmByName opened={confirm !== null} hostname={database.hostname} what={confirm?.what ?? ''}
        onCancel={() => setConfirm(null)} onConfirm={(t) => void doConfirmed(t)} />
    </Box>
  )
}

type Ask = (req: Record<string, unknown> & { op: string }) => Promise<StoreAnswer>

function QueueDetail({ name, ask, can, onConfirm }: {
  name: string; ask: Ask; can: (op: string) => boolean; onConfirm: (c: Confirm) => void
}) {
  const [q, setQ] = useState<Queue | null>(null)
  const [bindings, setBindings] = useState<Binding[]>([])
  const [err, setErr] = useState<string | null>(null)
  const [msgs, setMsgs] = useState<Message[] | null>(null)
  const [count, setCount] = useState('10')
  const [peeking, setPeeking] = useState(false)

  const load = useCallback(async () => {
    const a = await ask({ op: 'queue', name })
    if (a.type !== 'result') { setErr(a.error ?? 'could not read the queue'); return }
    const d = a.data as { queue?: Queue; bindings?: Binding[] }
    setErr(null); setQ(d.queue ?? null); setBindings(d.bindings ?? [])
  }, [ask, name])
  useEffect(() => { void load() }, [load])

  const peek = async () => {
    setPeeking(true)
    const a = await ask({ op: 'peek', name, count: Number(count) })
    setPeeking(false)
    if (a.type !== 'result') { notifyError('Could not peek', new Error(a.error ?? 'failed')); return }
    setMsgs(Array.isArray(a.data) ? (a.data as Message[]) : [])
    void load()
  }

  if (err) return <Alert color="rose" variant="light">{err}</Alert>
  if (!q) return <Loader size="sm" />
  const stat = (label: string, v: number | undefined) => (
    <Box><Text size={FS.micro} c="dimmed">{label}</Text><Text fw={650} size={FS.body}>{(v ?? 0).toLocaleString()}</Text></Box>
  )
  return (
    <Stack gap="sm">
      <Group gap="xs" wrap="wrap">
        <Text fw={650} ff="monospace" size={FS.body}>{q.name}</Text>
        {q.type && <Badge size="sm" variant="outline" color="slate" radius="sm">{q.type}</Badge>}
        {q.durable && <Badge size="sm" variant="outline" color="slate" radius="sm">durable</Badge>}
        <Button size="compact-xs" variant="subtle" onClick={() => void load()}>Refresh</Button>
      </Group>
      <Group gap="xl">
        {stat('Ready', q.messages_ready)}{stat('Unacked', q.messages_unacknowledged)}{stat('Consumers', q.consumers)}
      </Group>
      <Group gap="xs" wrap="wrap">
        <Select size="xs" w={110} value={count} onChange={(v) => setCount(v ?? '10')} aria-label="How many to peek"
          data={['1', '10', '50', '100'].map((n) => ({ value: n, label: `${n} message${n === '1' ? '' : 's'}` }))} allowDeselect={false} />
        <Button size="xs" onClick={() => void peek()} loading={peeking} disabled={!can('peek')}>Peek</Button>
        {can('purge') && (
          <Button size="xs" variant="light" color="rose"
            onClick={() => onConfirm({ op: 'purge', name, what: `Purged every ready message from ${name}.` })}>Purge</Button>
        )}
        {can('delete_queue') && (
          <Button size="xs" variant="subtle" color="rose"
            onClick={() => onConfirm({ op: 'delete_queue', name, what: `Deleted queue ${name} and its messages.` })}>Delete queue</Button>
        )}
      </Group>
      <Text size={FS.micro} c="dimmed">
        Peek puts each message back. RabbitMQ marks it redelivered for whoever consumes it next, and the order of the queue can change.
      </Text>
      {msgs && (
        msgs.length === 0 ? <Text size={FS.meta} c="dimmed">The queue had no ready messages.</Text> : (
          <Stack gap={6}>
            {msgs.map((m, i) => (
              <Box key={i} p="xs" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
                <Text size={FS.micro} c="dimmed" mb={4}>
                  #{i + 1} · routing key {m.routing_key || '(none)'}{m.exchange ? ` · from ${m.exchange}` : ''}{m.redelivered ? ' · redelivered' : ''}
                  {m.payload_encoding === 'base64' ? ' · binary, shown as base64' : ''}
                </Text>
                <Box style={{ fontFamily: 'var(--font-mono)', fontSize: 12.5, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{m.payload}</Box>
              </Box>
            ))}
          </Stack>
        )
      )}
      <Box>
        <Text size={FS.meta} fw={600} mb={4}>Bindings</Text>
        {bindings.length === 0 ? <Text size={FS.micro} c="dimmed">None besides the default exchange.</Text> : (
          <Table verticalSpacing={4} fz="xs" withTableBorder>
            <Table.Tbody>
              {bindings.map((b, i) => (
                <Table.Tr key={i}><Table.Td ff="monospace">{b.source || '(default)'}</Table.Td><Table.Td ff="monospace">{b.routing_key}</Table.Td></Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Box>
    </Stack>
  )
}

function Publish({ exchanges, queues, ask, defaultQueue, onSent }: {
  exchanges: Exchange[]; queues: Queue[]; ask: Ask; defaultQueue: string | null; onSent: () => void
}) {
  const [exchange, setExchange] = useState<string>('')
  const [key, setKey] = useState('')
  const [payload, setPayload] = useState('')
  const [sending, setSending] = useState(false)
  useEffect(() => { if (defaultQueue && !exchange) setKey(defaultQueue) }, [defaultQueue, exchange])
  const send = async () => {
    setSending(true)
    const a = await ask({ op: 'publish', exchange, routingKey: key, payload })
    setSending(false)
    if (a.type !== 'result') { notifyError('Not published', new Error(a.error ?? 'failed')); return }
    const routed = (a.data as { routed?: boolean } | undefined)?.routed
    if (routed) notifyOk('Published', 'The message was routed to at least one queue. It is in the session’s recording.')
    else notifyError('Published, but not routed', new Error('No queue is bound for that routing key, so RabbitMQ dropped it.'))
    onSent()
  }
  return (
    <Box p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
      <Text size={FS.meta} fw={600} mb={6}>Publish a message</Text>
      <Stack gap="xs">
        <Group gap="xs" grow>
          <Select size="xs" label="Exchange" value={exchange} onChange={(v) => setExchange(v ?? '')} allowDeselect={false}
            data={[{ value: '', label: '(default) by queue name' },
              ...exchanges.filter((x) => x.name && !x.internal).map((x) => ({ value: x.name, label: `${x.name} (${x.type})` }))]} />
          <TextInput size="xs" label="Routing key" value={key} onChange={(e) => setKey(e.currentTarget.value)}
            placeholder={exchange ? 'orders.created' : queues[0]?.name ?? 'queue name'} />
        </Group>
        <Textarea size="xs" label="Payload" value={payload} onChange={(e) => setPayload(e.currentTarget.value)} autosize minRows={3} maxRows={10}
          styles={{ input: { fontFamily: 'var(--font-mono)' } }} placeholder='{"id": 1}' />
        <Group gap="xs">
          <Button size="xs" onClick={() => void send()} loading={sending} disabled={!payload && !key}>Publish</Button>
          <Text size={FS.micro} c="dimmed"><IconInfoCircle size={11} style={{ verticalAlign: -1 }} /> Sent persistent. The payload is kept in the recording.</Text>
        </Group>
      </Stack>
    </Box>
  )
}

function NewQueue({ onCreate }: { onCreate: (name: string, durable: boolean) => Promise<void> }) {
  const [name, setName] = useState('')
  const [durable, setDurable] = useState(true)
  return (
    <Group gap={6} wrap="nowrap" pt={6} style={{ borderTop: '1px solid var(--color-line)' }}>
      <TextInput size="xs" placeholder="New queue name" value={name} onChange={(e) => setName(e.currentTarget.value)} style={{ flex: 1 }} aria-label="New queue name" />
      <Checkbox size="xs" label="Durable" checked={durable} onChange={(e) => setDurable(e.currentTarget.checked)} />
      <Button size="compact-xs" disabled={!name.trim()} onClick={() => { void onCreate(name.trim(), durable); setName('') }}>Create</Button>
    </Group>
  )
}
