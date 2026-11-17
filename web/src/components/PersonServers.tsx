import { Badge, Box, Button, Group, Loader, Modal, Stack, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { Mono } from '~/components/primitives'
import { meQuery, personAccessQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { AccessItem, AccessSection, User } from '~/types/domain'

/**
 * What one person may reach in this organization: servers, databases,
 * brokers, cloud, and any kind added later, and where each part comes from.
 * The one place to answer "what can Citra do?".
 *
 * The sections come from the control plane's list of kinds, so a new kind of
 * thing shows here without this file changing. It used to list servers only,
 * so a database given to someone appeared nowhere on their page.
 */
export function PersonServersModal({ user, onClose }: { user: User; onClose: () => void }) {
  const { data, isLoading, error } = useQuery(personAccessQuery(user.email))
  const { data: me } = useQuery(meQuery())
  const org = me?.multiOrg && me.org?.name ? ` in ${me.org.name}` : ''
  return (
    <Modal opened onClose={onClose} title={`What ${user.displayName} can do${org}`} size="lg">
      <Stack gap="sm">
        {isLoading && <Loader size="sm" />}
        {error && <Text size="sm" c="rose">{error instanceof Error ? error.message : 'Could not load.'}</Text>}
        {data?.map((s) => <Section key={s.kind} s={s} />)}
        {user.loginName && (
          <Text size={FS.meta} c="dimmed">
            Their own account on servers is <Mono>{user.loginName}</Mono>.
          </Text>
        )}
        {me?.multiOrg && (
          <Text size={FS.meta} c="dimmed">
            Only what this organization gave them. Each organization they are in decides on its own.
          </Text>
        )}
      </Stack>
    </Modal>
  )
}

function Section({ s }: { s: AccessSection }) {
  return (
    <Box>
      <SectionHeading>{s.label}</SectionHeading>
      {s.items.length === 0
        ? <Text size="sm" c="dimmed" py={SP.tight}>{s.empty}</Text>
        : s.items.map((it) => <Line key={it.id} it={it} />)}
    </Box>
  )
}

export function SectionHeading({ children }: { children: string }) {
  return (
    <Text size={FS.meta} fw={650} c="dimmed" tt="uppercase" mt={SP.tight} style={{ letterSpacing: '.05em' }}>
      {children}
    </Text>
  )
}

function Line({ it }: { it: AccessItem }) {
  return (
    <Group justify="space-between" wrap="nowrap" gap="sm" py={SP.tight}
      style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
      <Box style={{ minWidth: 0 }}>
        <Text size="sm" truncate>
          <Text span fw={500} ff="monospace">{it.name}</Text>
          {it.where && <Text span size={FS.meta} c="dimmed"> on {it.where}</Text>}
        </Text>
        <Text size={FS.meta} c="dimmed">
          {it.what}
          {it.until ? ` · until ${new Date(it.until).toLocaleString()}` : ''}
        </Text>
      </Box>
      <Group gap={SP.tight} wrap="nowrap">
        {it.own && <Badge variant="light" color="amber">set for them</Badge>}
        {it.teams.map((t) => <Badge key={t} variant="outline" color="slate">from {t}</Badge>)}
      </Group>
    </Group>
  )
}

/**
 * Each kind of thing, with a way to give it: the step after adding someone,
 * when nothing here is theirs yet. Drawn from the same list as the window.
 */
export function GiveAccess({ email, onGo }: { email: string; onGo: () => void }) {
  const { data } = useQuery(personAccessQuery(email))
  const navigate = useNavigate()
  if (!data) return <Loader size="sm" />
  return (
    <Group gap="xs">
      {data.map((s) => (
        <Button key={s.kind} size="compact-sm" variant="light"
          onClick={() => { onGo(); void navigate({ to: s.give } as never) }}>
          {s.label}{s.items.length ? ` (${s.items.length})` : ''}
        </Button>
      ))}
    </Group>
  )
}
