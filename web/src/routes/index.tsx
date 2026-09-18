import { Alert, Badge, Box, Grid, Group, Progress, ScrollArea, Stack, Table, Text, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconArrowRight, IconCertificate, IconCircleCheck,
  IconClipboardCheck, IconDoorExit, IconShieldOff, IconTerminal2,
} from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { ButtonLink } from '~/components/links'
import {
  Digest, FidelityBadge, HealthDot, Mono, OriginBadge, RiskFlags, Stat, Target,
  duration, relTime, rowNav,
} from '~/components/primitives'
import { FS, SP } from '~/theme'
import { assetsQuery, requestsQuery, sessionsQuery, statsQuery } from '~/lib/queries'

export const Route = createFileRoute('/')({
  component: Overview,
  loader: ({ context }) => context.queryClient.ensureQueryData(statsQuery()),
})

function Overview() {
  const navigate = useNavigate()
  const { data: stats } = useQuery(statsQuery())
  const { data: live } = useQuery(sessionsQuery('active'))
  const { data: pending } = useQuery(requestsQuery('pending'))
  const { data: assets } = useQuery(assetsQuery({}))

  const unpinned = assets?.filter((a) => a.hostKeyState !== 'pinned') ?? []
  const changed = unpinned.filter((a) => a.hostKeyState === 'changed')
  const caCoverage = stats
    ? Math.round(((stats.assetsTotal - stats.standingCredentialAssets) / stats.assetsTotal) * 100)
    : 0
  const needsAttention =
    assets?.filter((a) => a.health !== 'reachable' || a.hostKeyState !== 'pinned') ?? []

  return (
    <Box>
      <PageHeader
        title="Overview"
        description="Fleet posture and everything currently in flight."
      />

      <PageBody>
        {/* Recording coverage comes first. An unverified host is a risk you can
            see; an unmonitored one is a session you will never hear about. */}
        {(stats?.assetsUnmonitored ?? 0) > 0 && (
          <Alert
            color="rose"
            icon={<IconDoorExit size={17} />}
            title={`${stats?.assetsUnmonitored} host${stats?.assetsUnmonitored === 1 ? '' : 's'} would not record a bypass`}
          >
            {/* Text and actions share a row rather than stacking.
                Two sentences, too: the paragraph that used to be here lives on
                Coverage, where you act on it. A dashboard states the finding and
                hands over — repeating the full explanation on both, over a badge
                row of its own, is what pushed the fleet counters off a laptop
                screen. Capped at a readable measure instead of running the full
                width of a 1600px window. */}
            <Group justify="space-between" align="flex-start" gap="md">
              <Text size={FS.body} lh={1.5} maw={720} style={{ flex: '1 1 300px', minWidth: 0 }}>
                No agent and no lockdown: anyone with a standing key can reach sshd on port 22
                and Argus will never know the session happened. Your recording coverage is not
                the fleet — only the part of it that chooses to use the gateway.
              </Text>
              <Group gap="xs">
                {(stats?.agentsStale ?? 0) > 0 && (
                  <Badge color="rose" variant="filled">
                    {stats?.agentsStale} agent{stats?.agentsStale === 1 ? '' : 's'} went silent
                  </Badge>
                )}
                {(stats?.sessionsDirectToday ?? 0) > 0 && (
                  <Badge color="rose" variant="outline">
                    {stats?.sessionsDirectToday} direct session(s) in 24h
                  </Badge>
                )}
                {/* Coverage, not Assets. This said "Review coverage" and went to
                    the inventory table, which lists hosts but answers nothing
                    about whether an agent is reporting from each one. */}
                <ButtonLink
                  size="compact-xs"
                  variant="light"
                  color="rose"
                  to="/coverage"
                  rightSection={<IconArrowRight size={12} />}
                >
                  Review coverage
                </ButtonLink>
              </Group>
            </Group>
          </Alert>
        )}

        {/* Host-key problems are the one thing that must never be buried in a
            table — Argus terminates SSH, so an unverified target means nobody
            verified it. */}
        {changed.length > 0 && (
          <Alert
            color="rose"
            icon={<IconAlertTriangle size={17} />}
            title={`${changed.length} host key${changed.length > 1 ? 's' : ''} changed`}
          >
            <Group justify="space-between" align="flex-start" gap="md">
              <Text size={FS.body} lh={1.5} maw={720} style={{ flex: '1 1 300px', minWidth: 0 }}>
                The presented key no longer matches the pin, so Argus is refusing connections.
                Either {changed.length > 1 ? 'these hosts were' : 'this host was'} rebuilt, or
                something is intercepting the connection — verify out of band before re-pinning.
              </Text>
              <Group gap="xs">
                {changed.slice(0, 4).map((a) => (
                  <Badge key={a.id} color="rose" variant="outline" className="argus-digest">
                    {a.hostname.split('.')[0]}
                  </Badge>
                ))}
                {/* The count is of changed keys specifically, and the inventory
                    can now be asked for exactly that set. */}
                <ButtonLink
                  size="compact-xs"
                  variant="light"
                  color="rose"
                  to="/assets"
                  search={{ hostKey: 'changed' }}
                  rightSection={<IconArrowRight size={12} />}
                >
                  Review hosts
                </ButtonLink>
              </Group>
            </Group>
          </Alert>
        )}

        <Grid gap="sm">
          <Grid.Col span={{ base: 6, md: 3 }}>
            <Stat
              label="Live sessions"
              value={stats?.sessionsActive ?? '—'}
              sub={`${stats?.sessionsToday ?? 0} in the last 24h`}
              icon={IconTerminal2}
              color="sky"
              onClick={() => navigate({ to: '/sessions' })}
            />
          </Grid.Col>
          <Grid.Col span={{ base: 6, md: 3 }}>
            <Stat
              label="Pending approvals"
              value={stats?.requestsPending ?? '—'}
              sub="awaiting a decision"
              icon={IconClipboardCheck}
              color="amber"
              onClick={() => navigate({ to: '/requests' })}
              tone={(stats?.requestsPending ?? 0) > 0 ? 'warn' : 'ok'}
            />
          </Grid.Col>
          <Grid.Col span={{ base: 6, md: 3 }}>
            <Stat
              label="Unverified hosts"
              value={stats?.hostKeysUnpinned ?? '—'}
              sub="host key not pinned"
              icon={IconShieldOff}
              color="rose"
              onClick={() => navigate({ to: '/assets' })}
              tone={(stats?.hostKeysUnpinned ?? 0) > 0 ? 'warn' : 'ok'}
            />
          </Grid.Col>
          <Grid.Col span={{ base: 6, md: 3 }}>
            <Stat
              label="Bypassed gateway"
              value={stats?.sessionsDirectToday ?? '—'}
              sub="direct to sshd, 24h"
              icon={IconDoorExit}
              color="rose"
              onClick={() => navigate({ to: '/sessions' })}
              tone={(stats?.sessionsDirectToday ?? 0) > 0 ? 'warn' : 'ok'}
            />
          </Grid.Col>
        </Grid>

        <Grid gap="sm">
          <Grid.Col span={{ base: 12, lg: 8 }}>
            <SectionCard
              title="Live sessions"
              icon={IconTerminal2}
              iconColor="sky"
              flush
              badge={
                <Badge size="xs" color="sky" variant="light">
                  {live?.length ?? 0}
                </Badge>
              }
              action={
                <ButtonLink
                  size="compact-xs"
                  variant="subtle"
                  color="slate"
                  to="/sessions"
                  rightSection={<IconArrowRight size={12} />}
                >
                  All sessions
                </ButtonLink>
              }
            >
              <ScrollArea.Autosize mah={330}>
                <Table.ScrollContainer minWidth={760} type="native">
                  <Table>
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>User</Table.Th>
                        <Table.Th>Target</Table.Th>
                        <Table.Th>Origin</Table.Th>
                        <Table.Th>Elapsed</Table.Th>
                        <Table.Th>Recording</Table.Th>
                        <Table.Th>Risk</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {live?.map((s) => (
                        <Table.Tr
                          key={s.id}
                          {...rowNav(() =>
                            navigate({ to: '/sessions/$sessionId', params: { sessionId: s.id } }),
                          )}
                        >
                          <Table.Td>
                            <Text size="xs">{s.userEmail.split('@')[0]}</Text>
                          </Table.Td>
                          <Table.Td>
                            <Target principal={s.principal} hostname={s.assetHostname} />
                          </Table.Td>
                          <Table.Td><OriginBadge origin={s.origin} /></Table.Td>
                          <Table.Td>
                            <Text size="xs" c="dimmed">{duration(s.startedAt, null)}</Text>
                          </Table.Td>
                          <Table.Td><FidelityBadge fidelity={s.fidelity} /></Table.Td>
                          <Table.Td><RiskFlags flags={s.riskFlags} /></Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
                {live?.length === 0 && (
                  <EmptyState
                    compact
                    icon={IconCircleCheck}
                    title="Nobody is connected right now."
                    description="Sessions appear the moment the gateway brokers one — and an agent reports any that bypassed it."
                  />
                )}
              </ScrollArea.Autosize>
            </SectionCard>
          </Grid.Col>

          <Grid.Col span={{ base: 12, lg: 4 }}>
            <Stack gap="sm">
              <SectionCard
                title="Zero standing privilege"
                icon={IconCertificate}
                iconColor="teal"
                description="Assets where Argus mints a short-lived certificate per session, so no reusable credential exists to steal."
              >
                <Group justify="space-between" align="flex-end" mb={SP.snug}>
                  <Text size="xl" fw={600} lh={1}>{caCoverage}%</Text>
                  <Text size={FS.meta} c="dimmed">
                    {stats ? stats.assetsTotal - stats.standingCredentialAssets : 0} / {stats?.assetsTotal ?? 0}
                  </Text>
                </Group>
                <Progress value={caCoverage} color="teal" size="sm" radius="xl" />
                <Text size={FS.micro} c="dimmed" mt={SP.cozy} lh={1.5}>
                  The remaining {stats?.standingCredentialAssets ?? 0} use vaulted keys injected
                  by the gateway. Users never see them, but they are standing credentials —
                  move hosts to certificate auth where you can.
                </Text>
              </SectionCard>

              <SectionCard
                title="Awaiting approval"
                icon={IconClipboardCheck}
                iconColor="amber"
                flush
                action={
                  <ButtonLink
                    size="compact-xs"
                    variant="subtle"
                    color="slate"
                    to="/requests"
                    rightSection={<IconArrowRight size={12} />}
                  >
                    Queue
                  </ButtonLink>
                }
              >
                <Stack gap={0}>
                  {pending?.slice(0, 4).map((r) => (
                    <Box
                      key={r.id}
                      px="md"
                      py={SP.cozy}
                      style={{ borderTop: '1px solid var(--color-line)' }}
                    >
                      <Group justify="space-between" wrap="nowrap" mb={SP.tight}>
                        <Text size="xs" fw={500} truncate>
                          {r.requesterEmail.split('@')[0]}
                        </Text>
                        <Group gap={SP.snug} wrap="nowrap">
                          {r.breakGlass && (
                            <Tooltip label="Break-glass path — separate approval chain">
                              <Badge size="xs" color="rose">break-glass</Badge>
                            </Tooltip>
                          )}
                          <Text size={FS.micro} c="dimmed">{relTime(r.createdAt)}</Text>
                        </Group>
                      </Group>
                      <Text size={FS.micro} c="dimmed" lineClamp={2} lh={1.5}>
                        {r.justification}
                      </Text>
                      <Group gap={SP.snug} mt={SP.snug}>
                        <Badge size="xs" variant="outline" color="slate">
                          {r.assetHostnames.length} host{r.assetHostnames.length > 1 ? 's' : ''}
                        </Badge>
                        <Badge size="xs" variant="outline" color="slate">
                          as {r.principal}
                        </Badge>
                        <Badge size="xs" variant="outline" color="slate">
                          {r.durationMinutes / 60}h
                        </Badge>
                      </Group>
                    </Box>
                  ))}
                  {pending?.length === 0 && (
                    <EmptyState
                      compact
                      icon={IconCircleCheck}
                      title="Queue is clear."
                      description="Nothing is waiting on a decision."
                    />
                  )}
                </Stack>
              </SectionCard>
            </Stack>
          </Grid.Col>
        </Grid>

        {/* Named for what it is rather than "Fleet health": the table has always
            been filtered to hosts that are unreachable, degraded or unpinned, so
            a heading promising fleet-wide health described a list that was, by
            construction, only the bad part of it. */}
        <SectionCard
          title="Needs attention"
          icon={IconAlertTriangle}
          iconColor="amber"
          description="Hosts that are unreachable, degraded, or whose key Argus has not verified. A healthy fleet shows nothing here."
          flush
          action={
            <ButtonLink
              size="compact-xs"
              variant="subtle"
              color="slate"
              to="/assets"
              rightSection={<IconArrowRight size={12} />}
            >
              Inventory
            </ButtonLink>
          }
        >
          <ScrollArea.Autosize mah={260}>
            <Table.ScrollContainer minWidth={760} type="native">
              <Table>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Host</Table.Th>
                    <Table.Th>Address</Table.Th>
                    <Table.Th>OS</Table.Th>
                    <Table.Th>Host key</Table.Th>
                    <Table.Th>Last checked</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {needsAttention.slice(0, 8).map((a) => (
                    <Table.Tr
                      key={a.id}
                      {...rowNav(() =>
                        navigate({ to: '/assets/$assetId', params: { assetId: a.id } }),
                      )}
                    >
                      <Table.Td>
                        <Group gap={SP.cozy} wrap="nowrap">
                          <HealthDot health={a.health} />
                          <Mono>{a.hostname.split('.')[0]}</Mono>
                        </Group>
                      </Table.Td>
                      <Table.Td><Mono c="dimmed">{a.address}</Mono></Table.Td>
                      <Table.Td><Text size="xs" c="dimmed">{a.os}</Text></Table.Td>
                      <Table.Td>
                        {a.hostKeyFingerprint
                          ? <Digest value={a.hostKeyFingerprint} chars={18} />
                          : <Text size="xs" c="amber.4">none pinned</Text>}
                      </Table.Td>
                      <Table.Td>
                        <Text size="xs" c="dimmed">{relTime(a.lastCheckedAt)}</Text>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
            {assets && needsAttention.length === 0 && (
              <EmptyState
                compact
                icon={IconCircleCheck}
                title="Every host is reachable and pinned."
                description="Nothing in the inventory needs looking at."
              />
            )}
          </ScrollArea.Autosize>
        </SectionCard>
      </PageBody>
    </Box>
  )
}
