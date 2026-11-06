import { Box, Button, Group, Modal, SegmentedControl, Stack, Text, Textarea } from '@mantine/core'
import { useState } from 'react'
import { notifyError, notifyOk } from '~/lib/notify'
import { useRequestDatabaseAccess } from '~/lib/queries'
import { FS } from '~/theme'
import type { DBLevel } from '~/types/domain'

/** Ask for a level on a database or a broker, for a while, with a reason an approver reads. */
const LEVELS: DBLevel[] = ['read', 'write', 'owner', 'dba']

export function AskModal({ dbId, opened, onClose, top }: { dbId: string; opened: boolean; onClose: () => void; top: DBLevel }) {
  const [level, setLevel] = useState<DBLevel>(top)
  const [minutes, setMinutes] = useState('60')
  const [why, setWhy] = useState('')
  const req = useRequestDatabaseAccess()
  const options = LEVELS.slice(0, LEVELS.indexOf(top) + 1)
  const submit = async () => {
    try {
      await req.mutateAsync({ id: dbId, level, justification: why, durationMinutes: Number(minutes) })
      notifyOk('Request sent', 'An approver has it. You will be able to open the database as soon as they say yes.')
      onClose()
    } catch (err) {
      notifyError('Not sent', err)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title="Ask for access" centered>
      <Stack gap="sm">
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>Access</Text>
          <SegmentedControl value={level} onChange={(v) => setLevel(v as DBLevel)}
            data={options.map((l) => ({ label: l[0]!.toUpperCase() + l.slice(1), value: l }))} />
        </Box>
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>For</Text>
          <SegmentedControl value={minutes} onChange={setMinutes}
            data={[{ label: '15 min', value: '15' }, { label: '1 hour', value: '60' }, { label: '4 hours', value: '240' }]} />
        </Box>
        <Textarea label="Why" placeholder="What you need to do, and the ticket if there is one (at least 20 characters)"
          value={why} onChange={(e) => setWhy(e.currentTarget.value)} minRows={2} autosize />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void submit()} loading={req.isPending} disabled={why.trim().length < 20}>Send request</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
