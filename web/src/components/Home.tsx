import { Suspense, lazy, type ComponentType, type ReactNode } from 'react'
import {
  Badge, Box, Card, Group, SimpleGrid, Skeleton, Stack, Text, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { AnchorLink, ButtonLink } from '~/components/links'
import {
  IconActivityHeartbeat, IconCircleCheck, IconClipboardCheck, IconServerBolt, type IconProps,
} from '@tabler/icons-react'
import { PageBody, PageHeader, SectionCard, EmptyState } from '~/components/page'
import { Mono, Stat, duration, now, relTime } from '~/components/primitives'
import { isStatus, Label } from '~/components/home/parts'
import { isConfigured } from '~/lib/live'
import {
  controlStatusQuery, coverageQuery, directLoginsQuery, meQuery, recentAuditQuery,
  requestsQuery, sessionsQuery, statsQuery,
} from '~/lib/queries'
import { canSee, seesWholeFleet } from '~/lib/roles'
import { FS, SP } from '~/theme'
import type { AuditEvent, Session, User } from '~/types/domain'

// Each drawn for one kind of reader only, so each is its own download.
const ControlCard = lazy(() => import('~/components/home/ControlCard'))
const PersonalHome = lazy(() => import('~/components/home/PersonalHome'))

/**
 * Home: where the console opens, and what it says depends on who is signed in.
 *
 * - Someone who sees the whole fleet (owner, admin, auditor) gets what needs
 *   them, the fleet in four numbers, who is on a server right now, and what
 *   happened today. A platform manager also gets argus-control's own health,
 *   summarised from Settings › Status.
 * - Everyone else gets their own servers and databases with Connect and Open,
 *   their access requests, and their recent sessions. An approver also gets
 *   "Needs you" with their approvals. Nothing about the fleet.
 *
 * Every list is the one the control plane already scopes by role; this page
 * decides only which cards to draw. "Needs you" only shows cards with
 * something in them, and says so in one line when nothing is waiting.
 */
export function Home() {
  const { data: me } = useQuery(meQuery())
  const whole = seesWholeFleet(me?.role)
  const approves = canSee(me?.role, 'approvers')

  return (
    <>
      <PageHeader
        title={greeting(me)}
        description={subtitle(me)}
        actions={canSee(me?.role, 'admin') ? (
          <ButtonLink to="/assets/add" variant="default" size="sm">Add server</ButtonLink>
        ) : undefined}
      />
      <PageBody>
        <Stack gap="lg">
          {me && approves && <NeedsYou me={me} />}
          {/* Nothing until we know who this is: guessing would fetch, and
              flash, the other reader's page. */}
          {!me ? <Skeleton h={180} radius="md" /> : whole ? <FleetHome me={me} /> : (
            <Suspense fallback={<Skeleton h={180} radius="md" />}><PersonalHome me={me} /></Suspense>
          )}
        </Stack>
      </PageBody>
    </>
  )
}

/* ── Who and when ──────────────────────────────────────────────────────── */

function greeting(me: User | undefined): string {
  const h = new Date(now()).getHours()
  const part = h < 12 ? 'Good morning' : h < 18 ? 'Good afternoon' : 'Good evening'
  const name = me?.displayName?.split(' ')[0]
  return name ? `${part}, ${name}` : part
}

function subtitle(me: User | undefined): string {
  const day = new Date(now()).toLocaleDateString('en-GB', { weekday: 'long', day: 'numeric', month: 'long' })
  return me?.org && !me.org.default ? `${day} · ${me.org.name}` : day
}

/* ── Needs you ─────────────────────────────────────────────────────────── */

interface Need {
  key: string
  icon: ComponentType<IconProps>
  color: string
  title: string
  detail: string
  /** Where it is dealt with: a link, so it opens in a new tab like any other. */
  action: ReactNode
}


function NeedsYou({ me }: { me: User | undefined }) {
  const admin = canSee(me?.role, 'admin')
  const platform = Boolean(me?.platformOwner) && isConfigured()
  const { data: stats } = useQuery(statsQuery())
  const { data: held } = useQuery(directLoginsQuery())
  const { data: pending } = useQuery(requestsQuery('pending'))
  const { data: cov } = useQuery({ ...coverageQuery(), enabled: admin })
  const { data: status } = useQuery({ ...controlStatusQuery(), enabled: platform })

  if (!stats) return <Skeleton h={76} radius="md" />

  const heldNow = held?.filter((d) => d.state === 'pending') ?? []
  const waiting = stats.requestsPending + heldNow.length
  const needs: Need[] = []
  if (waiting > 0) {
    const first = pending?.[0]
    needs.push({
      key: 'approvals', icon: IconClipboardCheck, color: 'amber',
      title: `${waiting} ${waiting === 1 ? 'request is' : 'requests are'} waiting on you`,
      detail: heldNow.length > 0
        ? `${heldNow.length} held at a terminal right now`
        : first ? `${first.requesterEmail} asks for ${first.principal} on ${first.assetHostnames.join(', ')} · ${relTime(first.createdAt)}` : 'Someone is waiting for a decision',
      action: <ButtonLink variant="subtle" size="compact-sm" style={{ flexShrink: 0 }} to="/approvals">Review ›</ButtonLink>,
    })
  }
  const unreviewed = cov?.unreviewedHosts ?? 0
  if (admin && unreviewed > 0) {
    needs.push({
      key: 'servers', icon: IconServerBolt, color: 'azure',
      title: `${unreviewed} ${unreviewed === 1 ? 'server is' : 'servers are'} waiting to be added`,
      detail: 'Their agents checked in and nobody has finished adding them',
      action: <ButtonLink variant="subtle" size="compact-sm" style={{ flexShrink: 0 }} to="/assets">Finish ›</ButtonLink>,
    })
  }
  const problems = isStatus(status) ? status.problems : []
  if (problems.length > 0) {
    const critical = problems.some((p) => p.severity === 'critical')
    needs.push({
      key: 'control', icon: IconActivityHeartbeat, color: critical ? 'rose' : 'amber',
      title: problems.length === 1 ? `argus-control: ${lowerFirst(problems[0]!.title)}` : `argus-control has ${problems.length} problems`,
      detail: problems.length === 1 ? (problems[0]!.detail ?? '') : problems.map((p) => p.title).slice(0, 2).join(' · '),
      action: <ButtonLink variant="subtle" size="compact-sm" style={{ flexShrink: 0 }} to="/settings" search={{ section: 'status' }}>Status ›</ButtonLink>,
    })
  }

  if (needs.length === 0) {
    return (
      <Group gap={SP.cozy} data-testid="needs-you-clear">
        <IconCircleCheck size={18} color="var(--mantine-color-teal-6)" />
        <Text size={FS.body} c="dimmed">Nothing is waiting on you.</Text>
      </Group>
    )
  }
  return (
    <Box>
      <Label>Needs you</Label>
      <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm" data-testid="needs-you">
        {needs.map((n) => <NeedCard key={n.key} need={n} />)}
      </SimpleGrid>
    </Box>
  )
}

function NeedCard({ need: n }: { need: Need }) {
  return (
    <Card padding="md" data-need={n.key}>
      <Group gap="sm" wrap="nowrap" align="flex-start">
        <ThemeIcon variant="light" color={n.color} size={34} radius="md">
          <n.icon size={18} />
        </ThemeIcon>
        <Box style={{ flex: 1, minWidth: 0 }}>
          <Text fw={600} size="sm" lh={1.35}>{n.title}</Text>
          {n.detail && <Text size={FS.meta} c="dimmed" mt={SP.hair} lh={1.4} lineClamp={2}>{n.detail}</Text>}
        </Box>
        {n.action}
      </Group>
    </Card>
  )
}

/* ── The fleet, for whoever sees all of it ─────────────────────────────── */

function FleetHome({ me }: { me: User | undefined }) {
  const navigate = useNavigate()
  const { data: stats } = useQuery(statsQuery())
  const platform = Boolean(me?.platformOwner) && isConfigured()

  const online = stats ? stats.assetsTotal - stats.assetsUnreachable : undefined
  return (
    <>
      <Box>
        <Label>Fleet</Label>
        <SimpleGrid cols={{ base: 2, md: 4 }} spacing="sm">
          <Stat
            label="Servers online"
            value={stats ? `${online} of ${stats.assetsTotal}` : undefined}
            sub={stats ? (stats.assetsUnreachable > 0 ? `${stats.assetsUnreachable} not reachable` : 'Every server answers') : undefined}
            tone={stats && stats.assetsUnreachable > 0 ? 'warn' : 'ok'}
            onClick={() => void navigate({ to: '/assets' })}
          />
          <Stat
            label="Live sessions"
            value={stats?.sessionsActive}
            sub={stats ? (stats.sessionsSilent > 0 ? `${stats.sessionsSilent} gone silent` : 'Every one reporting') : undefined}
            tone={stats && stats.sessionsSilent > 0 ? 'warn' : 'ok'}
            onClick={() => void navigate({ to: '/activity' })}
          />
          <Stat
            label="Sessions, last 24 h"
            value={stats?.sessionsToday}
            sub={stats ? (stats.sessionsDirectToday > 0 ? `${stats.sessionsDirectToday} straight to a server` : 'All through the console') : undefined}
            tone={stats && stats.sessionsDirectToday > 0 ? 'warn' : 'ok'}
          />
          <Stat
            label="Agents not reporting"
            value={stats?.agentsStale}
            sub={stats ? (stats.assetsUnmonitored > 0 ? `${stats.assetsUnmonitored} unmonitored if bypassed` : 'Every server is watched') : undefined}
            tone={stats && stats.agentsStale > 0 ? 'warn' : 'ok'}
            onClick={() => void navigate({ to: '/settings', search: { section: 'hosts' } })}
          />
        </SimpleGrid>
      </Box>

      <Box>
        <Label>Now</Label>
        <SimpleGrid cols={{ base: 1, md: platform ? 2 : 1 }} spacing="sm">
          <LiveNow />
          {platform && <Suspense fallback={<Skeleton h={240} radius="md" />}><ControlCard /></Suspense>}
        </SimpleGrid>
      </Box>

      <Today />
    </>
  )
}

const LIVE_SHOWN = 5

function LiveNow() {
  const { data, isPending } = useQuery(sessionsQuery('active'))
  const live = (data ?? []).filter((s) => !s.silent)
  return (
    <SectionCard
      title="Live now"
      icon={IconActivityHeartbeat}
      iconColor="sky"
      badge={live.length > 0 ? <Badge color="sky" variant="light">{live.length}</Badge> : undefined}
      action={<AnchorLink to="/activity" size={FS.meta}>All sessions ›</AnchorLink>}
      flush
    >
      {isPending ? (
        <Stack gap="xs" px="md" pb="md"><Skeleton h={36} /><Skeleton h={36} /></Stack>
      ) : live.length === 0 ? (
        <EmptyState compact title="Nobody is on a server right now" />
      ) : (
        <Box data-testid="live-now">
          {live.slice(0, LIVE_SHOWN).map((s) => <LiveRow key={s.id} s={s} />)}
          {live.length > LIVE_SHOWN && (
            <Text size={FS.meta} c="dimmed" px="md" py="xs">and {live.length - LIVE_SHOWN} more</Text>
          )}
        </Box>
      )}
    </SectionCard>
  )
}

function LiveRow({ s }: { s: Session }) {
  const root = s.principal === 'root' || s.principal.toLowerCase() === 'administrator'
  const kind = s.protocol === 'rdp' ? 'Desktop' : s.protocol === 'postgres' ? 'Database' : s.protocol === 'sftp' ? 'Files' : 'SSH'
  return (
    <Group
      gap="sm" wrap="nowrap" px="md" py={SP.cozy}
      style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}
    >
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Text size={FS.body} truncate>
          <Text span inherit fw={550}>{s.userEmail.split('@')[0]}</Text>
          <Text span inherit c="dimmed"> as </Text>
          <Mono>{s.principal}</Mono>
          {root && <Badge ml={SP.snug} size="xs" color="rose" variant="light">{s.principal === 'root' ? 'root' : 'admin'}</Badge>}
        </Text>
        <Text size={FS.meta} c="dimmed" truncate>
          {s.assetHostname} · {kind} · {duration(s.startedAt, null)}
        </Text>
      </Box>
      <ButtonLink
        to="/sessions/$sessionId" params={{ sessionId: s.id }}
        variant="default" size="compact-sm" style={{ flexShrink: 0 }}
      >
        Watch
      </ButtonLink>
    </Group>
  )
}

