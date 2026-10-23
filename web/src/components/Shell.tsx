import {
  ActionIcon, Alert, AppShell, Avatar, Badge, Box, Burger, Center, Group, Indicator,
  Loader, Menu, NavLink as MantineNavLink, ScrollArea, Stack, Text, Tooltip,
  UnstyledButton,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { Link, useRouter, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconBell, IconChevronDown, IconChevronRight, IconLogout, IconSearch,
  IconSettings,
} from '@tabler/icons-react'
import { Suspense, lazy, useCallback, useEffect, useState } from 'react'
import { coverageQuery, meQuery, statsQuery } from '~/lib/queries'
import { SignIn } from '~/components/SignIn'
// Eager on purpose: lazy, it pulled seven shared Mantine pieces out into chunks
// of their own and cost the first visit 3.4 kB instead of 1.0. See weight.spec.ts.
import { Setup } from '~/components/Setup'
import { canSee, seesWholeFleet } from '~/lib/roles'
/**
 * Loaded on first open.
 *
 * It pulls in Modal and its focus trap, and it is opened by a minority of
 * visits. Measured: a cold load of `/` drops from 237.0 kB to 233.6 kB
 * compressed — 3.4 kB for a panel most sessions never see. Same reasoning as
 * the terminal and the replay players.
 */
const CommandPalette = lazy(() =>
  import('~/components/CommandPalette').then((m) => ({ default: m.CommandPalette })),
)
import {
  PRIMARY_ACTION, isActive, navFor, type NavCounts, type NavSection,
} from '~/components/nav'
import { ButtonLink } from '~/components/links'
import { Logo } from '~/components/Logo'
import {
  GATEWAY_URL, controlPlaneHost, isConfigured, isLive, logout, whoami, type Identity,
} from '~/lib/live'
import { FS, SP } from '~/theme'

const HEADER_H = 56
const NAVBAR_W = 260

/**
 * Blocks the console until the user is signed in.
 *
 * Only when a control plane is configured — with none, the console runs against
 * the in-memory fixture and there is nobody to authenticate.
 */
function LoginGate({ children }: { children: React.ReactNode }) {
  const [identity, setIdentity] = useState<Identity | null>(null)
  const queryClient = useQueryClient()
  const router = useRouter()

  const refresh = useCallback(() => {
    if (!isConfigured()) {
      setIdentity({ authenticated: true })
      return
    }
    void whoami().then(setIdentity)
  }, [])

  useEffect(refresh, [refresh])

  // Route loaders do not wait for this gate. `/` calls ensureQueryData(stats)
  // while nobody is signed in, takes a 401, and the router commits that error
  // to the match; acquiring a session does not re-run it. So the console drew
  // its error boundary on the first screen after every sign-in, and reloading
  // was the only way past -- which is a poor thing to ask of someone who has
  // just proved who they are.
  //
  // Both caches have to be cleared: invalidateQueries drops the 401 that
  // ensureQueryData stored, and router.invalidate re-runs the loaders that
  // stored it. Doing this before setIdentity means the loaders re-run while
  // the form is still up, so the console renders once, with data.
  const handleSignedIn = useCallback(async () => {
    await queryClient.invalidateQueries()
    await router.invalidate()
    refresh()
  }, [queryClient, router, refresh])

  if (!identity) {
    return (
      <Center h="100vh">
        <Loader size="sm" color="azure" />
      </Center>
    )
  }

  if (!identity.authenticated) {
    // First-run setup is the one page nobody can be signed in for. Read from
    // the location rather than the router: this gate sits above the routes.
    if (identity.unreachable) {
      return <SignIn identity={identity} onSignedIn={handleSignedIn} />
    }
    // First run sends everyone to /setup: a control plane with no
    // configuration has nothing else to show, and one with no accounts has
    // nobody to sign in. The address changes too, so a reload stays here.
    const firstRun = identity.installing ||
      (identity.setupAvailable && identity.accountsExist === false)
    if (firstRun && window.location.pathname !== '/setup') {
      window.history.replaceState(null, '', '/setup')
    }
    if (window.location.pathname === '/setup') {
      return <Setup />
    }
    return <SignIn identity={identity} onSignedIn={handleSignedIn} />
  }

  return <>{children}</>
}

export function Shell({ children }: { children: React.ReactNode }) {
  return (
    <LoginGate>
      <ShellInner>{children}</ShellInner>
    </LoginGate>
  )
}

function ShellInner({ children }: { children: React.ReactNode }) {
  const [opened, { toggle, close }] = useDisclosure()
  const [paletteOpen, palette] = useDisclosure(false)
  const { data: me } = useQuery(meQuery())
  const statsQuery_ = useQuery(statsQuery())
  const stats = statsQuery_.data
  const source = dataSource(statsQuery_.isPending)
  const { data: cov } = useQuery(coverageQuery())
  // Someone who was only given servers gets a console that is a list of their
  // servers: no search across the fleet, no live-session counters, and no
  // Connect button beside a page that is already one per server. An approver
  // keeps the request bell, because the queue is their job.
  const whole = seesWholeFleet(me?.role)
  const approves = canSee(me?.role, 'approvers')

  // Both directions count as a gap: an unmanaged host and a managed host with
  // no agent are each somewhere a privileged session goes unrecorded.
  const navStats = {
    requestsPending: stats?.requestsPending ?? 0,
    sessionsActive: stats?.sessionsActive ?? 0,
    coverageGaps: cov ? cov.unreviewedHosts + cov.assetsUnmonitored : 0,
    // Servers whose agent has checked in and is waiting for someone to approve
    // it. Counted on "Add a server" because that is where they are approved.
    serversWaiting: cov?.unreviewedHosts ?? 0,
  }
  const pathname = useRouterState({ select: (s) => s.location.pathname })

  // Cmd-K anywhere. Captured on the document so it works while focus is inside
  // a table, a filter or the replay player.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (whole && e.key === 'k' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault()
        palette.toggle()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [palette, whole])

  // Closing the drawer on navigation: on a phone the overlay otherwise stays up
  // over the page it just took you to.
  useEffect(close, [pathname, close])

  return (
    <AppShell
      header={{ height: HEADER_H }}
      navbar={{ width: NAVBAR_W, breakpoint: 'sm', collapsed: { mobile: !opened } }}
      padding={0}
      styles={{
        header: { background: 'var(--color-surface)', borderColor: 'var(--color-line)' },
        navbar: { background: 'var(--color-surface)', borderColor: 'var(--color-line)' },
        main: { background: 'var(--color-void)' },
      }}
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap" gap="md">
          <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
            <Burger
              opened={opened}
              onClick={toggle}
              hiddenFrom="sm"
              size="sm"
              aria-label={opened ? 'Close navigation' : 'Open navigation'}
            />
            <Box hiddenFrom="sm">
              <Logo markOnly />
            </Box>
            {/* On a phone the sidebar, and the source line in it, is hidden
                behind the burger. A warning is never hidden by breakpoint, so
                the badge comes back up here when there is one to give. */}
            {source !== 'live' && source !== 'connecting' && (
              <Box hiddenFrom="sm">
                <DataSourceBadge source={source} />
              </Box>
            )}
          </Group>

          {/* A search field rather than an icon: the shortcut is discoverable
              only if something on screen says it exists. */}
          {whole ? <UnstyledButton
            onClick={palette.open}
            visibleFrom="md"
            aria-label="Search pages, hosts, sessions and people"
            style={{
              flex: 1,
              maxWidth: 420,
              border: '1px solid var(--color-line)',
              background: 'var(--color-raised)',
              borderRadius: 8,
              padding: '7px 12px',
            }}
          >
            <Group gap={SP.cozy} wrap="nowrap">
              <IconSearch size={15} opacity={0.6} />
              <Text size={FS.body} c="dimmed" style={{ flex: 1 }}>
                Find a server, session or person
              </Text>
              <Badge size="xs" variant="default" color="slate" className="argus-digest">
                ⌘K
              </Badge>
            </Group>
          </UnstyledButton> : <Box style={{ flex: 1 }} />}

          <Group gap="xs" wrap="nowrap">
            {whole && (
              <ActionIcon
                hiddenFrom="md"
                variant="subtle"
                color="slate"
                aria-label="Search"
                onClick={palette.open}
              >
                <IconSearch size={17} />
              </ActionIcon>
            )}

            {whole && stats && stats.sessionsActive > 0 && (
              <Tooltip label={`${stats.sessionsActive} sessions in progress right now`}>
                <Badge
                  color="sky"
                  variant="light"
                  visibleFrom="xs"
                  leftSection={
                    <Box
                      w={6}
                      h={6}
                      className="rounded-full animate-pulse"
                      style={{ background: 'var(--color-live)' }}
                    />
                  }
                >
                  {stats.sessionsActive} live
                </Badge>
              </Tooltip>
            )}

            {/* Beside the live count rather than added to it. These sessions
                were counted as live until now — fifteen of them in dev, one for
                nineteen days — which is exactly the number an operator glances
                at to decide whether anything is happening. */}
            {whole && stats && stats.sessionsSilent > 0 && (
              <Tooltip
                label={`${stats.sessionsSilent} ${stats.sessionsSilent === 1 ? 'session is' : 'sessions are'} recorded as active but have not been reported in minutes. Their gateway most likely went away without reporting the end. Sessions › Unknown.`}
                multiline
                maw={300}
              >
                <Badge color="amber" variant="light" visibleFrom="sm">
                  {stats.sessionsSilent} unknown
                </Badge>
              </Tooltip>
            )}

            {approves && <Indicator
              disabled={!stats?.requestsPending}
              label={stats?.requestsPending}
              size={15}
              color="amber"
              offset={5}
            >
              <ActionIcon
                aria-label={`Access requests${stats?.requestsPending ? ` — ${stats.requestsPending} pending` : ''}`}
                variant="subtle"
                color="slate"
                component={Link}
                to="/requests"
              >
                <IconBell size={17} />
              </ActionIcon>
            </Indicator>}

            <Menu position="bottom-end" width={220} withArrow>
              <Menu.Target>
                <UnstyledButton aria-label="Account menu">
                  <Group gap={SP.cozy} wrap="nowrap">
                    <Avatar size={27} radius="sm" color="azure" variant="light">
                      {me?.displayName.slice(0, 2).toUpperCase() ?? '··'}
                    </Avatar>
                    <Box visibleFrom="sm" ta="left">
                      <Text size={FS.meta} fw={600} lh={1.15}>
                        {me?.displayName}
                      </Text>
                      <Text size={FS.micro} c="dimmed" lh={1.15} tt="capitalize">
                        {me?.role}
                      </Text>
                    </Box>
                    <IconChevronDown size={13} opacity={0.5} />
                  </Group>
                </UnstyledButton>
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Label>{me?.email}</Menu.Label>
                <Menu.Item leftSection={<IconSettings size={14} />} component={Link} to="/settings">
                  Gateway policy
                </Menu.Item>
                <Menu.Divider />
                <Menu.Item
                  leftSection={<IconLogout size={14} />}
                  color="rose"
                  onClick={async () => {
                    await logout()
                    window.location.reload()
                  }}
                >
                  Sign out
                </Menu.Item>
              </Menu.Dropdown>
            </Menu>
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p={0}>
        <Box px="md" pt="md" pb="sm" visibleFrom="sm">
          <Logo />
        </Box>

        {/* The reason most people open the console, as a button rather than
            one list item among many. */}
        {whole && <Box px="sm" pb="sm">
          <ButtonLink
            to={PRIMARY_ACTION.to as never}
            fullWidth
            size="sm"
            radius="md"
            leftSection={<PRIMARY_ACTION.icon size={17} />}
            justify="flex-start"
          >
            {PRIMARY_ACTION.label}
          </ButtonLink>
        </Box>}

        <AppShell.Section grow component={ScrollArea} px="sm" pb="xs">
          <Stack gap="md" pt={SP.tight}>
            {navFor(me?.role).map((section, si) => (
              <NavSectionView
                key={section.label ?? si}
                section={section}
                pathname={pathname}
                counts={navStats}
              />
            ))}
          </Stack>
        </AppShell.Section>

        {/* Where the figures on screen come from, said once, in the same place
            on every page. It used to be a badge in the header competing with
            the live count and the bell for the same glance. */}
        <AppShell.Section p="sm" visibleFrom="sm">
          <SourceLine source={source} />
        </AppShell.Section>
      </AppShell.Navbar>

      <AppShell.Main>
        {source === 'unreachable' && <FallbackBanner />}
        {children}
      </AppShell.Main>

      {/* Mounted only while open, so the chunk is fetched on the first ⌘K
          rather than on every page load. */}
      {paletteOpen && (
        <Suspense fallback={null}>
          <CommandPalette opened onClose={palette.close} />
        </Suspense>
      )}
    </AppShell>
  )
}

