import { createFileRoute } from '@tanstack/react-router'
import { DatabaseSettings } from '~/components/DatabaseSettings'
import { guard } from '~/lib/guard'
import { isConfigured } from '~/lib/live'
import { databaseQuery, meQuery } from '~/lib/queries'
import { settingsSearch } from '~/lib/settingsSearch'

/**
 * Who can connect to one broker, and its safety settings. Admin only. The
 * page is the database one; its address is under Brokers so the menu says
 * Brokers while it is open.
 */
export const Route = createFileRoute('/brokers/$brokerId_/settings')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  validateSearch: settingsSearch,
  component: Page,
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databaseQuery(params.brokerId)).catch(() => undefined) : undefined,
    ]),
})

function Page() {
  const { brokerId } = Route.useParams()
  const { tab } = Route.useSearch()
  return <DatabaseSettings key={brokerId} dbId={brokerId} initialTab={tab} section="brokers" />
}
