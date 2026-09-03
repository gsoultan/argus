import {
  ActionIcon, AppShell, Avatar, Badge, Box, Burger, Group, Indicator, Menu,
  NavLink as MantineNavLink, ScrollArea, Stack, Text, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { Link, useRouterState } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  IconActivity, IconBell, IconChevronDown, IconClipboardCheck, IconFileDescription,
  IconLogout, IconPlugConnected, IconServer2, IconSettings, IconShieldLock,
  IconTerminal2, IconUsers,
} from '@tabler/icons-react'
import { useEffect, useState } from 'react'
import { Button, Center, Loader, Paper } from '@mantine/core'
import { coverageQuery, meQuery, statsQuery } from '~/lib/queries'
import { isConfigured, loginURL, logout, whoami, type Identity } from '~/lib/live'

const NAV: {
  to: string
  label: string
  icon: typeof IconActivity
  badge?: (s: { requestsPending: number; sessionsActive: number; coverageGaps: number }) => number
}[] = [
  { to: '/', label: 'Overview', icon: IconActivity },
  { to: '/connect', label: 'Connect', icon: IconPlugConnected },
  { to: '/sessions', label: 'Sessions', icon: IconTerminal2, badge: (s) => s.sessionsActive },
  { to: '/requests', label: 'Access requests', icon: IconClipboardCheck, badge: (s) => s.requestsPending },
  { to: '/assets', label: 'Assets', icon: IconServer2 },
  // Badged on the unreviewed count: a coverage gap nobody is told about is one
  // nobody closes.
  { to: '/coverage', label: 'Coverage', icon: IconShieldLock, badge: (s) => s.coverageGaps },
  { to: '/audit', label: 'Audit log', icon: IconFileDescription },
  { to: '/users', label: 'Users & roles', icon: IconUsers },
  { to: '/settings', label: 'Settings', icon: IconSettings },
]

