import { useState, type ReactNode } from 'react'
import { Alert, Button, Group, Modal, Stack, Text, TextInput } from '@mantine/core'
import { IconCheck } from '@tabler/icons-react'
import { useQuery } from '@tanstack/react-query'
import { useDecidesAlone, useSelfApprovalPhrase } from '~/lib/selfApproval'
import { requestQuery, useDecideRequest } from '~/lib/queries'
import { run as notifyRun } from '~/lib/notify'
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

/**
 * Approve your own access request where it was asked for (a database, a
 * broker, a restore), without going to Approvals. Shown only when the control
 * plane marks the request as one nobody else could decide, which it works out
 * with the rule the decision is taken by, so the button is never offered on a
 * request it would refuse. Nothing is shown while that is being read, or once
 * the request is decided.
 */
export function ApproveOwnRequest({ id, what, size, label, onApproved, fallback = null }: {
  id: string | undefined
  what: string
  size?: 'compact-xs' | 'compact-sm' | 'xs' | 'sm'
  label?: string
  onApproved?: () => void
  /** Shown instead while nobody else deciding is not known to be the case. */
  fallback?: ReactNode
}) {
  const one = useQuery({ ...requestQuery(id ?? ''), enabled: !!id })
  const decide = useDecideRequest()
  if (!id || one.data?.state !== 'pending' || !one.data.approveOwn) return <>{fallback}</>
  return (
    <ApproveOwn what={what} size={size} label={label} loading={decide.isPending}
      onApprove={(confirmSelf) => notifyRun(async () => {
        await decide.mutateAsync({ id, decision: 'approved', note: '', confirmSelf })
        onApproved?.()
      }, { failure: 'Not approved' })} />
  )
}

/** Whether the control plane says the reader may approve request id themselves. */
export function useMayApproveOwn(id: string | undefined): boolean {
  const one = useQuery({ ...requestQuery(id ?? ''), enabled: !!id })
  return one.data?.state === 'pending' && !!one.data.approveOwn
}
