import { useState, type ReactNode } from 'react'
import { Alert, Button, Group, Modal, Stack, Text, TextInput } from '@mantine/core'
import { IconCheck } from '@tabler/icons-react'
import { useDecidesAlone, useSelfApprovalPhrase } from '~/lib/selfApproval'
import { FS } from '~/theme'

/**
 * Approve your own request, for the one person who could decide it
 * (lib/selfApproval.ts). Show it only where useDecidesAlone says so: the
 * control plane refuses it the moment anyone else could decide.
 */
export function ApproveOwn({ what, loading, onApprove, size = 'compact-xs', label = 'Approve my own' }: {
  /** What is approved, in words: "this grow", "the remove of pay-api-01". */
  what: string
  loading?: boolean
  /** Sends the approval with the phrase. Resolving to false keeps the dialog open. */
  onApprove: (confirmSelf: string) => Promise<unknown> | void
  size?: 'compact-xs' | 'compact-sm' | 'xs' | 'sm'
  label?: string
}) {
  const phrase = useSelfApprovalPhrase()
  const [open, setOpen] = useState(false)
  const [typed, setTyped] = useState('')
  const close = () => {
    setOpen(false)
    setTyped('')
  }
  return (
    <>
      <Button size={size} color="amber" variant="light" leftSection={<IconCheck size={12} />} loading={loading}
        onClick={() => setOpen(true)}>
        {label}
      </Button>
      <Modal opened={open} onClose={close} title={<Text fw={700}>Approve your own request</Text>}>
        <Stack gap="sm">
          <Alert color="amber" p="xs">
            <Text size={FS.body}>
              Nobody else in this organization can approve {what}, so you may approve it yourself. Activity records
              that nobody else agreed. Once someone else can approve, this goes away and they decide.
            </Text>
          </Alert>
          <TextInput label={`Type "${phrase}" to confirm`} placeholder={phrase} value={typed} data-autofocus
            onChange={(e) => setTyped(e.currentTarget.value)} />
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>Keep waiting</Button>
            <Button color="amber" disabled={typed.trim() !== phrase} loading={loading}
              onClick={() => void Promise.resolve(onApprove(typed.trim())).then((ok) => { if (ok !== false) close() }, () => undefined)}>
              Approve
            </Button>
          </Group>
        </Stack>
      </Modal>
    </>
  )
}

/** The line shown when someone asks for something only they can approve. */
export function decidesAloneWords(what: string): string {
  return `Nobody else in this organization can approve ${what}, so once you ask you approve it yourself, from the same place, by typing a confirmation. Activity records that nobody else agreed.`
}

/**
 * What waiting for a second person means, said before someone asks: the
 * usual words, or, when nobody else could decide it, that they approve it
 * themselves.
 */
export function AloneOr({ kind, what, children }: { kind: 'requests' | 'runs'; what: string; children: ReactNode }) {
  const alone = useDecidesAlone(kind)
  return <>{alone ? decidesAloneWords(what) : children}</>
}
