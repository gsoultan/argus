import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * Servers is the first page for everyone. What used to be the Overview's
 * findings lives where it is acted on: waiting servers on Servers, held logins
 * on Approvals, live sessions on Activity.
 */
export const Route = createFileRoute('/')({
  beforeLoad: () => {
    throw redirect({ to: '/assets', replace: true })
  },
})
