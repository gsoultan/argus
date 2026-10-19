import { Badge, Box, Button, Card, Group, Skeleton, Stack, Text, ThemeIcon, Tooltip } from '@mantine/core'
import {
  IconBrandWindows, IconClock, IconDeviceDesktop, IconLock, IconServer2, IconTerminal2,
} from '@tabler/icons-react'
import { ButtonLink } from '~/components/links'
import { relTime } from '~/components/primitives'
import type { Asset } from '~/types/domain'
import { FS, SP } from '~/theme'

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
  // A connected agent tunnel is how an SSH session opens without the gateway
  // reaching the server at all, so it counts as reachable on its own.
  if (a.protocol !== 'rdp' && a.tunnelConnected === true) return 'online'
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
export function ServerCard({ asset: a }: { asset: Asset }) {
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
            You have no account on this server yet. Ask an admin to add one for you.
          </Text>
        ) : (
          <Box>
            <Text size={FS.micro} c="dimmed" mb={SP.tight}>
              Log in as
            </Text>
            <Group gap={SP.snug}>
              {a.principals.map((p) => (
                <ButtonLink
                  key={p}
                  to="/connect"
                  search={{ host: a.hostname, as: p }}
                  size="compact-xs"
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
          <Text size={FS.micro} c="dimmed">
            Checked {relTime(a.lastCheckedAt)}
          </Text>
          {disabled ? (
            <Button size="xs" disabled leftSection={rdp ? <IconDeviceDesktop size={14} /> : <IconTerminal2 size={14} />}>
              Connect
            </Button>
          ) : (
            <ButtonLink
              to="/connect"
              search={{ host: a.hostname }}
              size="xs"
              variant={reach === 'offline' ? 'default' : 'filled'}
              leftSection={rdp ? <IconDeviceDesktop size={14} /> : <IconTerminal2 size={14} />}
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
          <Text size={FS.micro} c="amber.4">
            {expiresAt ? `Access ends ${relTime(expiresAt)}` : 'Approved'}
          </Text>
          <ButtonLink
            to="/connect"
            search={{ host: hostname, as: principal }}
            size="xs"
            leftSection={<IconTerminal2 size={14} />}
            aria-label={`Connect to ${name} as ${principal}`}
          >
            Connect
          </ButtonLink>
        </Group>
      </Stack>
    </Card>
  )
}
