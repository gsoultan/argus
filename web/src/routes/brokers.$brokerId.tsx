import { Alert, Box, Button, Stack } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Navigate, useNavigate } from '@tanstack/react-router'
import { IconInfoCircle } from '@tabler/icons-react'
import { useCallback } from 'react'
import { BrokerWorkspace, type BrokerTab } from '~/components/broker/BrokerWorkspace'
import { PageBody, PageHeader } from '~/components/page'
import { notifyError } from '~/lib/notify'
import { databaseTicket, isConfigured, postgresEndpoint } from '~/lib/live'
import { databaseQuery, meQuery } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import type { DBLevel } from '~/types/domain'
import { AskModal } from '~/components/DatabaseAskModal'

/**
 * One RabbitMQ vhost's workspace. As for a database, nothing here decides
 * access: the ticket route does, and the agent checks the grant again. The
 * tab, queue and exchange are in the address, so a link opens the same view.
 */
interface Search { tab?: BrokerTab; queue?: string; exchange?: string }
const TABS: BrokerTab[] = ['overview', 'queues', 'routing', 'connections']

export const Route = createFileRoute('/brokers/$brokerId')({
  component: BrokerPage,
  validateSearch: (s: Record<string, unknown>): Search => ({
    ...(typeof s.tab === 'string' && (TABS as string[]).includes(s.tab) ? { tab: s.tab as BrokerTab } : {}),
    ...(typeof s.queue === 'string' ? { queue: s.queue } : {}),
    ...(typeof s.exchange === 'string' ? { exchange: s.exchange } : {}),
  }),
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databaseQuery(params.brokerId)).catch(() => undefined) : undefined,
    ]),
})

function BrokerPage() {
  const { brokerId } = Route.useParams()
  const search = Route.useSearch()
  const navigate = useNavigate({ from: Route.fullPath })
  const { data: me } = useQuery(meQuery())
  const { data, error } = useQuery(databaseQuery(brokerId))
  const [asking, ask] = useDisclosure(false)

  const getTicket = useCallback(async (level?: DBLevel) => {
    const t = await databaseTicket(brokerId, level)
    if ('error' in t) {
      notifyError('Could not open the broker', new Error(t.error))
      return null
    }
    return t
  }, [brokerId])

  const crumbs = [{ label: 'Brokers', to: '/brokers' }]
  if (!isConfigured()) {
    return (
      <Box><PageHeader title="Broker" crumbs={crumbs} />
        <PageBody><Alert icon={<IconInfoCircle size={16} />} color="amber" variant="light">No control plane is configured.</Alert></PageBody>
      </Box>
    )
  }
  if (error) {
    return (
      <Box><PageHeader title="Broker" crumbs={crumbs} />
        <PageBody><Alert color="rose" variant="light">{(error as Error).message}</Alert></PageBody>
      </Box>
    )
  }
  if (!data) return null
  const { database, access } = data
  if (database.engine !== 'rabbitmq') return <Navigate to="/databases/$dbId" params={{ dbId: brokerId }} replace />

  if (!access.level) {
    return (
      <Box>
        <PageHeader title={database.dbname} crumbs={crumbs} description={`RabbitMQ vhost on ${database.hostname} · ${database.label}`} />
        <PageBody>
          <Stack gap="md" maw={560}>
            <Alert color="azure" variant="light" icon={<IconInfoCircle size={16} />}>
              You can ask for <b>{access.approvable}</b> on this vhost. An approver gets it straight away, and you can open it as soon as
              they say yes.
            </Alert>
            <Button onClick={ask.open} style={{ alignSelf: 'flex-start' }}>Ask for access</Button>
          </Stack>
          <AskModal dbId={brokerId} opened={asking} onClose={ask.close} top={access.approvable ?? 'read'} />
        </PageBody>
      </Box>
    )
  }

  return (
    <>
      <BrokerWorkspace
        database={database}
        access={access}
        getTicket={getTicket}
        endpoint={postgresEndpoint}
        onAskForAccess={access.approvable ? ask.open : undefined}
        tab={search.tab ?? 'overview'}
        queue={search.queue}
        exchange={search.exchange}
        onNavigate={(s) => void navigate({ search: { tab: s.tab === 'overview' ? undefined : s.tab, queue: s.queue, exchange: s.exchange } })}
        settingsLink={canSee(me?.role, 'admin')}
      />
      {access.approvable && <AskModal dbId={brokerId} opened={asking} onClose={ask.close} top={access.approvable} />}
    </>
  )
}
