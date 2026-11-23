import {
  ActionIcon, Badge, Box, Button, Card, Group, Progress, SegmentedControl, SimpleGrid, Stack, Table, Tabs, Text, TextInput,
} from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { Link } from '@tanstack/react-router'
import { IconAlertTriangle, IconLock, IconPlus, IconSearch, IconSend } from '@tabler/icons-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ButtonLink } from '~/components/links'
import { FatalBanner, LABEL_COLOR } from '~/components/StoreChrome'
import { useStoreSession, type StoreAnswer } from '~/components/useStoreSession'
import { ConsumerDrawer, ConsumerForm, PublishDrawer, StreamForm } from '~/components/nats/NatsForms'
import { StreamDetail } from '~/components/nats/NatsStream'
import { levelName } from '~/lib/engines'
import {
  ago, consumerFilters, consumerKind, fmtBytes, fmtCount, fmtNanos, items, natsAttention, streamsFor, subjectMatches, validSubject,
  type NatsConn, type NatsConsumer, type NatsOverview, type NatsStream, type NatsSubject,
} from '~/lib/nats'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS, INK } from '~/theme'
import type { DBAccess, DBLevel, Database } from '~/types/domain'

/*
One NATS account as a broker: an overview of what it holds and whether its
apps keep up, its JetStream streams with their messages, the consumers that
read them, the subjects in use and the apps connected. What the person may
do comes from the agent's answer (`ops`), the fixed list for their level,
and the permissions NATS itself holds the session's user to.

Browsing -- lists, counts, pending -- is not recorded and is refreshed every
ten seconds while the page is visible. Reading messages, publishing and
every change to a stream or consumer are recorded by the agent before they
run.
*/

export type NatsTab = 'overview' | 'streams' | 'consumers' | 'subjects' | 'connections'
export const NATS_TABS: NatsTab[] = ['overview', 'streams', 'consumers', 'subjects', 'connections']

export type Req = Record<string, unknown> & { op: string }
export type Ask = (r: Req) => Promise<StoreAnswer>

export interface NatsWorkspaceProps {
  database: Database
  access: DBAccess
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>
  endpoint: string
  onAskForAccess?: () => void
  tab: NatsTab
  stream?: string
  consumer?: string
  onNavigate: (s: { tab: NatsTab; stream?: string; consumer?: string }) => void
  settingsLink?: boolean
}

const POLL_MS = 10_000

