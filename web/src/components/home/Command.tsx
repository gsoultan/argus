import type { ReactNode } from 'react'
import { Box, Group, Text } from '@mantine/core'

/**
 * The top of Home: who, when, and the actions. One element for every reader
 * and for the moment before we know who this is, so the greeting is not torn
 * down and redrawn when the answer arrives.
 */
export function CommandBand({ title, subtitle, actions }: {
  title: string; subtitle: string; actions?: ReactNode
}) {
  return (
    <Box px={{ base: 'md', sm: 'xl' }} pt={{ base: 'md', sm: 'xl' }} pb="xs">
      <Group justify="space-between" align="flex-start" gap="md" className="argus-band-head">
        <Box style={{ minWidth: 0 }}>
          <Text component="h1" fw={700} fz={{ base: 22, sm: 26 }} lh={1.2} style={{ margin: 0, letterSpacing: '-0.01em' }}>
            {title}
          </Text>
          <Text size="sm" mt={4} c="dimmed">{subtitle}</Text>
        </Box>
        {actions && <Group gap="sm" wrap="nowrap" className="argus-band-actions">{actions}</Group>}
      </Group>
    </Box>
  )
}

