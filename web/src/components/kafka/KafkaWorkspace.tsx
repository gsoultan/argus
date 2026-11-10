import {
  ActionIcon, Alert, Badge, Box, Button, Card, Code, Drawer, Group, Modal, NumberInput, SegmentedControl, SimpleGrid, Stack, Table, Tabs, Text,
  TextInput, Textarea,
} from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { Link } from '@tanstack/react-router'
import { IconAlertTriangle, IconArrowLeft, IconLock, IconPlus, IconRefresh, IconSearch, IconSend, IconTrash, IconX } from '@tabler/icons-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ButtonLink } from '~/components/links'
import { ConfirmByName, FatalBanner, LABEL_COLOR } from '~/components/StoreChrome'
import { useStoreSession, type StoreAnswer } from '~/components/useStoreSession'
import {
  fmtCount, items, kafkaAttention, scopeLabel, showValue, totalLag,
  type KafkaGroup, type KafkaMessage, type KafkaOverview, type KafkaTopic, type KafkaTopicDetail,
} from '~/lib/kafka'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS, INK } from '~/theme'
import { levelName } from '~/lib/engines'
import type { DBAccess, DBLevel, Database } from '~/types/domain'

/*
One Kafka entry -- the topics one scope names -- as a broker: an overview of
whether the cluster is healthy and consumers keep up, the topics with their
partitions and messages, and the consumer groups with their lag. What the
person may do comes from the agent's answer (`ops`), the fixed list for their
level, and Kafka's own ACLs on the person's user hold it.

Browsing -- lists, offsets, lag -- is not recorded and is refreshed every ten
seconds while the page is visible. Peeking at messages, publishing and
creating or deleting a topic are recorded by the agent before they run.
*/

export type KafkaTab = 'overview' | 'topics' | 'groups'

type Req = Record<string, unknown> & { op: string }

export interface KafkaWorkspaceProps {
  database: Database
  access: DBAccess
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>
  endpoint: string
  onAskForAccess?: () => void
  tab: KafkaTab
  topic?: string
  onNavigate: (s: { tab: KafkaTab; topic?: string }) => void
  settingsLink?: boolean
}

const POLL_MS = 10_000
const MONO = { input: { fontFamily: 'var(--font-mono)' } }

