import { Badge, Box, Divider, Grid, Group, Stack, Text } from '@mantine/core'
import type { ReactNode } from 'react'
import { isElevatedAccount } from '~/lib/roles'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * One block of a settings-style tab: its name and a sentence on the left,
 * its controls on the right. On a phone the two stack.
 */
export function SettingsSection({ title, description, danger = false, children }: {
  title: string
  description?: ReactNode
  danger?: boolean
  children: ReactNode
}) {
  return (
    <Grid gap={{ base: 'xs', md: 'xl' }}>
      <Grid.Col span={{ base: 12, md: 4 }}>
        <Text component="h2" fw={600} size="sm" c={danger ? 'rose.6' : undefined} style={{ margin: 0 }}>
          {title}
        </Text>
        {description && (
          <Text size={FS.meta} c="dimmed" mt={SP.tight} lh={1.5} maw={320}>
            {description}
          </Text>
        )}
      </Grid.Col>
      <Grid.Col span={{ base: 12, md: 8 }} style={{ minWidth: 0 }}>
        {children}
      </Grid.Col>
    </Grid>
  )
}

/** Settings sections with a rule between each. */
export function SettingsSections({ children }: { children: ReactNode[] }) {
  const shown = children.filter(Boolean)
  return (
    <Stack gap="lg">
      {shown.map((c, i) => (
        <Box key={i}>
          {i > 0 && <Divider mb="lg" />}
          {c}
        </Box>
      ))}
    </Stack>
  )
}

/** The accounts Argus logs in as on a server, root-like ones in rose. */
export function LoginAccounts({ asset, empty }: { asset: Asset; empty?: ReactNode }) {
  if (asset.principals.length === 0) {
    return <>{empty ?? <Text size={FS.meta} c="dimmed">None yet</Text>}</>
  }
  return (
    <Group gap={SP.snug}>
      {asset.principals.map((p) => (
        <Badge
          key={p}
          size="sm"
          color={isElevatedAccount(p) ? 'rose' : 'slate'}
          variant={isElevatedAccount(p) ? 'light' : 'outline'}
          tt="none"
          ff="monospace"
        >
          {p}
        </Badge>
      ))}
    </Group>
  )
}
