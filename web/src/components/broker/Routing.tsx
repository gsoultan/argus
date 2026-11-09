import {
  Alert, Badge, Box, Button, Card, Checkbox, Group, Modal, ScrollArea, Select, Stack, Text, TextInput,
} from '@mantine/core'
import { IconCheck, IconMinus, IconPlus, IconSearch, IconX } from '@tabler/icons-react'
import { useElementSize } from '@mantine/hooks'
import { useEffect, useMemo, useState } from 'react'
import { fmtRate, rate, routeKey, type Binding, type Exchange } from '~/lib/rabbit'
import { FS } from '~/theme'
import { MONO, useBroker } from './BrokerWorkspace'

/*
Where messages go. A broker's logic lives in its bindings, so this page
draws them: who publishes to an exchange, the exchange, and each binding's
key to the queue (or exchange) behind it. "Test a routing key" matches a key
against the same bindings, as RabbitMQ would, without publishing anything.
*/

const KIND_COLOR: Record<string, string> = { topic: 'azure', direct: 'slate', fanout: 'sky', headers: 'amber' }

export function KindTag({ type }: { type?: string }) {
  if (!type) return null
  return <Badge size="xs" variant="light" color={KIND_COLOR[type] ?? 'slate'} radius="sm" style={{ textTransform: 'none' }}>{type}</Badge>
}

const label = (name: string) => name || '(default)'

/** "k=v, k2=v2" as headers for a headers exchange. */
export function parseHeaders(s: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const part of s.split(',')) {
    const i = part.indexOf('=')
    if (i > 0) out[part.slice(0, i).trim()] = part.slice(i + 1).trim()
  }
  return out
}