export function KafkaWorkspace(props: KafkaWorkspaceProps) {
  const { database, access, getTicket, endpoint, onAskForAccess, tab, topic, onNavigate } = props
  const narrow = useMediaQuery('(max-width: 48em)') ?? false
  const s = useStoreSession(database, getTicket, endpoint)
  const level = s.ready?.level ?? access.level
  const ops = s.ready?.ops
  const can = useCallback((op: string) => ops?.includes(op) ?? false, [ops])

  const [overview, setOverview] = useState<KafkaOverview | null>(null)
  const [topics, setTopics] = useState<KafkaTopic[]>([])
  const [groups, setGroups] = useState<KafkaGroup[]>([])
  const [loaded, setLoaded] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [publishing, setPublishing] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const busy = useRef(false)

  const reload = useCallback(async () => {
    if (busy.current) return
    busy.current = true
    try {
      const [o, t, g] = await Promise.all([s.ask({ op: 'overview' }), s.ask({ op: 'topics' }), s.ask({ op: 'groups' })])
      if (t.type !== 'result') { setErr(t.error ?? 'could not list topics'); return }
      setErr(null)
      setTopics(items<KafkaTopic>(t.data))
      if (o.type === 'result') setOverview(o.data as KafkaOverview)
      if (g.type === 'result') setGroups(items<KafkaGroup>(g.data))
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

  const needs = useMemo(() => kafkaAttention(topics, groups), [topics, groups])

  const act = useCallback(async (req: Req, ok: string) => {
    const a = await s.ask(req)
    if (a.type === 'result') {
      notifyOk('Done', `${ok} It is in the session's recording.`)
      void reload()
    } else if (!a.needsConfirm) {
      notifyError('Not done', new Error(a.error ?? 'failed'))
    }
    return a
  }, [s, reload])

  const openTopic = (name?: string) => onNavigate({ tab: 'topics', topic: name })
  const scope = database.dbname
  return (
    <Box>
      <Box px={{ base: 'md', lg: 'xl' }} pt="md" style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--mantine-color-body)' }}>
        <Text size={FS.micro} c="dimmed" mb={4}>
          <Link to="/brokers" style={{ color: INK.brand }}>Brokers</Link> / Kafka {scope}
        </Text>
        <Group gap="sm" wrap="wrap" align="center">
          <KafkaGlyph />
          <Text fw={650} fz={20}>{scope === '*' ? 'Kafka' : scope}</Text>
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
              <Button size="xs" leftSection={<IconSend size={14} />} onClick={() => setPublishing(topic ?? '')}>
                {narrow ? 'Publish' : 'Publish message'}
              </Button>
            )}
          </Group>
        </Group>
        <Text size={FS.micro} c="dimmed" mt={4}>
          {scopeLabel(scope)} on {database.hostname}:{database.port}
          {overview?.cluster ? <> · cluster <Text span ff="monospace" size={FS.micro}>{overview.cluster}</Text></> : null}
          {s.ready?.user ? <> · signed in as <Text span ff="monospace" size={FS.micro}>{s.ready.user}</Text></> : null}
        </Text>
        <Tabs value={tab} onChange={(v) => v && onNavigate({ tab: v as KafkaTab })} mt="sm" variant="default"
          styles={{ list: { borderBottom: 0, flexWrap: 'nowrap', overflowX: 'auto' }, tab: { whiteSpace: 'nowrap' } }}>
          <Tabs.List>
            <Tabs.Tab value="overview">Overview</Tabs.Tab>
            <Tabs.Tab value="topics" rightSection={<Badge size="xs" variant="light" color={needs.some((n) => n.topic) ? 'amber' : 'slate'}>{topics.length}</Badge>}>Topics</Tabs.Tab>
            <Tabs.Tab value="groups" rightSection={<Badge size="xs" variant="light" color={needs.some((n) => n.group) ? 'amber' : 'slate'}>{groups.length}</Badge>}>
              {narrow ? 'Groups' : 'Consumer groups'}
            </Tabs.Tab>
          </Tabs.List>
        </Tabs>
      </Box>
      <FatalBanner fatal={s.fatal} />
      <Box px={{ base: 'sm', sm: 'md', lg: 'xl' }} py="md">
        {err && <Text size={FS.meta} c="rose" mb="sm">{err}</Text>}
        {!loaded ? <Text size={FS.meta} c="dimmed">Reading the cluster…</Text> : (
          <>
            {tab === 'overview' && <Overview overview={overview} topics={topics} groups={groups} needs={needs}
              onTopic={openTopic} onGroups={() => onNavigate({ tab: 'groups' })} />}
            {tab === 'topics' && (topic
              ? <TopicDetail key={topic} name={topic} ask={s.ask} act={act} can={can} hostname={database.hostname} narrow={narrow}
                  onBack={() => openTopic()} onPublish={() => setPublishing(topic)} />
              : <Topics topics={topics} narrow={narrow} onOpen={openTopic} canCreate={can('create_topic')} onCreate={() => setCreating(true)} />)}
            {tab === 'groups' && <Groups groups={groups} onTopic={openTopic} />}
          </>
        )}
      </Box>
      {publishing !== null && (
        <PublishDrawer topics={topics} initial={publishing} narrow={narrow} ask={s.ask} onClose={() => setPublishing(null)} onDone={() => void reload()} />
      )}
      <NewTopic opened={creating} scope={scope} brokers={overview?.brokers?.length ?? 0} onClose={() => setCreating(false)}
        onCreate={async (name, partitions, properties) => {
          const a = await act({ op: 'create_topic', name, count: partitions, properties }, `Created topic ${name}.`)
          if (a.type === 'result') { setCreating(false); openTopic(name) }
        }} />
    </Box>
  )
}

