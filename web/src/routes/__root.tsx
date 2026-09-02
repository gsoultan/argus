import { Box, Loader, Text } from '@mantine/core'
import type { QueryClient } from '@tanstack/react-query'
import { Outlet, createRootRouteWithContext } from '@tanstack/react-router'
import { Shell } from '~/components/Shell'

export interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  component: RootLayout,
  pendingComponent: Pending,
  notFoundComponent: NotFound,
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

function NotFound() {
  return (
    <Box className="grid place-items-center" h="60vh">
      <Box ta="center">
        <Text size="42px" fw={700} c="slate.7" lh={1}>404</Text>
        <Text size="sm" c="dimmed" mt="xs">No such page.</Text>
      </Box>
    </Box>
  )
}
