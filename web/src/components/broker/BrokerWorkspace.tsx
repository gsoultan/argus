import { Badge, Box, Button, Group, Tabs, Text } from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { Link } from '@tanstack/react-router'
import { IconLock, IconSend, IconStack2 } from '@tabler/icons-react'
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { ButtonLink } from '~/components/links'
import { ConfirmByName, FatalBanner, LABEL_COLOR } from '~/components/StoreChrome'
import { useStoreSession, type StoreAnswer } from '~/components/useStoreSession'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  attention, deadLetterQueues, items, type Binding, type Exchange, type Node, type Queue, type VhostStats,
} from '~/lib/rabbit'
import { FS, INK } from '~/theme'
import { levelName } from '~/lib/engines'
import type { DBAccess, DBLevel, Database } from '~/types/domain'
import './broker.css'
import { Connections } from './Connections'
import { Overview } from './Overview'
import { PublishDrawer, type PublishPreset } from './Publish'
import { QueueDetail } from './QueueDetail'
import { Queues } from './Queues'
import { Routing } from './Routing'

/*
One RabbitMQ vhost as a broker, not as a database: an overview of whether it
keeps up, its queues with their rates, where each exchange sends messages,
and who is connected. What the person may do comes from the agent's answer
(`ops`), the fixed list for their level.

Browsing -- lists, rates, bindings -- is not recorded and is refreshed every
five seconds while the page is visible. Peeking, publishing, moving and
anything that changes the broker is recorded by the agent before it is sent.
*/

export type BrokerTab = 'overview' | 'queues' | 'routing' | 'connections'

export type Ask = (req: Record<string, unknown> & { op: string }) => Promise<StoreAnswer>

export type Confirm = { op: string; req: Record<string, unknown>; what: string; then?: () => void }

interface BrokerCtx {
  database: Database
  ask: Ask
  can: (op: string) => boolean
  narrow: boolean
  queues: Queue[]
  exchanges: Exchange[]
  bindings: Binding[]
  dlq: Map<string, string>
  vhost: VhostStats | null
  nodes: Node[] | null
  loaded: boolean
  /** Refreshes the lists; full also fetches exchanges and bindings now. */
  reload: (full?: boolean) => Promise<void>
  openQueue: (name: string | null) => void
  goTab: (t: BrokerTab, extra?: { exchange?: string }) => void
  confirm: (c: Confirm) => void
  publish: (p?: PublishPreset) => void
  /** Runs one recorded operation and says how it went. */
  act: (req: Record<string, unknown> & { op: string }, ok: string) => Promise<StoreAnswer>
}

const Ctx = createContext<BrokerCtx | null>(null)

export function useBroker(): BrokerCtx {
  const c = useContext(Ctx)
  if (!c) throw new Error('useBroker outside BrokerWorkspace')
  return c
}

export interface BrokerWorkspaceProps {
  database: Database
  access: DBAccess
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>
  endpoint: string
  onAskForAccess?: () => void
  tab: BrokerTab
  queue?: string
  exchange?: string
  onNavigate: (s: { tab: BrokerTab; queue?: string; exchange?: string }) => void
  settingsLink?: boolean
}

const POLL_MS = 5000
/**
 * Exchanges and bindings change when someone declares or binds, not from
 * second to second, and the vhost's whole binding list is the largest thing
 * the workspace asks for. They are refreshed this often, and at once after an
 * action or on opening Exchanges & routing; queues and rates every POLL_MS.
 * Every request waits its turn on the agent, so the five-second poll is kept
 * to what moves.
 */
const TOPOLOGY_MS = 30_000