/**
 * Which data is on the screen.
 *
 * This slot used to read `northwind-prod · ap-southeast-3`, hard-coded, over a
 * tooltip calling it "the tenant and region this console is pointed at". Argus
 * has no tenant and no region — neither word appears anywhere in the domain —
 * so it was a fabricated deployment name presented as fact, in the one place an
 * operator would look to check which deployment they were about to act on. It
 * also read as a switcher and switched nothing.
 *
 * What replaces it is the thing the console genuinely knows and the thing that
 * actually matters: where its figures come from. `live.orFallback` serves the
 * in-memory fixture whenever the control plane cannot be reached, so a console
 * showing invented numbers looks exactly like one showing real ones. That is
 * the case this badge exists to make visible — see the header of lib/live.ts.
 *
 * **It shows the host, and there is deliberately no configurable deployment
 * name.** Checked against the control plane before deciding: it has no identity
 * to report. No tenant, no region, no gateway registry — `gateway_policy` is a
 * single row and the only heartbeats are agents reporting their own hostname.
 * Multi-gateway does not change that; it is many gateways to one control plane,
 * and the console talks to the control plane.
 *
 * Same-origin is the supported shape because the session cookie depends on it,
 * so the browser's host *is* the control plane's address — not a stand-in for
 * it. A name from config would be weaker, not stronger: it can be typoed or
 * copied between environments, and two deployments can claim the same one.
 * Preferring the fact you can check over the label someone typed is the same
 * reasoning that made the hard-coded original wrong.
 *
 * A warning is never hidden by breakpoint. The neutral connected-host label is
 * a convenience and gives up its space on a narrow screen; "you are looking at
 * fixture data" is the whole point of the control and stays.
 */
