import { ApprovalBadge } from '~/components/ChangeImpact'
import { writesNeedApproval } from '~/lib/changeApproval'
import { Alert, Badge, Box, Button, Group, SegmentedControl, Stack, Table, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconInfoCircle, IconPlus, IconSearch, IconStack2 } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { databasesQuery, meQuery } from '~/lib/queries'
import { isConfigured } from '~/lib/live'
import { canSee } from '~/lib/roles'
import { FS } from '~/theme'
import type { DBLabel } from '~/types/domain'
import { LevelBadge } from '~/components/LevelBadge'
import { LABEL_COLOR } from '~/components/StoreChrome'
import { EngineMark } from '~/components/EngineMark'
import { isBroker, levelName } from '~/lib/engines'

/**
 * Brokers: every RabbitMQ vhost and Kafka entry this person may open, apart from the
 * databases, because a broker is not one: it is opened to see whether
 * messages keep moving, not to query rows. The entries are the same
 * `databases` rows with engine rabbitmq or kafka, so who may connect, approvals and
 * recordings work exactly as they do for a database.
 */
export const Route = createFileRoute('/brokers/')({
  component: Brokers,
  loader: ({ context }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databasesQuery()).catch(() => undefined) : undefined,
    ]),
})

function Brokers() {
  const { data: me } = useQuery(meQuery())
  const { data, isPending, error } = useQuery(databasesQuery())
  const admin = canSee(me?.role, 'admin')
  const navigate = useNavigate()
  const [label, setLabel] = useState<'all' | DBLabel>('all')
  const [q, setQ] = useState('')

  const all = useMemo(() => (data?.databases ?? []).filter((d) => isBroker(d.engine)), [data])
  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return all.filter((d) => (label === 'all' || d.label === label) &&
      (!needle || d.dbname.toLowerCase().includes(needle) || d.hostname.toLowerCase().includes(needle)))
  }, [all, label, q])

  const description = "Open a RabbitMQ vhost or Kafka topics you've been given to see whether messages keep moving: queues and routing, or topics, partitions and consumer lag. Everything you do is recorded."
  if (!isConfigured()) {
    return (
      <Box>
        <PageHeader title="Message brokers" description={description} />
        <PageBody>
          <Alert icon={<IconInfoCircle size={16} />} color="amber" variant="light">
            No control plane is configured, so there are no brokers to show.
          </Alert>
        </PageBody>
      </Box>
    )
  }

  return (
    <Box>
      <PageHeader
        title="Message brokers"
        description={description}
        actions={admin ? (
          <ButtonLink to="/assets" size="sm" variant="default" leftSection={<IconPlus size={16} />}>Add from a server's page</ButtonLink>
        ) : undefined}
      />
      <PageBody>
        <Stack gap="md">
          <Toolbar right={<Text size={FS.meta} c="dimmed">{data ? `${rows.length} of ${all.length}` : null}</Text>}>
            <TextInput placeholder="Find a vhost, topics or server" leftSection={<IconSearch size={14} />} value={q}
              onChange={(e) => setQ(e.currentTarget.value)} w={{ base: '100%', sm: 320 }} aria-label="Find a broker" />
            <SegmentedControl value={label} onChange={(v) => setLabel(v as 'all' | DBLabel)}
              data={[{ label: 'All', value: 'all' }, { label: 'Production', value: 'production' }, { label: 'Staging', value: 'staging' }, { label: 'Development', value: 'development' }]} />
          </Toolbar>
          {error && <Alert color="rose" variant="light">{(error as Error).message}</Alert>}
          <DataTable
            minWidth={720}
            loading={isPending}
            isEmpty={rows.length === 0}
            empty={
              <EmptyState
                icon={IconStack2}
                title={admin ? 'No brokers yet' : 'No brokers for you yet'}
                description={admin
                  ? 'Add a RabbitMQ vhost or Kafka topics from the server’s page. The agent on that server opens them; nothing else needs installing.'
                  : 'When an admin gives you a RabbitMQ vhost or Kafka topics they appear here.'}
              />
            }
            columns={['Broker', 'Label', 'Your access', 'Agent', { label: '', hidden: true }]}
          >
            {rows.map((d) => {
              const acc = data?.access[d.id]
              const canOpen = Boolean(acc?.level) && d.tunnelConnected
              return (
                <Table.Tr key={d.id}>
                  <Table.Td>
                    <Group gap="xs" wrap="nowrap">
                      <EngineMark engine={d.engine} size={28} />
                      <Box>
                        <Text fw={600} size={FS.body} ff="monospace">{d.engine === 'kafka' && d.dbname === '*' ? 'every topic' : d.dbname}</Text>
                        <Text size={FS.micro} c="dimmed">{d.engine === 'kafka' ? 'Kafka' : 'RabbitMQ'} on <Text span ff="monospace" size={FS.micro}>{d.hostname}</Text> · {d.engine === 'kafka' ? 'port' : 'management port'} {d.port}</Text>
                      </Box>
                    </Group>
                  </Table.Td>
                  <Table.Td><Group gap={6} wrap="nowrap">
                    <Badge variant="light" color={LABEL_COLOR[d.label]} radius="sm">{d.label}</Badge>
                    {writesNeedApproval(d) && <ApprovalBadge />}
                  </Group></Table.Td>
                  <Table.Td><LevelBadge level={acc?.level} engine={d.engine} />
                    {!acc?.level && acc?.approvable && <Text span size={FS.micro} c="dimmed" ml={6}>{levelName(acc.approvable, d.engine)} needs approval</Text>}</Table.Td>
                  <Table.Td><Text size={FS.meta} c={d.tunnelConnected ? 'dimmed' : 'rose'}>{d.tunnelConnected ? 'connected' : 'offline'}</Text></Table.Td>
                  <Table.Td><Group justify="flex-end" gap="xs" wrap="nowrap">
                    {admin && (
                      <ButtonLink to="/brokers/$brokerId/settings" params={{ brokerId: d.id }} size="xs" variant="subtle">Who can connect</ButtonLink>
                    )}
                    <Button size="xs" disabled={!canOpen && Boolean(acc?.level)}
                      title={!acc?.level ? 'Ask for access from the broker page' : !d.tunnelConnected ? 'The agent is offline' : undefined}
                      onClick={() => void navigate({ to: '/brokers/$brokerId', params: { brokerId: d.id } })}>
                      {acc?.level ? 'Open' : 'Ask'}
                    </Button>
                  </Group></Table.Td>
                </Table.Tr>
              )
            })}
          </DataTable>
          <Text size={FS.micro} c="dimmed">
            Queue counts, consumer lag and health are shown when you open one; this list does not open a session.
          </Text>
        </Stack>
      </PageBody>
    </Box>
  )
}