export function Routing({ selected, onSelect }: { selected?: string; onSelect: (x: string) => void }) {
  const { exchanges, bindings, queues, narrow, can, ask } = useBroker()
  const [filter, setFilter] = useState('')
  const [creating, setCreating] = useState(false)
  const [binding, setBinding] = useState(false)
  const visible = useMemo(() => exchanges.filter((x) => x.name !== '' && (!filter || x.name.toLowerCase().includes(filter.toLowerCase())))
    .sort((a, b) => Number(a.name.startsWith('amq.')) - Number(b.name.startsWith('amq.')) || a.name.localeCompare(b.name)), [exchanges, filter])
  const current = selected ?? visible.find((x) => bindings.some((b) => b.source === x.name))?.name ?? visible[0]?.name ?? ''
  const ex = exchanges.find((x) => x.name === current)

  const [testEx, setTestEx] = useState<string>(current)
  const [key, setKey] = useState('')
  const [hdrs, setHdrs] = useState('')
  useEffect(() => { setTestEx(current) }, [current])
  const testType = exchanges.find((x) => x.name === testEx)?.type
  const routes = useMemo(() => (key || testType === 'fanout' || testType === 'headers'
    ? routeKey(testEx, key, parseHeaders(hdrs), exchanges, bindings, queues) : null), [testEx, key, hdrs, exchanges, bindings, queues, testType])
  const matched = new Set(testEx === current && routes ? routes.flatMap((r) => [r.queue, ...r.via.map((b) => b.destination)]) : [])

  const [detail, setDetail] = useState<Exchange | null>(null)
  useEffect(() => {
    if (!current) return
    let live = true
    void ask({ op: 'exchange', name: current }).then((a) => {
      if (live && a.type === 'result') setDetail((a.data as { exchange?: Exchange }).exchange ?? null)
    })
    return () => { live = false }
  }, [ask, current])

  const out = bindings.filter((b) => b.source === current)
  const counts = new Map<string, number>()
  for (const b of bindings) counts.set(b.source, (counts.get(b.source) ?? 0) + 1)

  return (
    <Box style={{ display: 'grid', gap: 'var(--mantine-spacing-md)' }} className="argus-broker-side">
      {narrow ? (
        <Select label="Exchange" value={current} onChange={(v) => v && onSelect(v)} searchable
          data={visible.map((x) => ({ value: x.name, label: `${x.name} (${x.type})` }))} />
      ) : (
        <Card withBorder radius="md" padding="xs" style={{ alignSelf: 'start' }}>
          <TextInput size="xs" placeholder="Filter exchanges" leftSection={<IconSearch size={12} />} value={filter}
            onChange={(e) => setFilter(e.currentTarget.value)} mb={6} aria-label="Filter exchanges" />
          <ScrollArea.Autosize mah={520} type="auto">
            {visible.map((x) => (
              <button key={x.name} type="button" className="argus-broker-pick" aria-current={x.name === current ? 'true' : undefined}
                onClick={() => onSelect(x.name)}>
                <Text size={FS.meta} ff="monospace" truncate style={{ flex: 1, minWidth: 0 }}>{x.name}</Text>
                <KindTag type={x.type} />
                <Text size={FS.micro} c="dimmed" w={18} ta="right">{counts.get(x.name) ?? 0}</Text>
              </button>
            ))}
          </ScrollArea.Autosize>
          {can('declare_exchange') && (
            <Button fullWidth size="xs" variant="default" mt={6} leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New exchange</Button>
          )}
        </Card>
      )}

      <Stack gap="md" style={{ minWidth: 0 }}>
        <Card withBorder radius="md" padding="md">
          <Group gap="xs" wrap="wrap">
            <Text fw={600} size={FS.body}>Test a routing key</Text>
            <Text size={FS.micro} c="dimmed">Nothing is published. Argus matches the key against the bindings below.</Text>
          </Group>
          <Group gap="xs" mt="sm" wrap={narrow ? 'wrap' : 'nowrap'} align="flex-end">
            <Select size="sm" w={narrow ? '100%' : 200} value={testEx} onChange={(v) => setTestEx(v ?? '')} aria-label="Exchange to test" searchable
              data={[{ value: '', label: '(default) by queue name' }, ...visible.map((x) => ({ value: x.name, label: x.name }))]} />
            <TextInput size="sm" style={{ flex: 1 }} w={narrow ? '100%' : undefined} styles={MONO} value={key} onChange={(e) => setKey(e.currentTarget.value)}
              placeholder={testType === 'topic' ? 'orders.created.eu' : 'routing key'} aria-label="Routing key" />
            {testType === 'headers' && (
              <TextInput size="sm" w={narrow ? '100%' : 220} styles={MONO} value={hdrs} onChange={(e) => setHdrs(e.currentTarget.value)}
                placeholder="tenant=acme, region=eu" aria-label="Headers" />
            )}
          </Group>
          {routes && (
            <Alert mt="sm" variant="light" color={routes.length ? 'teal' : 'amber'} icon={routes.length ? <IconCheck size={16} /> : <IconX size={16} />}>
              {routes.length === 0 ? (
                <>Reaches <b>no queue</b>. RabbitMQ would drop it{testEx ? ', or return it to a publisher that asked with mandatory' : ''}.</>
              ) : (
                <>Reaches <b>{routes.length} queue{routes.length === 1 ? '' : 's'}</b>:{' '}
                  {routes.map((r, i) => (
                    <span key={r.queue}>{i ? ', ' : ''}<Text span ff="monospace" size={FS.meta}>{r.queue}</Text>
                      {r.via.length > 0 && <Text span c="dimmed" size={FS.micro}> ({r.via.map((b) => b.routing_key || '∅').join(' → ')})</Text>}</span>
                  ))}
                  {testEx === current ? '. Highlighted below.' : '.'}
                </>
              )}
            </Alert>
          )}
        </Card>

        {ex && (
          <Card withBorder radius="md" padding="md">
            <Group gap="xs" mb="sm" wrap="wrap">
              <Text fw={600} size={FS.body}>Where <Text span ff="monospace">{ex.name}</Text> sends messages</Text>
              <KindTag type={ex.type} />
              {typeof ex.arguments?.['alternate-exchange'] === 'string' && (
                <Text size={FS.micro} c="dimmed">unrouted → <Text span ff="monospace" size={FS.micro}>{String(ex.arguments['alternate-exchange'])}</Text></Text>
              )}
              <Group gap="xs" ml="auto">
                {can('bind') && <Button size="xs" variant="default" onClick={() => setBinding(true)}>Add binding</Button>}
              </Group>
            </Group>
            {narrow
              ? <BindingList bindings={out} matched={matched} />
              : <Diagram ex={ex} detail={detail} bindings={out} matched={matched} />}
          </Card>
        )}
      </Stack>
      <NewExchange opened={creating} onClose={() => setCreating(false)} onCreated={onSelect} />
      {ex && <AddBinding opened={binding} onClose={() => setBinding(false)} exchange={ex.name} />}
    </Box>
  )
}

