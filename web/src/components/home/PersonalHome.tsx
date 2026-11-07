import {
  Badge, Box, Card, Group, SimpleGrid, Skeleton, Text,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconActivityHeartbeat, IconClipboardCheck, IconDatabase, IconServer2, IconShieldCheck } from '@tabler/icons-react'
import { EnvBadge } from '~/components/ChangeImpact'
import { useChangesToReview } from '~/components/ChangesWaiting'
import { writesNeedApproval } from '~/lib/changeApproval'
import { AnchorLink, ButtonLink } from '~/components/links'
import { EmptyState, SectionCard } from '~/components/page'
import { Mono, duration, relTime } from '~/components/primitives'
import { ServerCard, ServerCardSkeleton } from '~/components/myservers'
import { Label } from '~/components/home/parts'
import { assetsQuery, databasesQuery, requestsQuery, sessionsQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { AccessRequest, User } from '~/types/domain'
import { isBroker } from '~/lib/engines'

/**
 * Home for someone who is given servers: theirs, with Connect and Open, what
 * they asked for, and what they did. Its own chunk, so an admin's Home does
 * not download the server cards it never draws.
 */
/* ── Someone's own servers, for everyone else ──────────────────────────── */

const levelWord = (l: string | undefined) => ({ read: 'Read', write: 'Write', owner: 'Owner', dba: 'DBA' } as Record<string, string>)[l ?? ''] ?? 'No access'


const SERVERS_SHOWN = 6

export default function PersonalHome({ me }: { me: User | undefined }) {
  const { data: assets, isPending } = useQuery(assetsQuery({}))
  const { data: dbList } = useQuery(databasesQuery())
  const dbs = dbList?.databases
  // Other people's changes this person may decide, by database.
  const { reqs: toReview } = useChangesToReview()
  const waitingOn = (id: string) => toReview.filter((r) => r.statement?.databaseId === id).length
  const editsWithApproval = (id: string) => {
    const d = dbs?.find((x) => x.id === id)
    const level = dbList?.access[id]?.level
    return !!d && writesNeedApproval(d) && d.policy?.writeApprovers === 'editors' && !!level && level !== 'read'
  }
  const shown = (assets ?? []).slice(0, SERVERS_SHOWN)

  return (
    <>
      <Box>
        <Group justify="space-between" align="baseline">
          <Label>Your servers</Label>
          {assets && assets.length > SERVERS_SHOWN && (
            <AnchorLink to="/assets" size={FS.meta}>All {assets.length} servers ›</AnchorLink>
          )}
        </Group>
        {isPending ? (
          <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm"><ServerCardSkeleton /><ServerCardSkeleton /></SimpleGrid>
        ) : shown.length === 0 ? (
          <Card padding="md">
            <EmptyState
              compact
              icon={IconServer2}
              title="No server is given to you yet"
              description="An admin gives you servers through a team. You can also ask for access to one."
              action={<ButtonLink to="/approvals" size="xs" variant="light">Ask for access</ButtonLink>}
            />
          </Card>
        ) : (
          <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm" data-testid="your-servers">
            {shown.map((a) => <ServerCard key={a.id} asset={a} />)}
          </SimpleGrid>
        )}
      </Box>

      {dbs && dbs.length > 0 && (
        <Box>
          <Group justify="space-between" align="center">
            <Label>Your databases</Label>
            {toReview.length > 0 && (
              <ButtonLink to="/approvals" size="compact-xs" variant="light" color="amber" leftSection={<IconShieldCheck size={12} />}>
                {toReview.length === 1 ? '1 change to review' : `${toReview.length} changes to review`}
              </ButtonLink>
            )}
          </Group>
          <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm">
            {dbs.slice(0, SERVERS_SHOWN).map((d) => (
              <Card key={d.id} padding="md">
                <Group justify="space-between" wrap="nowrap" gap="sm">
                  <Box style={{ minWidth: 0 }}>
                    <Group gap={SP.snug} wrap="nowrap">
                      <IconDatabase size={16} />
                      <Text fw={600} size="sm" truncate>{d.dbname}</Text>
                    </Group>
                    <Group gap={6} wrap="nowrap" mt={2}>
                      <EnvBadge label={d.label} size="xs" />
                      <Text size={FS.meta} c="dimmed" lineClamp={2}>
                        {levelWord(dbList?.access[d.id]?.level)}{editsWithApproval(d.id) ? " · you approve teammates' changes" : ` · ${engineName(d.engine)} on ${d.hostname}`}
                      </Text>
                    </Group>
                    {waitingOn(d.id) > 0 && (
                      <AnchorLink to="/approvals" size={FS.meta} fw={650}>{waitingOn(d.id)} waiting for you</AnchorLink>
                    )}
                  </Box>
                  <ButtonLink {...(isBroker(d.engine) ? { to: '/brokers/$brokerId', params: { brokerId: d.id } } as const : { to: '/databases/$dbId', params: { dbId: d.id } } as const)}
                    size="compact-sm" style={{ flexShrink: 0 }}>
                    Open
                  </ButtonLink>
                </Group>
              </Card>
            ))}
          </SimpleGrid>
        </Box>
      )}

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="sm">
        <YourRequests me={me} />
        <YourSessions />
      </SimpleGrid>
    </>
  )
}

const REQUEST_STATE: Record<AccessRequest['state'], { label: string; color: string }> = {
  pending: { label: 'Waiting', color: 'amber' },
  approved: { label: 'Approved', color: 'teal' },
  denied: { label: 'Denied', color: 'rose' },
  expired: { label: 'Expired', color: 'slate' },
  revoked: { label: 'Revoked', color: 'slate' },
  cancelled: { label: 'Cancelled', color: 'slate' },
}

function YourRequests({ me }: { me: User | undefined }) {
  const { data, isPending } = useQuery(requestsQuery())
  // An approver is sent everyone's; this card is about their own.
  const mine = (data ?? [])
    .filter((r) => me && r.requesterEmail.toLowerCase() === me.email.toLowerCase())
    .sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))
    .slice(0, 4)
  return (
    <SectionCard
      title="Your requests"
      icon={IconClipboardCheck}
      iconColor="amber"
      action={<AnchorLink to="/approvals" size={FS.meta}>Ask for access ›</AnchorLink>}
      flush
    >
      {isPending ? <Box px="md" pb="md"><Skeleton h={40} /></Box> : mine.length === 0 ? (
        <EmptyState compact title="You have not asked for access" />
      ) : (
        <Box data-testid="your-requests">
          {mine.map((r) => {
            const st = REQUEST_STATE[r.state]
            return (
              <Group key={r.id} justify="space-between" wrap="nowrap" gap="sm" px="md" py={SP.cozy}
                style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
                <Box style={{ minWidth: 0 }}>
                  <Text size={FS.body} fw={550} truncate><Mono>{r.principal}</Mono> on {r.assetHostnames.join(', ')}</Text>
                  <Text size={FS.meta} c="dimmed" truncate>{requestLine(r)}</Text>
                </Box>
                <Badge color={st.color} variant="light" style={{ flexShrink: 0 }}>{st.label}</Badge>
              </Group>
            )
          })}
        </Box>
      )}
    </SectionCard>
  )
}

