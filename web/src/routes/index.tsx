import {
  Alert, Badge, Box, Card, Grid, Group, Progress, ScrollArea, Stack, Table,
  Text, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconArrowRight, IconCertificate, IconClipboardCheck,
  IconDoorExit, IconShieldOff, IconTerminal2,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { ButtonLink } from '~/components/links'
import {
  Digest, FidelityBadge, HealthDot, Mono, OriginBadge, RiskFlags, Stat, duration,
  relTime, rowNav,
} from '~/components/primitives'
import { FS } from '~/theme'
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

  return (
    <Box>
      <PageHeader
        title="Overview"
        description="Fleet posture and everything currently in flight."
      />

      <Box p="lg">
        {/* Recording coverage comes first. An unverified host is a risk you can
            see; an unmonitored one is a session you will never hear about. */}
        {(stats?.assetsUnmonitored ?? 0) > 0 && (
          <Alert
            color="rose"
            variant="light"
            icon={<IconDoorExit size={17} />}
            mb="md"
            title={`${stats?.assetsUnmonitored} host${stats?.assetsUnmonitored === 1 ? '' : 's'} would not record a bypass`}
          >
            <Text size="xs" mb="xs">
              These hosts have no agent and no lockdown. Anyone with a standing key can
              connect straight to sshd on port 22 and Argus will never know the session
              happened. Until an agent is installed, your recording coverage is not the
              fleet — it is only the part of it that chooses to use the gateway.
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
              <ButtonLink
                size="compact-xs"
                variant="subtle"
                color="rose"
                to="/assets"
                rightSection={<IconArrowRight size={12} />}
              >
                Review coverage
              </ButtonLink>
            </Group>
          </Alert>
        )}

        {/* Host-key problems are the one thing that must never be buried in a
            table — Argus terminates SSH, so an unverified target means nobody
            verified it. */}
        {changed.length > 0 && (
          <Alert
            color="rose"
            variant="light"
            icon={<IconAlertTriangle size={17} />}
            mb="md"
            title={`${changed.length} host key${changed.length > 1 ? 's' : ''} changed`}
          >
            <Text size="xs" mb="xs">
              The key presented by {changed.length > 1 ? 'these hosts' : 'this host'} no longer
              matches the pinned fingerprint. Argus is refusing connections until an admin
              re-verifies out of band. This is either a rebuild or an active interception.
            </Text>
            <Group gap="xs">
              {changed.slice(0, 4).map((a) => (
                <Badge key={a.id} color="rose" variant="outline" className="argus-digest">
                  {a.hostname.split('.')[0]}
                </Badge>
              ))}
              <ButtonLink
                size="compact-xs"
                variant="subtle"
                color="rose"
                to="/assets"
                rightSection={<IconArrowRight size={12} />}
              >
                Review
              </ButtonLink>
            </Group>
          </Alert>
        )}

        <Grid gap="sm" mb="lg">
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
            <Card padding={0}>
              <Group justify="space-between" p="md" pb="sm">
                <Group gap={8}>
                  <Text fw={600} size="sm">Live sessions</Text>
                  <Badge size="xs" color="sky" variant="light">
                    {live?.length ?? 0}
                  </Badge>
                </Group>
                <ButtonLink
                  size="compact-xs"
                  variant="subtle"
                  color="slate"
                  to="/sessions"
                  rightSection={<IconArrowRight size={12} />}
                >
                  All sessions
                </ButtonLink>
              </Group>

              <ScrollArea.Autosize mah={330}>
                <Table.ScrollContainer minWidth={760} type="native">
            <Table verticalSpacing={7} horizontalSpacing="md" highlightOnHover>
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
                          <Mono>
                            <Text span c="teal.4" inherit>{s.principal}</Text>
                            <Text span c="dimmed" inherit>@</Text>
                            {s.assetHostname.split('.')[0]}
                          </Mono>
                        </Table.Td>
                        <Table.Td><OriginBadge origin={s.origin} /></Table.Td>
                        <Table.Td>
                          <Text size="xs" c="dimmed">{duration(s.startedAt, null)}</Text>
                        </Table.Td>
                        <Table.Td><FidelityBadge fidelity={s.fidelity} /></Table.Td>
                        <Table.Td><RiskFlags flags={s.riskFlags} /></Table.Td>
                      </Table.Tr>
                    ))}
                    {live?.length === 0 && (
                      <Table.Tr>
                        <Table.Td colSpan={6}>
                          <Text size="xs" c="dimmed" ta="center" py="lg">
                            Nobody is connected right now.
                          </Text>
                        </Table.Td>
                      </Table.Tr>
                    )}
                  </Table.Tbody>
                </Table>
            </Table.ScrollContainer>
              </ScrollArea.Autosize>
            </Card>
          </Grid.Col>

          <Grid.Col span={{ base: 12, lg: 4 }}>
            <Stack gap="sm">
              <Card padding="md">
                <Group gap={8} mb={4}>
                  <IconCertificate size={15} className="text-teal-400" />
                  <Text fw={600} size="sm">Zero standing privilege</Text>
                </Group>
                <Text size="xs" c="dimmed" mb="sm">
                  Assets where Argus mints a short-lived certificate per session, so no
                  reusable credential exists to steal.
                </Text>
                <Group justify="space-between" mb={6}>
                  <Text size="xl" fw={600} lh={1}>{caCoverage}%</Text>
                  <Text size="xs" c="dimmed">
                    {stats ? stats.assetsTotal - stats.standingCredentialAssets : 0} / {stats?.assetsTotal ?? 0}
                  </Text>
                </Group>
                <Progress value={caCoverage} color="teal" size="sm" radius="xl" />
                <Text size={FS.micro} c="dimmed" mt={8} lh={1.4}>
                  The remaining {stats?.standingCredentialAssets ?? 0} use vaulted keys injected
                  by the gateway. Users never see them, but they are standing credentials —
                  move hosts to certificate auth where you can.
                </Text>
              </Card>

              <Card padding={0}>
                <Group justify="space-between" p="md" pb="xs">
                  <Text fw={600} size="sm">Awaiting approval</Text>
                  <ButtonLink
                    size="compact-xs"
                    variant="subtle"
                    color="slate"
                    to="/requests"
                    rightSection={<IconArrowRight size={12} />}
                  >
                    Queue
                  </ButtonLink>
                </Group>
                <Stack gap={0}>
                  {pending?.slice(0, 4).map((r) => (
                    <Box
                      key={r.id}
                      px="md"
                      py={10}
                      style={{ borderTop: '1px solid var(--color-line)' }}
                    >
                      <Group justify="space-between" wrap="nowrap" mb={4}>
                        <Text size="xs" fw={500} truncate>
                          {r.requesterEmail.split('@')[0]}
                        </Text>
                        <Group gap={6} wrap="nowrap">
                          {r.breakGlass && (
                            <Tooltip label="Break-glass path — separate approval chain">
                              <Badge size="xs" color="rose">break-glass</Badge>
                            </Tooltip>
                          )}
                          <Text size={FS.micro} c="dimmed">{relTime(r.createdAt)}</Text>
                        </Group>
                      </Group>
                      <Text size={FS.micro} c="dimmed" lineClamp={2}>{r.justification}</Text>
                      <Group gap={6} mt={6}>
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
                    <Text size="xs" c="dimmed" ta="center" py="lg">Queue is clear.</Text>
                  )}
                </Stack>
              </Card>
            </Stack>
          </Grid.Col>
        </Grid>

        <Card padding={0} mt="sm">
          <Group justify="space-between" p="md" pb="sm">
            <Text fw={600} size="sm">Fleet health</Text>
            <ButtonLink
              size="compact-xs"
              variant="subtle"
              color="slate"
              to="/assets"
              rightSection={<IconArrowRight size={12} />}
            >
              Inventory
            </ButtonLink>
          </Group>
          <ScrollArea.Autosize mah={260}>
            <Table.ScrollContainer minWidth={760} type="native">
            <Table verticalSpacing={6} horizontalSpacing="md" highlightOnHover>
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
                {assets
                  ?.filter((a) => a.health !== 'reachable' || a.hostKeyState !== 'pinned')
                  .slice(0, 8)
                  .map((a) => (
                    <Table.Tr key={a.id}>
                      <Table.Td>
                        <Group gap={8} wrap="nowrap">
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
          </ScrollArea.Autosize>
        </Card>
      </Box>
    </Box>
  )
}
