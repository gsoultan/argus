import { Children, useMemo, useState, type ReactNode } from 'react'
import {
  Badge, Box, Button, Card, Grid, Group, SegmentedControl, Skeleton, Stack, Text, TextInput,
  ThemeIcon, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  IconBrandWindows, IconClock, IconDeviceDesktop, IconLock, IconSearch, IconServer2,
  IconServerOff, IconTerminal2,
} from '@tabler/icons-react'
import { EmptyState, Toolbar } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { now, relTime } from '~/components/primitives'
import { assetsQuery, meQuery, requestsQuery } from '~/lib/queries'
import { seesWholeFleet } from '~/lib/roles'
import type { Asset } from '~/types/domain'
import { FS, SP } from '~/theme'

type Show = 'all' | 'online' | 'offline'
type Kind = 'all' | 'linux' | 'windows'

/**
 * Whether a server is worth trying, in words someone without the vocabulary can
 * act on.
 *
 * Built from what the console was last told, never from a probe, so every label
 * is a statement about the last check rather than a promise about the next
 * connect -- the card says when that check was. Only `blocked` stops the
 * Connect button, because it is the one case Argus itself refuses: SSH to a
 * server whose host key no longer matches the pin.
 */
export type Reach = 'online' | 'slow' | 'offline' | 'blocked'

export function reachOf(a: Asset): Reach {
  if (a.protocol !== 'rdp' && a.hostKeyState === 'changed') return 'blocked'
  // A connected agent tunnel is how every session opens, terminal or desktop,
  // so it counts as reachable on its own.
  if (a.tunnelConnected === true) return 'online'
  if (a.health === 'reachable') return 'online'
  if (a.health === 'degraded') return 'slow'
  return 'offline'
}

const REACH: Record<Reach, { label: string; color: string; hint: string }> = {
  online: {
    label: 'Online',
    color: 'teal',
    hint: 'Answered at the last check.',
  },
  slow: {
    label: 'Slow',
    color: 'amber',
    hint: 'Answered at the last check, but slowly. Connecting may take a moment.',
  },
  offline: {
    label: 'Offline',
    color: 'rose',
    hint: 'Did not answer at the last check. You can still try; it may be back.',
  },
  blocked: {
    label: 'Blocked',
    color: 'rose',
    hint: 'This server’s identity changed, so Argus will not connect to it until an admin checks it.',
  },
}

export function ReachBadge({ reach }: { reach: Reach }) {
  const r = REACH[reach]
  return (
    <Tooltip label={r.hint} multiline w={240}>
      <Badge color={r.color} variant="light" size="sm" leftSection={<Dot color={r.color} />}>
        {r.label}
      </Badge>
    </Tooltip>
  )
}

function Dot({ color }: { color: string }) {
  return (
    <Box
      w={6}
      h={6}
      style={{ borderRadius: '50%', background: `var(--mantine-color-${color}-5)` }}
    />
  )
}

/** "Windows" or "Linux", which is the distinction a person picking a server makes. */
export function kindOf(a: Asset): 'windows' | 'linux' {
  return a.protocol === 'rdp' || /windows/i.test(a.os) ? 'windows' : 'linux'
}

/** The part of the hostname people say out loud. */
export function shortName(hostname: string): string {
  return hostname.split('.')[0] ?? hostname
}

/**
 * One server someone can use, and the shortest way onto it.
 *
 * Everything an administrator needs to judge a host -- pins, credential mode,
 * bypass posture -- is deliberately absent. The person reading this card has
 * one question, "can I get on it, and how", and the answer is the status, the
 * accounts they were given, and a button.
 */
