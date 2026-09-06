import { render } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { Notifications } from '@mantine/notifications'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { routeTree } from '~/routeTree.gen'
import { theme } from '~/theme'

/**
 * Renders the real route tree at a real path.
 *
 * Every bug found by driving a browser after the console rewrite — a route that
 * threw on mount, a control with no accessible name — was invisible to the unit
 * tests, because those exercise functions and this exercises pages.
 *
 * It has to be async: TanStack Router resolves its first match, runs the
 * route's loader and resolves its lazy component before anything is committed.
 * A synchronous render against it yields an empty document, which is how the
 * first attempt at these tests failed confusingly.
 *
 * The in-memory fixture backs the queries, so these run against the same
 * `api.ts` the console uses with no control plane — no mocking, and therefore
 * nothing that can drift from what the app actually does.
 */
export async function renderRoute(path: string) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: Infinity, staleTime: Infinity },
      mutations: { retry: false },
    },
  })

  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [path] }),
    // Off in tests: preloading fires loaders for routes the test never asked
    // for, and their failures surface as unrelated noise.
    defaultPreload: false,
  })

  await router.load()

  const result = render(
    <MantineProvider theme={theme} defaultColorScheme="dark" forceColorScheme="dark">
      <QueryClientProvider client={queryClient}>
        <Notifications position="bottom-right" limit={4} />
        {/* The generated tree's context type is not visible from here. */}
        <RouterProvider router={router as never} />
      </QueryClientProvider>
    </MantineProvider>,
  )
  return { ...result, router, queryClient }
}
