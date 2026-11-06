import { Alert, Box, Button, Group, Stack, Text } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link, Navigate } from '@tanstack/react-router'
import { IconInfoCircle } from '@tabler/icons-react'
import { useCallback, useEffect } from 'react'
import { AskModal } from '~/components/DatabaseAskModal'
import { DatabaseList } from '~/components/DatabaseList'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { SplitView } from '~/components/SplitView'
import { RedisWorkspace } from '~/components/RedisWorkspace'
import { PageBody, PageHeader } from '~/components/page'
import { notifyError } from '~/lib/notify'
import { databaseTicket, isConfigured, postgresEndpoint } from '~/lib/live'
import { noteDatabaseOpened } from '~/lib/recentDatabases'
import { databaseQuery, meQuery } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import { engineOf } from '~/lib/engines'
import { FS, INK } from '~/theme'
import type { DBLevel } from '~/types/domain'

/**
 * One database's workspace. Nothing here decides access: the ticket route
 * does, and the agent checks the grant again. This page only asks for a
 * ticket, opens the socket, and offers "ask for access" where the level is
 * not enough.
 */
export const Route = createFileRoute('/databases/$dbId')({
  component: DatabasePage,
  // ?panel=changes opens a panel, as a data change alert links to it.
  validateSearch: (s: Record<string, unknown>): { panel?: string } => (typeof s.panel === 'string' ? { panel: s.panel } : {}),
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databaseQuery(params.dbId)).catch(() => undefined) : undefined,
    ]),
})

function DatabasePage() {
  const { dbId } = Route.useParams()
  const { panel } = Route.useSearch()
  const { data: me } = useQuery(meQuery())
  const { data, error } = useQuery(databaseQuery(dbId))
  const [asking, ask] = useDisclosure(false)
  // Only a database the person may open counts as opened in this browser.
  const canOpen = Boolean(data?.access.level)
  useEffect(() => { if (canOpen) noteDatabaseOpened(dbId) }, [dbId, canOpen])

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
  // A RabbitMQ vhost is a message broker, not a database: it lives under
  // Brokers, and an old link to it here goes there.
  if (database.engine === 'rabbitmq') return <Navigate to="/brokers/$brokerId" params={{ brokerId: dbId }} replace />
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
    <SplitView storageKey="databases" label="databases" list={<DatabaseList currentId={dbId} />}>
      {database.engine !== 'postgres' && <Group px="md" pl={{ base: 'md', lg: 'xl' }} py={6} gap="xs" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <Text size={FS.micro} c="dimmed">
          <Link to="/databases" style={{ color: INK.brand }}>Databases</Link> / {engineOf(database.engine).label} · {database.dbname}
        </Text>
        {admin && (
          <Link to="/databases/$dbId/settings" params={{ dbId }} style={{ marginLeft: 'auto', fontSize: 11, color: INK.brand }}>
            Who can connect &amp; settings
          </Link>
        )}
      </Group>}
      {database.engine === 'redis' ? (
        <RedisWorkspace database={database} access={access} getTicket={getTicket} endpoint={postgresEndpoint}
          onAskForAccess={access.approvable ? ask.open : undefined} />
      ) : (
        <DatabaseWorkspace
          database={database}
          access={access}
          getTicket={getTicket}
          endpoint={postgresEndpoint}
          onAskForAccess={access.approvable ? ask.open : undefined}
          orgAdmin={admin}
          initialPanel={panel}
        />
      )}
      {access.approvable && (
        <AskModal dbId={dbId} opened={asking} onClose={ask.close} top={access.approvable} />
      )}
    </SplitView>
  )
}
