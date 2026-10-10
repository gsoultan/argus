import {
  Badge, Box, Grid, Group, Progress, ScrollArea, Skeleton, Stack, Table, Text, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconArrowRight, IconCertificate, IconCircleCheck,
  IconCirclePlus, IconClipboardCheck, IconDoorExit, IconShieldOff, IconTerminal2,
} from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { ButtonLink } from '~/components/links'
import {
  Digest, FidelityBadge, HealthDot, Mono, OriginBadge, RiskFlags, SessionDuration, Stat,
  Target, relTime, rowNav,
} from '~/components/primitives'
import { FS, SP } from '~/theme'
import { assetsQuery, coverageQuery, meQuery, requestsQuery, sessionsQuery, statsQuery } from '~/lib/queries'
import { Attention, ConnectHero, plural } from '~/components/overview'

export const Route = createFileRoute('/')({
  component: Overview,
  loader: ({ context }) => context.queryClient.ensureQueryData(statsQuery()),
})

function Overview() {
  const navigate = useNavigate()
  const { data: me } = useQuery(meQuery())
  // Every figure on this page is scoped to what the signed-in account can see:
  // an operator gets their assigned hosts and their own sessions. The numbers
  // are right either way -- what changes is what they are about, and a page
  // headed "fleet posture" while counting two hosts would be the wrong frame
  // around the right data.
  const wholeFleet =
    me?.role === 'admin' || me?.role === 'owner' || me?.role === 'auditor'
  const { data: stats } = useQuery(statsQuery())
  // Filtered, not merely fetched. The query asks for `state = 'active'`, which
  // includes the sessions nothing has reported in minutes -- so a section
  // headed "Live sessions" was listing 15 that were not, badging the total, and
  // disagreeing with the Stat tile two rows above it. They are not hidden: the
  // alert above links straight to them.
  const { data: activeSessions } = useQuery(sessionsQuery('active'))
  const live = activeSessions?.filter((s) => s.state === 'active' && !s.silent)
  const { data: pending } = useQuery(requestsQuery('pending'))
  const { data: assets } = useQuery(assetsQuery({}))

  const unpinned = assets?.filter((a) => a.hostKeyState !== 'pinned') ?? []
  const changed = unpinned.filter((a) => a.hostKeyState === 'changed')
  const caCoverage = stats
    ? Math.round(((stats.assetsTotal - stats.standingCredentialAssets) / stats.assetsTotal) * 100)
    : 0
  const needsAttention =
    assets?.filter((a) => a.health !== 'reachable' || a.hostKeyState !== 'pinned') ?? []
  const { data: cov } = useQuery(coverageQuery())
  const waiting = cov?.unreviewedHosts

  return (
    <Box>
      <PageHeader
        title="Overview"
        description={
          wholeFleet
            ? 'Fleet posture and everything currently in flight.'
            : 'The hosts assigned to you, and your own sessions. Fleet-wide posture is an administrator\u2019s view.'
        }
      />

      <PageBody>
        <ConnectHero assets={assets} />

        <Attention
          items={[
            // Recording coverage comes first. An unverified host is a risk you
            // can see; an unmonitored one is a session you will never hear about.
            (stats?.assetsUnmonitored ?? 0) > 0 && {
              key: 'unmonitored',
              tone: 'rose' as const,
              icon: IconDoorExit,
              // Says what can go wrong, not the mechanism: "no agent watching"
              // left people asking what an agent was and why it mattered.
              title: `${plural(stats!.assetsUnmonitored, 'server is', 'servers are')} not recorded if someone logs in directly`,
              detail:
                'Sessions opened through Argus are always recorded. These servers have no Argus agent running, so a login straight to the server, for example ssh from a laptop, leaves no trace. Install the agent on them to close the gap.' +
                ((stats?.agentsStale ?? 0) > 0
                  ? ` ${plural(stats!.agentsStale, 'agent has', 'agents have')} also stopped reporting.`
                  : ''),
              // Coverage, not Assets: the inventory lists hosts but answers
              // nothing about whether an agent is reporting from each one.
              action: { label: 'Review coverage', to: '/coverage' as const },
            },
            // Host-key problems must never be buried in a table: Argus
            // terminates SSH, so an unverified target means nobody verified it.
            changed.length > 0 && {
              key: 'hostkey',
              tone: 'rose' as const,
              icon: IconAlertTriangle,
              title: `${plural(changed.length, 'server', 'servers')} presented a different host key`,
              detail: `Argus is refusing connections to ${changed
                .slice(0, 3)
                .map((a) => a.hostname.split('.')[0])
                .join(', ')}${changed.length > 3 ? ' and others' : ''}. Check it was rebuilt before trusting the new key.`,
              action: {
                label: 'Review hosts',
                to: '/assets' as const,
                search: { hostKey: 'changed' as const },
              },
            },
            (waiting ?? 0) > 0 && {
              key: 'waiting',
              tone: 'amber' as const,
              icon: IconCirclePlus,
              title: `${plural(waiting!, 'new server is', 'new servers are')} waiting for approval`,
              detail: 'Its agent has checked in. Nobody can connect to it until an admin approves it.',
              action: { label: 'Approve servers', to: '/assets/add' as const },
            },
            (stats?.requestsPending ?? 0) > 0 && {
              key: 'requests',
              tone: 'amber' as const,
              icon: IconClipboardCheck,
              title: `${plural(stats!.requestsPending, 'access request needs', 'access requests need')} a decision`,
              detail: 'Someone is waiting to connect until an approver says yes or no.',
              action: { label: 'Review requests', to: '/requests' as const },
            },
            // Nobody is being attacked, but the console has lost track of a
            // session, so the live count is smaller than it looks.
            (stats?.sessionsSilent ?? 0) > 0 && {
              key: 'silent',
              tone: 'amber' as const,
              icon: IconAlertTriangle,
              title: `${plural(stats!.sessionsSilent, 'session is', 'sessions are')} unaccounted for`,
              detail:
                'Recorded as active, but no gateway has reported on them in minutes. Most likely a gateway stopped without saying the session ended.',
              action: {
                label: 'Review sessions',
                to: '/sessions' as const,
                search: { show: 'silent' as const },
              },
            },
          ]}
          loading={!stats}
        />

        {/* Every tile below lands on exactly the set it counted.
            They all linked unfiltered before, so clicking "2 live sessions"
            opened a log of twelve thousand rows and clicking "3 bypassed
            today" opened every bypass ever recorded. The rule was already
            written down for the alerts on this page after the same mistake
            shipped twice -- a counter links to a filter only when the two mean
            the same set -- and the tiles had simply never been held to it. */}
        <Grid gap="sm">
          <Grid.Col span={{ base: 6, md: 3 }}>
            <Stat
              label="Live sessions"
              value={stats?.sessionsActive}
              sub={stats ? `${stats.sessionsToday} in the last 24h` : undefined}
              icon={IconTerminal2}
              color="sky"
              onClick={() => navigate({ to: '/sessions', search: { show: 'active' } })}
            />
          </Grid.Col>
          <Grid.Col span={{ base: 6, md: 3 }}>
            <Stat
              label="Pending approvals"
              value={stats?.requestsPending}
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
              value={stats?.hostKeysUnpinned}
              sub="host key not pinned"
              icon={IconShieldOff}
              color="rose"
              // `unverified`, not `unpinned`: the count is everything that is
              // not pinned, which includes a key that changed.
              onClick={() => navigate({ to: '/assets', search: { hostKey: 'unverified' } })}
              tone={(stats?.hostKeysUnpinned ?? 0) > 0 ? 'warn' : 'ok'}
            />
          </Grid.Col>
          <Grid.Col span={{ base: 6, md: 3 }}>
            <Stat
              label="Bypassed gateway"
              value={stats?.sessionsDirectToday}
              sub="direct to sshd, 24h"
              icon={IconDoorExit}
              color="rose"
              // Both halves of "direct to sshd, 24h". Without the window this
              // landed on every bypass ever recorded, which is a superset --
              // the same defect as an alert counting 5 and selecting 2, only
              // in the other direction.
              onClick={() =>
                navigate({ to: '/sessions', search: { show: 'direct', since: '24h' } })
              }
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
                live && (
                  <Badge size="xs" color="sky" variant="light">
                    {live.length}
                  </Badge>
                )
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
                            <SessionDuration session={s} />
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
                {stats ? (
                  <>
                    <Group justify="space-between" align="flex-end" mb={SP.snug}>
                      <Text size="xl" fw={600} lh={1}>{caCoverage}%</Text>
                      <Text size={FS.meta} c="dimmed">
                        {stats.assetsTotal - stats.standingCredentialAssets} / {stats.assetsTotal}
                      </Text>
                    </Group>
                    <Progress value={caCoverage} color="teal" size="sm" radius="xl" />
                    <Text size={FS.micro} c="dimmed" mt={SP.cozy} lh={1.5}>
                      The remaining {stats.standingCredentialAssets} use vaulted keys injected
                      by the gateway. Users never see them, but they are standing credentials —
                      move hosts to certificate auth where you can.
                    </Text>
                    {/* The sentence above is an instruction, and until now there
                        was nowhere to carry it out: the inventory had no filter
                        for "not on certificate auth", so the only way to find
                        these hosts was to read every row. */}
                    {stats.standingCredentialAssets > 0 && (
                      <ButtonLink
                        size="compact-xs"
                        variant="subtle"
                        color="slate"
                        mt={SP.snug}
                        to="/assets"
                        search={{ credential: 'standing' }}
                        rightSection={<IconArrowRight size={12} />}
                      >
                        Show these hosts
                      </ButtonLink>
                    )}
                  </>
                ) : (
                  <Skeleton height={46} radius="sm" />
                )}
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
