import { useMemo, useState } from 'react'
import {
  Badge, Box, Card, Group, SegmentedControl, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconPlayerPlay, IconSearch } from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { ButtonLink } from '~/components/links'
import {
  FidelityBadge, Mono, OriginBadge, RiskFlags, SessionStateBadge, absTime, bytes,
  duration, relTime,
} from '~/components/primitives'
import { sessionsQuery } from '~/lib/queries'
import type { Session } from '~/types/domain'

export const Route = createFileRoute('/sessions/')({
  component: Sessions,
  loader: ({ context }) => context.queryClient.ensureQueryData(sessionsQuery()),
})

type Filter = 'all' | 'active' | 'direct' | 'flagged'

function Sessions() {
  const { data: sessions } = useQuery(sessionsQuery())
  const [filter, setFilter] = useState<Filter>('all')
  const [search, setSearch] = useState('')

  const rows = useMemo(() => {
    let out: Session[] = sessions ?? []
    if (filter === 'active') out = out.filter((s) => s.state === 'active')
    if (filter === 'direct') out = out.filter((s) => s.origin === 'direct')
    if (filter === 'flagged') out = out.filter((s) => s.riskFlags.length > 0)
    const needle = search.trim().toLowerCase()
    if (needle) {
      out = out.filter((s) =>
        `${s.userEmail} ${s.assetHostname} ${s.principal} ${s.clientIp}`
          .toLowerCase()
          .includes(needle),
      )
    }
    return out.slice(0, 120)
  }, [sessions, filter, search])

  const activeCount = sessions?.filter((s) => s.state === 'active').length ?? 0
  const flaggedCount = sessions?.filter((s) => s.riskFlags.length > 0).length ?? 0
  const directCount = sessions?.filter((s) => s.origin === 'direct').length ?? 0

  return (
    <Box>
      <PageHeader
        title="Sessions"
        description="Every brokered connection, live and historical. Click any row to replay it."
        actions={
          <SegmentedControl
            size="xs"
            value={filter}
            onChange={(v) => setFilter(v as Filter)}
            data={[
              { label: 'All', value: 'all' },
              { label: `Live (${activeCount})`, value: 'active' },
              { label: `Bypassed (${directCount})`, value: 'direct' },
              { label: `Flagged (${flaggedCount})`, value: 'flagged' },
            ]}
          />
        }
      />

      <Box p="lg">
        <TextInput
          size="xs"
          mb="sm"
          maw={340}
          placeholder="Filter by user, host, principal or client IP"
          leftSection={<IconSearch size={14} />}
          value={search}
          onChange={(e) => setSearch(e.currentTarget.value)}
        />

        <Card padding={0}>
          <Table verticalSpacing={8} horizontalSpacing="md" highlightOnHover striped="even">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>State</Table.Th>
                <Table.Th>User</Table.Th>
                <Table.Th>Target</Table.Th>
                <Table.Th>Origin</Table.Th>
                <Table.Th>Started</Table.Th>
                <Table.Th>Duration</Table.Th>
                <Table.Th>Recording</Table.Th>
                <Table.Th>Risk</Table.Th>
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((s) => (
                <Table.Tr key={s.id}>
                  <Table.Td><SessionStateBadge state={s.state} /></Table.Td>
                  <Table.Td>
                    <Text size="xs">{s.userEmail.split('@')[0]}</Text>
                    <Mono c="dimmed">{s.clientIp}</Mono>
                  </Table.Td>
                  <Table.Td>
                    <Mono>
                      <Text span c="teal.4" inherit>{s.principal}</Text>
                      <Text span c="dimmed" inherit>@</Text>
                      {s.assetHostname.split('.')[0]}
                    </Mono>
                    <Badge size="xs" variant="outline" color="slate" mt={2}>
                      {s.protocol}
                    </Badge>
                  </Table.Td>
                  <Table.Td><OriginBadge origin={s.origin} /></Table.Td>
                  <Table.Td>
                    <Tooltip label={absTime(s.startedAt)}>
                      <Text size="xs" c="dimmed">{relTime(s.startedAt)}</Text>
                    </Tooltip>
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs" c="dimmed">{duration(s.startedAt, s.endedAt)}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <FidelityBadge fidelity={s.fidelity} />
                      <Text size="10px" c="dimmed">{bytes(s.recordingBytes)}</Text>
                    </Group>
                  </Table.Td>
                  <Table.Td><RiskFlags flags={s.riskFlags} /></Table.Td>
                  <Table.Td>
                    <ButtonLink
                      size="compact-xs"
                      variant="light"
                      color="teal"
                      to="/sessions/$sessionId"
                      params={{ sessionId: s.id }}
                      leftSection={<IconPlayerPlay size={12} />}
                    >
                      Replay
                    </ButtonLink>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
          {rows.length === 0 && (
            <Text size="xs" c="dimmed" ta="center" py="xl">No sessions match.</Text>
          )}
        </Card>

        <Text size="10px" c="dimmed" mt="xs">
          Showing {rows.length} of {sessions?.length ?? 0} sessions.
        </Text>
      </Box>
    </Box>
  )
}
