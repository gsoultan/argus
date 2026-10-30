import { createFileRoute, redirect } from '@tanstack/react-router'

/** The old address of People, which is a tab on Teams now. */
export const Route = createFileRoute('/users')({
  beforeLoad: () => {
    throw redirect({ to: '/teams', search: { tab: 'people' }, replace: true })
  },
})
