import { Box, Button, Group, Modal, SegmentedControl, Stack, Text, Textarea } from '@mantine/core'
import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { ApproveOwnRequest } from '~/components/ApproveOwn'
import { levelName } from '~/lib/engines'
import { notifyError, notifyOk } from '~/lib/notify'
import { requestQuery, useRequestDatabaseAccess } from '~/lib/queries'
import { FS } from '~/theme'
import type { DBLevel } from '~/types/domain'

/** Ask for a level on a database or a broker, for a while, with a reason an approver reads. */
const LEVELS: DBLevel[] = ['read', 'write', 'owner', 'dba']

export function AskModal({ dbId, opened, onClose, top, what = 'database', engine }: {
  dbId: string; opened: boolean; onClose: () => void; top: DBLevel
  /** The entry's engine, so the levels carry its names. */
  engine?: string
  /** What is being asked for, in a sentence: database, or broker. */
  what?: string
}) {
  const [level, setLevel] = useState<DBLevel>(top)
  const [minutes, setMinutes] = useState('60')
  const [why, setWhy] = useState('')
  const req = useRequestDatabaseAccess()
  const qc = useQueryClient()
  // Sent, and nobody else could decide it: approved here, not on Approvals.
  const [sent, setSent] = useState<string | null>(null)
  const options = LEVELS.slice(0, LEVELS.indexOf(top) + 1)
  const close = () => {
    setSent(null)
    onClose()
  }
  const submit = async () => {
    let id: string
    try {
      id = (await req.mutateAsync({ id: dbId, level, justification: why, durationMinutes: Number(minutes) })).id
    } catch (err) {
      notifyError('Not sent', err)
      return
    }
    // The control plane says whether they may approve it themselves, with
    // the rule it decides by.
    const mine = await qc.fetchQuery(requestQuery(id)).catch(() => null)
    if (mine?.state === 'pending' && mine.approveOwn) {
      setSent(id)
      return
    }
    notifyOk('Request sent', `An approver has it. You will be able to open the ${what} as soon as they say yes.`)
    close()
  }
  if (sent) {
    return (
      <Modal opened={opened} onClose={close} title="Approve your own request" centered>
        <Stack gap="sm">
          <Text size={FS.body}>
            Sent. Nobody else in this organization can approve it, so you may approve it yourself, here. Activity
            records that nobody else agreed. Then you can open the {what}.
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>Later</Button>
            <ApproveOwnRequest id={sent} what="this request" size="sm" label="Approve it"
              onApproved={() => { notifyOk('Approved', `You can open the ${what} now.`); close() }} />
          </Group>
        </Stack>
      </Modal>
    )
  }
  return (
    <Modal opened={opened} onClose={close} title="Ask for access" centered>
      <Stack gap="sm">
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>Access</Text>
          <SegmentedControl value={level} onChange={(v) => setLevel(v as DBLevel)}
            data={options.map((l) => ({ label: levelName(l, engine), value: l }))} />
        </Box>
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>For</Text>
          <SegmentedControl value={minutes} onChange={setMinutes}
            data={[{ label: '15 min', value: '15' }, { label: '1 hour', value: '60' }, { label: '4 hours', value: '240' }]} />
        </Box>
        <Textarea label="Why" placeholder="What you need to do, and the ticket if there is one (at least 20 characters)"
          value={why} onChange={(e) => setWhy(e.currentTarget.value)} minRows={2} autosize />
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button onClick={() => void submit()} loading={req.isPending} disabled={why.trim().length < 20}>Send request</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
