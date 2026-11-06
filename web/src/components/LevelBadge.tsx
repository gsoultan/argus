import { Badge, Text } from '@mantine/core'
import { IconLock } from '@tabler/icons-react'
import { FS } from '~/theme'
import type { DBLevel } from '~/types/domain'

/** The level a person has on a database or broker: the one piece of security they see every day. */
export function LevelBadge({ level, approval }: { level: DBLevel | '' | undefined; approval?: boolean }) {
  if (!level) return <Text size={FS.meta} c="dimmed">none</Text>
  const label = { read: 'Read', write: 'Write', owner: 'Owner', dba: 'DBA' }[level]
  return (
    <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm"
      leftSection={approval ? <IconLock size={10} /> : undefined}>
      {label}
    </Badge>
  )
}