export function BrokerWorkspace(props: BrokerWorkspaceProps) {
  const { database, access, getTicket, endpoint, onAskForAccess, tab, queue, exchange, onNavigate } = props
  const narrow = useMediaQuery('(max-width: 48em)') ?? false
  const s = useStoreSession(database, getTicket, endpoint)
  const level = s.ready?.level ?? access.level
  const ops = s.ready?.ops
  const can = useCallback((op: string) => ops?.includes(op) ?? false, [ops])

  const [queues, setQueues] = useState<Queue[]>([])
  const [exchanges, setExchanges] = useState<Exchange[]>([])
  const [bindings, setBindings] = useState<Binding[]>([])
  const [vhost, setVhost] = useState<VhostStats | null>(null)
  const [nodes, setNodes] = useState<Node[] | null>(null)
  const [loaded, setLoaded] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [confirming, setConfirming] = useState<Confirm | null>(null)
  const [publishing, setPublishing] = useState<PublishPreset | null>(null)
  const busy = useRef(false)
  // When exchanges and bindings were last fetched; 0 is never, or stale.
  const topologyAt = useRef(0)
  const again = useRef(false)

  const reload = useCallback(async (full = false) => {
    if (full) topologyAt.current = 0
    if (busy.current) {
      // Asked for while a refresh is out, after an action say: run once more
      // when it is back, so what the action changed is shown.
      again.current = true
      return
    }
    busy.current = true
    try {
      const topology = Date.now() - topologyAt.current >= TOPOLOGY_MS
      const none: StoreAnswer = { id: 0, type: 'result' }
      const [q, e, b, o] = await Promise.all([
        s.ask({ op: 'queues', count: 500 }),
        topology ? s.ask({ op: 'exchanges', count: 500 }) : Promise.resolve(none),
        topology ? s.ask({ op: 'bindings' }) : Promise.resolve(none),
        s.ask({ op: 'overview' }),
      ])
      if (q.type !== 'result') { setErr(q.error ?? 'could not list queues'); return }
      setErr(null)
      setQueues(items<Queue>(q.data))
      if (topology && e.type === 'result' && b.type === 'result') topologyAt.current = Date.now()
      if (topology && e.type === 'result') setExchanges(items<Exchange>(e.data))
      if (topology && b.type === 'result') setBindings(items<Binding>(b.data))
      if (o.type === 'result') {
        const d = o.data as { vhost?: VhostStats; nodes?: Node[] | null }
        setVhost(d.vhost ?? null)
        setNodes(Array.isArray(d.nodes) ? d.nodes : null)
      }
      setLoaded(true)
    } finally {
      busy.current = false
      if (again.current) {
        again.current = false
        void reload()
      }
    }
  }, [s])

  useEffect(() => {
    if (s.status !== 'ready' || !s.ready) return
    void reload()
    const t = window.setInterval(() => { if (document.visibilityState === 'visible') void reload() }, POLL_MS)
    return () => window.clearInterval(t)
  }, [s.status, s.ready, reload])

  // Exchanges & routing shows the topology, so it is fetched fresh there.
  useEffect(() => {
    if (tab === 'routing' && s.status === 'ready' && s.ready) void reload(true)
  }, [tab, s.status, s.ready, reload])

  const dlq = useMemo(() => deadLetterQueues(queues, bindings), [queues, bindings])
  const needs = useMemo(() => attention(queues, bindings, vhost, nodes), [queues, bindings, vhost, nodes])

  const act = useCallback(async (req: Record<string, unknown> & { op: string }, ok: string) => {
    const a = await s.ask(req)
    if (a.type === 'result') {
      notifyOk('Done', `${ok} It is in the session's recording.`)
      void reload(true)
    } else if (!a.needsConfirm) {
      notifyError('Not done', new Error(a.error ?? 'failed'))
    }
    return a
  }, [s, reload])

  const ctx: BrokerCtx = {
    database, ask: s.ask, can, narrow, queues, exchanges, bindings, dlq, vhost, nodes, loaded, reload, act,
    openQueue: (name) => onNavigate({ tab: 'queues', queue: name ?? undefined }),
    goTab: (t, extra) => onNavigate({ tab: t, ...extra }),
    confirm: setConfirming,
    publish: (p) => setPublishing(p ?? {}),
  }

  const doConfirmed = async (typed: string) => {
    const c = confirming
    setConfirming(null)
    if (!c) return
    const a = await act({ op: c.op, ...c.req, confirm: typed }, c.what)
    if (a.type === 'result') c.then?.()
  }

  const version = s.ready?.version
  return (
    <Ctx.Provider value={ctx}>
      <Box>
        <Box px={{ base: 'md', lg: 'xl' }} pt="md" style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--mantine-color-body)' }}>
          <Text size={FS.micro} c="dimmed" mb={4}>
            <Link to="/brokers" style={{ color: INK.brand }}>Brokers</Link> / {database.dbname}
          </Text>
          <Group gap="sm" wrap="wrap" align="center">
            <IconStack2 size={20} color="var(--mantine-color-amber-6)" aria-hidden />
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
                <ButtonLink size="xs" variant="subtle" to="/brokers/$brokerId/settings" params={{ brokerId: database.id }}>
                  Who can connect
                </ButtonLink>
              )}
              {can('publish') && (
                <Button size="xs" leftSection={<IconSend size={14} />} onClick={() => setPublishing({})}>
                  {narrow ? 'Publish' : 'Publish message'}
                </Button>
              )}
            </Group>
          </Group>
          <Text size={FS.micro} c="dimmed" mt={4}>
            {version ? `RabbitMQ ${version} · ` : ''}vhost <Text span ff="monospace" size={FS.micro}>{database.dbname}</Text> on {database.hostname}
            {s.ready?.user ? <> · signed in as <Text span ff="monospace" size={FS.micro}>{s.ready.user}</Text></> : null}
          </Text>
          <Tabs value={tab} onChange={(v) => v && onNavigate({ tab: v as BrokerTab })} mt="sm" variant="default"
            styles={{ list: { borderBottom: 0, flexWrap: 'nowrap', overflowX: 'auto' }, tab: { whiteSpace: 'nowrap' } }}>
            <Tabs.List>
              <Tabs.Tab value="overview">Overview</Tabs.Tab>
              <Tabs.Tab value="queues" rightSection={<Badge size="xs" variant="light" color={needs.some((n) => n.queue) ? 'amber' : 'slate'}>{queues.length}</Badge>}>Queues</Tabs.Tab>
              <Tabs.Tab value="routing" rightSection={<Badge size="xs" variant="light" color="slate">{exchanges.filter((x) => x.name).length}</Badge>}>
                {narrow ? 'Routing' : 'Exchanges & routing'}
              </Tabs.Tab>
              {can('connections') && <Tabs.Tab value="connections">Connections</Tabs.Tab>}
            </Tabs.List>
          </Tabs>
        </Box>
        <FatalBanner fatal={s.fatal} />
        <Box px={{ base: 'sm', sm: 'md', lg: 'xl' }} py="md">
          {err && <Text size={FS.meta} c="rose" mb="sm">{err}</Text>}
          {tab === 'overview' && <Overview needs={needs} />}
          {tab === 'queues' && (queue ? <QueueDetail key={queue} name={queue} /> : <Queues />)}
          {tab === 'routing' && <Routing selected={exchange} onSelect={(x) => onNavigate({ tab: 'routing', exchange: x })} />}
          {tab === 'connections' && can('connections') && <Connections />}
        </Box>
        <ConfirmByName opened={confirming !== null} hostname={database.hostname} what={confirming?.what ?? ''}
          onCancel={() => setConfirming(null)} onConfirm={(t) => void doConfirmed(t)} />
        {publishing && <PublishDrawer preset={publishing} onClose={() => setPublishing(null)} />}
      </Box>
    </Ctx.Provider>
  )
}

/** Monospace for what is typed into an input, not its label: names, keys, ids. */
export const MONO = { input: { fontFamily: 'var(--font-mono)' } }