const ACTION_WORDS: Partial<Record<AuditEvent['action'], string>> = {
  'session.start': 'opened a session on',
  'session.end': 'ended a session on',
  'session.terminate': 'stopped a session on',
  'file.upload': 'uploaded a file to',
  'file.download': 'downloaded a file from',
  'files.opened': 'opened files on',
  'request.create': 'asked for access to',
  'request.approve': 'approved access to',
  'request.deny': 'denied access to',
  'credential.rotate': 'rotated the credential on',
  'hostkey.pin': 'pinned the host key of',
  'hostkey.mismatch': 'saw a changed host key on',
  'asset.enrolled': 'added',
  'asset.dismissed': 'dismissed',
  'session.direct_detected': 'logged in straight to',
  'session.direct_refused': 'was refused a direct login to',
  'agent.went_silent': 'lost contact with the agent on',
  'unmanaged_key.found': 'found an unmanaged key on',
  'unmanaged_key.revoked': 'revoked an unmanaged key on',
  'policy.change': 'changed the policy for',
  'auth.login': 'signed in',
  'auth.mfa_fail': 'failed a second-factor check',
}

function Today() {
  const { data, isPending } = useQuery(recentAuditQuery(6))
  return (
    <SectionCard
      title="Latest in the audit log"
      icon={IconClipboardCheck}
      iconColor="violet"
      action={<AnchorLink to="/audit" size={FS.meta}>Audit log ›</AnchorLink>}
      flush
    >
      {isPending ? (
        <Stack gap="xs" px="md" pb="md"><Skeleton h={20} /><Skeleton h={20} /><Skeleton h={20} /></Stack>
      ) : !data || data.length === 0 ? (
        <EmptyState compact title="Nothing has been recorded yet" />
      ) : (
        <Box data-testid="recent-audit">
          {data.map((e) => (
            <Group key={e.seq} gap="md" wrap="nowrap" align="flex-start" px="md" py={SP.cozy}
              style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
              <Text size={FS.meta} c="dimmed" w={70} style={{ flexShrink: 0 }}>{relTime(e.at)}</Text>
              <Box style={{ minWidth: 0, flex: 1 }}>
                <Text size={FS.meta} fw={550} truncate>
                  {e.actorEmail || 'argus'}
                  {e.severity === 'critical' && <Badge ml={SP.snug} size="xs" color="rose" variant="light">critical</Badge>}
                </Text>
                {/* The control plane writes each entry's detail as a sentence;
                    the action's code is only for an entry that has none. */}
                <Text size={FS.meta} c="dimmed" lineClamp={2} style={{ overflowWrap: 'anywhere' }}>
                  {e.detail || <>{ACTION_WORDS[e.action] ?? <Mono>{e.action}</Mono>} {e.target}</>}
                </Text>
              </Box>
            </Group>
          ))}
        </Box>
      )}
    </SectionCard>
  )
}


function lowerFirst(s: string): string {
  return s ? s[0]!.toLowerCase() + s.slice(1) : s
}