function BindingList({ bindings, matched }: { bindings: Binding[]; matched: Set<string> }) {
  if (bindings.length === 0) return <Text size={FS.meta} c="dimmed">Nothing is bound to this exchange, so everything published to it is dropped.</Text>
  return (
    <Stack gap={0}>
      {bindings.map((b, i) => {
        const hit = matched.has(b.destination)
        return (
          <Group key={i} gap="sm" py="sm" px={6} wrap="nowrap" align="flex-start"
            style={{ borderTop: i ? '1px solid var(--color-line)' : undefined, background: hit ? 'var(--mantine-color-teal-light)' : undefined }}>
            <Box pt={2} style={{ flex: 'none' }}>
              {hit ? <IconCheck size={16} color="var(--mantine-color-teal-6)" /> : <IconMinus size={16} color="var(--mantine-color-dimmed)" />}
            </Box>
            {/* Names wrap rather than truncate: on a phone the whole name is the point. */}
            <Stack gap={4} style={{ minWidth: 0, flex: 1 }}>
              <Text ff="monospace" fw={600} size={FS.body} style={{ overflowWrap: 'anywhere' }}>
                {b.destination}{b.destination_type === 'exchange' ? ' (exchange)' : ''}
              </Text>
              <Text ff="monospace" size={FS.meta} c={hit ? 'azure' : 'dimmed'} style={{ overflowWrap: 'anywhere' }}>
                key {b.routing_key || '(none)'}
              </Text>
            </Stack>
          </Group>
        )
      })}
    </Stack>
  )
}

const cut = (s: string, n: number) => (s.length > n ? `${s.slice(0, Math.max(1, n - 1))}…` : s)

/* Rough text widths, to size the diagram's columns to the names in them.
   Mono glyphs are 0.6em wide; the sans subtitles average a little less. */
const monoW = (s: string, px: number) => s.length * px * 0.6
const sansW = (s: string, px: number) => s.length * px * 0.55
const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v))

