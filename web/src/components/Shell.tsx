import {
  ActionIcon, AppShell, Avatar, Badge, Box, Burger, Center, Group, Indicator, Loader,
  Menu, NavLink as MantineNavLink, Progress, ScrollArea, Stack, Text, Tooltip,
  UnstyledButton,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { Link, useRouter, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  IconBell, IconChevronDown, IconLogout, IconSearch, IconSettings, IconShieldLock,
} from '@tabler/icons-react'
import { useCallback, useEffect, useState } from 'react'
import { coverageQuery, meQuery, statsQuery } from '~/lib/queries'
import { SignIn } from '~/components/SignIn'
import { CommandPalette } from '~/components/CommandPalette'
import { NAV_SECTIONS, isActive } from '~/components/nav'
import {
  GATEWAY_URL, controlPlaneHost, isConfigured, isLive, logout, whoami, type Identity,
} from '~/lib/live'
import { FS, SP } from '~/theme'

const HEADER_H = 52
const NAVBAR_W = 244

/**
 * The mark.
 *
 * Azure, not teal: the logo is brand, and teal now means one thing only —
 * something Argus verified. A product mark wearing the verification colour
 * spent the console's loudest signal on decoration.
 */
function Logo({ markOnly = false }: { markOnly?: boolean }) {
  return (
    <Group gap={SP.cozy} wrap="nowrap">
      <Box
        w={26}
        h={26}
        className="grid place-items-center shrink-0"
        style={{
          borderRadius: 7,
          background: 'linear-gradient(140deg, var(--color-brand-bright), #1b4ec4)',
          boxShadow: '0 0 14px rgba(37, 99, 235, 0.35)',
        }}
      >
        <IconShieldLock size={15} color="#fff" stroke={2.4} />
      </Box>
      {!markOnly && (
        <Box>
          <Text fw={700} size={FS.body} lh={1.1} c="slate.0" style={{ letterSpacing: '0.04em' }}>
            ARGUS
          </Text>
          <Text size={FS.micro} c="dimmed" lh={1.15} style={{ letterSpacing: '0.09em' }}>
            PRIVILEGED ACCESS
          </Text>
        </Box>
      )}
    </Group>
  )
}

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
  const statsPending = statsQuery_.isPending
  const { data: cov } = useQuery(coverageQuery())

  // Both directions count as a gap: an unmanaged host and a managed host with
  // no agent are each somewhere a privileged session goes unrecorded.
  const navStats = {
    requestsPending: stats?.requestsPending ?? 0,
    sessionsActive: stats?.sessionsActive ?? 0,
    coverageGaps: cov ? cov.unreviewedHosts + cov.assetsUnmonitored : 0,
  }
  const pathname = useRouterState({ select: (s) => s.location.pathname })

  // Cmd-K anywhere. Captured on the document so it works while focus is inside
  // a table, a filter or the replay player.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'k' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault()
        palette.toggle()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [palette])

  // Closing the drawer on navigation: on a phone the overlay otherwise stays up
  // over the page it just took you to.
  useEffect(close, [pathname, close])

  const caCoverage = stats
    ? Math.round(((stats.assetsTotal - stats.standingCredentialAssets) / stats.assetsTotal) * 100)
    : 0

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
            <DataSourceBadge pending={statsPending} />
          </Group>

          {/* A search field rather than an icon: the shortcut is discoverable
              only if something on screen says it exists. */}
          <UnstyledButton
            onClick={palette.open}
            visibleFrom="md"
            aria-label="Search pages, hosts, sessions and people"
            style={{
              flex: 1,
              maxWidth: 340,
              border: '1px solid var(--color-line)',
              background: 'var(--color-raised)',
              borderRadius: 7,
              padding: '5px 10px',
            }}
          >
            <Group gap={SP.snug} wrap="nowrap">
              <IconSearch size={14} opacity={0.6} />
              <Text size={FS.meta} c="dimmed" style={{ flex: 1 }}>
                Search…
              </Text>
              <Badge size="xs" variant="default" color="slate" className="argus-digest">
                ⌘K
              </Badge>
            </Group>
          </UnstyledButton>

          <Group gap="xs" wrap="nowrap">
            <ActionIcon
              hiddenFrom="md"
              variant="subtle"
              color="slate"
              aria-label="Search"
              onClick={palette.open}
            >
              <IconSearch size={17} />
            </ActionIcon>

            {stats && stats.sessionsActive > 0 && (
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

            <Indicator
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
            </Indicator>

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
        <Box p="md" pb="sm" visibleFrom="sm">
          <Logo />
        </Box>

        <AppShell.Section grow component={ScrollArea} px="xs" pb="xs">
          <Stack gap="md">
            {NAV_SECTIONS.map((section) => (
              <Box key={section.label}>
                <Text
                  size={FS.micro}
                  c="dimmed"
                  fw={700}
                  tt="uppercase"
                  px="xs"
                  mb={SP.tight}
                  style={{ letterSpacing: '0.1em' }}
                >
                  {section.label}
                </Text>
                <Stack gap={SP.hair}>
                  {section.items.map((item) => {
                    const active = isActive(pathname, item.to)
                    const count = item.badge ? item.badge(navStats) : 0
                    return (
                      <Tooltip
                        key={item.to}
                        label={item.blurb}
                        position="right"
                        openDelay={600}
                        maw={230}
                        multiline
                      >
                        <MantineNavLink
                          component={Link}
                          to={item.to as never}
                          label={item.label}
                          active={active}
                          variant="light"
                          color="azure"
                          leftSection={<item.icon size={16} stroke={1.7} />}
                          rightSection={
                            count > 0 ? (
                              <Badge size="xs" circle color={item.badgeColor ?? 'amber'}>
                                {count}
                              </Badge>
                            ) : null
                          }
                          styles={{ label: { fontSize: 13 } }}
                        />
                      </Tooltip>
                    )
                  })}
                </Stack>
              </Box>
            ))}
          </Stack>
        </AppShell.Section>

        {/* The product's central claim, kept in view rather than stated once on
            a marketing page: how much of the fleet has no reusable credential
            left to steal.

            Hidden on the Overview, which renders the same figure as a full card
            with its progress bar and explanation — the two were on screen
            together, saying the same thing twice. */}
        <AppShell.Section p="md" pt="xs" display={pathname === '/' ? 'none' : undefined}>
          <Box
            p="xs"
            style={{
              border: '1px solid var(--color-line)',
              borderRadius: 7,
              background: 'var(--color-raised)',
            }}
          >
            <Group gap={SP.snug} mb={SP.snug} wrap="nowrap">
              <IconShieldLock size={13} style={{ color: 'var(--color-verified)' }} />
              <Text size={FS.micro} fw={700} c="slate.2" style={{ letterSpacing: '0.06em' }}>
                ZERO STANDING PRIVILEGE
              </Text>
            </Group>
            <Progress value={caCoverage} color="teal" size={4} radius="xl" mb={SP.snug} />
            <Text size={FS.micro} c="dimmed" lh={1.45}>
              {stats
                ? `${stats.assetsTotal - stats.standingCredentialAssets} of ${stats.assetsTotal} assets on certificate auth`
                : '—'}
            </Text>
          </Box>
        </AppShell.Section>
      </AppShell.Navbar>

      <AppShell.Main>{children}</AppShell.Main>

      <CommandPalette opened={paletteOpen} onClose={palette.close} />
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
function DataSourceBadge({ pending }: { pending: boolean }) {
  if (!isConfigured()) {
    return (
      <Tooltip
        label="No control plane is configured. Every figure on this screen is generated demo data, not your fleet."
        multiline
        maw={280}
      >
        <Badge variant="light" color="amber" size="sm">
          Local fixture
        </Badge>
      </Tooltip>
    )
  }

  if (isLive()) {
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

  // Before the first request settles, "not answering" would be a guess.
  if (pending) {
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