function KafkaGlyph() {
  return (
    <Box aria-hidden w={20} h={20} style={{ display: 'grid', placeItems: 'center', borderRadius: 5, background: 'var(--mantine-color-slate-light)' }}>
      <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.4">
        <circle cx="7" cy="2.5" r="1.6" /><circle cx="7" cy="7" r="1.6" /><circle cx="7" cy="11.5" r="1.6" />
        <circle cx="11.5" cy="4.6" r="1.4" /><circle cx="11.5" cy="9.4" r="1.4" />
        <path d="M8.5 6.3l1.6-.9M8.5 7.7l1.6.9" />
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

function Overview({ overview, topics, groups, needs, onTopic, onGroups }: {
  overview: KafkaOverview | null; topics: KafkaTopic[]; groups: KafkaGroup[]; needs: ReturnType<typeof kafkaAttention>
  onTopic: (t: string) => void; onGroups: () => void
}) {
  const messages = topics.reduce((n, t) => n + t.messages, 0)
  const lag = groups.reduce((n, g) => n + totalLag(g), 0)
  const biggest = topics.slice().sort((a, b) => b.messages - a.messages).slice(0, 6)
  const lagging = groups.slice().sort((a, b) => totalLag(b) - totalLag(a)).filter((g) => totalLag(g) > 0).slice(0, 6)
  const brokers = overview?.brokers ?? []
  return (
    <Stack gap="md">
      <SimpleGrid cols={{ base: 2, sm: 3, lg: 5 }} spacing="sm">
        <Kpi label="Brokers" value={String(brokers.length || '–')} hint={brokers.find((b) => b.controller) ? `controller ${brokers.find((b) => b.controller)?.id}` : undefined} />
        <Kpi label="Topics" value={String(topics.length)} hint={`${overview?.partitions ?? topics.reduce((n, t) => n + t.partitions, 0)} partitions`} />
        <Kpi label="Messages kept" value={fmtCount(messages)} hint="what the logs hold now" />
        <Kpi label="Consumer groups" value={String(groups.length)} hint="reading these topics" />
        <Kpi label="Total lag" value={fmtCount(lag)} hint={lag ? 'messages not yet read' : 'every group is caught up'} tone={lag > 10_000 ? 'amber' : undefined} />
      </SimpleGrid>
      <Card withBorder radius="md" padding="md">
        <Text fw={600} size={FS.body} mb="xs">Needs attention</Text>
        {needs.length === 0
          ? <Text size={FS.meta} c="dimmed">Every partition has a leader and its replicas, and no group has stopped with messages waiting.</Text>
          : (
            <Stack gap={6}>
              {needs.map((n, i) => (
                <Group key={i} gap="xs" wrap="nowrap" align="flex-start">
                  <IconAlertTriangle size={16} color={`var(--mantine-color-${n.level}-6)`} style={{ flexShrink: 0, marginTop: 2 }} />
                  <Text size={FS.meta} style={{ flex: 1 }}>{n.text}</Text>
                  {n.topic && <Button size="compact-xs" variant="subtle" onClick={() => onTopic(n.topic!)}>Open</Button>}
                  {n.group && <Button size="compact-xs" variant="subtle" onClick={onGroups}>Groups</Button>}
                </Group>
              ))}
            </Stack>
          )}
      </Card>
      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
        <Card withBorder radius="md" padding="md">
          <Text fw={600} size={FS.body} mb="xs">Largest topics</Text>
          {biggest.length === 0 ? <Text size={FS.meta} c="dimmed">No topics in this scope yet.</Text> : (
            <Table fz="sm" verticalSpacing={6} highlightOnHover>
              <Table.Tbody>
                {biggest.map((t) => (
                  <Table.Tr key={t.name} onClick={() => onTopic(t.name)} style={{ cursor: 'pointer' }}>
                    <Table.Td><Text ff="monospace" size={FS.meta} truncate>{t.name}</Text></Table.Td>
                    <Table.Td ta="right"><Text size={FS.meta} c="dimmed">{t.partitions} part.</Text></Table.Td>
                    <Table.Td ta="right"><Text size={FS.meta} fw={600}>{fmtCount(t.messages)}</Text></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
        </Card>
        <Card withBorder radius="md" padding="md">
          <Text fw={600} size={FS.body} mb="xs">Most behind</Text>
          {lagging.length === 0 ? <Text size={FS.meta} c="dimmed">No group has messages waiting.</Text> : (
            <Table fz="sm" verticalSpacing={6}>
              <Table.Tbody>
                {lagging.map((g) => (
                  <Table.Tr key={g.group}>
                    <Table.Td><Text ff="monospace" size={FS.meta} truncate>{g.group}</Text></Table.Td>
                    <Table.Td><GroupState g={g} /></Table.Td>
                    <Table.Td ta="right"><Text size={FS.meta} fw={600}>{fmtCount(totalLag(g))}</Text></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
        </Card>
      </SimpleGrid>
      {brokers.length > 0 && (
        <Text size={FS.micro} c="dimmed">
          Brokers: {brokers.map((b) => `${b.id} ${b.host}:${b.port}${b.rack ? ` (${b.rack})` : ''}${b.controller ? ' · controller' : ''}`).join(' · ')}
        </Text>
      )}
    </Stack>
  )
}

function Health({ t }: { t: KafkaTopic }) {
  if (t.error) return <Badge size="sm" variant="light" color="rose" radius="sm">{t.error.split(':')[0]}</Badge>
  if (t.offline) return <Badge size="sm" variant="light" color="rose" radius="sm">{t.offline} offline</Badge>
  if (t.underReplicated) return <Badge size="sm" variant="light" color="amber" radius="sm">{t.underReplicated} under-replicated</Badge>
  return <Badge size="sm" variant="light" color="teal" radius="sm">Healthy</Badge>
}

function Topics({ topics, narrow, onOpen, canCreate, onCreate }: {
  topics: KafkaTopic[]; narrow: boolean; onOpen: (t: string) => void; canCreate: boolean; onCreate: () => void
}) {
  const [q, setQ] = useState('')
  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return topics.filter((t) => !needle || t.name.toLowerCase().includes(needle))
      .sort((a, b) => Number(Boolean(b.offline || b.underReplicated)) - Number(Boolean(a.offline || a.underReplicated)) || a.name.localeCompare(b.name))
  }, [topics, q])
  return (
    <Stack gap="sm">
      <Group gap="xs">
        <TextInput size="xs" placeholder="Filter by name" leftSection={<IconSearch size={12} />} value={q}
          onChange={(e) => setQ(e.currentTarget.value)} aria-label="Filter topics by name" w={narrow ? '100%' : 240} />
        {canCreate && <Button size="xs" ml={narrow ? undefined : 'auto'} leftSection={<IconPlus size={14} />} onClick={onCreate}>New topic</Button>}
      </Group>
      <Card withBorder radius="md" padding={0}>
        <Table.ScrollContainer minWidth={620}>
          <Table verticalSpacing={8} fz="sm" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Topic</Table.Th><Table.Th>Health</Table.Th>
                <Table.Th ta="right">Partitions</Table.Th><Table.Th ta="right">Replicas</Table.Th><Table.Th ta="right">Messages</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((t) => (
                <Table.Tr key={t.name} onClick={() => onOpen(t.name)} style={{ cursor: 'pointer' }} title={t.name}>
                  <Table.Td><Text span ff="monospace" fw={500} size={FS.meta}>{t.name}</Text></Table.Td>
                  <Table.Td><Health t={t} /></Table.Td>
                  <Table.Td ta="right">{t.partitions}</Table.Td>
                  <Table.Td ta="right">{t.replication}</Table.Td>
                  <Table.Td ta="right">{fmtCount(t.messages)}</Table.Td>
                </Table.Tr>
              ))}
              {rows.length === 0 && (
                <Table.Tr><Table.Td colSpan={5}><Text size={FS.meta} c="dimmed" p="sm">{topics.length ? 'No topic matches.' : 'No topics in this scope.'}</Text></Table.Td></Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Card>
      <Text size={FS.micro} c="dimmed">Internal topics are left out. Messages is what the logs hold now, after retention; refreshed every 10 seconds.</Text>
    </Stack>
  )
}

function GroupState({ g }: { g: KafkaGroup }) {
  const color = g.state === 'Stable' ? 'teal' : g.state === 'Empty' || g.state === 'Dead' ? (totalLag(g) > 0 ? 'amber' : 'slate') : 'sky'
  return <Badge size="sm" variant="light" color={color} radius="sm" style={{ textTransform: 'none' }}>{g.state || 'unknown'}</Badge>
}

function Groups({ groups, onTopic }: { groups: KafkaGroup[]; onTopic: (t: string) => void }) {
  const rows = groups.slice().sort((a, b) => totalLag(b) - totalLag(a) || a.group.localeCompare(b.group))
  return (
    <Stack gap="sm">
      <Card withBorder radius="md" padding={0}>
        <Table.ScrollContainer minWidth={620}>
          <Table verticalSpacing={8} fz="sm">
            <Table.Thead>
              <Table.Tr><Table.Th>Group</Table.Th><Table.Th>State</Table.Th><Table.Th ta="right">Members</Table.Th><Table.Th>Lag by topic</Table.Th><Table.Th ta="right">Total lag</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((g) => (
                <Table.Tr key={g.group}>
                  <Table.Td><Text ff="monospace" fw={500} size={FS.meta}>{g.group}</Text>{g.error && <Text size={FS.micro} c="rose">{g.error}</Text>}</Table.Td>
                  <Table.Td><GroupState g={g} /></Table.Td>
                  <Table.Td ta="right">{g.members}</Table.Td>
                  <Table.Td>
                    <Group gap={6} wrap="wrap">
                      {Object.entries(g.lag ?? {}).sort((a, b) => b[1] - a[1]).map(([t, n]) => (
                        <Badge key={t} size="sm" variant="default" radius="sm" style={{ textTransform: 'none', cursor: 'pointer' }} onClick={() => onTopic(t)}>
                          <Text span ff="monospace" size={FS.micro}>{t}</Text> {fmtCount(n)}
                        </Badge>
                      ))}
                    </Group>
                  </Table.Td>
                  <Table.Td ta="right" fw={totalLag(g) > 0 ? 600 : undefined}>{fmtCount(totalLag(g))}</Table.Td>
                </Table.Tr>
              ))}
              {rows.length === 0 && (
                <Table.Tr><Table.Td colSpan={5}><Text size={FS.meta} c="dimmed" p="sm">No consumer group reads these topics.</Text></Table.Td></Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Card>
      <Text size={FS.micro} c="dimmed">Lag is the end of each partition less the offset the group last committed. Groups that read none of these topics are left out.</Text>
    </Stack>
  )
}

function TopicDetail({ name, ask, act, can, hostname, narrow, onBack, onPublish }: {
  name: string; ask: (r: Req) => Promise<StoreAnswer>; act: (r: Req, ok: string) => Promise<StoreAnswer>; can: (op: string) => boolean
  hostname: string; narrow: boolean; onBack: () => void; onPublish: () => void
}) {
  const [detail, setDetail] = useState<KafkaTopicDetail | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [msgs, setMsgs] = useState<KafkaMessage[] | null>(null)
  const [count, setCount] = useState<number | string>(20)
  const [partition, setPartition] = useState<number | string>('')
  const [peeking, setPeeking] = useState(false)
  const [deleting, setDeleting] = useState(false)

  const load = useCallback(async () => {
    const a = await ask({ op: 'topic', name })
    if (a.type === 'result') { setDetail(a.data as KafkaTopicDetail); setErr(null) } else setErr(a.error ?? 'could not read the topic')
  }, [ask, name])
  useEffect(() => { void load() }, [load])

  const peek = async () => {
    setPeeking(true)
    const a = await ask({ op: 'peek', name, count: Number(count) || 20, ...(partition !== '' ? { properties: { partition: String(partition) } } : {}) })
    setPeeking(false)
    if (a.type === 'result') setMsgs(items<KafkaMessage>(a.data))
    else notifyError('Could not read messages', new Error(a.error ?? 'failed'))
  }

  const parts = detail?.partitions ?? []
  const total = parts.reduce((n, p) => n + Math.max(0, p.end - Math.max(0, p.start)), 0)
  return (
    <Stack gap="md">
      <Group gap="xs" wrap="wrap">
        <ActionIcon variant="subtle" color="slate" onClick={onBack} aria-label="Back to topics"><IconArrowLeft size={16} /></ActionIcon>
        <Text ff="monospace" fw={650} size={FS.body}>{name}</Text>
        <Text size={FS.micro} c="dimmed">{parts.length} partitions · {fmtCount(total)} messages kept</Text>
        <Group gap={6} ml="auto">
          <Button size="xs" variant="default" leftSection={<IconRefresh size={12} />} onClick={() => void load()}>Refresh</Button>
          {can('publish') && <Button size="xs" variant="light" leftSection={<IconSend size={12} />} onClick={onPublish}>Publish here</Button>}
          {can('delete_topic') && <Button size="xs" variant="subtle" color="rose" leftSection={<IconTrash size={12} />} onClick={() => setDeleting(true)}>Delete topic</Button>}
        </Group>
      </Group>
      {err && <Alert color="rose" variant="light">{err}</Alert>}

      <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md">
        <Card withBorder radius="md" padding={0}>
          <Text fw={600} size={FS.body} p="sm" pb={0}>Partitions</Text>
          <Table.ScrollContainer minWidth={340}>
            <Table verticalSpacing={6} fz="sm">
              <Table.Thead><Table.Tr><Table.Th>#</Table.Th><Table.Th>Leader</Table.Th><Table.Th>In sync</Table.Th><Table.Th ta="right">Start</Table.Th><Table.Th ta="right">End</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {parts.map((p) => (
                  <Table.Tr key={p.partition}>
                    <Table.Td>{p.partition}</Table.Td>
                    <Table.Td c={p.leader < 0 ? 'rose' : undefined}>{p.leader < 0 ? 'none' : p.leader}</Table.Td>
                    <Table.Td c={p.isr.length < p.replicas.length ? 'amber' : undefined}>{p.isr.length} of {p.replicas.length}</Table.Td>
                    <Table.Td ta="right" ff="monospace">{p.start < 0 ? '–' : p.start.toLocaleString()}</Table.Td>
                    <Table.Td ta="right" ff="monospace">{p.end < 0 ? '–' : p.end.toLocaleString()}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Card>
        <Stack gap="md">
          <Card withBorder radius="md" padding="sm">
            <Text fw={600} size={FS.body} mb={6}>Consumer groups</Text>
            {(detail?.groups ?? []).length === 0 ? <Text size={FS.meta} c="dimmed">No group reads this topic.</Text> : (
              <Stack gap={4}>
                {(detail?.groups ?? []).map((g) => (
                  <Group key={g.group} gap="xs" wrap="nowrap">
                    <Text ff="monospace" size={FS.meta} truncate style={{ flex: 1 }}>{g.group}</Text>
                    <GroupState g={g} />
                    <Text size={FS.meta} fw={600} w={70} ta="right">{fmtCount(g.lag?.[name] ?? 0)}</Text>
                  </Group>
                ))}
              </Stack>
            )}
          </Card>
          <Card withBorder radius="md" padding="sm">
            <Text fw={600} size={FS.body} mb={6}>Settings changed from the cluster's defaults</Text>
            {Object.keys(detail?.configs ?? {}).length === 0 ? <Text size={FS.meta} c="dimmed">None: this topic uses the broker defaults.</Text> : (
              <Stack gap={2}>
                {Object.entries(detail?.configs ?? {}).sort().map(([k, v]) => (
                  <Text key={k} size={FS.meta} ff="monospace"><Text span c="dimmed" ff="monospace" size={FS.meta}>{k}</Text> = {v}</Text>
                ))}
              </Stack>
            )}
          </Card>
        </Stack>
      </SimpleGrid>

      <Card withBorder radius="md" padding="sm">
        <Group gap="xs" align="flex-end" wrap="wrap">
          <Box style={{ flex: 1, minWidth: 200 }}>
            <Text fw={600} size={FS.body}>Newest messages</Text>
            <Text size={FS.micro} c="dimmed">Read without a consumer group, so nothing is committed and no reader is moved. Recorded.</Text>
          </Box>
          <NumberInput size="xs" label="How many" value={count} onChange={setCount} min={1} max={200} w={100} />
          <NumberInput size="xs" label="Partition" value={partition} onChange={setPartition} min={0} max={Math.max(0, parts.length - 1)} placeholder="all" w={100} />
          <Button size="xs" onClick={() => void peek()} loading={peeking}>Read</Button>
        </Group>
        {msgs && (
          <Stack gap={8} mt="sm">
            {msgs.length === 0 && <Text size={FS.meta} c="dimmed">No messages.</Text>}
            {msgs.map((m) => <Message key={`${m.partition}:${m.offset}`} m={m} narrow={narrow} />)}
          </Stack>
        )}
      </Card>

      <ConfirmByName opened={deleting} hostname={hostname} what={`Delete topic ${name} and every message in it, on every broker.`}
        onCancel={() => setDeleting(false)}
        onConfirm={(typed) => {
          setDeleting(false)
          void act({ op: 'delete_topic', name, confirm: typed }, `Deleted topic ${name}.`).then((a) => { if (a.type === 'result') onBack() })
        }} />
    </Stack>
  )
}

function Message({ m, narrow }: { m: KafkaMessage; narrow: boolean }) {
  const v = showValue(m)
  return (
    <Box style={{ border: '1px solid var(--color-line)', borderRadius: 8 }} p="xs">
      <Group gap="xs" wrap="wrap" mb={4}>
        <Badge size="xs" variant="light" color="slate" radius="sm" style={{ textTransform: 'none' }}>p{m.partition} · {m.offset}</Badge>
        {m.key !== undefined && m.key !== '' && <Text size={FS.micro} ff="monospace">key <b>{m.keyB64 ? `base64:${m.key}` : m.key}</b></Text>}
        {Object.entries(m.headers ?? {}).map(([k, h]) => (
          <Text key={k} size={FS.micro} ff="monospace" c="dimmed">{k}={h}</Text>
        ))}
        <Text size={FS.micro} c="dimmed" ml={narrow ? undefined : 'auto'}>{new Date(m.time).toLocaleString()}</Text>
      </Group>
      <Code block style={{ maxHeight: 260, overflow: 'auto', fontSize: 12 }}>{v.text}</Code>
      {(v.kind === 'base64' || m.cut) && (
        <Text size={FS.micro} c="dimmed" mt={2}>{v.kind === 'base64' ? 'Not text, shown as base64. ' : ''}{m.cut ? 'Cut at 64 KB.' : ''}</Text>
      )}
    </Box>
  )
}

function PublishDrawer({ topics, initial, narrow, ask, onClose, onDone }: {
  topics: KafkaTopic[]; initial: string; narrow: boolean; ask: (r: Req) => Promise<StoreAnswer>; onClose: () => void; onDone: () => void
}) {
  const [topic, setTopic] = useState(initial || topics[0]?.name || '')
  const [key, setKey] = useState('')
  const [payload, setPayload] = useState('')
  const [headers, setHeaders] = useState<{ k: string; v: string }[]>([])
  const [sending, setSending] = useState(false)
  const json = useMemo(() => {
    const t = payload.trim()
    if (!t || !(t.startsWith('{') || t.startsWith('['))) return null
    try { JSON.parse(t); return true } catch { return false }
  }, [payload])
  const known = topics.some((t) => t.name === topic)
  const send = async () => {
    setSending(true)
    const hdrs = Object.fromEntries(headers.filter((h) => h.k.trim()).map((h) => [h.k.trim(), h.v]))
    const a = await ask({ op: 'publish', name: topic, routingKey: key, payload, headers: hdrs })
    setSending(false)
    if (a.type !== 'result') { notifyError('Not published', new Error(a.error ?? 'failed')); return }
    const at = a.data as { partition?: number; offset?: number } | undefined
    notifyOk('Published', `Written to ${topic} partition ${at?.partition ?? '?'} at offset ${at?.offset ?? '?'}. It is in the session’s recording.`)
    onDone()
    onClose()
  }
  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 520} title={<Text fw={650}>Publish a message</Text>}>
      <Stack gap="sm">
        <TextInput label="Topic" value={topic} onChange={(e) => setTopic(e.currentTarget.value)} styles={MONO} list="argus-kafka-topics"
          description={topic && !known ? 'Not a topic in this scope. Kafka refuses it unless it creates topics on first write and your level allows it.' : undefined} />
        <datalist id="argus-kafka-topics">{topics.map((t) => <option key={t.name} value={t.name} />)}</datalist>
        <TextInput label="Key" value={key} onChange={(e) => setKey(e.currentTarget.value)} styles={MONO} placeholder="optional"
          description="Messages with the same key go to the same partition, in order." />
        <Box>
          <Group gap="xs" mb={4}>
            <Text size={FS.meta} fw={500}>Value</Text>
            {json !== null && <Text size={FS.micro} c={json ? 'teal' : 'rose'} ml="auto">{json ? 'valid JSON' : 'not valid JSON'}</Text>}
          </Group>
          <Textarea value={payload} onChange={(e) => setPayload(e.currentTarget.value)} autosize minRows={6} maxRows={16}
            styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12.5 } }} placeholder='{"id": 1}' aria-label="Value" />
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
        <Button size="compact-xs" variant="subtle" leftSection={<IconPlus size={12} />} style={{ alignSelf: 'flex-start' }}
          onClick={() => setHeaders((hs) => [...hs, { k: '', v: '' }])} disabled={headers.length >= 64}>Add header</Button>
        <Group mt="md" gap="xs">
          <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>The key, value and headers are kept in the recording.</Text>
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void send()} loading={sending} disabled={!topic}>Publish</Button>
        </Group>
      </Stack>
    </Drawer>
  )
}

function NewTopic({ opened, scope, brokers, onClose, onCreate }: {
  opened: boolean; scope: string; brokers: number; onClose: () => void
  onCreate: (name: string, partitions: number, properties: Record<string, string>) => Promise<void>
}) {
  const prefix = scope === '*' ? '' : scope
  const [name, setName] = useState(prefix)
  const [partitions, setPartitions] = useState<number | string>(3)
  const [replicas, setReplicas] = useState<number | string>('')
  const [keepDays, setKeepDays] = useState<number | string>('')
  const [cleanup, setCleanup] = useState<'' | 'delete' | 'compact'>('')
  useEffect(() => {
    if (opened) { setName(prefix); setPartitions(3); setReplicas(''); setKeepDays(''); setCleanup('') }
  }, [opened, prefix])
  const ok = /^[A-Za-z0-9._-]{1,249}$/.test(name) && name.startsWith(prefix) && name !== prefix
    && (replicas === '' || brokers === 0 || Number(replicas) <= brokers)
  const properties: Record<string, string> = {
    ...(Number(replicas) > 0 ? { replication: String(Number(replicas)) } : {}),
    ...(keepDays !== '' ? { retention: String(Number(keepDays) < 0 ? -1 : Math.round(Number(keepDays) * 86_400_000)) } : {}),
    ...(cleanup ? { cleanup } : {}),
  }
  return (
    <Modal opened={opened} onClose={onClose} title="New topic" centered size="lg">
      <Stack gap="sm">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} data-autofocus styles={MONO}
          description={prefix ? `In this entry's scope, so it starts with ${prefix}` : 'Letters, digits, . _ and -'} />
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <NumberInput label="Partitions" value={partitions} onChange={setPartitions} min={1} max={1000}
            description="Can be added later, never removed." />
          <NumberInput label="Replicas" value={replicas} onChange={setReplicas} min={1} max={brokers || 16} placeholder="Cluster default"
            description={brokers ? `Copies of each partition, of ${brokers} broker${brokers === 1 ? '' : 's'}.` : 'Copies of each partition.'} />
          <NumberInput label="Keep messages for (days)" value={keepDays} onChange={setKeepDays} min={-1} decimalScale={3}
            placeholder="Broker default" description="-1 keeps them for ever." />
          <Stack gap={4}>
            <Text size={FS.meta} fw={500}>Clean-up</Text>
            <SegmentedControl value={cleanup} onChange={(v) => setCleanup(v as typeof cleanup)} fullWidth
              data={[{ label: 'Default', value: '' }, { label: 'Delete old', value: 'delete' }, { label: 'Compact', value: 'compact' }]} />
            <Text size={FS.micro} c="dimmed">Compact keeps the latest message for each key.</Text>
          </Stack>
        </SimpleGrid>
        <Text size={FS.micro} c="dimmed">Creating it is recorded with these settings.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!ok} onClick={() => void onCreate(name, Number(partitions) || 1, properties)}>Create</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
