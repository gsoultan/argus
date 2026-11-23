import {
  Alert, Badge, Box, Button, Card, Group, NumberInput, SegmentedControl, Select, Stack, Table, Text, TextInput,
} from '@mantine/core'
import { IconCopy } from '@tabler/icons-react'
import { useCallback, useEffect, useState } from 'react'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  MAX_STREAM_READ, pageMoves, pageWords, streamBody, type StreamFrom, type StreamMessage, type StreamPage, type StreamRead,
  type StreamReaders,
} from '~/lib/rabbitStreams'
import { FS } from '~/theme'
import { useBroker } from './BrokerWorkspace'

/*
A stream is read, not taken: from its first message, its last ones, an
offset or a time, then Older and Newer. The agent reads as a reader would,
with one AMQP consumer as your own user, so nothing is taken and nobody
else's place moves. Each read is recorded, as a peek is.
*/

const pad = (n: number) => String(n).padStart(2, '0')
const localInput = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`

export function StreamMessages({ name }: { name: string }) {
  const { ask, can } = useBroker()
  const [from, setFrom] = useState<StreamFrom>('last')
  const [offset, setOffset] = useState<number | string>(0)
  const [at, setAt] = useState(() => localInput(new Date(Date.now() - 3600_000)))
  const [count, setCount] = useState('20')
  const [page, setPage] = useState<StreamPage | null>(null)
  const [reading, setReading] = useState(false)
  const [view, setView] = useState<'pretty' | 'raw' | 'headers'>('pretty')

  const read = useCallback(async (r: StreamRead) => {
    setReading(true)
    const a = await ask({ op: 'stream_read', name, count: Number(count), stream: r })
    setReading(false)
    if (a.type !== 'result') { notifyError('Could not read the stream', new Error(a.error ?? 'failed')); return }
    setPage(a.data as StreamPage)
  }, [ask, name, count])

  const start = (): StreamRead | null => {
    if (from === 'offset') return { from, offset: Math.max(0, Math.floor(Number(offset) || 0)) }
    if (from === 'time') {
      const t = new Date(at).getTime()
      return Number.isFinite(t) && t > 0 ? { from, at: t } : null
    }
    return { from }
  }
  const moves = page ? pageMoves(page, Number(count)) : { older: null, newer: null }

  // A new stream starts from nothing read.
  useEffect(() => { setPage(null) }, [name])

  return (
    <Stack gap="sm">
      <Group gap="xs" wrap="wrap" align="flex-end">
        <SegmentedControl size="xs" value={from} onChange={(v) => setFrom(v as StreamFrom)} aria-label="Read from"
          data={[{ label: 'First', value: 'first' }, { label: 'Last', value: 'last' }, { label: 'Offset', value: 'offset' }, { label: 'Time', value: 'time' }]} />
        {from === 'offset' && (
          <NumberInput size="xs" w={130} min={0} value={offset} onChange={setOffset} aria-label="Start at offset" thousandSeparator="," hideControls />
        )}
        {from === 'time' && (
          <TextInput size="xs" w={200} type="datetime-local" value={at} onChange={(e) => setAt(e.currentTarget.value)} aria-label="Time to start from" />
        )}
        <Select size="xs" w={140} value={count} onChange={(v) => setCount(v ?? '20')} aria-label="How many to read" allowDeselect={false}
          data={['10', '20', '50', '100', String(MAX_STREAM_READ)].map((n) => ({ value: n, label: `${n} messages` }))} />
        <Button size="xs" loading={reading} disabled={!can('stream_read') || start() === null}
          onClick={() => { const r = start(); if (r) void read(r) }}>Read</Button>
        <SegmentedControl size="xs" ml="auto" value={view} onChange={(v) => setView(v as typeof view)}
          data={[{ label: 'Pretty', value: 'pretty' }, { label: 'Raw', value: 'raw' }, { label: 'Headers', value: 'headers' }]} />
      </Group>
      <Text size={FS.micro} c="dimmed">
        Read as a reader would, from the place you choose. Nothing is taken and nobody else's place moves. Each read is in the session's recording.
      </Text>
      {page?.messages.map((m) => <StreamMessageCard key={m.offset} m={m} view={view} />)}
      {page && (
        <Group gap="xs" wrap="wrap">
          <Button size="xs" variant="default" disabled={moves.older === null || reading}
            onClick={() => moves.older !== null && void read({ from: 'offset', offset: moves.older })}>Older</Button>
          <Button size="xs" variant="default" disabled={moves.newer === null || reading}
            onClick={() => moves.newer !== null && void read({ from: 'offset', offset: moves.newer })}>Newer</Button>
          <Text size={FS.micro} c="dimmed" ml="auto">{pageWords(page)}</Text>
        </Group>
      )}
    </Stack>
  )
}

function StreamMessageCard({ m, view }: { m: StreamMessage; view: 'pretty' | 'raw' | 'headers' }) {
  const body = streamBody(m)
  const props = Object.entries(m.properties ?? {})
  const headers = Object.entries(m.headers ?? {})
  const copy = () => { void navigator.clipboard?.writeText(m.body).then(() => notifyOk('Copied', 'The body is on your clipboard.')) }
  return (
    <Card withBorder radius="md" padding={0}>
      <Group gap="xs" px="sm" py={6} wrap="wrap" style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--mantine-color-default-hover)' }}>
        <Badge size="sm" variant="light" color="slate" radius="sm" style={{ textTransform: 'none' }}>offset {m.offset.toLocaleString('en-US')}</Badge>
        {m.time ? <Text size={FS.micro} c="dimmed">{new Date(m.time).toLocaleString()}</Text> : null}
        {view === 'pretty' && props.map(([k, v]) => (
          <Text key={k} size={FS.micro} ff="monospace" c="dimmed">{k}={String(v)}</Text>
        ))}
        {m.encoding === 'base64' && <Badge size="xs" variant="light" color="slate">binary, shown as base64</Badge>}
        {m.truncated && <Badge size="xs" variant="light" color="amber" style={{ textTransform: 'none' }}>first {m.body.length.toLocaleString()} of {m.size.toLocaleString()} bytes</Badge>}
        <Button size="compact-xs" variant="subtle" ml="auto" leftSection={<IconCopy size={12} />} onClick={copy}>Copy</Button>
      </Group>
      {view === 'headers' ? (
        <Box px="sm" py={8}>
          {headers.length === 0 && props.length === 0 && <Text size={FS.micro} c="dimmed">No headers or properties.</Text>}
          {[...headers, ...props].map(([k, v]) => (
            <Group key={k} gap="xs" wrap="nowrap"><Text size={FS.micro} ff="monospace" c="dimmed">{k}</Text><Text size={FS.micro} ff="monospace">{typeof v === 'object' ? JSON.stringify(v) : String(v)}</Text></Group>
          ))}
        </Box>
      ) : (
        <pre className="argus-broker-pre">{view === 'pretty' ? body.text : m.body}</pre>
      )}
    </Card>
  )
}

/** Who reads the stream and how far behind its end they are. */
export function StreamReadersCard({ name }: { name: string }) {
  const { ask } = useBroker()
  const [r, setR] = useState<StreamReaders | null>(null)
  const [err, setErr] = useState<string | null>(null)
  useEffect(() => {
    let live = true
    const load = () => void ask({ op: 'stream_readers', name }).then((a) => {
      if (!live) return
      if (a.type !== 'result') { setErr(a.error ?? 'could not list the readers'); return }
      setErr(null); setR(a.data as StreamReaders)
    })
    load()
    const t = window.setInterval(() => { if (document.visibilityState === 'visible') load() }, 10_000)
    return () => { live = false; window.clearInterval(t) }
  }, [ask, name])
  const amqp = r?.readers.some((x) => x.protocol === 'amqp')
  return (
    <Card withBorder radius="md" padding={0}>
      <Box p="md" pb="xs">
        <Text fw={600} size={FS.body}>Readers</Text>
        <Text size={FS.micro} c="dimmed">Who reads this stream and how far behind the end they are.</Text>
      </Box>
      {err && <Alert mx="md" mb="md" color="rose" variant="light">{err}</Alert>}
      {r && r.readers.length === 0 && <Text size={FS.meta} c="dimmed" px="md" pb="md">Nobody is reading this stream now.</Text>}
      {r && r.readers.length > 0 && (
        <Table.ScrollContainer minWidth={420}>
          <Table verticalSpacing={6} fz="sm">
            <Table.Thead><Table.Tr><Table.Th>Reader</Table.Th><Table.Th>Protocol</Table.Th><Table.Th ta="right">At</Table.Th><Table.Th ta="right">Behind</Table.Th><Table.Th>From</Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {r.readers.map((x, i) => (
                <Table.Tr key={i}>
                  <Table.Td ff="monospace" fz={FS.micro} style={{ wordBreak: 'break-all' }}>{x.name}</Table.Td>
                  <Table.Td fz={FS.micro}>{x.protocol === 'stream' ? 'Stream protocol' : 'AMQP 0-9-1'}</Table.Td>
                  <Table.Td ta="right" ff="monospace" fz={FS.micro}>{x.offset !== undefined ? x.offset.toLocaleString('en-US') : '—'}</Table.Td>
                  <Table.Td ta="right" ff="monospace" fz={FS.micro} fw={x.behind ? 650 : undefined}>{x.behind !== undefined ? x.behind.toLocaleString('en-US') : '—'}</Table.Td>
                  <Table.Td fz={FS.micro}>{[x.from, x.user].filter(Boolean).join(' · ') || '—'}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
      {amqp && <Text size={FS.micro} c="dimmed" px="md" py="xs">AMQP readers keep no place on the server, so how far they are is not known.</Text>}
      {r && r.publishers.length > 0 && (
        <Text size={FS.micro} c="dimmed" px="md" pb="xs">
          Writing to it with the stream protocol: {r.publishers.map((p) => p.name).join(', ')}.
        </Text>
      )}
      {r?.noPlugin && (
        <Text size={FS.micro} c="dimmed" px="md" pb="md">
          Stream-protocol readers are listed by RabbitMQ's stream management plugin, which is off here. Turn it on with
          {' '}<Text span ff="monospace" size={FS.micro}>rabbitmq-plugins enable rabbitmq_stream_management</Text>.
        </Text>
      )}
    </Card>
  )
}
