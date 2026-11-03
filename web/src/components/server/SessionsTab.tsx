import { useState } from 'react'
import { Box, Button, Card, Group, SegmentedControl, Stack, Table, Text, TextInput } from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { useNavigate } from '@tanstack/react-router'
import { IconChevronRight, IconHistory, IconSearch } from '@tabler/icons-react'
import { ButtonLink } from '~/components/links'
import { EmptyState, TableSkeleton, Toolbar } from '~/components/page'
import {
  ArtefactPending, FidelityBadge, Mono, SessionDuration, SessionStateBadge, absTime, relTime, rowNav,
} from '~/components/primitives'
import { FS, SP } from '~/theme'
import type { Asset, Session } from '~/types/domain'

type Filter = 'all' | 'live' | 'brokered' | 'direct'
const PAGE = 25

/**
 * Every session on this server, newest first, with a search and the filters
 * someone looking at one server asks for. The Sessions page has the rest of
 * the fleet, and this list opens there already narrowed to this server.
 */
export function SessionsTab({ asset, sessions }: { asset: Asset; sessions: Session[] | undefined }) {
  const navigate = useNavigate()
  const [q, setQ] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [shown, setShown] = useState(PAGE)
  const phone = useMediaQuery('(max-width: 48em)') ?? false

  const live = sessions?.filter((s) => s.state === 'active' && !s.silent).length
  const needle = q.trim().toLowerCase()
  const rows = (sessions ?? []).filter((s) => {
    if (filter === 'live' && !(s.state === 'active' && !s.silent)) return false
    if (filter === 'brokered' && s.origin !== 'brokered') return false
    if (filter === 'direct' && s.origin !== 'direct') return false
    return !needle || s.userEmail.toLowerCase().includes(needle) || s.principal.toLowerCase().includes(needle)
  })
  const page = rows.slice(0, shown)
  const open = (s: Session) => void navigate({ to: '/sessions/$sessionId', params: { sessionId: s.id } })

  return (
    <Stack gap="sm">
      <Toolbar
        right={
          <ButtonLink size="compact-sm" variant="subtle" to="/sessions" search={{ host: asset.hostname }}>
            All sessions on this server
          </ButtonLink>
        }
      >
        <TextInput
          aria-label="Search sessions"
          placeholder="Search person or account"
          leftSection={<IconSearch size={14} />}
          value={q}
          onChange={(e) => { setQ(e.currentTarget.value); setShown(PAGE) }}
          w={{ base: '100%', sm: 260 }}
        />
        <Box className="argus-tabs-scroll" style={{ maxWidth: '100%' }}>
          <SegmentedControl
            value={filter}
            onChange={(v) => { setFilter(v as Filter); setShown(PAGE) }}
            data={[
              { value: 'all', label: 'All' },
              { value: 'live', label: live === undefined ? 'Live' : `Live ${live}` },
              { value: 'brokered', label: 'Through Argus' },
              { value: 'direct', label: 'Direct' },
            ]}
          />
        </Box>
      </Toolbar>

      <Card padding={0}>
        {sessions === undefined ? (
          <Table><Table.Tbody><TableSkeleton rows={5} cols={6} /></Table.Tbody></Table>
        ) : rows.length === 0 ? (
          <EmptyState
            compact
            icon={IconHistory}
            title={sessions.length === 0 ? 'Nobody has connected to this host yet.' : 'No session matches.'}
            description={sessions.length === 0
              ? 'Sessions through Argus, and direct logins the agent reports, appear here.'
              : 'Clear the search or pick All to see every session on this server.'}
          />
        ) : (
          <>
            {/* A table from a tablet up; a list on a phone. */}
            {!phone ? (
              <Table.ScrollContainer minWidth={640} type="native">
                <Table>
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>State</Table.Th>
                      <Table.Th>Person</Table.Th>
                      <Table.Th>As</Table.Th>
                      <Table.Th>Started</Table.Th>
                      <Table.Th>Duration</Table.Th>
                      <Table.Th>Recording</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.map((s) => (
                      <Table.Tr key={s.id} {...rowNav(() => open(s))}>
                        <Table.Td>
                          <SessionStateBadge state={s.state} silent={s.silent} lastReportedAt={s.lastReportedAt} endInferred={s.endInferred} />
                        </Table.Td>
                        <Table.Td><Text size={FS.meta}>{s.userEmail.split('@')[0]}</Text></Table.Td>
                        <Table.Td><Mono c="azure.3">{s.principal}</Mono></Table.Td>
                        <Table.Td>
                          <Text size={FS.meta} c="dimmed" title={absTime(s.startedAt)}>{relTime(s.startedAt)}</Text>
                        </Table.Td>
                        <Table.Td><SessionDuration session={s} /></Table.Td>
                        <Table.Td>
                          <Group gap={SP.snug} wrap="nowrap">
                            <FidelityBadge fidelity={s.fidelity} />
                            <ArtefactPending session={s} />
                          </Group>
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            ) : (
            <Box>
              {page.map((s, i) => (
                <Group
                  key={s.id}
                  {...rowNav(() => open(s))}
                  gap="sm"
                  wrap="nowrap"
                  px="md"
                  py="sm"
                  style={i > 0 ? { borderTop: '1px solid var(--color-line)' } : undefined}
                >
                  <Box style={{ flex: 1, minWidth: 0 }}>
                    <Group gap={SP.snug} wrap="nowrap">
                      <Text size={FS.body} fw={600} truncate>{s.userEmail.split('@')[0]}</Text>
                      <Text size={FS.micro} c="dimmed">as</Text>
                      <Mono c="azure.3">{s.principal}</Mono>
                    </Group>
                    <Group gap={SP.tight} mt={2} wrap="nowrap">
                      <Text size={FS.meta} c="dimmed" title={absTime(s.startedAt)}>{relTime(s.startedAt)} ·</Text>
                      <SessionDuration session={s} />
                    </Group>
                  </Box>
                  <SessionStateBadge state={s.state} silent={s.silent} lastReportedAt={s.lastReportedAt} endInferred={s.endInferred} />
                  <IconChevronRight size={14} opacity={0.4} />
                </Group>
              ))}
            </Box>
            )}

            <Group justify="space-between" px="md" py="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
              <Text size={FS.meta} c="dimmed">Showing {page.length} of {rows.length}</Text>
              {rows.length > page.length && (
                <Button size="compact-sm" variant="default" onClick={() => setShown((n) => n + PAGE)}>
                  Show more
                </Button>
              )}
            </Group>
          </>
        )}
      </Card>
    </Stack>
  )
}