function Logo() {
  return (
    <Group gap={9} wrap="nowrap">
      <Box
        w={26}
        h={26}
        className="grid place-items-center shrink-0"
        style={{
          borderRadius: 7,
          background: 'linear-gradient(140deg, var(--color-verified), #0f766e)',
          boxShadow: '0 0 14px rgba(45,212,167,0.32)',
        }}
      >
        <IconShieldLock size={15} color="#04140f" stroke={2.4} />
      </Box>
      <Box>
        <Text fw={700} size="sm" lh={1.1} c="slate.0" style={{ letterSpacing: '0.02em' }}>
          ARGUS
        </Text>
        <Text size="9px" c="dimmed" lh={1.1} style={{ letterSpacing: '0.09em' }}>
          PRIVILEGED ACCESS
        </Text>
      </Box>
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

  useEffect(() => {
    if (!isConfigured()) {
      setIdentity({ authenticated: true })
      return
    }
    void whoami().then(setIdentity)
  }, [])

  if (!identity) {
    return <Center h="100vh"><Loader size="sm" color="teal" /></Center>
  }

  if (!identity.authenticated) {
    return (
      <Center h="100vh">
        <Paper p="xl" withBorder maw={380}>
          <Group gap={9} mb="md">
            <Box
              w={26}
              h={26}
              className="grid place-items-center shrink-0"
              style={{
                borderRadius: 7,
                background: 'linear-gradient(140deg, var(--color-verified), #0f766e)',
              }}
            >
              <IconShieldLock size={15} color="#04140f" stroke={2.4} />
            </Box>
            <Text fw={700} size="sm" style={{ letterSpacing: '0.02em' }}>ARGUS</Text>
          </Group>
          <Text size="sm" fw={600} mb={4}>Sign in required</Text>
          <Text size="xs" c="dimmed" mb="lg">
            Every action in Argus is attributed to a person, so there is no anonymous
            access — not even read-only.
          </Text>
          {identity.unreachable ? (
            <Text size="xs" c="amber.4">
              The control plane could not be reached. It may not be running, or the
              console may be pointed at the wrong address or scheme — it serves HTTPS
              when a certificate is configured.
            </Text>
          ) : identity.oidcEnabled ? (
            <Button fullWidth component="a" href={loginURL()}
              leftSection={<IconShieldLock size={15} />}>
              Sign in with SSO
            </Button>
          ) : (
            <Text size="xs" c="amber.4">
              The control plane is running but has no identity provider configured.
            </Text>
          )}
        </Paper>
      </Center>
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
  const [opened, { toggle }] = useDisclosure()
  const { data: me } = useQuery(meQuery())
  const { data: stats } = useQuery(statsQuery())
  const { data: cov } = useQuery(coverageQuery())

  // Both directions count as a gap: an unmanaged host and a managed host with
  // no agent are each somewhere a privileged session goes unrecorded.
  const navStats = {
    requestsPending: stats?.requestsPending ?? 0,
    sessionsActive: stats?.sessionsActive ?? 0,
    coverageGaps: cov ? cov.unreviewedHosts + cov.assetsUnmonitored : 0,
  }
  const pathname = useRouterState({ select: (s) => s.location.pathname })

  return (
    <AppShell
      header={{ height: 52 }}
      navbar={{ width: 232, breakpoint: 'sm', collapsed: { mobile: !opened } }}
      padding={0}
      styles={{
        header: { background: 'var(--color-surface)', borderColor: 'var(--color-line)' },
        navbar: { background: 'var(--color-surface)', borderColor: 'var(--color-line)' },
        main: { background: 'var(--color-void)' },
      }}
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap">
          <Group gap="md" wrap="nowrap">
            <Burger opened={opened} onClick={toggle} hiddenFrom="sm" size="sm" />
            <Box hiddenFrom="sm"><Logo /></Box>
            <Badge
              variant="outline"
              color="slate"
              size="sm"
              visibleFrom="sm"
              className="argus-digest"
            >
              northwind-prod · ap-southeast-3
            </Badge>
          </Group>

          <Group gap="xs" wrap="nowrap">
            {stats && stats.sessionsActive > 0 && (
              <Tooltip label={`${stats.sessionsActive} sessions in progress right now`}>
                <Badge
                  color="sky"
                  variant="light"
                  leftSection={<Box w={6} h={6} className="rounded-full bg-sky-400 animate-pulse" />}
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
              <ActionIcon variant="subtle" color="slate" component={Link} to="/requests">
                <IconBell size={17} />
              </ActionIcon>
            </Indicator>

            <Menu position="bottom-end" width={210} withArrow>
              <Menu.Target>
                <Group gap={7} className="cursor-pointer select-none" wrap="nowrap">
                  <Avatar size={27} radius="sm" color="teal" variant="light">
                    {me?.displayName.slice(0, 2).toUpperCase() ?? '··'}
                  </Avatar>
                  <Box visibleFrom="sm">
                    <Text size="xs" fw={600} lh={1.15}>{me?.displayName}</Text>
                    <Text size="10px" c="dimmed" lh={1.15} tt="capitalize">{me?.role}</Text>
                  </Box>
                  <IconChevronDown size={13} opacity={0.5} />
                </Group>
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Label>{me?.email}</Menu.Label>
                <Menu.Item leftSection={<IconSettings size={14} />} component={Link} to="/settings">
                  Preferences
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

        <AppShell.Section grow component={ScrollArea} px="xs">
          <Stack gap={2}>
            {NAV.map((item) => {
              const active = item.to === '/' ? pathname === '/' : pathname.startsWith(item.to)
              const count = item.badge ? item.badge(navStats) : 0
              return (
                <MantineNavLink
                  key={item.to}
                  component={Link}
                  to={item.to as never}
                  label={item.label}
                  active={active}
                  variant="light"
                  color="teal"
                  leftSection={<item.icon size={16} stroke={1.7} />}
                  rightSection={
                    count > 0 ? (
                      <Badge size="xs" circle color={item.to === '/sessions' ? 'sky' : 'amber'}>
                        {count}
                      </Badge>
                    ) : null
                  }
                  styles={{ root: { borderRadius: 7 }, label: { fontSize: 13 } }}
                />
              )
            })}
          </Stack>
        </AppShell.Section>

        <AppShell.Section p="md">
          <Box
            p="xs"
            style={{
              border: '1px solid var(--color-line)',
              borderRadius: 7,
              background: 'var(--color-raised)',
            }}
          >
            <Group gap={6} mb={4} wrap="nowrap">
              <IconShieldLock size={13} className="text-teal-400" />
              <Text size="10px" fw={600} c="slate.2" style={{ letterSpacing: '0.05em' }}>
                ZERO STANDING PRIVILEGE
              </Text>
            </Group>
            <Text size="10px" c="dimmed" lh={1.4}>
              {stats
                ? `${stats.assetsTotal - stats.standingCredentialAssets} of ${stats.assetsTotal} assets on certificate auth`
                : '—'}
            </Text>
          </Box>
        </AppShell.Section>
      </AppShell.Navbar>

      <AppShell.Main>{children}</AppShell.Main>
    </AppShell>
  )
}

/** Consistent page header used by every route. */
export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string
  description?: string
  actions?: React.ReactNode
}) {
  return (
    <Group
      justify="space-between"
      align="flex-start"
      px="lg"
      py="md"
      wrap="nowrap"
      style={{ borderBottom: '1px solid var(--color-line)' }}
    >
      <Box>
        <Text component="h1" fw={600} size="19px" lh={1.3}>{title}</Text>
        {description && (
          <Text size="xs" c="dimmed" mt={3} maw={640}>{description}</Text>
        )}
      </Box>
      {actions && <Group gap="xs" wrap="nowrap">{actions}</Group>}
    </Group>
  )
}
