import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { NetworksPage, parseNetworkTab, type NetworkTab } from '~/components/Networks'
import { guard } from '~/lib/guard'

/** Networks: VPN links and tunnels on the servers, as their agents see them. `tab` opens one kind. */
export const Route = createFileRoute('/networks')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  validateSearch: (s: Record<string, unknown>): { tab?: NetworkTab } => ({ tab: parseNetworkTab(s.tab) }),
  component: Networks,
})

function Networks() {
  const navigate = useNavigate({ from: Route.fullPath })
  const tab = Route.useSearch().tab ?? 'overview'
  return <NetworksPage tab={tab} onTab={(t) => void navigate({ search: { tab: t === 'overview' ? undefined : t }, replace: true })} />
}