export function NatsWorkspace(props: NatsWorkspaceProps) {
  const { database, access, getTicket, endpoint, onAskForAccess, tab, stream, consumer, onNavigate } = props
  const narrow = useMediaQuery('(max-width: 48em)') ?? false
  const s = useStoreSession(database, getTicket, endpoint)
  const level = s.ready?.level ?? access.level
  const ops = s.ready?.ops
  const can = useCallback((op: string) => ops?.includes(op) ?? false, [ops])

  const [overview, setOverview] = useState<NatsOverview | null>(null)
  const [streams, setStreams] = useState<NatsStream[]>([])
  const [consumers, setConsumers] = useState<NatsConsumer[]>([])
  const [loaded, setLoaded] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [publishing, setPublishing] = useState<string | null>(null)
  const [streamForm, setStreamForm] = useState<{ edit?: NatsStream } | null>(null)
  const [consumerForm, setConsumerForm] = useState<{ stream?: string } | null>(null)
  const busy = useRef(false)

  const reload = useCallback(async () => {
    if (busy.current) return
    busy.current = true
    try {
      const o = await s.ask({ op: 'overview' })
      if (o.type !== 'result') { setErr(o.error ?? 'could not read the account'); return }
      const ov = o.data as NatsOverview
      setOverview(ov)
      setErr(null)
      if (ov.jetstream) {
        const [st, co] = await Promise.all([s.ask({ op: 'streams' }), s.ask({ op: 'consumers' })])
        if (st.type === 'result') setStreams(items<NatsStream>(st.data))
        else setErr(st.error ?? 'could not list streams')
        if (co.type === 'result') setConsumers(items<NatsConsumer>(co.data))
      }
      setLoaded(true)
    } finally {
      busy.current = false
    }
  }, [s])

  useEffect(() => {
    if (s.status !== 'ready' || !s.ready) return
    void reload()
    const t = window.setInterval(() => { if (document.visibilityState === 'visible') void reload() }, POLL_MS)
    return () => window.clearInterval(t)
  }, [s.status, s.ready, reload])

  const needs = useMemo(() => natsAttention(overview, streams, consumers), [overview, streams, consumers])

  const act = useCallback(async (req: Req, ok: string) => {
    const a = await s.ask(req)
    if (a.type === 'result') {
      notifyOk('Done', `${ok} It is in the session’s recording.`)
      void reload()
    } else if (!a.needsConfirm) {
      notifyError('Not done', new Error(a.error ?? 'failed'))
    }
    return a
  }, [s, reload])

  const openStream = (name?: string) => onNavigate({ tab: 'streams', stream: name })
  const openConsumer = (st: string, c: string) => onNavigate({ tab: 'consumers', stream: st, consumer: c })
  const js = overview?.jetstream ?? true
  const openedConsumer = tab === 'consumers' && stream && consumer ? consumers.find((c) => c.stream_name === stream && c.name === consumer) : undefined
  return (
    <Box>
      <Box px={{ base: 'md', lg: 'xl' }} pt="md" style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--mantine-color-body)' }}>
        <Text size={FS.micro} c="dimmed" mb={4}>
          <Link to="/brokers" style={{ color: INK.brand }}>Brokers</Link> / NATS {database.dbname}
        </Text>
        <Group gap="sm" wrap="wrap" align="center">
          <NatsGlyph />
          <Text fw={650} fz={20}>{database.dbname}</Text>
          <Badge variant="light" color={LABEL_COLOR[database.label]} radius="sm">{database.label}</Badge>
          <Group gap={6} ml="auto" wrap="wrap">
            <Badge variant="light" color={s.status === 'ready' ? 'teal' : s.status === 'connecting' ? 'slate' : 'rose'} radius="sm"
              leftSection={<span style={{ display: 'inline-block', width: 6, height: 6, borderRadius: 3, background: 'currentColor' }} />}>
              {s.status === 'ready' ? 'Connected · recording' : s.status === 'connecting' ? 'Connecting' : 'Closed'}
            </Badge>
            <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm">{level ? levelName(level, database.engine) : '…'}</Badge>
            {access.approvable && onAskForAccess && (
              <Button size="xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>
                {narrow ? 'Ask' : `Ask for ${levelName(access.approvable, database.engine)}`}
              </Button>
            )}
            {props.settingsLink && !narrow && (
              <ButtonLink size="xs" variant="subtle" to="/brokers/$brokerId/settings" params={{ brokerId: database.id }}>Who can connect</ButtonLink>
            )}
            {can('publish') && (
              <Button size="xs" variant={can('stream_create') ? 'default' : 'filled'} leftSection={<IconSend size={14} />} onClick={() => setPublishing('')}>
                Publish
              </Button>
            )}
            {can('stream_create') && js && (
              <Button size="xs" leftSection={<IconPlus size={14} />} onClick={() => setStreamForm({})}>New stream</Button>
            )}
          </Group>
        </Group>
        <Text size={FS.micro} c="dimmed" mt={4}>
          account <Text span ff="monospace" size={FS.micro}>{database.dbname}</Text> on {database.hostname}:{database.port}
          {overview?.version ? <> · NATS {overview.version}</> : null}
          {overview ? <> · JetStream {overview.jetstream ? 'on' : 'off'}</> : null}
          {overview?.cluster ? <>, cluster <Text span ff="monospace" size={FS.micro}>{overview.cluster}</Text></> : null}
          {s.ready?.user ? <> · signed in as <Text span ff="monospace" size={FS.micro}>{s.ready.user}</Text></> : null}
        </Text>
        <Tabs value={tab} onChange={(v) => v && onNavigate({ tab: v as NatsTab })} mt="sm" variant="default"
          styles={{ list: { borderBottom: 0, flexWrap: 'nowrap', overflowX: 'auto' }, tab: { whiteSpace: 'nowrap' } }}>
          <Tabs.List>
            <Tabs.Tab value="overview">Overview</Tabs.Tab>
            <Tabs.Tab value="streams" rightSection={<Badge size="xs" variant="light" color={needs.some((n) => n.stream && !n.consumer) ? 'amber' : 'slate'}>{streams.length}</Badge>}>Streams</Tabs.Tab>
            <Tabs.Tab value="consumers" rightSection={<Badge size="xs" variant="light" color={needs.some((n) => n.consumer) ? 'amber' : 'slate'}>{consumers.length}</Badge>}>Consumers</Tabs.Tab>
            <Tabs.Tab value="subjects">Subjects</Tabs.Tab>
            <Tabs.Tab value="connections" rightSection={overview?.stat ? <Badge size="xs" variant="light" color="slate">{overview.stat.conns}</Badge> : undefined}>Connections</Tabs.Tab>
          </Tabs.List>
        </Tabs>
      </Box>
      <FatalBanner fatal={s.fatal} />
      <Box px={{ base: 'sm', sm: 'md', lg: 'xl' }} py="md">
        {err && <Text size={FS.meta} c="rose" mb="sm">{err}</Text>}
        {!js && loaded && tab !== 'subjects' && tab !== 'connections' && (
          <Text size={FS.meta} c="dimmed" mb="sm">JetStream is not on for this account, so there are no streams or consumers. Subjects, Connections and Publish still work.</Text>
        )}
        {!loaded ? <Text size={FS.meta} c="dimmed">Reading the account…</Text> : (
          <>
            {tab === 'overview' && <Overview overview={overview} streams={streams} consumers={consumers} needs={needs}
              onStream={openStream} onConsumer={openConsumer} />}
            {tab === 'streams' && (stream
              ? <StreamDetail key={stream} name={stream} streams={streams} consumers={consumers.filter((c) => c.stream_name === stream)}
                  ask={s.ask} act={act} can={can} narrow={narrow}
                  onBack={() => openStream()} onConsumer={(c) => openConsumer(stream, c)} onPublish={(subject) => setPublishing(subject)}
                  onEdit={(st) => setStreamForm({ edit: st })} onNewConsumer={() => setConsumerForm({ stream })} />
              : <Streams streams={streams} needs={needs} narrow={narrow} onOpen={openStream} />)}
            {tab === 'consumers' && <Consumers consumers={consumers} needs={needs} canCreate={can('consumer_create') && streams.length > 0}
              onNew={() => setConsumerForm({})} onOpen={openConsumer} onStream={openStream} />}
            {tab === 'subjects' && <Subjects ask={s.ask} streams={streams} canPublish={can('publish')} onPublish={(x) => setPublishing(x)} onStream={openStream} />}
            {tab === 'connections' && <Connections ask={s.ask} />}
          </>
        )}
      </Box>
      {openedConsumer && (
        <ConsumerDrawer c={openedConsumer} narrow={narrow} act={act} can={can}
          onClose={() => onNavigate({ tab: 'consumers' })} onStream={() => openStream(openedConsumer.stream_name)} />
      )}
      {publishing !== null && (
        <PublishDrawer initial={publishing} streams={streams} narrow={narrow} ask={s.ask} canStream={js}
          onClose={() => setPublishing(null)} onDone={() => void reload()} />
      )}
      {streamForm && (
        <StreamForm edit={streamForm.edit} streams={streams} narrow={narrow} onClose={() => setStreamForm(null)}
          onSave={async (spec) => {
            const editing = Boolean(streamForm.edit)
            const a = await act({ op: editing ? 'stream_update' : 'stream_create', nats: { streamSpec: spec } },
              editing ? `Changed stream ${spec.name}.` : `Created stream ${spec.name}.`)
            if (a.type === 'result') { setStreamForm(null); openStream(spec.name) }
          }} />
      )}
      {consumerForm && (
        <ConsumerForm streams={streams} initialStream={consumerForm.stream} narrow={narrow} onClose={() => setConsumerForm(null)}
          onSave={async (st, spec) => {
            const a = await act({ op: 'consumer_create', nats: { stream: st, consumerSpec: spec } }, `Created consumer ${spec.name} on ${st}.`)
            return a.type === 'result'
          }} />
      )}
    </Box>
  )
}

