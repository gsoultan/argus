import { useState } from 'react'
import { Alert, Badge, Button, Group, Modal, Stack, Table, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconClipboardCheck } from '@tabler/icons-react'
import { DataTable, EmptyState, FS, Mono, SP, relTime, run } from '@argus/shell'
import { requestsQuery, useCancelRequest, useDecide, type ReadRequest } from './api'

/** What self-approval asks to be typed: control.SelfApprovalPhrase. */
const SELF_PHRASE = 'approve my own request'

const STATE: Record<ReadRequest['state'], { label: string; color: string }> = {
  pending: { label: 'Waiting', color: 'amber' },
  approved: { label: 'Approved', color: 'teal' },
  denied: { label: 'Denied', color: 'rose' },
  cancelled: { label: 'Cancelled', color: 'slate' },
}

const DECIDERS = new Set(['approver', 'admin', 'owner'])

export function RequestsTab({ me, role }: { me: string; role: string }) {
  const { data, isLoading, error } = useQuery(requestsQuery())
  const cancel = useCancelRequest()
  const [deciding, setDeciding] = useState<{ r: ReadRequest; approve: boolean } | null>(null)
  const decider = DECIDERS.has(role)
  const mine = (r: ReadRequest) => r.requester.toLowerCase() === me.toLowerCase()

  return (
    <Stack gap="md">
      {error && <Alert color="rose"><Text size={FS.body}>Could not read the requests: {error.message}</Text></Alert>}
      <DataTable
        columns={['Secret', 'Who', 'Why', 'For', 'State', '']}
        minWidth={820}
        loading={isLoading}
        isEmpty={!!data && data.length === 0}
        empty={<EmptyState compact icon={IconClipboardCheck} title="No requests"
          description="A request is made from a secret that needs an approval, when you try to open it." />}
      >
        {data?.map((r) => (
          <Table.Tr key={r.id}>
            <Table.Td><Mono>{r.name}</Mono></Table.Td>
            <Table.Td><Text size={FS.meta}>{mine(r) ? 'You' : r.requester}</Text><Text size={FS.micro} c="dimmed">{relTime(r.createdAt)}</Text></Table.Td>
            <Table.Td><Text size={FS.meta} lineClamp={2} title={r.reason}>{r.reason}</Text></Table.Td>
            <Table.Td><Text size={FS.meta}>{r.minutes >= 60 ? `${r.minutes / 60} h` : `${r.minutes} min`}</Text></Table.Td>
            <Table.Td>
              <Badge size="sm" variant="light" color={STATE[r.state].color}>{STATE[r.state].label}</Badge>
              {r.state === 'approved' && r.expiresAt && <Text size={FS.micro} c="dimmed">until {relTime(r.expiresAt)}</Text>}
              {r.decidedBy && r.state !== 'pending' && <Text size={FS.micro} c="dimmed" title={r.decisionNote}>by {r.decidedBy}</Text>}
            </Table.Td>
            <Table.Td>
              {r.state === 'pending' && (
                <Group gap={SP.snug} justify="flex-end" wrap="nowrap">
                  {mine(r) && (
                    <Button size="xs" variant="default" loading={cancel.isPending && cancel.variables === r.id}
                      onClick={() => void run(() => cancel.mutateAsync(r.id), { failure: 'Not cancelled' })}>
                      Cancel
                    </Button>
                  )}
                  {decider && (!mine(r) || r.approveOwn) && (
                    <>
                      <Button size="xs" variant="default" onClick={() => setDeciding({ r, approve: true })}>Approve</Button>
                      <Button size="xs" variant="subtle" color="rose" onClick={() => setDeciding({ r, approve: false })}>Deny</Button>
                    </>
                  )}
                </Group>
              )}
            </Table.Td>
          </Table.Tr>
        ))}
      </DataTable>
      {deciding && <Decide r={deciding.r} approve={deciding.approve} own={mine(deciding.r)} onClose={() => setDeciding(null)} />}
    </Stack>
  )
}

function Decide({ r, approve, own, onClose }: { r: ReadRequest; approve: boolean; own: boolean; onClose: () => void }) {
  const decide = useDecide()
  const [note, setNote] = useState('')
  const [phrase, setPhrase] = useState('')
  const ready = (approve || note.trim() !== '') && (!own || phrase.trim().toLowerCase() === SELF_PHRASE)
  return (
    <Modal opened onClose={onClose} title={approve ? `Let ${own ? 'yourself' : r.requester} read ${r.name}?` : `Deny reading ${r.name}?`}>
      <Stack gap="sm">
        <Text size={FS.body}>“{r.reason}”</Text>
        {approve && <Text size={FS.body}>They can open it for {r.minutes} minutes from now. Every read is in the audit log.</Text>}
        <TextInput label={approve ? 'Note (optional)' : 'Why not?'} value={note} onChange={(e) => setNote(e.currentTarget.value)} maxLength={500} />
        {own && (
          <TextInput label={`You are the only one who can decide it. Type “${SELF_PHRASE}”`} value={phrase}
            onChange={(e) => setPhrase(e.currentTarget.value)} />
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color={approve ? 'azure' : 'rose'} disabled={!ready} loading={decide.isPending}
            onClick={() => void run(() => decide.mutateAsync({ id: r.id, approve, note: note.trim(), ...(own ? { confirmSelf: phrase.trim() } : {}) }), {
              failure: 'Not decided', success: [approve ? 'Approved' : 'Denied', approve ? `${r.requester} can read ${r.name}.` : `${r.requester} was told.`],
            }).then((ok) => { if (ok) onClose() })}>
            {approve ? 'Approve' : 'Deny'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
