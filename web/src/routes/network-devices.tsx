import { createFileRoute } from '@tanstack/react-router'
import { NetworkDevicesPage } from '~/components/NetworkDevices'
import { guardChecks } from '~/lib/guard'

/** Network devices: switches, routers and the like, over SNMP. `device` opens one. */
export const Route = createFileRoute('/network-devices')({
  // The fleet roles, and anyone given a check (checks_access.go).
  beforeLoad: ({ context }) => guardChecks(context.queryClient),
  validateSearch: (s: Record<string, unknown>): { device?: string } => ({
    device: typeof s.device === 'string' && s.device ? s.device : undefined,
  }),
  component: NetworkDevices,
})

function NetworkDevices() {
  const { device } = Route.useSearch()
  return <NetworkDevicesPage openId={device} />
}