export function NatsGlyph() {
  return (
    <Box aria-hidden w={20} h={20} style={{ display: 'grid', placeItems: 'center', borderRadius: 5, background: 'var(--mantine-color-slate-light)' }}>
      <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.4">
        <path d="M2 2.5h10v7H8.5L6 12V9.5H2z" />
        <path d="M5 5.2l1.6 1.6L9 4.4" />
      </svg>
    </Box>
  )
}

function Kpi({ label, value, hint, tone }: { label: string; value: string; hint?: string; tone?: string }) {
  return (
    <Card withBorder padding="sm" radius="md">
      <Text size={FS.micro} c="dimmed">{label}</Text>
      <Text fw={650} fz={22} mt={2} style={{ fontVariantNumeric: 'tabular-nums' }}>{value}</Text>
      {hint && <Text size={FS.micro} c={tone ?? 'dimmed'}>{hint}</Text>}
    </Card>
  )
}

function LimitBar({ label, used, max, f }: { label: string; used: number; max: number; f: (n: number) => string }) {
  const pct = max > 0 ? Math.min(100, (used / max) * 100) : 0
  return (
    <Box>
      <Group justify="space-between" gap="xs">
        <Text size={FS.meta}>{label}</Text>
        <Text size={FS.micro} c="dimmed">{f(used)} of {max > 0 ? f(max) : 'no limit'}</Text>
      </Group>
      <Progress value={pct} size="sm" mt={4} color={pct >= 90 ? 'rose' : pct >= 75 ? 'amber' : 'azure'} aria-label={`${label} used`} />
    </Box>
  )
}

function Attention({ needs, onStream, onConsumer, quiet }: {
  needs: ReturnType<typeof natsAttention>; onStream: (s: string) => void; onConsumer: (s: string, c: string) => void; quiet: string
}) {
  return (
    <Card withBorder radius="md" padding="md">
      <Text fw={600} size={FS.body} mb="xs">Needs attention</Text>
      {needs.length === 0 ? <Text size={FS.meta} c="dimmed">{quiet}</Text> : (
        <Stack gap={6}>
          {needs.map((n, i) => (
            <Group key={i} gap="xs" wrap="nowrap" align="flex-start">
              <IconAlertTriangle size={16} color={`var(--mantine-color-${n.level}-6)`} style={{ flexShrink: 0, marginTop: 2 }} />
              <Text size={FS.meta} style={{ flex: 1 }}>{n.text}</Text>
              {n.consumer && n.stream ? <Button size="compact-xs" variant="subtle" onClick={() => onConsumer(n.stream!, n.consumer!)}>Open</Button>
                : n.stream ? <Button size="compact-xs" variant="subtle" onClick={() => onStream(n.stream!)}>Open</Button> : null}
            </Group>
          ))}
        </Stack>
      )}
    </Card>
  )
}

