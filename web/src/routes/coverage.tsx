import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * The old Coverage page. A server that is not watched says so on its own row
 * on Servers, and servers waiting to be added are at the top of that list.
 */
export const Route = createFileRoute('/coverage')({
  beforeLoad: () => {
    throw redirect({ to: '/assets', replace: true })
  },
})
