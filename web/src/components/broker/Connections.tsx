import {
  Alert, Badge, Box, Button, Card, Group, Modal, SegmentedControl, Stack, Table, Text, TextInput, Textarea, UnstyledButton,
} from '@mantine/core'
import { IconChevronDown, IconChevronRight } from '@tabler/icons-react'
import { Fragment, useCallback, useEffect, useMemo, useState } from 'react'
import { fmtRate, items, rate, type Channel, type Connection, type Consumer } from '~/lib/rabbit'
import { FS } from '~/theme'
import { useBroker } from './BrokerWorkspace'

/*
Who is connected: applications by the name they gave their connection and
the library they use, not sockets. A connection opens to show its channels
and what each is doing. Closing one asks for a reason, which RabbitMQ sends
to the client, and the server's name, as every action that cuts someone off.
*/

type View = 'connections' | 'channels' | 'consumers'

const appName = (c: Connection) => c.client_properties?.connection_name || c.name
const client = (c: Connection) => [c.client_properties?.product, c.client_properties?.version].filter(Boolean).join(' ') +
  (c.client_properties?.platform ? ` · ${c.client_properties.platform}` : '')

function StateBadge({ state }: { state?: string }) {
  if (state === 'blocked' || state === 'blocking') return <Badge size="sm" variant="light" color="amber" radius="sm">Blocked by an alarm</Badge>
  if (state === 'running') return <Badge size="sm" variant="light" color="teal" radius="sm">Running</Badge>
  if (state === 'flow') return <Badge size="sm" variant="light" color="amber" radius="sm">Flow control</Badge>
  return <Badge size="sm" variant="light" color="slate" radius="sm">{state ?? '—'}</Badge>
}

