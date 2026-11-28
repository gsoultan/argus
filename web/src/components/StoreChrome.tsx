import { Box, Button, Group, Modal, Stack, Text, TextInput } from '@mantine/core'
import { IconAlertTriangle } from '@tabler/icons-react'
import { useEffect, useState } from 'react'
import { FS, SP } from '~/theme'

/*
What the MongoDB, Redis and broker workspaces share: the banner when the
session ends, and the type-the-name confirm. The header is DatabaseShell's.
*/

export const LABEL_COLOR = { production: 'rose', staging: 'amber', development: 'teal' } as const

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
