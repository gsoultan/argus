import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * The old address of My servers, which is now the Connect view of Servers.
 *
 * "My servers" and "Servers" sat side by side in the menu and nobody could say
 * which was which: one was the list you connect from, the other the list you
 * manage, and both were the same servers. They are one page now. Kept so a
 * bookmark lands somewhere sensible.
 */
export const Route = createFileRoute('/mine')({
  beforeLoad: () => {
    throw redirect({ to: '/assets', replace: true })
  },
})
