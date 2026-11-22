import { createFileRoute } from '@tanstack/react-router'
import { MyVPNPage } from '~/components/MyVPN'

/** My VPN: anyone's own devices on the staff VPN. Open to every role. */
export const Route = createFileRoute('/vpn')({
  component: MyVPNPage,
})