function Overview({ overview, streams, consumers, needs, onStream, onConsumer }: {
  overview: NatsOverview | null; streams: NatsStream[]; consumers: NatsConsumer[]; needs: ReturnType<typeof natsAttention>
  onStream: (s: string) => void; onConsumer: (s: string, c: string) => void
}) {
  const messages = streams.reduce((n, s) => n + s.state.messages, 0)
  const bytes = streams.reduce((n, s) => n + s.state.bytes, 0)
  const pending = consumers.reduce((n, c) => n + c.num_pending, 0)
  const biggest = streams.slice().sort((a, b) => b.state.bytes - a.state.bytes).slice(0, 6)
  const behind = consumers.slice().filter((c) => c.num_pending > 0).sort((a, b) => b.num_pending - a.num_pending).slice(0, 6)
  const st = overview?.stat
  const info = overview?.info
  return (
    <Stack gap="md">
      <SimpleGrid cols={{ base: 2, sm: 3, lg: 5 }} spacing="sm">
        <Kpi label="Connections" value={st ? String(st.conns) : '–'} hint={st ? `${fmtCount(st.subscriptions)} subscriptions` : overview?.statError ? 'monitoring port not reached' : undefined}
          tone={overview?.statError ? 'amber' : undefined} />
        <Kpi label="Messages in / out" value={st ? `${fmtCount(st.msgsIn)} / ${fmtCount(st.msgsOut)}` : '–'} hint={st ? `${fmtBytes(st.bytesIn)} in · since the server started` : undefined} />
        <Kpi label="Streams" value={String(streams.length)} hint={`${fmtCount(messages)} messages · ${fmtBytes(bytes)}`} />
        <Kpi label="Consumers" value={String(consumers.length)} hint={pending ? `${fmtCount(pending)} messages waiting` : 'every consumer is caught up'} tone={pending > 10_000 ? 'amber' : undefined} />
        <Kpi label="Slow consumers" value={st ? String(st.slowConsumers) : '–'} hint={st?.slowConsumers ? 'messages were dropped for them' : 'none since the server started'}
          tone={st?.slowConsumers ? 'amber' : undefined} />
      </SimpleGrid>
      <Attention needs={needs} onStream={onStream} onConsumer={onConsumer}
        quiet="Every stream has its leader and replicas, no stream is near full, and no consumer has messages waiting with nobody pulling." />
      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
        <Card withBorder radius="md" padding="md">
          <Text fw={600} size={FS.body} mb="xs">Largest streams</Text>
          {biggest.length === 0 ? <Text size={FS.meta} c="dimmed">No streams in this account yet.</Text> : (
            <Table fz="sm" verticalSpacing={6} highlightOnHover>
              <Table.Tbody>
                {biggest.map((s) => (
                  <Table.Tr key={s.config.name} onClick={() => onStream(s.config.name)} style={{ cursor: 'pointer' }}>
                    <Table.Td><Text ff="monospace" size={FS.meta} truncate>{s.config.name}</Text></Table.Td>
                    <Table.Td ta="right"><Text size={FS.meta} c="dimmed">{fmtCount(s.state.messages)} msgs</Text></Table.Td>
                    <Table.Td ta="right"><Text size={FS.meta} fw={600}>{fmtBytes(s.state.bytes)}</Text></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
        </Card>
        <Card withBorder radius="md" padding="md">
          <Text fw={600} size={FS.body} mb="xs">Most behind</Text>
          {behind.length === 0 ? <Text size={FS.meta} c="dimmed">No consumer has messages waiting.</Text> : (
            <Table fz="sm" verticalSpacing={6} highlightOnHover>
              <Table.Tbody>
                {behind.map((c) => (
                  <Table.Tr key={`${c.stream_name}/${c.name}`} onClick={() => onConsumer(c.stream_name, c.name)} style={{ cursor: 'pointer' }}>
                    <Table.Td><Text ff="monospace" size={FS.meta} truncate>{c.stream_name} › {c.name}</Text></Table.Td>
                    <Table.Td><Text size={FS.micro} c="dimmed">{c.paused ? 'paused' : `active ${ago(c.delivered.last_active)} ago`}</Text></Table.Td>
                    <Table.Td ta="right"><Text size={FS.meta} fw={600}>{fmtCount(c.num_pending)}</Text></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
        </Card>
      </SimpleGrid>
      {info?.limits && (
        <Card withBorder radius="md" padding="md">
          <Text fw={600} size={FS.body} mb="xs">What JetStream lets this account keep</Text>
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md">
            <LimitBar label="File storage" used={info.storage} max={info.limits.max_storage} f={fmtBytes} />
            <LimitBar label="Memory" used={info.memory} max={info.limits.max_memory} f={fmtBytes} />
            <LimitBar label="Streams" used={info.streams} max={info.limits.max_streams} f={(n) => fmtCount(n)} />
            <LimitBar label="Consumers" used={info.consumers} max={info.limits.max_consumers} f={(n) => fmtCount(n)} />
          </SimpleGrid>
        </Card>
      )}
      {overview?.statError && (
        <Text size={FS.micro} c="dimmed">Connections and message rates come from the server&rsquo;s monitoring port, which did not answer: {overview.statError}</Text>
      )}
    </Stack>
  )
}

function keeps(s: NatsStream): string {
  const c = s.config
  if (c.retention === 'workqueue') return 'work queue'
  if (c.retention === 'interest') return 'until read'
  const parts = [c.max_age > 0 ? fmtNanos(c.max_age) : '', c.max_msgs > 0 ? `${fmtCount(c.max_msgs)} msgs` : '', c.max_bytes > 0 ? fmtBytes(c.max_bytes) : '']
  return parts.filter(Boolean).join(' · ') || 'for ever'
}

export function isSystemStream(name: string) {
  return name.startsWith('KV_') || name.startsWith('OBJ_')
}

function Streams({ streams, needs, narrow, onOpen }: {
  streams: NatsStream[]; needs: ReturnType<typeof natsAttention>; narrow: boolean; onOpen: (s: string) => void
}) {
  const [q, setQ] = useState('')
  const [show, setShow] = useState<'all' | 'attention' | 'workqueue' | 'apps'>('all')
  const flagged = new Set(needs.filter((n) => n.stream && !n.consumer).map((n) => n.stream))
  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return streams.filter((s) => {
      if (needle && !s.config.name.toLowerCase().includes(needle) && !(s.config.subjects ?? []).some((x) => x.toLowerCase().includes(needle))) return false
      if (show === 'attention') return flagged.has(s.config.name)
      if (show === 'workqueue') return s.config.retention === 'workqueue'
      if (show === 'apps') return !isSystemStream(s.config.name)
      return true
    }).sort((a, b) => Number(flagged.has(b.config.name)) - Number(flagged.has(a.config.name)) || a.config.name.localeCompare(b.config.name))
  }, [streams, q, show, flagged])
  return (
    <Stack gap="sm">
      <Group gap="xs" wrap="wrap">
        <TextInput size="xs" placeholder="Filter by name or subject" leftSection={<IconSearch size={12} />} value={q}
          onChange={(e) => setQ(e.currentTarget.value)} aria-label="Filter streams" w={narrow ? '100%' : 260} />
        <SegmentedControl size="xs" value={show} onChange={(v) => setShow(v as typeof show)}
          data={[{ label: 'All', value: 'all' }, { label: 'Needs attention', value: 'attention' }, { label: 'Work queues', value: 'workqueue' }, { label: 'Hide KV and objects', value: 'apps' }]} />
      </Group>
      <Card withBorder radius="md" padding={0}>
        <Table.ScrollContainer minWidth={760}>
          <Table verticalSpacing={8} fz="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Stream</Table.Th><Table.Th>Stored</Table.Th><Table.Th>Keeps</Table.Th>
                <Table.Th ta="right">Messages</Table.Th><Table.Th ta="right">Size</Table.Th><Table.Th ta="right">Consumers</Table.Th><Table.Th>Last message</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((s) => (
                <Table.Tr key={s.config.name} onClick={() => onOpen(s.config.name)} style={{ cursor: 'pointer' }}>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <Text span ff="monospace" fw={500} size={FS.meta}>{s.config.name}</Text>
                      {flagged.has(s.config.name) && <IconAlertTriangle size={14} color="var(--mantine-color-amber-6)" aria-label="Needs attention" />}
                      {s.config.name.startsWith('KV_') && <Badge size="xs" variant="light" color="slate" radius="sm">key-value</Badge>}
                      {s.config.name.startsWith('OBJ_') && <Badge size="xs" variant="light" color="slate" radius="sm">object store</Badge>}
                      {s.config.mirror && <Badge size="xs" variant="light" color="sky" radius="sm">mirror</Badge>}
                    </Group>
                    <Text size={FS.micro} c="dimmed" ff="monospace" truncate maw={320}>{(s.config.subjects ?? []).join('  ') || (s.config.mirror ? `mirror of ${s.config.mirror.name}` : '')}</Text>
                  </Table.Td>
                  <Table.Td><Text size={FS.meta}>{s.config.storage} · R{s.config.num_replicas || 1}</Text></Table.Td>
                  <Table.Td><Text size={FS.meta}>{keeps(s)}</Text></Table.Td>
                  <Table.Td ta="right">{fmtCount(s.state.messages)}</Table.Td>
                  <Table.Td ta="right">{fmtBytes(s.state.bytes)}</Table.Td>
                  <Table.Td ta="right">{s.state.consumer_count}</Table.Td>
                  <Table.Td><Text size={FS.micro} c="dimmed">{s.state.messages ? `${ago(s.state.last_ts)} ago` : 'empty'}</Text></Table.Td>
                </Table.Tr>
              ))}
              {rows.length === 0 && (
                <Table.Tr><Table.Td colSpan={7}><Text size={FS.meta} c="dimmed" p="sm">{streams.length ? 'No stream matches.' : 'No streams in this account yet.'}</Text></Table.Td></Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Card>
      <Text size={FS.micro} c="dimmed">Messages and size are what each stream holds now, after its limits; refreshed every 10 seconds.</Text>
    </Stack>
  )
}

function ConsumerState({ c }: { c: NatsConsumer }) {
  if (c.paused) return <Badge size="sm" variant="light" color="slate" radius="sm">paused</Badge>
  if (c.num_pending > 0 && c.num_waiting === 0 && !c.push_bound) return <Badge size="sm" variant="light" color="amber" radius="sm">nobody pulling</Badge>
  if (c.num_pending > 0) return <Badge size="sm" variant="light" color="sky" radius="sm">behind</Badge>
  return <Badge size="sm" variant="light" color="teal" radius="sm">caught up</Badge>
}

function Consumers({ consumers, needs, canCreate, onNew, onOpen, onStream }: {
  consumers: NatsConsumer[]; needs: ReturnType<typeof natsAttention>; canCreate: boolean
  onNew: () => void; onOpen: (s: string, c: string) => void; onStream: (s: string) => void
}) {
  const [show, setShow] = useState<'all' | 'behind' | 'idle' | 'redelivering'>('all')
  const flagged = new Set(needs.filter((n) => n.consumer).map((n) => `${n.stream}/${n.consumer}`))
  const rows = consumers.filter((c) => show === 'all' || (show === 'behind' && c.num_pending > 0)
    || (show === 'idle' && c.num_pending > 0 && c.num_waiting === 0 && !c.push_bound) || (show === 'redelivering' && c.num_redelivered > 0))
    .sort((a, b) => Number(flagged.has(`${b.stream_name}/${b.name}`)) - Number(flagged.has(`${a.stream_name}/${a.name}`)) || b.num_pending - a.num_pending)
  return (
    <Stack gap="sm">
      <Group gap="xs" wrap="wrap">
        <SegmentedControl size="xs" value={show} onChange={(v) => setShow(v as typeof show)}
          data={[{ label: 'All', value: 'all' }, { label: 'Behind', value: 'behind' }, { label: 'Nobody pulling', value: 'idle' }, { label: 'Redelivering', value: 'redelivering' }]} />
        {canCreate && <Button size="xs" ml="auto" leftSection={<IconPlus size={14} />} onClick={onNew}>New consumer</Button>}
      </Group>
      <Card withBorder radius="md" padding={0}>
        <Table.ScrollContainer minWidth={820}>
          <Table verticalSpacing={8} fz="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Consumer</Table.Th><Table.Th>Reads</Table.Th><Table.Th>Kind</Table.Th><Table.Th>State</Table.Th>
                <Table.Th ta="right">Waiting</Table.Th><Table.Th ta="right">Unacked</Table.Th><Table.Th ta="right">Redelivered</Table.Th><Table.Th>Last active</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((c) => (
                <Table.Tr key={`${c.stream_name}/${c.name}`} onClick={() => onOpen(c.stream_name, c.name)} style={{ cursor: 'pointer' }}>
                  <Table.Td>
                    <Text ff="monospace" fw={500} size={FS.meta}>{c.name}</Text>
                    <Text size={FS.micro} c="dimmed" ff="monospace" style={{ cursor: 'pointer' }} onClick={(e) => { e.stopPropagation(); onStream(c.stream_name) }}>{c.stream_name}</Text>
                  </Table.Td>
                  <Table.Td><Text size={FS.micro} ff="monospace">{consumerFilters(c.config).join(' ') || 'everything'}</Text></Table.Td>
                  <Table.Td><Text size={FS.meta}>{consumerKind(c.config)} · ack {c.config.ack_policy}</Text></Table.Td>
                  <Table.Td><ConsumerState c={c} /></Table.Td>
                  <Table.Td ta="right" fw={c.num_pending ? 600 : undefined}>{fmtCount(c.num_pending)}</Table.Td>
                  <Table.Td ta="right">{fmtCount(c.num_ack_pending)}</Table.Td>
                  <Table.Td ta="right" c={c.num_redelivered ? 'amber' : undefined}>{fmtCount(c.num_redelivered)}</Table.Td>
                  <Table.Td><Text size={FS.micro} c="dimmed">{c.delivered.last_active ? `${ago(c.delivered.last_active)} ago` : 'never'}</Text></Table.Td>
                </Table.Tr>
              ))}
              {rows.length === 0 && (
                <Table.Tr><Table.Td colSpan={8}><Text size={FS.meta} c="dimmed" p="sm">{consumers.length ? 'No consumer matches.' : 'No consumers in this account yet.'}</Text></Table.Td></Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Card>
      <Text size={FS.micro} c="dimmed">Waiting is what the stream holds that the consumer has not had yet; unacked is what it had and has not confirmed.</Text>
    </Stack>
  )
}

function Subjects({ ask, streams, canPublish, onPublish, onStream }: {
  ask: Ask; streams: NatsStream[]; canPublish: boolean; onPublish: (s: string) => void; onStream: (s: string) => void
}) {
  const [list, setList] = useState<NatsSubject[] | null>(null)
  const [monitorErr, setMonitorErr] = useState('')
  const [test, setTest] = useState('')
  const [q, setQ] = useState('')
  const load = useCallback(async () => {
    const a = await ask({ op: 'subjects' })
    if (a.type === 'result') {
      const d = a.data as { subjects?: NatsSubject[]; monitorError?: string }
      setList(d.subjects ?? [])
      setMonitorErr(d.monitorError ?? '')
    } else notifyError('Could not list subjects', new Error(a.error ?? 'failed'))
  }, [ask])
  useEffect(() => { void load() }, [load])
  const literal = test.trim()
  const ok = literal !== '' && validSubject(literal) && !/(^|\.)[*>](\.|$)/.test(literal)
  const storedBy = ok ? streamsFor(literal, streams) : []
  const listeners = ok ? (list ?? []).filter((x) => x.subs > 0 && subjectMatches(x.subject, literal)) : []
  const rows = (list ?? []).filter((x) => !q.trim() || x.subject.toLowerCase().includes(q.trim().toLowerCase()))
  return (
    <Stack gap="md">
      <Card withBorder radius="md" padding="md">
        <Text fw={600} size={FS.body}>Test a subject</Text>
        <Text size={FS.micro} c="dimmed" mb="xs">Which streams would store a message on it, and who is listening. Nothing is sent.</Text>
        <Group gap="xs" align="flex-start" wrap="wrap">
          <TextInput size="xs" placeholder="orders.eu.created" value={test} onChange={(e) => setTest(e.currentTarget.value)} aria-label="Subject to test"
            styles={{ input: { fontFamily: 'var(--font-mono)' } }} w={300} error={literal && !ok ? 'A subject with no wildcards' : undefined} />
          {canPublish && ok && <Button size="xs" variant="light" leftSection={<IconSend size={12} />} onClick={() => onPublish(literal)}>Publish to it</Button>}
        </Group>
        {ok && (
          <Stack gap={4} mt="sm">
            <Text size={FS.meta}>
              {storedBy.length ? <>Stored by {storedBy.map((s, i) => <span key={s}>{i ? ', ' : ''}<Text span ff="monospace" c={INK.brand} style={{ cursor: 'pointer' }} onClick={() => onStream(s)}>{s}</Text></span>)}.</>
                : 'No stream stores it: a message on it reaches only who is listening now, and is gone after.'}
            </Text>
            <Text size={FS.meta}>
              {listeners.length
                ? `Heard by ${listeners.reduce((n, x) => n + x.subs, 0)} subscription${listeners.reduce((n, x) => n + x.subs, 0) === 1 ? '' : 's'}: ${listeners.map((x) => `${x.subject}${x.queues?.length ? ` (queue ${x.queues.join(', ')})` : ''}`).join(', ')}.`
                : 'Nobody subscribes to it right now.'}
            </Text>
          </Stack>
        )}
      </Card>
      <Group gap="xs">
        <TextInput size="xs" placeholder="Filter subjects" leftSection={<IconSearch size={12} />} value={q} onChange={(e) => setQ(e.currentTarget.value)} aria-label="Filter subjects" w={260} />
        <Button size="xs" variant="default" ml="auto" onClick={() => void load()}>Refresh</Button>
      </Group>
      <Card withBorder radius="md" padding={0}>
        <Table.ScrollContainer minWidth={700}>
          <Table verticalSpacing={8} fz="sm">
            <Table.Thead>
              <Table.Tr><Table.Th>Subject</Table.Th><Table.Th ta="right">Subscriptions</Table.Th><Table.Th>Queue groups</Table.Th><Table.Th>Apps</Table.Th><Table.Th>Stored by</Table.Th><Table.Th /></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {list === null && <Table.Tr><Table.Td colSpan={6}><Text size={FS.meta} c="dimmed" p="sm">Reading subjects…</Text></Table.Td></Table.Tr>}
              {rows.map((x) => (
                <Table.Tr key={x.subject}>
                  <Table.Td><Text ff="monospace" size={FS.meta}>{x.subject}</Text></Table.Td>
                  <Table.Td ta="right">{x.subs}</Table.Td>
                  <Table.Td><Text size={FS.micro} ff="monospace">{(x.queues ?? []).join(', ')}</Text></Table.Td>
                  <Table.Td><Text size={FS.micro} c="dimmed" truncate maw={220}>{(x.apps ?? []).join(', ')}</Text></Table.Td>
                  <Table.Td>
                    <Group gap={4}>{(x.streams ?? []).map((s) => (
                      <Badge key={s} size="sm" variant="default" radius="sm" style={{ textTransform: 'none', cursor: 'pointer' }} onClick={() => onStream(s)}>{s}</Badge>
                    ))}</Group>
                  </Table.Td>
                  <Table.Td ta="right">
                    {canPublish && !/[*>]/.test(x.subject) && <ActionIcon size="sm" variant="subtle" aria-label={`Publish to ${x.subject}`} onClick={() => onPublish(x.subject)}><IconSend size={14} /></ActionIcon>}
                  </Table.Td>
                </Table.Tr>
              ))}
              {list !== null && rows.length === 0 && <Table.Tr><Table.Td colSpan={6}><Text size={FS.meta} c="dimmed" p="sm">No subjects in use.</Text></Table.Td></Table.Tr>}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Card>
      <Text size={FS.micro} c="dimmed">
        From the apps&rsquo; subscriptions and the streams&rsquo; subjects. Argus&rsquo;s own sessions and NATS&rsquo;s internal subjects are left out.
        {monitorErr ? ` Subscriptions come from the monitoring port, which did not answer: ${monitorErr}` : ''}
      </Text>
    </Stack>
  )
}

function Connections({ ask }: { ask: Ask }) {
  const [list, setList] = useState<NatsConn[] | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const load = useCallback(async () => {
    const a = await ask({ op: 'connections' })
    if (a.type === 'result') { setList(items<NatsConn>(a.data)); setErr(null) } else setErr(a.error ?? 'could not list connections')
  }, [ask])
  useEffect(() => { void load() }, [load])
  return (
    <Stack gap="sm">
      <Group><Button size="xs" variant="default" ml="auto" onClick={() => void load()}>Refresh</Button></Group>
      {err && <Text size={FS.meta} c="rose">{err}</Text>}
      <Card withBorder radius="md" padding={0}>
        <Table.ScrollContainer minWidth={820}>
          <Table verticalSpacing={8} fz="sm">
            <Table.Thead>
              <Table.Tr><Table.Th>App</Table.Th><Table.Th>Client</Table.Th><Table.Th>From</Table.Th><Table.Th ta="right">Subscriptions</Table.Th>
                <Table.Th ta="right">In / out</Table.Th><Table.Th ta="right">Pending</Table.Th><Table.Th>Connected</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {list === null && !err && <Table.Tr><Table.Td colSpan={7}><Text size={FS.meta} c="dimmed" p="sm">Reading connections…</Text></Table.Td></Table.Tr>}
              {(list ?? []).map((c) => (
                <Table.Tr key={c.cid}>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <Text size={FS.meta} fw={500}>{c.name || `connection ${c.cid}`}</Text>
                      {c.argus && <Badge size="xs" variant="light" color="azure" radius="sm">Argus</Badge>}
                    </Group>
                    {c.user && <Text size={FS.micro} c="dimmed" ff="monospace" truncate maw={240}>{c.user}</Text>}
                  </Table.Td>
                  <Table.Td><Text size={FS.micro}>{[c.lang, c.version].filter(Boolean).join(' ') || '–'}</Text></Table.Td>
                  <Table.Td><Text size={FS.micro} ff="monospace">{c.ip}:{c.port}</Text>{c.rtt && <Text size={FS.micro} c="dimmed">rtt {c.rtt}</Text>}</Table.Td>
                  <Table.Td ta="right">{c.subscriptions}</Table.Td>
                  <Table.Td ta="right"><Text size={FS.micro}>{fmtCount(c.inMsgs)} / {fmtCount(c.outMsgs)}</Text></Table.Td>
                  <Table.Td ta="right" c={c.pendingBytes > 1 << 20 ? 'amber' : undefined}>{fmtBytes(c.pendingBytes)}</Table.Td>
                  <Table.Td><Text size={FS.micro} c="dimmed">{ago(c.start)} ago · active {ago(c.lastActivity)} ago</Text></Table.Td>
                </Table.Tr>
              ))}
              {list !== null && list.length === 0 && <Table.Tr><Table.Td colSpan={7}><Text size={FS.meta} c="dimmed" p="sm">No apps are connected to this account.</Text></Table.Td></Table.Tr>}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Card>
      <Text size={FS.micro} c="dimmed">From the server&rsquo;s monitoring port. Pending is what the server holds for an app that has not read it yet; a lot of it means the app is falling behind.</Text>
    </Stack>
  )
}

