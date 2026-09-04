import { Box, Loader, Text } from '@mantine/core'
import type { QueryClient } from '@tanstack/react-query'
import { Outlet, createRootRouteWithContext } from '@tanstack/react-router'
import { Shell } from '~/components/Shell'
import { ErrorState } from '~/components/ErrorState'
import { ButtonLink } from '~/components/links'
import { FS } from '~/theme'

export interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  component: RootLayout,
  pendingComponent: Pending,
  notFoundComponent: NotFound,
  errorComponent: RootError,
})

function RootLayout() {
  return (
    <Shell>
      <Outlet />
    </Shell>
  )
}

function Pending() {
  return (
    <Box className="grid place-items-center" h="60vh">
      <Loader size="sm" color="teal" />
    </Box>
  )
}

/**
 * Catches anything a route throws while rendering.
 *
 * `reset` re-runs the route rather than reloading the document, so a transient
 * failure — a query that resolved to something unexpected — costs a click
 * instead of the whole application state.
 */
function RootError({ error, reset }: { error: unknown; reset: () => void }) {
  return <ErrorState error={error} reset={reset} />
}

function NotFound() {
  return (
    <Box className="grid place-items-center" h="60vh">
      <Box ta="center">
        <Text size={FS.display} fw={700} c="slate.7" lh={1}>404</Text>
        <Text size="sm" c="dimmed" mt="xs" mb="md">No such page.</Text>
        <ButtonLink size="xs" variant="light" color="teal" to="/">
          Back to overview
        </ButtonLink>
      </Box>
    </Box>
  )
}