function requestLine(r: AccessRequest): string {
  if (r.state === 'pending') return `asked ${relTime(r.createdAt)}`
  if (r.state === 'approved') {
    const until = r.expiresAt ? `until ${new Date(r.expiresAt).toLocaleString('en-GB', { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' })}` : ''
    return [until, r.decidedByEmail ? `by ${r.decidedByEmail}` : ''].filter(Boolean).join(', ')
  }
  return r.decidedByEmail ? `${r.state} by ${r.decidedByEmail}` : r.state
}

function YourSessions() {
  const { data, isPending } = useQuery(sessionsQuery())
  const recent = (data ?? []).slice().sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt)).slice(0, 5)
  return (
    <SectionCard
      title="Your recent sessions"
      icon={IconActivityHeartbeat}
      iconColor="sky"
      action={<AnchorLink to="/activity" size={FS.meta}>My activity ›</AnchorLink>}
      flush
    >
      {isPending ? <Box px="md" pb="md"><Skeleton h={40} /></Box> : recent.length === 0 ? (
        <EmptyState compact title="No sessions yet" />
      ) : (
        <Box data-testid="your-sessions">
          {recent.map((s) => (
            <Group key={s.id} gap="md" wrap="nowrap" px="md" py={SP.cozy}
              style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
              <Text size={FS.meta} c="dimmed" w={70} style={{ flexShrink: 0 }}>{relTime(s.startedAt)}</Text>
              <AnchorLink to="/sessions/$sessionId" params={{ sessionId: s.id }} size={FS.meta} c="inherit" style={{ minWidth: 0 }}>
                <Text span inherit truncate>{s.assetHostname} as <Mono>{s.principal}</Mono></Text>
              </AnchorLink>
              <Text size={FS.meta} c="dimmed" ml="auto" style={{ flexShrink: 0 }}>
                {s.state === 'active' ? 'live' : duration(s.startedAt, s.endedAt)}
              </Text>
            </Group>
          ))}
        </Box>
      )}
    </SectionCard>
  )
}


function engineName(e: string): string {
  return e === 'postgres' ? 'PostgreSQL' : e === 'mysql' ? 'MySQL' : e === 'mongodb' ? 'MongoDB' : e === 'redis' ? 'Redis' : e === 'rabbitmq' ? 'RabbitMQ' : e === 'kafka' ? 'Kafka' : e
}
