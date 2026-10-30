import { createFileRoute, redirect } from '@tanstack/react-router'

/** The menu's Activity: sessions first, the audit log a tab beside them. */
export const Route = createFileRoute('/activity')({
  beforeLoad: () => {
    throw redirect({ to: '/sessions', replace: true })
  },
})