function Diagram({ ex, detail, bindings, matched }: { ex: Exchange; detail: Exchange | null; bindings: Binding[]; matched: Set<string> }) {
  const { queues, openQueue } = useBroker()
  const { ref, width: avail } = useElementSize()
  const shown = bindings.slice(0, 30)
  const pubs = (detail?.incoming ?? []).filter((p) => p.channel_details).slice(0, 8)
  const qByName = new Map(queues.map((q) => [q.name, q]))
  const pubName = (p: (typeof pubs)[number]) => p.channel_details?.connection_name ?? p.channel_details?.name ?? 'publisher'
  const keyOf = (b: Binding) => b.routing_key || (ex.type === 'headers' ? 'headers' : '(empty)')
  const subOf = (b: Binding) => {
    const q = qByName.get(b.destination)
    const dlx = q?.arguments?.['x-dead-letter-exchange']
    const sub = b.destination_type === 'exchange' ? 'exchange'
      : q ? `${q.consumers ?? 0} consumer${q.consumers === 1 ? '' : 's'} · ${(q.messages_ready ?? 0).toLocaleString()} ready` : 'queue'
    return `${sub}${typeof dlx === 'string' ? ` · DLX ${dlx || '(default)'}` : ''}`
  }
  const exSub = `${ex.type} · ${ex.durable ? 'durable' : 'transient'} · ${bindings.length} binding${bindings.length === 1 ? '' : 's'}`
  const showKeys = ex.type !== 'fanout'

  // Columns are as wide as their longest name, within limits; past the limit
  // a name is cut and the full one is in its tooltip.
  const PAD = 14
  let pubW = pubs.length ? clamp(Math.max(...pubs.map((p) => monoW(pubName(p), 12))) + PAD * 2, 170, 300) : 170
  let exW = clamp(Math.max(monoW(ex.name, 14), sansW(exSub, 12)) + PAD * 2, 220, 420)
  const keyW = showKeys ? clamp(Math.max(0, ...shown.map((b) => monoW(keyOf(b), 12))) + 28, 90, 340) : 40
  const qW = clamp(Math.max(240, ...shown.map((b) => Math.max(monoW(b.destination, 13), sansW(subOf(b), 12)))) + PAD * 2, 240, 460)
  let gap1 = 64
  let gap2 = 56
  // Too wide for the card: give back room where a cut name costs least
  // (publishers, then the connectors, then the exchange), so routing keys and
  // queue names keep theirs. If that is not enough, the card is too narrow
  // for a readable diagram and the bindings are listed instead.
  let over = avail ? pubW + gap1 + exW + gap2 + keyW + qW - avail : 0
  const give = (have: number, min: number) => { const d = Math.max(0, Math.min(over, have - min)); over -= d; return have - d }
  pubW = give(pubW, 150)
  gap1 = give(gap1, 36)
  gap2 = give(gap2, 36)
  exW = give(exW, 200)
  const natural = pubW + gap1 + exW + gap2 + keyW + qW
  // Spare width goes to the connectors first, then to the queue column.
  let spare = Math.max(0, avail - natural)
  const g = Math.min(spare / 2, 90)
  gap1 += g; gap2 += g; spare -= g * 2
  const qWide = qW + spare
  const W = Math.max(natural, Math.floor(avail) || natural)

  const exX = pubW + gap1
  const lineX = exX + exW + gap2 // where the straight run to a queue starts
  const qx = lineX + keyW

  const ROW = 76
  const NH = 54
  const EH = 80
  const rows = Math.max(shown.length, pubs.length, 1)
  const body = Math.max(rows * ROW, EH + 24)
  const H = body + 28
  const yOf = (i: number, n: number) => body / 2 - (n * ROW) / 2 + i * ROW + (ROW - NH) / 2
  const exY = body / 2 - EH / 2
  const exMid = exY + EH / 2
  const curve = (x1: number, y1: number, x2: number, y2: number) => `M${x1},${y1} C${(x1 + x2) / 2},${y1} ${(x1 + x2) / 2},${y2} ${x2},${y2}`
  const fit = (w: number, px: number) => Math.max(4, Math.floor((w - PAD * 2) / (px * 0.6)))
  const fitSans = (w: number, px: number) => Math.max(4, Math.floor((w - PAD * 2) / (px * 0.55)))
  const mono = 'var(--font-mono)'
  if (over > 0) return <Box ref={ref}><BindingList bindings={bindings} matched={matched} /></Box>
  return (
    <Box ref={ref}>
      <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img" aria-label={`Bindings of exchange ${ex.name}`}
        style={{ fontFamily: 'var(--font-sans)', display: 'block' }}>
        <defs>
          <marker id="argus-ar" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--mantine-color-dimmed)" />
          </marker>
        </defs>
        {pubs.map((_, i) => (
          <path key={`pe${i}`} d={curve(pubW, yOf(i, pubs.length) + NH / 2, exX, exMid)} fill="none" stroke="var(--mantine-color-azure-3)" strokeWidth={2} />
        ))}
        {shown.map((b, i) => {
          const hit = matched.has(b.destination)
          const ty = yOf(i, shown.length) + NH / 2
          return <path key={`be${i}`} d={`${curve(exX + exW, exMid, lineX, ty)} L${qx - 2},${ty}`} fill="none" markerEnd="url(#argus-ar)"
            stroke={hit ? 'var(--mantine-color-azure-6)' : 'var(--mantine-color-slate-4)'} strokeWidth={hit ? 2.6 : 1.4} />
        })}
        {pubs.map((p, i) => {
          const y = yOf(i, pubs.length)
          const name = pubName(p)
          return (
            <g key={`p${i}`}>
              <title>{name}</title>
              <rect x={0} y={y} width={pubW} height={NH} rx={9} fill="var(--mantine-color-azure-light)" stroke="var(--mantine-color-azure-3)" />
              <text x={PAD} y={y + 22} fontSize={12} fontWeight={600} fontFamily={mono} fill="var(--mantine-color-text)">{cut(name, fit(pubW, 12))}</text>
              <text x={PAD} y={y + 41} fontSize={12} fill="var(--mantine-color-dimmed)">{fmtRate(rate(p.stats?.publish_details))} publishing</text>
            </g>
          )
        })}
        {pubs.length === 0 && (
          <text x={pubW / 2} y={exMid + 4} fontSize={12} textAnchor="middle" fill="var(--mantine-color-dimmed)">No publisher right now</text>
        )}
        <g>
          <title>{ex.name}</title>
          <rect x={exX} y={exY} width={exW} height={EH} rx={10} fill="var(--mantine-color-default-hover)" stroke="var(--mantine-color-azure-5)" strokeWidth={1.5} />
          <text x={exX + PAD} y={exY + 27} fontSize={14} fontWeight={700} fontFamily={mono} fill="var(--mantine-color-text)">{cut(ex.name, fit(exW, 14))}</text>
          <text x={exX + PAD} y={exY + 48} fontSize={12} fill="var(--mantine-color-dimmed)">{cut(exSub, fitSans(exW, 12))}</text>
          <text x={exX + PAD} y={exY + 66} fontSize={12} fill="var(--mantine-color-dimmed)">{fmtRate(rate(ex.message_stats?.publish_in_details))} in</text>
        </g>
        {shown.map((b, i) => {
          const y = yOf(i, shown.length)
          const hit = matched.has(b.destination)
          const key = keyOf(b)
          const sub = subOf(b)
          return (
            <g key={`b${i}`} style={{ cursor: b.destination_type === 'exchange' ? undefined : 'pointer' }}
              onClick={() => { if (b.destination_type !== 'exchange') openQueue(b.destination) }}>
              <title>{showKeys ? `${key} → ${b.destination}` : b.destination}</title>
              {showKeys && (
                <text x={qx - 12} y={y + NH / 2 - 7} fontSize={12} fontFamily={mono} textAnchor="end"
                  fill={hit ? 'var(--mantine-color-azure-text)' : 'var(--mantine-color-dimmed)'}>{cut(key, Math.floor((keyW - 20) / 7.2))}</text>
              )}
              <rect x={qx} y={y} width={qWide - 2} height={NH} rx={9}
                fill={hit ? 'var(--mantine-color-teal-light)' : 'var(--mantine-color-body)'}
                stroke={hit ? 'var(--mantine-color-teal-5)' : 'var(--color-line)'} strokeWidth={hit ? 1.5 : 1} />
              <text x={qx + PAD} y={y + 23} fontSize={13} fontWeight={600} fontFamily={mono} fill="var(--mantine-color-text)">{cut(b.destination, fit(qWide, 13))}</text>
              <text x={qx + PAD} y={y + 42} fontSize={12} fill="var(--mantine-color-dimmed)">{cut(sub, fitSans(qWide, 12))}</text>
            </g>
          )
        })}
        {shown.length === 0 && (
          <text x={qx + PAD} y={exMid + 4} fontSize={12} fill="var(--mantine-color-dimmed)">Nothing bound: messages are dropped</text>
        )}
        <text x={0} y={H - 6} fontSize={12} fill="var(--mantine-color-dimmed)">Publishers</text>
        <text x={exX} y={H - 6} fontSize={12} fill="var(--mantine-color-dimmed)">Exchange</text>
        <text x={showKeys ? lineX : qx} y={H - 6} fontSize={12} fill="var(--mantine-color-dimmed)">{showKeys ? 'Routing key → queue' : 'Bound queues'}</text>
      </svg>
      {bindings.length > shown.length && <Text size={FS.micro} c="dimmed">Showing 30 of {bindings.length} bindings.</Text>}
    </Box>
  )
}