export function Connections() {
  const { ask, can, narrow, act, database } = useBroker()
  const [view, setView] = useState<View>('connections')
  const [conns, setConns] = useState<Connection[]>([])
  const [chans, setChans] = useState<Channel[]>([])
  const [consumers, setConsumers] = useState<Consumer[]>([])
  const [err, setErr] = useState<string | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  const [filter, setFilter] = useState('')
  const [closing, setClosing] = useState<Connection | null>(null)

  const load = useCallback(async () => {
    const [c, ch, co] = await Promise.all([
      ask({ op: 'connections' }),
      can('channels') ? ask({ op: 'channels' }) : Promise.resolve(null),
      ask({ op: 'consumers' }),
    ])
    if (c.type !== 'result') { setErr(c.error ?? 'could not list connections'); return }
    setErr(null)
    setConns(items<Connection>(c.data))
    if (ch?.type === 'result') setChans(items<Channel>(ch.data))
    if (co.type === 'result') setConsumers(items<Consumer>(co.data))
  }, [ask, can])
  useEffect(() => {
    void load()
    const t = window.setInterval(() => { if (document.visibilityState === 'visible') void load() }, 5000)
    return () => window.clearInterval(t)
  }, [load])

  const byConn = useMemo(() => {
    const m = new Map<string, Channel[]>()
    for (const ch of chans) {
      const k = ch.connection_details?.name ?? ''
      m.set(k, [...(m.get(k) ?? []), ch])
    }
    return m
  }, [chans])
  const consumersByChannel = useMemo(() => {
    const m = new Map<string, Consumer[]>()
    for (const c of consumers) {
      const k = c.channel_details?.name ?? ''
      m.set(k, [...(m.get(k) ?? []), c])
    }
    return m
  }, [consumers])
  const needle = filter.trim().toLowerCase()
  const rows = conns.filter((c) => !needle || [appName(c), c.user, c.peer_host, client(c)].some((s) => s?.toLowerCase().includes(needle)))
  const blocked = conns.filter((c) => c.state === 'blocked' || c.state === 'blocking')
  const sum = (cs: Channel[] | undefined, f: (c: Channel) => number) => (cs ?? []).reduce((n, c) => n + f(c), 0)

  return (
    <Stack gap="sm">
      <Group gap="xs" wrap="wrap">
        <SegmentedControl size="xs" value={view} onChange={(v) => setView(v as View)}
          data={[{ label: `Connections ${conns.length}`, value: 'connections' }, ...(can('channels') ? [{ label: `Channels ${chans.length}`, value: 'channels' }] : []),
            { label: `Consumers ${consumers.length}`, value: 'consumers' }]} />
        <TextInput size="xs" ml={narrow ? undefined : 'auto'} w={narrow ? '100%' : 260} placeholder="Filter by app, user or address"
          value={filter} onChange={(e) => setFilter(e.currentTarget.value)} aria-label="Filter connections" />
      </Group>
      {err && <Alert color="rose" variant="light">{err}</Alert>}
      {blocked.length > 0 && (
        <Alert color="amber" variant="light">
          <b>{blocked.map(appName).join(', ')}</b> {blocked.length === 1 ? 'is' : 'are'} blocked: RabbitMQ stopped reading from
          {blocked.length === 1 ? ' it' : ' them'} because a memory or disk alarm is on. Publishes wait until the alarm clears; closing the
          connection does not help. See the node on Overview.
        </Alert>
      )}

      {view === 'connections' && (
        <Card withBorder radius="md" padding={0}>
          <Table.ScrollContainer minWidth={820}>
            <Table verticalSpacing={8} fz="sm">
              <Table.Thead><Table.Tr>
                <Table.Th>Application</Table.Th><Table.Th>User</Table.Th><Table.Th>From</Table.Th><Table.Th>State</Table.Th>
                <Table.Th ta="right">Channels</Table.Th><Table.Th ta="right">Publishing</Table.Th><Table.Th ta="right">Receiving</Table.Th><Table.Th />
              </Table.Tr></Table.Thead>
              <Table.Tbody>
                {rows.map((c) => {
                  const cs = byConn.get(c.name)
                  const isOpen = open === c.name
                  return (
                    <Fragment key={c.name}>
                      <Table.Tr style={{ background: isOpen ? 'var(--mantine-color-azure-light)' : undefined }}>
                        <Table.Td>
                          <UnstyledButton onClick={() => setOpen(isOpen ? null : c.name)} aria-expanded={isOpen} disabled={!can('channels')}>
                            <Group gap={6} wrap="nowrap" align="flex-start">
                              {can('channels') && (isOpen ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />)}
                              <Box>
                                <Text ff="monospace" fw={500} size={FS.meta}>{appName(c)}</Text>
                                {client(c) && <Text size={FS.micro} c="dimmed">{client(c)}</Text>}
                              </Box>
                            </Group>
                          </UnstyledButton>
                        </Table.Td>
                        <Table.Td ff="monospace" fz={FS.meta}>{c.user}</Table.Td>
                        <Table.Td ff="monospace" fz={FS.meta}>{c.peer_host}</Table.Td>
                        <Table.Td><StateBadge state={c.state} /></Table.Td>
                        <Table.Td ta="right">{c.channels ?? 0}</Table.Td>
                        <Table.Td ta="right" ff="monospace">{cs ? fmtRate(sum(cs, (x) => rate(x.message_stats?.publish_details))) : '—'}</Table.Td>
                        <Table.Td ta="right" ff="monospace">{cs ? fmtRate(sum(cs, (x) => rate(x.message_stats?.deliver_get_details))) : '—'}</Table.Td>
                        <Table.Td ta="right">
                          {can('close_connection') && <Button size="compact-xs" variant="subtle" color="rose" onClick={() => setClosing(c)}>Close…</Button>}
                        </Table.Td>
                      </Table.Tr>
                      {isOpen && (
                        <Table.Tr>
                          <Table.Td colSpan={8} style={{ background: 'var(--mantine-color-default-hover)', paddingLeft: 32 }}>
                            <ChannelTable channels={cs ?? []} consumers={consumersByChannel} />
                          </Table.Td>
                        </Table.Tr>
                      )}
                    </Fragment>
                  )
                })}
                {rows.length === 0 && <Table.Tr><Table.Td colSpan={8}><Text size={FS.meta} c="dimmed" p="sm">{conns.length ? 'Nothing matches.' : 'Nobody is connected to this vhost.'}</Text></Table.Td></Table.Tr>}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Card>
      )}
      {view === 'channels' && <Card withBorder radius="md" padding="xs"><ChannelTable channels={chans} consumers={consumersByChannel} withConnection /></Card>}
      {view === 'consumers' && (
        <Card withBorder radius="md" padding={0}>
          <Table.ScrollContainer minWidth={620}>
            <Table verticalSpacing={6} fz="sm">
              <Table.Thead><Table.Tr><Table.Th>Queue</Table.Th><Table.Th>Application</Table.Th><Table.Th>Ack</Table.Th><Table.Th ta="right">Prefetch</Table.Th><Table.Th>Tag</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {consumers.filter((c) => !needle || [c.queue?.name, c.channel_details?.connection_name].some((s) => s?.toLowerCase().includes(needle))).map((c, i) => (
                  <Table.Tr key={i}>
                    <Table.Td ff="monospace">{c.queue?.name}</Table.Td>
                    <Table.Td ff="monospace" fz={FS.meta}>{c.channel_details?.connection_name ?? c.channel_details?.name}</Table.Td>
                    <Table.Td>{c.ack_required ? 'manual' : 'auto'}</Table.Td>
                    <Table.Td ta="right">{c.prefetch_count || 'unlimited'}</Table.Td>
                    <Table.Td ff="monospace" fz={FS.micro}>{c.consumer_tag}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Card>
      )}
      <CloseModal conn={closing} hostname={database.hostname} onClose={() => setClosing(null)}
        onConfirm={async (reason, typed) => {
          const c = closing
          setClosing(null)
          if (!c) return
          const a = await act({ op: 'close_connection', name: c.name, reason, confirm: typed }, `Closed ${appName(c)}.`)
          if (a.type === 'result') void load()
        }} />
    </Stack>
  )
}

function ChannelTable({ channels, consumers, withConnection }: { channels: Channel[]; consumers: Map<string, Consumer[]>; withConnection?: boolean }) {
  if (channels.length === 0) return <Text size={FS.meta} c="dimmed">No channels.</Text>
  return (
    <Table.ScrollContainer minWidth={620}>
      <Table verticalSpacing={4} fz="sm">
        <Table.Thead><Table.Tr>
          <Table.Th>Channel</Table.Th>{withConnection && <Table.Th>Connection</Table.Th>}<Table.Th>Doing</Table.Th><Table.Th>Mode</Table.Th>
          <Table.Th ta="right">Prefetch</Table.Th><Table.Th ta="right">Unacked</Table.Th><Table.Th ta="right">Publishing</Table.Th><Table.Th ta="right">Receiving</Table.Th>
        </Table.Tr></Table.Thead>
        <Table.Tbody>
          {channels.map((ch) => {
            const cs = consumers.get(ch.name) ?? []
            const pub = rate(ch.message_stats?.publish_details)
            const doing = [cs.length ? `consume ${cs.map((c) => c.queue?.name).join(', ')}` : '', pub > 0 ? 'publish' : ''].filter(Boolean).join(' · ') || 'idle'
            return (
              <Table.Tr key={ch.name}>
                <Table.Td ff="monospace">#{ch.number ?? ch.name.split(' ').pop()}</Table.Td>
                {withConnection && <Table.Td ff="monospace" fz={FS.micro}>{ch.connection_details?.name}</Table.Td>}
                <Table.Td ff="monospace" fz={FS.meta}>{doing}</Table.Td>
                <Table.Td>{ch.confirm ? 'confirm' : cs.some((c) => c.ack_required) ? 'manual ack' : cs.length ? 'auto ack' : '—'}</Table.Td>
                <Table.Td ta="right">{ch.prefetch_count || (cs.length ? 'unlimited' : '—')}</Table.Td>
                <Table.Td ta="right">{ch.messages_unacknowledged ?? 0}</Table.Td>
                <Table.Td ta="right" ff="monospace">{fmtRate(pub)}</Table.Td>
                <Table.Td ta="right" ff="monospace">{fmtRate(rate(ch.message_stats?.deliver_get_details))}</Table.Td>
              </Table.Tr>
            )
          })}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  )
}

function CloseModal({ conn, hostname, onClose, onConfirm }: {
  conn: Connection | null; hostname: string; onClose: () => void; onConfirm: (reason: string, typed: string) => void
}) {
  const [reason, setReason] = useState('')
  const [typed, setTyped] = useState('')
  useEffect(() => { if (conn) { setReason(''); setTyped('') } }, [conn])
  return (
    <Modal opened={conn !== null} onClose={onClose} centered title={<Text fw={650}>Close {conn ? appName(conn) : ''}</Text>}>
      <Stack gap="sm">
        <Text size={FS.meta}>
          The application is disconnected at once. Unacknowledged messages go back to their queues; most clients reconnect on their own.
        </Text>
        <Textarea label="Reason" description="Sent to the application in the close frame, and kept in the recording." value={reason}
          onChange={(e) => setReason(e.currentTarget.value)} maxLength={200} autosize minRows={2} data-autofocus />
        <TextInput label={<>Type <b>{hostname}</b> to confirm</>} value={typed} onChange={(e) => setTyped(e.currentTarget.value)}
          placeholder={hostname} spellCheck={false} autoCapitalize="off" />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" disabled={typed !== hostname || !reason.trim()} onClick={() => onConfirm(reason.trim(), typed)}>Close connection</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
