import { Badge, Box, Group, Loader, Modal, Stack, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Mono } from '~/components/primitives'
import { describeTeamAccess } from '~/components/WhoCanConnect'
import { personServersQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { PersonServer, User } from '~/types/domain'

/**
 * What one person may do on every server, and where each part comes from:
 * their teams, or a setting of their own that replaces their teams' there.
 * The one place to answer "what can Citra do?".
 */
export function PersonServersModal({ user, onClose }: { user: User; onClose: () => void }) {
  const { data, isLoading, error } = useQuery(personServersQuery(user.email))
  return (
    <Modal opened onClose={onClose} title={`What ${user.displayName} can do`} size="lg">
      <Stack gap="sm">
        {isLoading && <Loader size="sm" />}
        {error && <Text size="sm" c="rose">{error instanceof Error ? error.message : 'Could not load.'}</Text>}
        {data && data.length === 0 && (
          <Text size="sm" c="dimmed">No servers yet. Give one to a team of theirs, or to them, on the server's page.</Text>
        )}
        {data?.map((s) => <ServerLine key={s.assetId} s={s} />)}
        {user.loginName && (
          <Text size={FS.meta} c="dimmed">
            Their own account on servers is <Mono>{user.loginName}</Mono>.
          </Text>
        )}
      </Stack>
    </Modal>
  )
}

function ServerLine({ s }: { s: PersonServer }) {
  const what = describeTeamAccess({
    principals: s.principals, personalAccounts: s.personalAccount, sudo: s.sudo, directSsh: s.directSsh,
  }).replace('own accounts', 'own account')
  return (
    <Group justify="space-between" wrap="nowrap" gap="sm" py={SP.tight}
      style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
      <Box style={{ minWidth: 0 }}>
        <Text size="sm" fw={500} ff="monospace" truncate>{s.hostname}</Text>
        <Text size={FS.meta} c="dimmed">{what || 'no access of their own here'}</Text>
      </Box>
      <Group gap={SP.tight} wrap="nowrap">
        {s.ownSetting
          ? <Badge variant="light" color="amber">set for them</Badge>
          : s.teams.map((t) => <Badge key={t} variant="outline" color="slate">from {t}</Badge>)}
      </Group>
    </Group>
  )
}
