import { createFileRoute, redirect } from '@tanstack/react-router'

/** The old address of Approvals. Kept so a bookmark lands somewhere sensible. */
export const Route = createFileRoute('/requests')({
  beforeLoad: () => {
    throw redirect({ to: '/approvals', replace: true })
  },
})
