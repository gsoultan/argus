import { Badge, Box, Button, Group, Modal, Stack, Text, TextInput } from '@mantine/core'
import { IconAlertTriangle, IconLock } from '@tabler/icons-react'
import { useEffect, useState, type ReactNode } from 'react'
import { FS, SP } from '~/theme'
import type { DBAccess, DBLevel, Database } from '~/types/domain'

/*
What the Redis and RabbitMQ workspaces share with the Postgres one: the bar
across the top that says where you are, what you can do and that it is
recorded; the banner when the session ends; and the type-the-name confirm.
*/

export const LABEL_COLOR = { production: 'rose', staging: 'amber', development: 'teal' } as const

export function StoreHeader({ database, icon, title, detail, level, access, status, onAskForAccess, narrow, left }: {
  database: Database
  icon: ReactNode
  title: string
  detail?: string
  level: DBLevel | ''
  access: DBAccess
  status: 'connecting' | 'ready' | 'closed'
  onAskForAccess?: () => void
  narrow: boolean
  left?: ReactNode
}) {
  return (
    <Group gap="xs" px="md" py={SP.cozy} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
      {left}
      {icon}
      <Text fw={650} size={FS.body} style={{ whiteSpace: 'nowrap' }}>{title}</Text>
      {!narrow && detail && <Text size={FS.micro} c="dimmed" ff="monospace">{detail}</Text>}
      <Badge variant="light" color={LABEL_COLOR[database.label]} radius="sm" style={{ flexShrink: 0 }}>{database.label}</Badge>
      <Group gap={6} ml="auto" wrap="nowrap">
        <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm">
          {narrow ? '' : 'You can: '}{level ? level.toUpperCase() : '…'}
        </Badge>
        {access.approvable && onAskForAccess && (
          <Button size="xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>
            {narrow ? 'Ask' : `Ask for ${access.approvable}`}
          </Button>
        )}
        <Badge variant="light" color="rose" radius="sm"
          leftSection={<span style={{ display: 'inline-block', width: 6, height: 6, borderRadius: 3, background: 'currentColor' }} />}>
          {status === 'ready' ? 'Recorded' : status === 'connecting' ? 'Connecting' : 'Closed'}
        </Badge>
      </Group>
    </Group>
  )
}

export function FatalBanner({ fatal }: { fatal: string | null }) {
  if (!fatal) return null
  return (
    <Box px="md" py={SP.cozy} style={{ background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
      <Text size={FS.meta}>{fatal}. Open the page again for a new session.</Text>
    </Box>
  )
}

/**
 * Asks the person to type the server's name before something that wipes or
 * cuts off. The agent refuses the command without it, so this is the
 * console asking first, not the console deciding.
 */
export function ConfirmByName({ opened, what, hostname, onCancel, onConfirm }: {
  opened: boolean
  what: string
  hostname: string
  onCancel: () => void
  onConfirm: (typed: string) => void
}) {
  const [typed, setTyped] = useState('')
  useEffect(() => { if (opened) setTyped('') }, [opened])
  return (
    <Modal opened={opened} onClose={onCancel} centered
      title={<Group gap={6}><IconAlertTriangle size={16} color="var(--mantine-color-rose-6)" /><Text fw={650}>Confirm on {hostname}</Text></Group>}>
      <Stack gap="sm">
        <Text size={FS.body}>{what}</Text>
        <Text size={FS.meta} c="dimmed">
          This cannot be undone. It is recorded with your name either way. Type <b>{hostname}</b> to go ahead.
        </Text>
        <TextInput value={typed} onChange={(e) => setTyped(e.currentTarget.value)} placeholder={hostname}
          aria-label="Server name" autoFocus data-autofocus spellCheck={false} autoCapitalize="off" />
        <Group justify="flex-end">
          <Button variant="default" onClick={onCancel}>Cancel</Button>
          <Button color="rose" disabled={typed !== hostname} onClick={() => onConfirm(typed)}>Do it</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
