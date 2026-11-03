import { createFileRoute } from '@tanstack/react-router'
import { Home } from '~/components/Home'

/**
 * Home: where the console opens. What needs the signed-in person, then the
 * fleet for whoever sees all of it, or their own servers for everyone else.
 */
export const Route = createFileRoute('/')({
  component: Home,
})