type DataSource = 'fixture' | 'live' | 'connecting' | 'unreachable'

/**
 * One section of the menu.
 *
 * A collapsible one starts folded, opens by itself while you are on one of its
 * pages, and while folded carries the sum of its items' counts so nothing that
 * needs you is hidden by the fold.
 */
function NavSectionView({
  section, pathname, counts,
}: {
  section: NavSection
  pathname: string
  counts: NavCounts
}) {
  const inside = section.items.some((i) => isActive(pathname, i.to))
  const [open, setOpen] = useState(inside)
  useEffect(() => {
    if (inside) setOpen(true)
  }, [inside])
  const shown = !section.collapsible || open
  const folded = section.items.reduce((n, i) => n + (i.badge ? i.badge(counts) : 0), 0)

  return (
    <Box>
      {section.label &&
        (section.collapsible ? (
          <UnstyledButton
            onClick={() => setOpen((o) => !o)}
            aria-expanded={open}
            className="argus-navsection"
            w="100%"
            px="sm"
            py={SP.tight}
            mb={SP.tight}
          >
            <Group gap={SP.snug} justify="space-between" wrap="nowrap">
              <Group gap={SP.snug} wrap="nowrap">
                <IconChevronRight
                  size={14}
                  style={{ transform: open ? 'rotate(90deg)' : undefined, transition: 'transform 120ms' }}
                />
                <Text size={FS.meta} c="dimmed" fw={500}>
                  {section.label}
                </Text>
              </Group>
              {!open && folded > 0 && (
                <Badge size="sm" circle color="amber">
                  {folded}
                </Badge>
              )}
            </Group>
          </UnstyledButton>
        ) : (
          <Text size={FS.meta} c="dimmed" fw={500} px="sm" mb={SP.snug}>
            {section.label}
          </Text>
        ))}
      {shown && (
        <Stack gap={SP.hair}>
          {section.items.map((item) => {
            const count = item.badge ? item.badge(counts) : 0
            return (
              <MantineNavLink
                key={item.to}
                component={Link}
                to={item.to as never}
                activeOptions={{ exact: item.to === '/' }}
                label={item.label}
                description={item.blurb}
                active={isActive(pathname, item.to)}
                variant="light"
                color="azure"
                leftSection={<item.icon size={18} stroke={1.6} />}
                rightSection={
                  count > 0 ? (
                    <Badge size="sm" circle color={item.badgeColor ?? 'amber'}>
                      {count}
                    </Badge>
                  ) : null
                }
                className="argus-navlink"
              />
            )
          })}
        </Stack>
      )}
    </Box>
  )
}

