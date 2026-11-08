import { Anchor, Badge, Box, Card, Grid, Group, SimpleGrid, Stack, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconHistory } from '@tabler/icons-react'
import { reachOf } from '~/components/myservers'
import { EmptyState, SectionCard } from '~/components/page'
import {
  HostKeyBadge, Mono, SessionDuration, SessionStateBadge, Stat, absTime, relTime, rowNav,
} from '~/components/primitives'
import { HostKeyUnpinned } from '~/components/server/hostKey'
import { LoginAccounts } from '~/components/server/layout'
import { SystemCard, systemQuery } from '~/components/server/SystemCard'
import type { ServerTab } from '~/components/server/tabs'
import { directSshLabel } from '~/lib/directSsh'
import { agentUpdateQuery, assignmentsQuery, policyQuery, teamAssignmentsQuery } from '~/lib/queries'
import { FS, SP, INK } from '~/theme'
import type { Asset, Session } from '~/types/domain'

const WEEK = 7 * 24 * 60 * 60 * 1000

/**
 * Whether this server is all right, at a glance: four facts, what the
 * machine is and how busy it is, the last few sessions and the essentials. Each tile opens the tab that owns its detail.
 */
export function OverviewTab({ asset, sessions, onTab, canFreeSpace = false }: {
  asset: Asset
  /** This server's sessions, newest first; undefined while loading. */
  sessions: Session[] | undefined
  onTab: (t: ServerTab) => void
  /** Whether a filling disk links to the Logs tab. */
  canFreeSpace?: boolean
}) {
  const navigate = useNavigate()
  const { data: people } = useQuery(assignmentsQuery(asset.id))
  const { data: teams } = useQuery(teamAssignmentsQuery(asset.id))
  const { data: policy } = useQuery(policyQuery())
  const { data: agent } = useQuery(agentUpdateQuery(asset.id))

  // Computed against the browser clock, which is fine for "this week": it is
  // a rough count, and the Sessions tab lists the rows it is counted from.
  const since = Date.now() - WEEK
  const week = sessions?.filter((s) => Date.parse(s.startedAt) >= since)
  const live = sessions?.filter((s) => s.state === 'active' && !s.silent).length
  const direct = week?.filter((s) => s.origin === 'direct').length

  const teamCount = teams?.length
  const personCount = people?.length
  const given = teamCount === undefined || personCount === undefined ? undefined : teamCount + personCount

  const effective = asset.directSsh ?? policy?.directSsh ?? null
  const recent = (sessions ?? []).slice(0, 6)

  return (
    <Stack gap="md">
      <HostKeyUnpinned asset={asset} />

      <SimpleGrid cols={{ base: 2, md: 4 }} spacing={{ base: 'xs', sm: 'sm' }}>
        <Stat
          label="Status"
          value={reachLabel(asset)}
          sub={`Checked ${relTime(asset.lastCheckedAt)}`}
          tone={reachLabel(asset) === 'Online' ? 'ok' : 'warn'}
        />
        <Stat
          label="Sessions this week"
          value={week?.length}
          sub={live === undefined ? undefined : [
            live > 0 ? `${live} live now` : 'None live now',
            direct ? `${direct} direct` : null,
          ].filter(Boolean).join(' · ')}
          onClick={() => onTab('sessions')}
        />
        <Stat
          label="Who can connect"
          value={given === undefined ? undefined : given === 0 ? 'Admins' : given}
          sub={given === undefined ? undefined : given === 0
            ? 'Nobody else yet'
            : `${plural(teamCount!, 'team')}, ${plural(personCount!, 'person', 'people')}, plus admins`}
          onClick={() => onTab('access')}
        />
        <Stat
          label="Agent"
          value={agent?.agentVersion || agentStateLabel(asset)}
          sub={agent?.agentVersion
            ? agent.latest && agent.latest !== agent.agentVersion ? `${agent.latest} available` : 'Up to date'
            : asset.agentLastSeenAt ? `Checked in ${relTime(asset.agentLastSeenAt)}` : 'Never checked in'}
          tone={asset.agentState === 'healthy' ? 'ok' : 'warn'}
          onClick={() => onTab('agent')}
        />
      </SimpleGrid>

      <SystemCard asset={asset} onTab={onTab} canFreeSpace={canFreeSpace} />

      <Grid gap="md">
        <Grid.Col span={{ base: 12, lg: 8 }}>
          <SectionCard
            title="Recent sessions"
            icon={IconHistory}
            flush
            action={
              sessions && sessions.length > 0 ? (
                <Anchor component="button" type="button" size={FS.meta} fw={500} onClick={() => onTab('sessions')}>
                  See all {sessions.length}
                </Anchor>
              ) : undefined
            }
          >
            {sessions && recent.length === 0 ? (
              <EmptyState
                compact
                icon={IconHistory}
                title="Nobody has connected to this host yet."
                description="Sessions through Argus, and direct logins the agent reports, appear here."
              />
            ) : (
              <Table>
                <Table.Tbody>
                  {recent.map((s) => (
                    <Table.Tr
                      key={s.id}
                      {...rowNav(() => void navigate({ to: '/sessions/$sessionId', params: { sessionId: s.id } }))}
                    >
                      <Table.Td w={1}>
                        <SessionStateBadge state={s.state} silent={s.silent} lastReportedAt={s.lastReportedAt} endInferred={s.endInferred} />
                      </Table.Td>
                      <Table.Td>
                        <Text size={FS.meta} fw={500}>{s.userEmail.split('@')[0]}</Text>
                        <Text size={FS.micro} c="dimmed" hiddenFrom="sm" title={absTime(s.startedAt)}>{relTime(s.startedAt)}</Text>
                      </Table.Td>
                      <Table.Td visibleFrom="sm"><Mono c={INK.brand}>{s.principal}</Mono></Table.Td>
                      <Table.Td visibleFrom="sm">
                        <Text size={FS.meta} c="dimmed" title={absTime(s.startedAt)}>{relTime(s.startedAt)}</Text>
                      </Table.Td>
                      <Table.Td ta="right"><SessionDuration session={s} /></Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )}
          </SectionCard>
        </Grid.Col>

        <Grid.Col span={{ base: 12, lg: 4 }}>
          <Card padding="md" h="100%">
            <Text component="h2" fw={600} size="sm" style={{ margin: 0 }}>About</Text>
            <Stack gap="sm" mt="sm">
              <Fact label="Address"><Mono>{asset.address}{asset.port ? `:${asset.port}` : ''}</Mono></Fact>
              <Fact label="Connects by">
                <Text size={FS.meta}>{connectsBy(asset)}</Text>
              </Fact>
              <Fact label={asset.kind === 'android' ? 'Opened as' : 'Login accounts'}><LoginAccounts asset={asset} /></Fact>
              {asset.kind === 'android' && <DeviceSecurity asset={asset} />}
              {asset.protocol !== 'rdp' && (
                <Fact label="Direct logins">
                  <Text size={FS.meta}>
                    {effective ? directSshLabel(effective) : '…'}
                    {!asset.directSsh && <Text span size={FS.micro} c="dimmed"> (from Settings)</Text>}
                  </Text>
                </Fact>
              )}
              <Fact label="Host key"><HostKeyBadge state={asset.hostKeyState} /></Fact>
              {asset.tags.length > 0 && (
                <Fact label="Tags">
                  <Group gap={SP.tight}>
                    {asset.tags.map((t) => <Badge key={t} size="xs" variant="outline" color="slate">{t}</Badge>)}
                  </Group>
                </Fact>
              )}
            </Stack>
          </Card>
        </Grid.Col>
      </Grid>
    </Stack>
  )
}

/**
 * The two facts a device's security turns on that a server has no equal to:
 * which engine drives it, and whether wireless debugging — a shell-level
 * door that listens on every interface on Android 11+ — is on. Read from
 * the same system call the card uses, so it says what the device reports,
 * not what the console set.
 */
export function DeviceSecurity({ asset }: { asset: Asset }) {
  const { data } = useQuery(systemQuery(asset))
  const sys = data?.system
  const engine = sys?.engine
  return (
    <>
      <Fact label="Engine">
        <Text size={FS.meta}>
          {engine === 'unattended' ? 'Unattended (the helper, nobody there)'
            : engine === 'attended' ? 'Attended (accepted on the device)'
            : '…'}
        </Text>
      </Fact>
      {sys?.wirelessDebugging && (
        <Fact label="Wireless debugging">
          <Text size={FS.meta} c="amber.7" fw={500}>On — listens on every interface</Text>
        </Fact>
      )}
    </>
  )
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <Text size={FS.meta} c="dimmed" w={110} style={{ flexShrink: 0 }}>{label}</Text>
      <Box style={{ minWidth: 0 }}>{children}</Box>
    </Group>
  )
}

/** How a session reaches it, in the words the Settings form uses. */
export function connectsBy(a: Asset): string {
  if (a.kind === 'android') {
    return a.controlMode === 'unattended'
      ? 'The Argus app, unattended'
      : 'The Argus app, accepted on the device'
  }
  if (a.protocol === 'rdp') return a.desktopMode === 'screen' ? 'Screen sharing (no RDP)' : 'Remote Desktop (RDP)'
  return 'SSH'
}

function reachLabel(a: Asset): string {
  return { online: 'Online', slow: 'Slow', offline: 'Offline', blocked: 'Blocked' }[reachOf(a)]
}

export function agentStateLabel(a: Asset): string {
  return { healthy: 'Connected', stale: 'Quiet', absent: 'None' }[a.agentState]
}

function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`
}