function NewExchange({ opened, onClose, onCreated }: { opened: boolean; onClose: () => void; onCreated: (n: string) => void }) {
  const { act } = useBroker()
  const [name, setName] = useState('')
  const [kind, setKind] = useState<string>('topic')
  const [durable, setDurable] = useState(true)
  const create = async () => {
    const n = name.trim()
    const a = await act({ op: 'declare_exchange', name: n, kind, durable }, `Created ${kind} exchange ${n}.`)
    if (a.type === 'result') { onClose(); setName(''); onCreated(n) }
  }
  return (
    <Modal opened={opened} onClose={onClose} title="New exchange" centered>
      <Stack gap="sm">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} data-autofocus styles={MONO} />
        <Select label="Type" value={kind} onChange={(v) => setKind(v ?? 'topic')} allowDeselect={false}
          data={[{ value: 'topic', label: 'topic: by pattern (orders.*.eu)' }, { value: 'direct', label: 'direct: by exact key' },
            { value: 'fanout', label: 'fanout: to every bound queue' }, { value: 'headers', label: 'headers: by message headers' }]} />
        <Checkbox label="Durable: survives a broker restart" checked={durable} onChange={(e) => setDurable(e.currentTarget.checked)} />
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button disabled={!name.trim()} onClick={() => void create()}>Create</Button></Group>
      </Stack>
    </Modal>
  )
}

function AddBinding({ opened, onClose, exchange }: { opened: boolean; onClose: () => void; exchange: string }) {
  const { act, queues } = useBroker()
  const [queue, setQueue] = useState<string | null>(null)
  const [key, setKey] = useState('')
  const bind = async () => {
    if (!queue) return
    const a = await act({ op: 'bind', exchange, name: queue, routingKey: key }, `Bound ${queue} to ${exchange} with key ${key || '(empty)'}.`)
    if (a.type === 'result') { onClose(); setKey('') }
  }
  return (
    <Modal opened={opened} onClose={onClose} title={`Bind a queue to ${label(exchange)}`} centered>
      <Stack gap="sm">
        <Select label="Queue" searchable value={queue} onChange={setQueue} data={queues.map((q) => q.name)} />
        <TextInput label="Routing key" styles={MONO} value={key} onChange={(e) => setKey(e.currentTarget.value)} placeholder="orders.*.eu" />
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button disabled={!queue} onClick={() => void bind()}>Bind</Button></Group>
      </Stack>
    </Modal>
  )
}
