import { Children, useMemo, useState, type ReactNode } from 'react'
import { Box, Button, Grid, SegmentedControl, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconClipboardCheck, IconSearch, IconServerOff } from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { ButtonLink } from '~/components/links'
import {
  GrantCard, ServerCard, ServerCardSkeleton, kindOf, reachOf, shortName,
} from '~/components/myservers'
import { now } from '~/components/primitives'
import { assetsQuery, meQuery, requestsQuery } from '~/lib/queries'
import { seesWholeFleet } from '~/lib/roles'
import { FS, SP } from '~/theme'

type Show = 'all' | 'online' | 'offline'
type Kind = 'all' | 'linux' | 'windows'

/**
 * The servers the signed-in person can reach, and a button for each.
 *
 * Asked for directly: someone who has been given servers opened the console and
 * met fleet posture, host-key pins and a coverage report before finding the two
 * machines they were there to use. This page is the answer to "which servers
 * can I get on, and how", for people who do not need the vocabulary behind it.
 *
 * It shows only what the control plane already sent. For an operator that list
 * is narrowed on the server to their assignments (`AssetsForUser`), with each
 * host's accounts cut down to the ones they were given -- so nothing here is a
 * filter the console applies and could get wrong. An administrator is sent the
 * whole fleet and may connect to all of it, and the page says so rather than
 * implying those are assignments.
 *
 * No loader, like Connect: a slow control plane should show the page and a few
 * placeholder cards, not hold the whole console on a blank screen.
 */
export const Route = createFileRoute('/mine')({
  component: MyServers,
})

function MyServers() {
  const { data: me } = useQuery(meQuery())
  const { data: assets } = useQuery(assetsQuery({}))
  const { data: approved } = useQuery(requestsQuery('approved'))
  const whole = seesWholeFleet(me?.role)

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
    <Box>
      <PageHeader
        title="My servers"
        description={
          whole
            ? 'As an administrator you can connect to every server, so all of them are listed here. Press Connect to open one in this browser.'
            : 'The servers you have been given access to. Press Connect to open one in this browser, or pick the account to log in as.'
        }
        actions={
          !whole && (assets?.length ?? 0) > 0 && (
            <ButtonLink to="/requests" variant="default" leftSection={<IconClipboardCheck size={14} />}>
              Request more access
            </ButtonLink>
          )
        }
      />

      <PageBody>
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
                ? 'Install the Argus agent on a server and approve it, and it will appear here.'
                : // Not "request access": the request form can only offer the
                  // servers the control plane sent, which for this person is
                  // none, so pointing at it would be a next step that cannot be
                  // taken. Assignment is an administrator's, so say so.
                  'An admin decides who can reach each server. Ask your admin to give you the servers you need, and they will appear here.'
            }
            action={whole && <ButtonLink to="/assets/add">Add a server</ButtonLink>}
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
            {visible.map((a) => <ServerCard key={a.id} asset={a} />)}
          </CardGrid>
        )}
      </PageBody>
    </Box>
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
