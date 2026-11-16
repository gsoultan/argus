import { useState } from 'react'
import { Alert, Button, Group, Modal, Stack, Text, TextInput, Textarea } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconAlertTriangle } from '@tabler/icons-react'
import { bytes } from '~/components/primitives'
import { useRemoveRecording } from '~/lib/queries'
import { FS } from '~/theme'
import type { Session } from '~/types/domain'

/**
 * Removing a session's recording, for an admin.
 *
 * The host name is typed back and a reason given; the control plane checks
 * both again, keeps the session, and writes the removal to the audit log
 * before the file is deleted.
 */
export function RemoveRecordingDialog({
  session,
  onClose,
}: {
  session: Pick<Session, 'id' | 'assetHostname' | 'recordingBytes'>
  onClose: () => void
}) {
  const remove = useRemoveRecording()
  const [host, setHost] = useState('')
  const [why, setWhy] = useState('')

  const onRemove = async () => {
    let out: { fileNotDeleted?: string }
    try {
      out = await remove.mutateAsync({ id: session.id, confirm: host.trim(), reason: why.trim() })
    } catch (err) {
      // The dialog stays open: closing it would read as done.
      notifications.show({
        color: 'rose',
        title: 'Recording not removed',
        message: err instanceof Error ? err.message : 'the control plane refused',
      })
      return
    }
    onClose()
    notifications.show({
      color: out.fileNotDeleted ? 'amber' : 'teal',
      title: 'Recording removed',
      message: out.fileNotDeleted
        ? `Argus no longer holds it, but ${out.fileNotDeleted}.`
        : 'The file is deleted. The session stays, and the removal is in the audit log.',
    })
  }

  return (
    <Modal opened onClose={onClose} title="Remove recording" size="md">
      <Alert color="rose" icon={<IconAlertTriangle size={16} />} mb="md">
        <Text size={FS.body} lh={1.5}>
          The recording of this session ({bytes(session.recordingBytes)}) is deleted from
          storage and cannot be replayed again. The session stays in Activity with who
          opened it, from where, its chain head and its size, and the removal is recorded in
          the audit log against your account.
        </Text>
      </Alert>
      <Stack gap="sm">
        <TextInput
          size="sm"
          label={`Type ${session.assetHostname} to confirm`}
          value={host}
          onChange={(e) => setHost(e.currentTarget.value)}
          autoComplete="off"
        />
        <Textarea
          size="sm"
          label="Reason"
          description="Kept with the session and in the audit log."
          placeholder="e.g. A CI deploy copied a build through this session; nothing else ran."
          minRows={2}
          autosize
          value={why}
          onChange={(e) => setWhy(e.currentTarget.value)}
        />
      </Stack>
      <Group justify="flex-end" mt="md">
        <Button variant="subtle" color="slate" onClick={onClose}>
          Cancel
        </Button>
        <Button
          color="rose"
          loading={remove.isPending}
          disabled={host.trim().toLowerCase() !== session.assetHostname.toLowerCase() || why.trim().length < 4}
          onClick={onRemove}
        >
          Remove recording
        </Button>
      </Group>
    </Modal>
  )
}
