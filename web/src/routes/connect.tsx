import { Box } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useEffect } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
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
 * (mounted by the shell) covers this page; for a link naming an asset it is
 * that asset's Connect panel as a page.
 *
 * With neither, there is nothing here to show: a bare "pick a server" page
 * read as a dead end once the last session ended. So it leads back instead,
 * to the asset that session was on, or to Assets.
 */
function Connect() {
  const { host, as, open, view } = Route.useSearch()
  const { sessions, lastEnded } = useSessions()
  const { data: assets } = useQuery(assetsQuery({}))
  const navigate = useNavigate()
  const empty = !host && sessions.length === 0
  const endedOn = lastEnded ? assets?.find((a) => a.hostname === lastEnded) : undefined
  // Waits for the asset list only when there is an ended session to find in it.
  const ready = empty && (!lastEnded || assets !== undefined)
  useEffect(() => {
    if (!ready) return
    if (endedOn) void navigate({ to: '/assets/$assetId', params: { assetId: endedOn.id }, replace: true })
    else void navigate({ to: '/assets', replace: true })
  }, [ready, endedOn, navigate])
  if (empty) return null
  const asset = assets?.find((a) => a.hostname === host)
  const name = host ? (host.split('.')[0] ?? host) : ''
  const initialWay: Way | undefined = view === 'split' ? 'split' : view
  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Assets', to: '/assets' }, { label: name || 'Connect' }]}
        title={asset ? `Connect to ${name}` : 'Connect'}
        description="Pick how to connect. It opens here in your browser, and the session is recorded."
      />
      <PageBody>
        <Box maw={640} w="100%">
          <ConnectPanel hostname={host} initialPrincipal={as} initialWay={initialWay} autoOpen={open === true} />
        </Box>
      </PageBody>
    </Box>
  )
}