/**
 * Which data is on screen, decided once.
 *
 * `pending` is why this takes an argument: before the first request settles,
 * "not answering" would be a guess rather than an answer.
 */
function dataSource(pending: boolean): DataSource {
  if (!isConfigured()) return 'fixture'
  if (isLive()) return 'live'
  return pending ? 'connecting' : 'unreachable'
}

/**
 * A configured control plane that is not answering, said at the size of the
 * problem.
 *
 * Seeing the rose badge rendered is what prompted this: the corner said "not
 * answering" while the page underneath it announced "13 hosts would not record
 * a bypass", three agents gone silent and six live sessions — every figure
 * invented, because `live.orFallback` serves the fixture rather than showing
 * nothing. A specific, alarming, actionable claim about a fleet that is not
 * yours outweighs a badge, and the badge is the only thing that was dissenting.
 *
 * In the shell rather than per route, so no page can forget it. Settings has
 * carried an equivalent for the *unconfigured* case for a while; the configured
 * one is more dangerous, because then the operator expects real data.
 */
function FallbackBanner() {
  return (
    <Alert
      color="rose"
      variant="filled"
      radius={0}
      icon={<IconAlertTriangle size={17} />}
      title={`${controlPlaneHost()} is not answering — nothing below is your fleet`}
    >
      <Text size={FS.body} lh={1.5}>
        The console fell back to its built-in sample data so the interface still
        works, which means every count, host and session on this page is
        invented. Do not act on any of it. Check that the control plane is
        running and reachable.
      </Text>
    </Alert>
  )
}

