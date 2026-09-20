import { useMemo, useState } from 'react'
import { Badge, Box, Group, SegmentedControl, Table, Text, TextInput, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconPlayerPlay, IconSearch, IconTerminal2 } from '@tabler/icons-react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { ButtonLink } from '~/components/links'
import {
  FidelityBadge, Mono, OriginBadge, RiskFlags, SessionStateBadge, Target, absTime,
  bytes, duration, relTime, rowNav,
} from '~/components/primitives'
import { FS, SP } from '~/theme'
import { sessionsQuery } from '~/lib/queries'
import type { Session } from '~/types/domain'

const FILTERS = ['all', 'active', 'silent', 'direct', 'flagged'] as const
type Filter = (typeof FILTERS)[number]

interface SessionSearch {
  show?: Filter
}

export const Route = createFileRoute('/sessions/')({
  component: Sessions,
  // In the URL rather than in component state, so "the sessions nobody is
  // reporting" is a place that can be linked to -- the Overview alert and the
  // header pill both point here. Unrecognised values are dropped rather than
  // passed through, so a hand-edited URL cannot put the table into a state the
  // segmented control cannot show.
  validateSearch: (search: Record<string, unknown>): SessionSearch => ({
    show:
      typeof search.show === 'string' && (FILTERS as readonly string[]).includes(search.show)
        ? (search.show as Filter)
        : undefined,
  }),
  loader: ({ context }) => context.queryClient.ensureQueryData(sessionsQuery()),
})


const LIMIT = 120

function Sessions() {
  const navigate = useNavigate()
  const { data: sessions } = useQuery(sessionsQuery())
  const filter = Route.useSearch().show ?? 'all'
  const setFilter = (show: Filter) =>
    void navigate({ to: '/sessions', search: show === 'all' ? {} : { show }, replace: true })
  const [search, setSearch] = useState('')

  const rows = useMemo(() => {
    let out: Session[] = sessions ?? []
    if (filter === 'active') out = out.filter((s) => s.state === 'active' && !s.silent)
    if (filter === 'silent') out = out.filter((s) => s.state === 'active' && s.silent)
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
    return out.slice(0, LIMIT)
  }, [sessions, filter, search])

  const activeCount = sessions?.filter((s) => s.state === 'active' && !s.silent).length ?? 0
  const silentCount = sessions?.filter((s) => s.state === 'active' && s.silent).length ?? 0
  const flaggedCount = sessions?.filter((s) => s.riskFlags.length > 0).length ?? 0
  const directCount = sessions?.filter((s) => s.origin === 'direct').length ?? 0
  const filtered = filter !== 'all' || search.trim().length > 0

  return (
    <Box>
      <PageHeader
        title="Sessions"
        description="Every connection Argus knows about, live and historical. Open any row to replay it."
      />

      <PageBody>
        {/* Both controls narrow the same table, so they sit together directly
            above it. The segmented filter used to live in the page header's
            action slot — beside buttons that perform operations — while the
            search box sat down here, which read as two unrelated controls. */}
        <Toolbar
          right={
            sessions && (
              <Text size={FS.micro} c="dimmed">
                {rows.length === LIMIT
                  ? `First ${LIMIT} of ${sessions.length}`
                  : `${rows.length} of ${sessions.length} sessions`}
              </Text>
            )
          }
        >
          <TextInput
            w={300}
            placeholder="Filter by user, host, principal or client IP"
            leftSection={<IconSearch size={14} />}
            value={search}
            onChange={(e) => setSearch(e.currentTarget.value)}
          />
          <SegmentedControl
            value={filter}
            onChange={(v) => setFilter(v as Filter)}
            // No parenthetical until there is something to put in it: "Live (0)"
            // while the log is still loading is an answer, and a wrong one.
            data={[
              { label: 'All', value: 'all' },
              { label: sessions ? `Live (${activeCount})` : 'Live', value: 'active' },
              // Its own segment rather than folded into Live: these are the
              // sessions somebody has to go and look at, and burying them in a
              // log sorted newest-first hides the oldest ones the deepest.
              { label: sessions ? `Unknown (${silentCount})` : 'Unknown', value: 'silent' },
              { label: sessions ? `Bypassed (${directCount})` : 'Bypassed', value: 'direct' },
              { label: sessions ? `Flagged (${flaggedCount})` : 'Flagged', value: 'flagged' },
            ]}
          />
        </Toolbar>

        <DataTable
          minWidth={1000}
          loading={sessions === undefined}
          isEmpty={rows.length === 0}
          columns={[
            'State', 'User', 'Target', 'Origin', 'Started', 'Duration', 'Recording', 'Risk',
            { label: '', hidden: true, width: 90 },
          ]}
          empty={
            <EmptyState
              icon={IconTerminal2}
              title="No sessions match."
              description={
                filtered
                  ? filter === 'silent'
                    ? 'Every active session has reported in recently. Nothing here is a good result.'
                    : 'Nothing in the log fits this filter. Widen it, or clear the search to see everything.'
                  : 'No session has been brokered or reported yet. One appears here as soon as somebody connects.'
              }
            />
          }
        >
          {rows.map((s) => (
            <Table.Tr
              key={s.id}
              {...rowNav(() =>
                navigate({ to: '/sessions/$sessionId', params: { sessionId: s.id } }),
              )}
            >
              <Table.Td>
                <SessionStateBadge
                  state={s.state}
                  silent={s.silent}
                  lastReportedAt={s.lastReportedAt}
                />
              </Table.Td>
              <Table.Td>
                <Text size="xs">{s.userEmail.split('@')[0]}</Text>
                <Mono c="dimmed">{s.clientIp}</Mono>
              </Table.Td>
              <Table.Td>
                <Group gap={SP.snug} wrap="nowrap">
                  <Target principal={s.principal} hostname={s.assetHostname} />
                  <Badge size="xs" variant="outline" color="slate">
                    {s.protocol}
                  </Badge>
                </Group>
              </Table.Td>
              <Table.Td><OriginBadge origin={s.origin} /></Table.Td>
              <Table.Td>
                <Tooltip label={absTime(s.startedAt)}>
                  <Text size="xs" c="dimmed">{relTime(s.startedAt)}</Text>
                </Tooltip>
              </Table.Td>
              <Table.Td>
                {/* A silent session's clock stops at its last report. Running
                    it to now would say a gateway nobody can find has been
                    holding a session for nineteen days. */}
                <Text size="xs" c="dimmed">
                  {s.silent
                    ? `${duration(s.startedAt, s.lastReportedAt)}+`
                    : duration(s.startedAt, s.endedAt)}
                </Text>
              </Table.Td>
              <Table.Td>
                <Group gap={SP.snug} wrap="nowrap">
                  <FidelityBadge fidelity={s.fidelity} />
                  <Text size={FS.micro} c="dimmed" style={{ whiteSpace: 'nowrap' }}>
                    {bytes(s.recordingBytes)}
                  </Text>
                </Group>
              </Table.Td>
              <Table.Td><RiskFlags flags={s.riskFlags} /></Table.Td>
              {/* The row navigates too; the button stays as an explicit
                  affordance, so its click must not also bubble up. */}
              <Table.Td onClick={(e) => e.stopPropagation()}>
                <ButtonLink
                  size="compact-xs"
                  variant="light"
                  to="/sessions/$sessionId"
                  params={{ sessionId: s.id }}
                  leftSection={<IconPlayerPlay size={12} />}
                >
                  Replay
                </ButtonLink>
              </Table.Td>
            </Table.Tr>
          ))}
        </DataTable>
      </PageBody>
    </Box>
  )
}
