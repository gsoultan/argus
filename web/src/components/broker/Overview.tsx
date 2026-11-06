import { Badge, Box, Button, Card, Group, SimpleGrid, Stack, Table, Text, ThemeIcon } from '@mantine/core'
import { IconAlertTriangle, IconArrowsSplit, IconCircleCheck, IconSkull } from '@tabler/icons-react'
import { useMemo } from 'react'
import {
  fmtBytes, fmtCount, fmtRate, rate, rateSeries, type Attention,
} from '~/lib/rabbit'
import { FS } from '~/theme'
import { useBroker } from './BrokerWorkspace'
import { LineChart, Meter } from './charts'
import { TypeTag } from './Queues'

const C = { publish: 'var(--mantine-color-azure-6)', deliver: 'var(--mantine-color-teal-6)', ack: 'var(--mantine-color-slate-5)' }

function Kpi({ label, value, hint, color, tone }: { label: string; value: string; hint?: string; color?: string; tone?: string }) {
  return (
    <Card withBorder padding="sm" radius="md">
      <Group gap={6}>
        {color && <Box w={10} h={3} style={{ borderRadius: 2, background: color }} />}
        <Text size={FS.micro} c="dimmed">{label}</Text>
      </Group>
      <Text fw={650} fz={22} mt={2} style={{ fontVariantNumeric: 'tabular-nums' }}>{value}</Text>
      {hint && <Text size={FS.micro} c={tone ?? 'dimmed'}>{hint}</Text>}
    </Card>
  )
}

const ICON: Record<Attention['tone'], typeof IconAlertTriangle> = { rose: IconAlertTriangle, amber: IconSkull, sky: IconArrowsSplit }