/**
 * The sidebar's footer: which data is on screen, as a sentence with a dot.
 *
 * The same four states as the badge, with room to say what each one means
 * rather than leaving it to a tooltip.
 */
function SourceLine({ source }: { source: DataSource }) {
  const tone = {
    live: { dot: 'var(--color-verified)', title: controlPlaneHost(), sub: 'Connected' },
    connecting: { dot: 'var(--mantine-color-slate-5)', title: 'Connecting…', sub: controlPlaneHost() },
    fixture: { dot: 'var(--color-pending)', title: 'Local fixture', sub: 'Sample data, not your servers' },
    unreachable: { dot: 'var(--color-denied)', title: 'Not answering', sub: `${controlPlaneHost()} is unreachable` },
  }[source]
  return (
    <Tooltip
      label={
        source === 'live'
          ? `Connected to ${controlPlaneHost()}. Sessions are brokered through ${gatewayHost()}.`
          : source === 'fixture'
            ? 'No control plane is configured. Every figure on this screen is generated demo data, not your fleet.'
            : source === 'unreachable'
              ? `${controlPlaneHost()} has not answered. The console falls back to sample data rather than showing nothing, so treat anything on screen as unverified.`
              : 'Waiting for the control plane to answer.'
      }
      multiline
      maw={280}
      position="right"
    >
      <Group
        gap={SP.cozy}
        wrap="nowrap"
        px="sm"
        py={SP.cozy}
        style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}
        className="argus-source"
        data-source={source}
      >
        <Box w={8} h={8} className="rounded-full shrink-0" style={{ background: tone.dot }} />
        <Box style={{ minWidth: 0 }}>
          <Text size={FS.meta} fw={500} truncate className="argus-source-title">
            {tone.title}
          </Text>
          <Text size={FS.micro} c="dimmed" truncate>
            {tone.sub}
          </Text>
        </Box>
      </Group>
    </Tooltip>
  )
}

function DataSourceBadge({ source }: { source: DataSource }) {
  if (source === 'fixture') {
    return (
      <Tooltip
        label="No control plane is configured. Every figure on this screen is generated demo data, not your fleet."
        multiline
        maw={280}
      >
        <Badge variant="light" color="amber" size="sm">
          Sample data
        </Badge>
      </Tooltip>
    )
  }

  if (source === 'live') {
    return (
      <Tooltip
        label={`Connected to ${controlPlaneHost()}. Sessions are brokered through ${gatewayHost()}.`}
        multiline
        maw={300}
      >
        <Badge variant="default" color="slate" size="sm" visibleFrom="sm" className="argus-digest">
          {controlPlaneHost()}
        </Badge>
      </Tooltip>
    )
  }

  if (source === 'connecting') {
    return (
      <Badge variant="default" color="slate" size="sm" visibleFrom="sm">
        Connecting…
      </Badge>
    )
  }

  return (
    <Tooltip
      label={`${controlPlaneHost()} has not answered. The console falls back to fixture data rather than showing nothing, so treat anything on screen as unverified.`}
      multiline
      maw={300}
    >
      <Badge variant="light" color="rose" size="sm">
        Not answering
      </Badge>
    </Tooltip>
  )
}

/**
 * The gateway is configured separately from the control plane and can point
 * somewhere else entirely, so "which deployment am I about to act on" is not
 * fully answered by the control plane's host alone.
 */
function gatewayHost(): string {
  try {
    return new URL(GATEWAY_URL).host
  } catch {
    return GATEWAY_URL
  }
}
