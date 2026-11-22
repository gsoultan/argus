import { createFileRoute } from '@tanstack/react-router'
import { NetworksPage } from '~/components/Networks'
import { guard } from '~/lib/guard'

/** Networks: VPN links and tunnels on the servers, as their agents see them. */
export const Route = createFileRoute('/networks')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  component: NetworksPage,
})
