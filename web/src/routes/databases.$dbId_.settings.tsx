import { createFileRoute } from '@tanstack/react-router'
import { DatabaseSettings } from '~/components/DatabaseSettings'
import { guard } from '~/lib/guard'
import { isConfigured } from '~/lib/live'
import { databaseQuery, meQuery } from '~/lib/queries'
import { settingsSearch } from '~/lib/settingsSearch'

/** Who can connect to one database, and its safety settings. Admin only. */
export const Route = createFileRoute('/databases/$dbId_/settings')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  validateSearch: settingsSearch,
  component: Page,
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databaseQuery(params.dbId)).catch(() => undefined) : undefined,
    ]),
})

function Page() {
  const { dbId } = Route.useParams()
  const { tab } = Route.useSearch()
  return <DatabaseSettings key={dbId} dbId={dbId} initialTab={tab} section="databases" />
}