export function ServerCard({ asset: a, admin = false }: { asset: Asset; admin?: boolean }) {
  const reach = reachOf(a)
  const rdp = a.protocol === 'rdp'
  const KindIcon = kindOf(a) === 'windows' ? IconBrandWindows : IconServer2
  const name = shortName(a.hostname)
  const noAccount = a.principals.length === 0
  const disabled = reach === 'blocked' || noAccount

  return (
    <Card padding="md" className="argus-server" data-reach={reach} h="100%">
      <Stack gap="sm" h="100%">
        <Group justify="space-between" align="flex-start" wrap="nowrap" gap="sm">
          <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
            <ThemeIcon variant="light" color="azure" size={36} radius="md">
              <KindIcon size={20} stroke={1.6} />
            </ThemeIcon>
            <Box style={{ minWidth: 0 }}>
              <Text component="h3" fw={600} size="md" truncate style={{ margin: 0 }}>
                {name}
              </Text>
              <Text size={FS.meta} c="dimmed" truncate>
                {a.os} · {rdp ? 'Remote Desktop' : 'Terminal (SSH)'}
              </Text>
            </Box>
          </Group>
          <ReachBadge reach={reach} />
        </Group>

        {reach === 'blocked' ? (
          <Group gap={SP.snug} wrap="nowrap" align="flex-start">
            <IconLock size={14} style={{ flexShrink: 0, marginTop: 2 }} opacity={0.7} />
            <Text size={FS.meta} c="dimmed" lh={1.5}>
              This server&rsquo;s identity changed. Nobody can connect until an admin checks it.
            </Text>
          </Group>
        ) : noAccount ? (
          <Text size={FS.meta} c="dimmed" lh={1.5}>
            {admin ? (
              <>
                Nobody can log in yet.{' '}
                <Link
                  to="/assets/$assetId"
                  params={{ assetId: a.id }}
                  className="argus-above"
                  style={{ color: 'var(--mantine-color-azure-7)' }}
                >
                  Add a login account
                </Link>
              </>
            ) : (
              'You have no account on this server yet. Ask an admin to add one for you.'
            )}
          </Text>
        ) : a.principals.length === 1 ? (
          <Text size={FS.meta} c="dimmed">
            Logs you in as <Text span ff="monospace" size={FS.meta}>{a.principals[0]}</Text>
          </Text>
        ) : (
          // More than one account: each is its own way in, and the card itself
          // opens the first.
          <Box className="argus-above">
            <Text size={FS.micro} c="dimmed" mb={SP.tight}>
              Log in as
            </Text>
            <Group gap={SP.snug}>
              {a.principals.map((p) => (
                <ButtonLink
                  key={p}
                  to="/connect"
                  search={{ host: a.hostname, as: p, open: true }}
                  size="compact-sm"
                  variant="default"
                  ff="monospace"
                  aria-label={`Connect to ${name} as ${p}`}
                >
                  {p}
                </ButtonLink>
              ))}
            </Group>
          </Box>
        )}

        <Group justify="space-between" align="center" mt="auto" wrap="nowrap" gap="sm">
          {admin ? (
            <Link
              to="/assets/$assetId"
              params={{ assetId: a.id }}
              className="argus-above"
              aria-label={`Manage ${name}`}
              style={{ color: 'var(--mantine-color-azure-7)', fontSize: 13, fontWeight: 500, textDecoration: 'none' }}
            >
              Who can connect, settings
            </Link>
          ) : (
            <Text size={FS.micro} c="dimmed">
              Checked {relTime(a.lastCheckedAt)}
            </Text>
          )}
          {disabled ? (
            <Button size="sm" disabled leftSection={rdp ? <IconDeviceDesktop size={16} /> : <IconTerminal2 size={16} />}>
              Connect
            </Button>
          ) : (
            <ButtonLink
              to="/connect"
              search={{ host: a.hostname, as: a.principals[0], open: true }}
              size="sm"
              className="argus-stretch"
              variant={reach === 'offline' ? 'default' : 'filled'}
              leftSection={rdp ? <IconDeviceDesktop size={16} /> : <IconTerminal2 size={16} />}
              aria-label={`Connect to ${name}`}
            >
              Connect
            </ButtonLink>
          )}
        </Group>
      </Stack>
    </Card>
  )
}

export function ServerCardSkeleton() {
  return (
    <Card padding="md" h={170}>
      <Group gap="sm" wrap="nowrap">
        <Skeleton height={36} width={36} radius="md" />
        <Stack gap={SP.snug} style={{ flex: 1 }}>
          <Skeleton height={12} width="50%" />
          <Skeleton height={10} width="70%" />
        </Stack>
      </Group>
      <Skeleton height={22} width="60%" mt="lg" />
      <Skeleton height={26} width={90} mt="lg" ml="auto" />
    </Card>
  )
}

/**
 * A server reached through an approved access request rather than an
 * assignment.
 *
 * The control plane's asset list is assignments only, so a host someone was
 * granted for an hour is often not in it at all. Without this, the approval
 * arrived and the server it was for appeared nowhere on the page meant to list
 * what you can reach.
 */
