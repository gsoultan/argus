import { createFileRoute } from '@tanstack/react-router'
import { WebsitesPage } from '~/components/Websites'
import { guard } from '~/lib/guard'

/** Websites: whether each site is up, and its uptime. `check` opens one. */
export const Route = createFileRoute('/websites')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  validateSearch: (s: Record<string, unknown>): { check?: string } => ({
    check: typeof s.check === 'string' && s.check ? s.check : undefined,
  }),
  component: Websites,
})

function Websites() {
  const { check } = Route.useSearch()
  return <WebsitesPage openId={check} />
}
