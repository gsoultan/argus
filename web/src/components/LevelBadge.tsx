import { Badge, Text } from '@mantine/core'
import { IconLock } from '@tabler/icons-react'
import { FS } from '~/theme'
import { levelName } from '~/lib/engines'
import type { DBLevel } from '~/types/domain'

/** The level a person has on a database or broker: the one piece of security they see every day. */
export function LevelBadge({ level, approval, engine }: { level: DBLevel | '' | undefined; approval?: boolean; engine?: string }) {
  if (!level) return <Text size={FS.meta} c="dimmed">none</Text>
  const label = levelName(level, engine)
  return (
    <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm"
      leftSection={approval ? <IconLock size={10} /> : undefined}>
      {label}
    </Badge>
  )
}