export function Overview({ needs }: { needs: Attention[] }) {
  const { queues, vhost, nodes, loaded, openQueue, goTab, can } = useBroker()
  const ms = vhost?.message_stats
  const consumers = queues.reduce((n, q) => n + (q.consumers ?? 0), 0)
  const busiest = useMemo(() => queues.slice()
    .sort((a, b) => rate(b.message_stats?.publish_details) + rate(b.message_stats?.deliver_get_details) -
      rate(a.message_stats?.publish_details) - rate(a.message_stats?.deliver_get_details))
    .slice(0, 6), [queues])
  const ready = vhost?.messages_ready ?? queues.reduce((n, q) => n + (q.messages_ready ?? 0), 0)
  const unacked = vhost?.messages_unacknowledged ?? queues.reduce((n, q) => n + (q.messages_unacknowledged ?? 0), 0)
  const readySamples = vhost?.messages_ready_details?.samples ?? []
  const readyDelta = readySamples.length > 1 ? (readySamples[0]?.sample ?? 0) - (readySamples[readySamples.length - 1]?.sample ?? 0) : 0

  if (!loaded) return <Text size={FS.meta} c="dimmed">Reading the broker…</Text>
  return (
    <Stack gap="md">
      <SimpleGrid cols={{ base: 2, sm: 3, lg: 6 }} spacing="sm">
        <Kpi label="Publish" value={fmtRate(rate(ms?.publish_details))} color={C.publish} />
        <Kpi label="Deliver" value={fmtRate(rate(ms?.deliver_get_details))} color={C.deliver} hint={`to ${consumers} consumer${consumers === 1 ? '' : 's'}`} />
        <Kpi label="Acknowledge" value={fmtRate(rate(ms?.ack_details))} color={C.ack} />
        <Kpi label="Ready" value={fmtCount(ready)} hint={readyDelta > 0 ? `▲ ${fmtCount(readyDelta)} in 10 min` : readyDelta < 0 ? `▼ ${fmtCount(-readyDelta)} in 10 min` : 'steady'}
          tone={readyDelta > 100 ? 'amber' : undefined} />
        <Kpi label="Unacked" value={fmtCount(unacked)} hint="delivered, not yet acknowledged" />
        <Kpi label="Queues" value={String(queues.length)} hint={`${queues.filter((q) => (q.consumers ?? 0) === 0).length} without consumers`} />
      </SimpleGrid>

      <Box style={{ display: 'grid', gap: 'var(--mantine-spacing-md)' }} className="argus-broker-grid">
        <Stack gap="md" style={{ minWidth: 0 }}>
          <Card withBorder radius="md" padding="md">
            <Text fw={600} size={FS.body} mb="xs">Message rates</Text>
            <LineChart series={[
              { label: 'Publish', values: rateSeries(ms?.publish_details), color: C.publish },
              { label: 'Deliver', values: rateSeries(ms?.deliver_get_details), color: C.deliver },
              { label: 'Ack', values: rateSeries(ms?.ack_details), color: C.ack, dashed: true },
            ]} />
          </Card>
          <Card withBorder radius="md" padding={0}>
            <Group px="md" pt="sm" pb={4}>
              <Text fw={600} size={FS.body}>Busiest queues</Text>
              <Button size="compact-xs" variant="subtle" ml="auto" onClick={() => goTab('queues')}>All {queues.length} queues</Button>
            </Group>
            <Table.ScrollContainer minWidth={420}>
              <Table verticalSpacing={6} fz="sm" highlightOnHover>
                <Table.Thead><Table.Tr><Table.Th>Queue</Table.Th><Table.Th ta="right">Consumers</Table.Th><Table.Th ta="right">In</Table.Th><Table.Th ta="right">Out</Table.Th><Table.Th ta="right">Ready</Table.Th></Table.Tr></Table.Thead>
                <Table.Tbody>
                  {busiest.map((q) => (
                    <Table.Tr key={q.name} style={{ cursor: 'pointer' }} onClick={() => openQueue(q.name)}>
                      <Table.Td><Text span ff="monospace" size={FS.meta}>{q.name}</Text> <TypeTag type={q.type} /></Table.Td>
                      <Table.Td ta="right" c={(q.consumers ?? 0) === 0 && (q.messages_ready ?? 0) > 0 ? 'rose' : undefined}>{q.consumers ?? 0}</Table.Td>
                      <Table.Td ta="right" ff="monospace">{fmtRate(rate(q.message_stats?.publish_details))}</Table.Td>
                      <Table.Td ta="right" ff="monospace">{fmtRate(rate(q.message_stats?.deliver_get_details))}</Table.Td>
                      <Table.Td ta="right">{fmtCount(q.messages_ready)}</Table.Td>
                    </Table.Tr>
                  ))}
                  {busiest.length === 0 && <Table.Tr><Table.Td colSpan={5}><Text size={FS.meta} c="dimmed">No queues in this vhost.</Text></Table.Td></Table.Tr>}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          </Card>
        </Stack>

        <Stack gap="md" style={{ minWidth: 0 }}>
          <Card withBorder radius="md" padding={0}>
            <Group px="md" py="sm" gap="xs">
              <Text fw={600} size={FS.body}>Needs attention</Text>
              {needs.length > 0 && <Badge size="sm" variant="light" color="amber">{needs.length}</Badge>}
            </Group>
            {needs.length === 0 && (
              <Group px="md" pb="md" gap="xs">
                <IconCircleCheck size={16} color="var(--mantine-color-teal-6)" />
                <Text size={FS.meta} c="dimmed">Every queue with messages has consumers keeping up, and nothing is being dropped.</Text>
              </Group>
            )}
            {needs.map((n, i) => {
              const Icon = ICON[n.tone]
              return (
                <Group key={i} gap="sm" px="md" py="sm" align="flex-start" wrap="nowrap" style={{ borderTop: '1px solid var(--color-line)' }}>
                  <ThemeIcon variant="light" color={n.tone} radius="md" size={30}><Icon size={16} /></ThemeIcon>
                  <Box style={{ minWidth: 0 }}>
                    <Text size={FS.meta} fw={600} style={{ wordBreak: 'break-word' }}>{n.title}</Text>
                    <Text size={FS.micro} c="dimmed" mt={2}>{n.detail}</Text>
                    {n.action === 'queue' && n.queue && (
                      <Button size="compact-xs" variant="default" mt={6} onClick={() => openQueue(n.queue!)}>Open queue</Button>
                    )}
                    {n.action === 'routing' && (
                      <Button size="compact-xs" variant="default" mt={6} onClick={() => goTab('routing')}>Test routing</Button>
                    )}
                  </Box>
                </Group>
              )
            })}
          </Card>
          {nodes && nodes.map((n) => (
            <Card key={n.name} withBorder radius="md" padding="md">
              <Group gap="xs">
                <Text fw={600} size={FS.body} ff="monospace">{n.name}</Text>
                <Badge ml="auto" variant="light" radius="sm" color={n.mem_alarm || n.disk_free_alarm ? 'rose' : 'teal'}>
                  {n.mem_alarm ? 'Memory alarm' : n.disk_free_alarm ? 'Disk alarm' : n.running === false ? 'Not running' : 'No alarms'}
                </Badge>
              </Group>
              <Box mt="sm">
                <Group gap={4}><Text size={FS.micro}>Memory</Text><Text size={FS.micro} c="dimmed" ml="auto">{fmtBytes(n.mem_used)} · alarm at {fmtBytes(n.mem_limit)}</Text></Group>
                <Meter value={n.mem_used ?? 0} max={(n.mem_limit ?? 0) * 1.25} mark={n.mem_limit}
                  color={(n.mem_used ?? 0) > (n.mem_limit ?? Infinity) * 0.8 ? 'amber' : 'teal'} />
              </Box>
              <Box mt="sm">
                <Group gap={4}><Text size={FS.micro}>Disk free</Text><Text size={FS.micro} c="dimmed" ml="auto">{fmtBytes(n.disk_free)} · alarm under {fmtBytes(n.disk_free_limit)}</Text></Group>
              </Box>
              <Box mt="sm">
                <Group gap={4}><Text size={FS.micro}>File descriptors</Text><Text size={FS.micro} c="dimmed" ml="auto">{fmtCount(n.fd_used)} of {fmtCount(n.fd_total)}</Text></Group>
                <Meter value={n.fd_used ?? 0} max={n.fd_total ?? 0} />
              </Box>
              {n.uptime !== undefined && <Text size={FS.micro} c="dimmed" mt="sm">Up {Math.floor(n.uptime / 86_400_000)} days</Text>}
            </Card>
          ))}
          {!nodes && can('overview') && (
            <Text size={FS.micro} c="dimmed">Node memory and disk need RabbitMQ's monitoring tag, which DBA level has.</Text>
          )}
        </Stack>
      </Box>
    </Stack>
  )
}