export function GrantCard({
  hostname, principal, expiresAt,
}: { hostname: string; principal: string; expiresAt: string | null }) {
  const name = shortName(hostname)
  return (
    <Card padding="md" className="argus-server" data-reach="granted" h="100%">
      <Stack gap="sm" h="100%">
        <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
          <ThemeIcon variant="light" color="amber" size={36} radius="md">
            <IconClock size={20} stroke={1.6} />
          </ThemeIcon>
          <Box style={{ minWidth: 0 }}>
            <Text component="h3" fw={600} size="md" truncate style={{ margin: 0 }}>
              {name}
            </Text>
            <Text size={FS.meta} c="dimmed" truncate>
              Temporary access, as <Text span ff="monospace" size={FS.meta}>{principal}</Text>
            </Text>
          </Box>
        </Group>
        <Group justify="space-between" align="center" mt="auto" wrap="nowrap" gap="sm">
          <Text size={FS.micro} c="amber.7">
            {expiresAt ? `Access ends ${relTime(expiresAt)}` : 'Approved'}
          </Text>
          <ButtonLink
            to="/connect"
            search={{ host: hostname, as: principal, open: true }}
            size="sm"
            className="argus-stretch"
            leftSection={<IconTerminal2 size={16} />}
            aria-label={`Connect to ${name} as ${principal}`}
          >
            Connect
          </ButtonLink>
        </Group>
      </Stack>
    </Card>
  )
}

/**
 * The servers the signed-in person can reach, as cards with a Connect button
 * on each, plus any temporary access from approved requests.
 *
 * Asked for directly: someone who has been given servers opened the console and
 * met fleet posture, host-key pins and a coverage report before finding the two
 * machines they were there to use. These cards answer "which servers can I get
 * on, and how", for people who do not need the vocabulary behind it.
 *
 * It shows only what the control plane already sent. For an operator that list
 * is narrowed on the server to their assignments (`AssetsForUser`), with each
 * host's accounts cut down to the ones they were given -- so nothing here is a
 * filter the console applies and could get wrong.
 */
