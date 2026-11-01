import { Alert, Box, Button, Group, Modal, SegmentedControl, Stack, Text, Textarea } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { IconInfoCircle } from '@tabler/icons-react'
import { useCallback, useState } from 'react'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { PageBody, PageHeader } from '~/components/page'
import { notifyError, notifyOk } from '~/lib/notify'
import { databaseTicket, isConfigured, postgresEndpoint } from '~/lib/live'
import { databaseQuery, meQuery, useRequestDatabaseAccess } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import { FS } from '~/theme'
import type { DBLevel } from '~/types/domain'

/**
 * One database's workspace. Nothing here decides access: the ticket route
 * does, and the agent checks the grant again. This page only asks for a
 * ticket, opens the socket, and offers "ask for access" where the level is
 * not enough.
 */
export const Route = createFileRoute('/databases/$dbId')({
  component: DatabasePage,
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databaseQuery(params.dbId)).catch(() => undefined) : undefined,
    ]),
})

function DatabasePage() {
  const { dbId } = Route.useParams()
  const { data: me } = useQuery(meQuery())
  const { data, error } = useQuery(databaseQuery(dbId))
  const [asking, ask] = useDisclosure(false)

  const getTicket = useCallback(async (level?: DBLevel) => {
    const t = await databaseTicket(dbId, level)
    if ('error' in t) {
      notifyError('Could not open the database', new Error(t.error))
      return null
    }
    return t
  }, [dbId])

  if (!isConfigured()) {
    return (
      <Box>
        <PageHeader title="Database" crumbs={[{ label: 'Databases', to: '/databases' }]} />
        <PageBody>
          <Alert icon={<IconInfoCircle size={16} />} color="amber" variant="light">No control plane is configured.</Alert>
        </PageBody>
      </Box>
    )
  }
  if (error) {
    return (
      <Box>
        <PageHeader title="Database" crumbs={[{ label: 'Databases', to: '/databases' }]} />
        <PageBody>
          <Alert color="rose" variant="light">{(error as Error).message}</Alert>
        </PageBody>
      </Box>
    )
  }
  if (!data) return null
  const { database, access } = data
  const admin = canSee(me?.role, 'admin')

  if (!access.level) {
    return (
      <Box>
        <PageHeader title={database.dbname} crumbs={[{ label: 'Databases', to: '/databases' }]}
          description={`${database.hostname} · ${database.label}`} />
        <PageBody>
          <Stack gap="md" maw={560}>
            <Alert color="azure" variant="light" icon={<IconInfoCircle size={16} />}>
              You can ask for <b>{access.approvable}</b> on this database. An approver gets it straight away, and you
              can open it as soon as they say yes.
            </Alert>
            <Button onClick={ask.open} style={{ alignSelf: 'flex-start' }}>Ask for access</Button>
          </Stack>
          <AskModal dbId={dbId} opened={asking} onClose={ask.close} top={access.approvable ?? 'read'} />
        </PageBody>
      </Box>
    )
  }

  return (
    <Box>
      <Group px="md" py={6} gap="xs" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <Text size={FS.micro} c="dimmed">
          <Link to="/databases" style={{ color: 'var(--color-brand)' }}>Databases</Link> / {database.dbname}
        </Text>
        {admin && (
          <Link to="/databases/$dbId/settings" params={{ dbId }} style={{ marginLeft: 'auto', fontSize: 11, color: 'var(--color-brand)' }}>
            Who can connect &amp; settings
          </Link>
        )}
      </Group>
      <DatabaseWorkspace
        database={database}
        access={access}
        getTicket={getTicket}
        endpoint={postgresEndpoint}
        onAskForAccess={access.approvable ? ask.open : undefined}
      />
      {access.approvable && (
        <AskModal dbId={dbId} opened={asking} onClose={ask.close} top={access.approvable} />
      )}
    </Box>
  )
}

const LEVELS: DBLevel[] = ['read', 'write', 'owner', 'dba']

function AskModal({ dbId, opened, onClose, top }: { dbId: string; opened: boolean; onClose: () => void; top: DBLevel }) {
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
