import { Box } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { ConnectPanel } from '~/components/connect/ConnectPanel'
import { PageBody, PageHeader } from '~/components/page'
import { assetsQuery } from '~/lib/queries'
import { useSessions, type SessionView, type Way } from '~/lib/sessions'

/**
 * No loader, deliberately.
 *
 * A route loader is awaited before the router's first render, so it blanks the
 * whole console until it resolves. The panel says it is loading instead.
 */
export const Route = createFileRoute('/connect')({
  component: Connect,
  // `?host=` preselects an asset, so a "Connect" link anywhere in the console
  // lands one step from a session. `?as=` preselects the account; it is only a
  // preselection, and an account the asset does not list for this person is
  // dropped. `?open=true` opens at once, through the same ticket request the
  // button makes. `?view=files` (or `split`) says which way it opens.
  validateSearch: (
    s: Record<string, unknown>,
  ): { host?: string; as?: string; open?: boolean; view?: SessionView } => ({
    ...(typeof s.host === 'string' && s.host ? { host: s.host } : {}),
    ...(typeof s.as === 'string' && s.as ? { as: s.as } : {}),
    ...(s.open === true || s.open === 'true' || s.open === 1 ? { open: true } : {}),
    ...(s.view === 'files' || s.view === 'split' || s.view === 'terminal' ? { view: s.view } : {}),
  }),
})


/**
 * /connect is where open sessions live. With sessions open the workspace
 * (mounted by the shell) covers this page; without, or for a link naming an
 * asset, it is the Connect panel as a page.
 */
function Connect() {
  const { host, as, open, view } = Route.useSearch()
  const { sessions } = useSessions()
  const { data: assets } = useQuery(assetsQuery({}))
  const asset = assets?.find((a) => a.hostname === host)
  const name = host ? (host.split('.')[0] ?? host) : ''
  const initialWay: Way | undefined = view === 'split' ? 'split' : view
  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Assets', to: '/assets' }, { label: name || 'Connect' }]}
        title={asset ? `Connect to ${name}` : 'Connect'}
        description={sessions.length > 0 && !host
          ? 'Your open sessions are in the tabs. Pick another server or device to open one more.'
          : 'Pick how to connect. It opens here in your browser, and the session is recorded.'}
      />
      <PageBody>
        <Box maw={640} w="100%">
          <ConnectPanel hostname={host} initialPrincipal={as} initialWay={initialWay} autoOpen={open === true} />
        </Box>
      </PageBody>
    </Box>
  )
}