export function ServerCards() {
  const { data: me } = useQuery(meQuery())
  const { data: assets } = useQuery(assetsQuery({}))
  const { data: approved } = useQuery(requestsQuery('approved'))
  const whole = seesWholeFleet(me?.role)
  const canAdmin = me?.role === 'admin' || me?.role === 'owner'

  const [text, setText] = useState('')
  const [show, setShow] = useState<Show>('all')
  const [kind, setKind] = useState<Kind>('all')

  // Approved and not yet expired, for this person only: an approver is sent the
  // whole queue, and someone else's grant is not a server you can reach.
  // A grant for an account the person already holds on that server adds
  // nothing they can do, so it is left to the list below rather than shown
  // twice.
  const grants = useMemo(() => {
    if (!approved || !me) return []
    const t = now()
    const held = new Set((assets ?? []).flatMap((a) => a.principals.map((p) => `${a.hostname}\u0000${p}`)))
    return approved
      .filter((r) => r.requesterEmail.toLowerCase() === me.email.toLowerCase())
      .filter((r) => !r.expiresAt || Date.parse(r.expiresAt) > t)
      .flatMap((r) =>
        r.assetHostnames.map((h) => ({
          key: `${r.id}:${h}`,
          hostname: h,
          principal: r.principal,
          expiresAt: r.expiresAt,
        })),
      )
      .filter((g) => !held.has(`${g.hostname}\u0000${g.principal}`))
  }, [approved, me, assets])

  const hasBothKinds = useMemo(() => {
    const kinds = new Set((assets ?? []).map(kindOf))
    return kinds.size > 1
  }, [assets])

  const counts = useMemo(() => {
    if (!assets) return undefined
    let online = 0
    for (const a of assets) {
      const r = reachOf(a)
      if (r === 'online' || r === 'slow') online++
    }
    return { all: assets.length, online, offline: assets.length - online }
  }, [assets])

  const needle = text.trim().toLowerCase()
  const visible = useMemo(
    () =>
      (assets ?? [])
        .filter((a) => {
          if (needle) {
            const hay = `${a.hostname} ${a.address} ${a.os} ${a.tags.join(' ')} ${a.principals.join(' ')}`
            if (!hay.toLowerCase().includes(needle)) return false
          }
          const r = reachOf(a)
          if (show === 'online' && r !== 'online' && r !== 'slow') return false
          if (show === 'offline' && (r === 'online' || r === 'slow')) return false
          if (kind !== 'all' && kindOf(a) !== kind) return false
          return true
        })
        .sort((x, y) => shortName(x.hostname).localeCompare(shortName(y.hostname))),
    [assets, needle, show, kind],
  )

  const filtering = needle !== '' || show !== 'all' || kind !== 'all'
  const clear = () => {
    setText('')
    setShow('all')
    setKind('all')
  }

  return (
    <>
      {grants.length > 0 && (
        <Box>
          <Text component="h2" size="sm" fw={600} mb={SP.tight} style={{ margin: 0 }}>
            Temporary access
          </Text>
          <Text size={FS.meta} c="dimmed" mb="sm">
            Approved requests. These stop working when the time runs out.
          </Text>
          <CardGrid>
            {grants.map((g) => (
              <GrantCard key={g.key} hostname={g.hostname} principal={g.principal} expiresAt={g.expiresAt} />
            ))}
          </CardGrid>
        </Box>
      )}

      <Toolbar
        right={
          assets && (
            <Text size={FS.micro} c="dimmed">
              {filtering ? `${visible.length} of ${assets.length}` : assets.length}{' '}
              {assets.length === 1 ? 'server' : 'servers'}
            </Text>
          )
        }
      >
        <TextInput
          w={260}
          aria-label="Find a server"
          placeholder="Find a server"
          leftSection={<IconSearch size={14} />}
          value={text}
          onChange={(e) => setText(e.currentTarget.value)}
        />
        <SegmentedControl
          aria-label="Status"
          value={show}
          onChange={(v) => setShow(v as Show)}
          data={[
            { value: 'all', label: counts ? `All (${counts.all})` : 'All' },
            { value: 'online', label: counts ? `Online (${counts.online})` : 'Online' },
            { value: 'offline', label: counts ? `Offline (${counts.offline})` : 'Offline' },
          ]}
        />
        {hasBothKinds && (
          <SegmentedControl
            aria-label="Kind of server"
            value={kind}
            onChange={(v) => setKind(v as Kind)}
            data={[
              { value: 'all', label: 'Any' },
              { value: 'linux', label: 'Linux' },
              { value: 'windows', label: 'Windows' },
            ]}
          />
        )}
      </Toolbar>

      {assets === undefined ? (
        <CardGrid busy>
          {Array.from({ length: 6 }, (_, i) => <ServerCardSkeleton key={i} />)}
        </CardGrid>
      ) : assets.length === 0 ? (
        <EmptyState
          icon={IconServerOff}
          title={whole ? 'There are no servers yet.' : 'No servers have been given to you yet.'}
          description={
            whole
              ? 'Add your first server. It takes one line on the server and about a minute.'
              : // Not "request access": the request form can only offer the
                // servers the control plane sent, which for this person is
                // none, so pointing at it would be a next step that cannot be
                // taken. Assignment is an administrator's, so say so.
                'An admin decides who can reach each server. Ask your admin to give you the servers you need, and they will appear here.'
          }
          action={canAdmin && <ButtonLink to="/assets/add">Add server</ButtonLink>}
        />
      ) : visible.length === 0 ? (
        <EmptyState
          icon={IconSearch}
          title="No server matches."
          description={`None of your ${assets.length} servers fit what you picked above.`}
          action={
            <Button variant="default" onClick={clear}>
              Show all servers
            </Button>
          }
        />
      ) : (
        <CardGrid>
          {visible.map((a) => <ServerCard key={a.id} asset={a} admin={canAdmin} />)}
        </CardGrid>
      )}
    </>
  )
}

/**
 * One, two or three cards a row, by width.
 *
 * Grid rather than SimpleGrid: the console imports Mantine's stylesheets one
 * by one in a load-bearing order, and Grid's is already among them.
 */
function CardGrid({ children, busy }: { children: ReactNode; busy?: boolean }) {
  return (
    <Grid gap="sm" aria-busy={busy || undefined} aria-label={busy ? 'Loading servers' : undefined}>
      {Children.map(children, (c) => (
        <Grid.Col span={{ base: 12, sm: 6, lg: 4 }}>{c}</Grid.Col>
      ))}
    </Grid>
  )
}
