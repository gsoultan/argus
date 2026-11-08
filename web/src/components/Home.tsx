import { Suspense, lazy, useEffect, type ComponentType, type ReactNode } from 'react'
import {
  Box, Card, Group, SimpleGrid, Skeleton, Stack, Text, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { ButtonLink } from '~/components/links'
import {
  IconActivityHeartbeat, IconCircleCheck, IconClipboardCheck, IconDatabaseExclamation, IconServerBolt,
  IconShieldExclamation, type IconProps,
} from '@tabler/icons-react'
import { PageBody } from '~/components/page'
import { Mono, now, relTime } from '~/components/primitives'
import { isStatus, Label } from '~/components/home/parts'
import { CommandBand } from '~/components/home/Command'
import { FleetHealth, HappeningNow, HomeTiles, LiveNow, LivePill, SessionsChart, useLiveNeeds } from '~/components/home/Live'
import { retainHome } from '~/lib/homeLive'
import { isConfigured } from '~/lib/live'
import {
  controlStatusQuery, coverageQuery, directLoginsQuery, meQuery, requestsQuery, statsQuery,
} from '~/lib/queries'
import { canSee, seesWholeFleet } from '~/lib/roles'
import { FS, SP } from '~/theme'
import type { AuditEvent, User } from '~/types/domain'

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

  const addServer = canSee(me?.role, 'admin')

  return (
    <>
      <CommandBand
        title={greeting(me)}
        subtitle={subtitle(me)}
        actions={(me && whole) || addServer ? (
          <>
            {me && whole && <LivePill />}
            {addServer && <ButtonLink to="/assets/add" size="sm" variant="default">Add server</ButtonLink>}
          </>
        ) : undefined}
      />
      <PageBody>
        <Stack gap="lg">
          {/* Nothing until we know who this is: guessing would fetch, and
              flash, the other reader's page. */}
          {!me ? <Skeleton h={180} radius="md" /> : whole ? <FleetHome me={me} /> : (
            <>
              {approves && <NeedsYou me={me} />}
              <Suspense fallback={<Skeleton h={180} radius="md" />}><PersonalHome me={me} /></Suspense>
            </>
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
  const control = controlNeed(status)
  if (control) needs.push(control)
  return <NeedsList needs={needs} />
}

function NeedsList({ needs }: { needs: Need[] }) {
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
      <SimpleGrid cols={{ base: 1, sm: 2, lg: 3, xl: 4 }} spacing="sm" data-testid="needs-you">
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

/**
 * The fleet, live: what needs someone, six numbers, the day by hour, who is
 * on a server, how the servers are doing, and the audit log as it is written.
 * All of it from one dashboard the control plane builds and streams; see
 * lib/homeLive.ts. argus-control's own card is for platform managers.
 */
function FleetHome({ me }: { me: User }) {
  useEffect(() => retainHome(), [])
  const platform = Boolean(me.platformOwner) && isConfigured()
  return (
    <Stack gap="lg">
      <FleetNeeds me={me} />
      <Box>
        <Label>Right now</Label>
        <HomeTiles />
      </Box>
      <Box className="argus-homegrid">
        <Stack gap="md" className="argus-homegrid-main">
          <SessionsChart />
          <LiveNow />
        </Stack>
        <Stack gap="md" className="argus-homegrid-side">
          <FleetHealth />
          {platform && <Suspense fallback={<Skeleton h={240} radius="md" />}><ControlCard /></Suspense>}
        </Stack>
      </Box>
      <HappeningNow words={(e) => e.detail || <>{ACTION_WORDS[e.action] ?? <Mono>{e.action}</Mono>} {e.target}</>} />
    </Stack>
  )
}

/**
 * Needs you, for the fleet roles: from the live dashboard, so it moves with
 * it. Approvals, servers waiting to be added, a disk at 95% or fuller, a
 * burst of failed sign-ins, and argus-control's own problems.
 */
function FleetNeeds({ me }: { me: User }) {
  const admin = canSee(me.role, 'admin')
  const platform = Boolean(me.platformOwner) && isConfigured()
  const { waiting, unreviewed, fullDisks, failed15m } = useLiveNeeds()
  const { data: status } = useQuery({ ...controlStatusQuery(), enabled: platform })
  if (!waiting) return <Skeleton h={48} radius="md" />

  const needs: Need[] = []
  if (waiting.count > 0) {
    const mins = waiting.oldestAt ? Math.max(0, Math.round((now() - Date.parse(waiting.oldestAt)) / 60_000)) : null
    needs.push({
      key: 'approvals', icon: IconClipboardCheck, color: 'amber',
      title: `${waiting.count} ${waiting.count === 1 ? 'request is' : 'requests are'} waiting on you`,
      detail: mins === null ? '' : mins < 1 ? 'the oldest just now' : `the oldest for ${mins} min`,
      action: <ButtonLink variant="subtle" size="compact-sm" style={{ flexShrink: 0 }} to="/approvals">Review ›</ButtonLink>,
    })
  }
  for (const d of fullDisks.slice(0, 2)) {
    needs.push({
      key: `disk-${d.assetId}-${d.mount}`, icon: IconDatabaseExclamation, color: 'rose',
      title: `${d.name}: ${d.mount} is ${Math.round(d.used)}% full`,
      detail: 'From its agent\'s last report',
      action: <ButtonLink variant="subtle" size="compact-sm" style={{ flexShrink: 0 }} to="/assets/$assetId" params={{ assetId: d.assetId }}>Open ›</ButtonLink>,
    })
  }
  if (failed15m >= 5) {
    needs.push({
      key: 'signins', icon: IconShieldExclamation, color: 'rose',
      title: `${failed15m} failed sign-ins in 15 minutes`,
      detail: 'Someone may be guessing a password',
      action: <ButtonLink variant="subtle" size="compact-sm" style={{ flexShrink: 0 }} to="/audit">Audit log ›</ButtonLink>,
    })
  }
  if (admin && (unreviewed ?? 0) > 0) {
    needs.push({
      key: 'servers', icon: IconServerBolt, color: 'azure',
      title: `${unreviewed} ${unreviewed === 1 ? 'server is' : 'servers are'} waiting to be added`,
      detail: 'Their agents checked in and nobody has finished adding them',
      action: <ButtonLink variant="subtle" size="compact-sm" style={{ flexShrink: 0 }} to="/assets">Finish ›</ButtonLink>,
    })
  }
  const control = controlNeed(status)
  if (control) needs.push(control)
  return <NeedsList needs={needs} />
}

function controlNeed(status: unknown): Need | null {
  const problems = isStatus(status) ? status.problems : []
  if (problems.length === 0) return null
  const critical = problems.some((p) => p.severity === 'critical')
  return {
    key: 'control', icon: IconActivityHeartbeat, color: critical ? 'rose' : 'amber',
    title: problems.length === 1 ? `argus-control: ${lowerFirst(problems[0]!.title)}` : `argus-control has ${problems.length} problems`,
    detail: problems.length === 1 ? (problems[0]!.detail ?? '') : problems.map((p) => p.title).slice(0, 2).join(' · '),
    action: <ButtonLink variant="subtle" size="compact-sm" style={{ flexShrink: 0 }} to="/settings" search={{ section: 'status' }}>Status ›</ButtonLink>,
  }
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

function lowerFirst(s: string): string {
  return s ? s[0]!.toLowerCase() + s.slice(1) : s
}

