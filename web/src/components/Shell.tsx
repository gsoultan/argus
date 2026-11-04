import {
  ActionIcon, Alert, AppShell, Avatar, Badge, Box, Burger, Button, Center, Group,
  Loader, Menu, NavLink as MantineNavLink, Stack, Text, Tooltip,
  UnstyledButton,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { Link, useNavigate, useRouter, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconBuilding, IconCheck, IconSettings, IconChevronDown, IconInfoCircle, IconKey, IconLogout,
  IconSearch, IconShieldLock,
} from '@tabler/icons-react'
import { Suspense, lazy, useCallback, useEffect, useState } from 'react'
import { coverageQuery, directLoginsQuery, meQuery, myServerAccountQuery, statsQuery } from '~/lib/queries'
import { SignIn } from '~/components/SignIn'
// Eager on purpose: lazy, it pulled seven shared Mantine pieces out into chunks
// of their own and cost the first visit 3.4 kB instead of 1.0. See weight.spec.ts.
import { Setup } from '~/components/Setup'
import { canSee, roleLabel, seesWholeFleet } from '~/lib/roles'
import type { UserRole } from '~/types/domain'
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
// Lazy for the same reason: eager, it put the first visit over its budget.
const MySshKeyModal = lazy(() =>
  import('~/components/MySshKey').then((m) => ({ default: m.MySshKeyModal })),
)
// Lazy too: the QR code generator and the enrolment flow are for the first
// sign-in of a privileged account, and never again for most.
const TwoFactorModal = lazy(() =>
  import('~/components/TwoFactor').then((m) => ({ default: m.TwoFactorModal })),
)
const TwoFactorRequired = lazy(() =>
  import('~/components/TwoFactor').then((m) => ({ default: m.TwoFactorRequired })),
)
const PasswordChangeRequired = lazy(() =>
  import('~/components/ChoosePassword').then((m) => ({ default: m.PasswordChangeRequired })),
)
const ChangePasswordModal = lazy(() =>
  import('~/components/ChoosePassword').then((m) => ({ default: m.ChangePasswordModal })),
)
import {
  isActive, navFor, type NavCounts, type NavItem,
} from '~/components/nav'
import { LogoMark } from '~/components/Logo'
import { AlertBell } from '~/components/AlertBell'
import { ColorSchemeMenu } from '~/components/ColorSchemeMenu'
import {
  argusVersion, controlPlaneHost, isConfigured, isLive, logout, signOutEverywhere, switchOrganization,
  whoami, type Identity,
} from '~/lib/live'
import { FS, SP } from '~/theme'

const HEADER_H = 60

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

  // Signed in with a temporary password: choosing one's own comes first,
  // before the second step, so the person who sets up the authenticator is
  // the one who chose the password.
  if (identity.passwordChangeRequired) {
    return (
      <Suspense fallback={null}>
        <PasswordChangeRequired email={identity.email} name={identity.displayName}
          expiresAt={identity.temporaryPasswordExpiresAt} onDone={handleSignedIn} />
      </Suspense>
    )
  }

  // Signed in, but the role must have a second step and has none. The
  // control plane refuses every other request, so drawing the console would
  // be drawing errors.
  if (identity.mfaSetupRequired) {
    return (
      <Suspense fallback={null}>
        <TwoFactorRequired email={identity.email} onDone={handleSignedIn} />
      </Suspense>
    )
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
  const [sshKeyOpen, sshKeyModal] = useDisclosure(false)
  const [twoFactorOpen, twoFactorModal] = useDisclosure(false)
  const [passwordOpen, passwordModal] = useDisclosure(false)
  const { data: me } = useQuery(meQuery())
  const queryClient = useQueryClient()
  // A key is only asked of people a team lets SSH straight to a server; the
  // rest connect from the console and have no use for one. Someone who lost
  // direct SSH still sees it while a key is saved, so they can take it off.
  const { data: serverAccount } = useQuery({ ...myServerAccountQuery(), enabled: !!me, retry: false })
  const wantsKey = !!(serverAccount?.directSsh || serverAccount?.sshKey)
  const statsQuery_ = useQuery(statsQuery())
  const stats = statsQuery_.data
  const source = dataSource(statsQuery_.isPending)
  const { data: cov } = useQuery(coverageQuery())
  const whole = seesWholeFleet(me?.role)
  const approves = canSee(me?.role, 'approvers')
  // Held logins are counted with requests: both are someone waiting on a
  // decision, and the person deciding should not have to add them up.
  const { data: held } = useQuery({ ...directLoginsQuery(), enabled: approves })

  const counts: NavCounts = {
    approvalsPending:
      (stats?.requestsPending ?? 0) + (held?.filter((d) => d.state === 'pending').length ?? 0),
    // Only for those who can finish adding one: on anyone else's Servers entry
    // it would be a number they can do nothing about.
    serversWaiting: canSee(me?.role, 'admin') ? (cov?.unreviewedHosts ?? 0) : 0,
  }
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const items = navFor(me?.role)

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
      // The menu lives in the top bar. On a phone the same items open from the
      // burger as a panel down the side.
      navbar={{ width: 260, breakpoint: 'md', collapsed: { mobile: !opened, desktop: true } }}
      padding={0}
      styles={{
        header: { background: 'var(--color-surface)', borderColor: 'var(--color-line)' },
        navbar: { background: 'var(--color-surface)', borderColor: 'var(--color-line)' },
        main: { background: 'var(--color-void)' },
      }}
    >
      <AppShell.Header>
        <Group h="100%" px={{ base: 'md', sm: 'xl' }} justify="space-between" wrap="nowrap" gap="md">
          <Group gap="lg" wrap="nowrap" style={{ minWidth: 0 }}>
            <Burger
              opened={opened}
              onClick={toggle}
              hiddenFrom="md"
              size="sm"
              aria-label={opened ? 'Close navigation' : 'Open navigation'}
            />
            <Link to="/" style={{ textDecoration: 'none', color: 'inherit' }} aria-label="Argus home">
              <Group gap={SP.cozy} wrap="nowrap">
                <LogoMark size={28} />
                <Text fw={700} size="md" visibleFrom="xs">Argus</Text>
              </Group>
            </Link>
            <Group component="nav" aria-label="Main" gap={4} wrap="nowrap" visibleFrom="md">
              {items.map((item) => (
                <TopNavLink key={item.to} item={item} pathname={pathname} counts={counts} />
              ))}
            </Group>
          </Group>

          <Group gap="xs" wrap="nowrap">
            {source !== 'live' && source !== 'connecting' && <DataSourceBadge source={source} />}

            {/* Which organization this console is signed in to, always, so
                the feature is visible before a second organization exists.
                The menu switches between the ones this person is in and, for
                a platform owner, opens the place to add more. */}
            {me?.org && (
              <OrgSwitcher
                current={me.org}
                orgs={me.orgs?.length ? me.orgs : [{ ...me.org, role: me.role, home: true }]}
                canManage={me.platformOwner === true}
              />
            )}

            {whole && stats && stats.sessionsActive > 0 && (
              <Tooltip label={`${stats.sessionsActive} sessions in progress right now`}>
                <Badge
                  component={Link}
                  to="/activity"
                  color="sky"
                  variant="light"
                  visibleFrom="sm"
                  style={{ cursor: 'pointer' }}
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

            {whole && (
              <Tooltip label="Find a server, session or person (⌘K)">
                <ActionIcon
                  variant="subtle"
                  color="slate"
                  size="lg"
                  aria-label="Search pages, hosts, sessions and people"
                  onClick={palette.open}
                >
                  <IconSearch size={18} />
                </ActionIcon>
              </Tooltip>
            )}

            <ColorSchemeMenu />

            {me && <AlertBell />}

            <Menu position="bottom-end" width={240} withArrow>
              <Menu.Target>
                <UnstyledButton aria-label="Account menu">
                  <Group gap={SP.cozy} wrap="nowrap">
                    <Avatar size={32} radius="xl" color="azure" variant="light">
                      {me?.displayName.slice(0, 2).toUpperCase() ?? '··'}
                    </Avatar>
                    <Box visibleFrom="sm" ta="left">
                      <Text size={FS.meta} fw={600} lh={1.15}>
                        {me?.displayName}
                      </Text>
                      <Text size={FS.micro} c="dimmed" lh={1.15}>
                        {me ? roleLabel(me.role) : ''}
                      </Text>
                    </Box>
                    <IconChevronDown size={13} opacity={0.5} />
                  </Group>
                </UnstyledButton>
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Label>{me?.email}</Menu.Label>
                <Menu.Item disabled leftSection={<SourceDot source={source} />}>
                  <Text size={FS.meta} className="argus-source-title">{sourceSentence(source)}</Text>
                </Menu.Item>
                <Menu.Divider />
                {wantsKey && (
                  <Menu.Item leftSection={<IconKey size={14} />} onClick={sshKeyModal.open}>
                    My SSH key
                  </Menu.Item>
                )}
                {isLive() && (
                  <Menu.Item leftSection={<IconShieldLock size={14} />} onClick={twoFactorModal.open}>
                    {me?.mfaEnrolled ? 'Replace my authenticator' : 'Turn on two-step sign-in'}
                  </Menu.Item>
                )}
                {isLive() && !me?.shared && (
                  <Menu.Item leftSection={<IconKey size={14} />} onClick={passwordModal.open}>
                    Change my password
                  </Menu.Item>
                )}
                <Menu.Item
                  component={Link}
                  to="/about"
                  leftSection={<IconInfoCircle size={14} />}
                  rightSection={
                    <Text size={FS.micro} c="dimmed" className="argus-digest">{argusVersion()}</Text>
                  }
                >
                  About Argus
                </Menu.Item>
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
                {isLive() && (
                  <Menu.Item
                    leftSection={<IconLogout size={14} />}
                    color="rose"
                    onClick={async () => {
                      await signOutEverywhere()
                      window.location.reload()
                    }}
                  >
                    Sign out everywhere
                  </Menu.Item>
                )}
              </Menu.Dropdown>
            </Menu>
            {sshKeyOpen && (
              <Suspense fallback={null}>
                <MySshKeyModal opened onClose={sshKeyModal.close} />
              </Suspense>
            )}
            {passwordOpen && (
              <Suspense fallback={null}>
                <ChangePasswordModal opened email={me?.email} name={me?.displayName} onClose={passwordModal.close} />
              </Suspense>
            )}
            {twoFactorOpen && (
              <Suspense fallback={null}>
                <TwoFactorModal
                  opened
                  enrolled={!!me?.mfaEnrolled}
                  onClose={() => {
                    twoFactorModal.close()
                    void queryClient.invalidateQueries({ queryKey: meQuery().queryKey })
                  }}
                />
              </Suspense>
            )}
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="sm" hiddenFrom="md">
        <Stack gap={2}>
          {items.map((item) => {
            const count = item.badge ? item.badge(counts) : 0
            return (
              <MantineNavLink
                key={item.to}
                component={Link}
                to={item.to as never}
                label={item.label}
                active={isActive(pathname, item.to)}
                variant="light"
                color="azure"
                leftSection={<item.icon size={18} stroke={1.6} />}
                rightSection={count > 0 ? <Badge size="sm" circle color="rose" variant="filled">{count}</Badge> : null}
                className="argus-navlink"
              />
            )
          })}
        </Stack>
      </AppShell.Navbar>

      <AppShell.Main>
        {source === 'unreachable' && <FallbackBanner />}
        {/* Full width: the page owns its own gutters (PageHeader, PageBody)
            and a workspace runs edge to edge. A centred 1200px column left a
            third of a wide screen empty while tables truncated. */}
        <Box className="argus-main">
          {children}
        </Box>
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

/** One word in the top bar, with a red count when something waits on you. */
function TopNavLink({ item, pathname, counts }: { item: NavItem; pathname: string; counts: NavCounts }) {
  const count = item.badge ? item.badge(counts) : 0
  const active = isActive(pathname, item.to)
  return (
    <Link
      to={item.to as never}
      className="argus-topnav"
      data-active={active || undefined}
      aria-current={active ? 'page' : undefined}
    >
      {item.label}
      {count > 0 && (
        <Badge size="sm" circle color="rose" variant="filled" aria-label={`${count} waiting`}>
          {count}
        </Badge>
      )}
    </Link>
  )
}

function SourceDot({ source }: { source: DataSource }) {
  const dot = {
    live: 'var(--color-verified)',
    connecting: 'var(--color-ink-muted)',
    fixture: 'var(--color-pending)',
    unreachable: 'var(--color-denied)',
  }[source]
  return <Box w={8} h={8} className="rounded-full shrink-0" style={{ background: dot }} />
}

function sourceSentence(source: DataSource): string {
  switch (source) {
    case 'live':
      return `Connected to ${controlPlaneHost()}`
    case 'connecting':
      return 'Connecting…'
    case 'fixture':
      return 'Sample data, not your servers'
    case 'unreachable':
      return `${controlPlaneHost()} is not answering`
  }
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
        label={`Connected to ${controlPlaneHost()}. Sessions open through each server's Argus agent.`}
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
 * The organization menu: which one this is, the others this person can
 * switch to, and for a platform owner the way to manage them.
 *
 * Switching replaces the session cookie with the other organization's and
 * reloads, so every query on screen starts over there: nothing from the
 * organization being left survives the switch, which is the point of it.
 */
function OrgSwitcher({ current, orgs, canManage }: {
  current: NonNullable<Identity['org']>
  orgs: NonNullable<Identity['orgs']>
  canManage: boolean
}) {
  const navigate = useNavigate()
  const [switching, setSwitching] = useState<string | null>(null)
  return (
    <Menu position="bottom-end" withinPortal width={280}>
      <Menu.Target>
        <Button
          variant="default"
          size="xs"
          radius="md"
          aria-label={`Organization: ${current.name}. Open the organization menu`}
          leftSection={<IconBuilding size={14} color="var(--mantine-color-violet-6)" />}
          rightSection={<IconChevronDown size={13} opacity={0.6} />}
          styles={{ label: { maxWidth: 180, overflow: 'hidden', textOverflow: 'ellipsis' } }}
        >
          {current.name}
        </Button>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>{orgs.length > 1 ? 'Switch organization' : 'Organization'}</Menu.Label>
        {orgs.map((o) => (
          <Menu.Item
            key={o.id}
            disabled={switching !== null && o.slug !== switching}
            fw={o.id === current.id ? 600 : undefined}
            leftSection={o.id === current.id ? <IconCheck size={14} color="var(--mantine-color-violet-6)" /> : <Box w={14} />}
            rightSection={<Text size={FS.micro} c="dimmed">{roleLabel(o.role as UserRole)}</Text>}
            onClick={async () => {
              if (o.id === current.id) return
              setSwitching(o.slug)
              try {
                await switchOrganization(o.slug)
                window.location.replace('/')
              } catch {
                setSwitching(null)
              }
            }}
          >
            {o.name}
            {o.home && orgs.length > 1 && <Text span size={FS.micro} c="dimmed"> · home</Text>}
          </Menu.Item>
        ))}
        {canManage && (
          <>
            <Menu.Divider />
            <Menu.Item
              leftSection={<IconSettings size={14} />}
              onClick={() => void navigate({ to: '/settings', search: { section: 'organizations' } })}
            >
              Manage organizations
            </Menu.Item>
          </>
        )}
      </Menu.Dropdown>
    </Menu>
  )
}
