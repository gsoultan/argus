import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { MantineProvider } from '@mantine/core'
import { Notifications } from '@mantine/notifications'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createRouter } from '@tanstack/react-router'
// Self-hosted, not fetched from a CDN: the console must render identically on
// an air-gapped network, and a third-party font request from a PAM console
// leaks which operator is looking at it and when. Variable weights ship as one
// file per family, so this costs two requests, both precached by the worker.
import '@fontsource-variable/inter'
import '@fontsource-variable/jetbrains-mono'
import { theme } from '~/theme'
import { ErrorState } from '~/components/ErrorState'
import { RoutePending } from '~/components/page'
import { PWAUpdate } from '~/components/PWAUpdate'
import { routeTree } from './routeTree.gen'
import '~/app.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
})

const router = createRouter({
  routeTree,
  context: { queryClient },
  defaultPreload: 'intent',
  defaultPreloadStaleTime: 0,
  scrollRestoration: true,
  /**
   * Show the shell while a route's loader is in flight, immediately.
   *
   * Seven routes prefetch their data in a loader, which the router awaits
   * before committing the route. With no pending component and the default
   * 1000ms grace, a cold load against a slow control plane rendered *nothing*
   * — no header, no navigation, not even a spinner — for as long as the
   * request took. Measured at a blank viewport past 2.2s.
   *
   * These two lines put the pending UI inside the shell from the first frame,
   * which is what keeps the loaders worth having: they still start the fetch in
   * parallel with the route chunk and still power preload-on-intent, without
   * owning the whole screen while they do it.
   */
  // 150ms, not 0: long enough that a warm navigation swaps straight to the new
  // page instead of blinking a spinner at it, short enough that a cold load is
  // never mistaken for a broken one. Measured — warm route changes land in
  // ~36ms, first visits take ~800ms and are the ones that want the spinner.
  defaultPendingMs: 150,
  defaultPendingComponent: RoutePending,
  // Any route without its own errorComponent still lands inside the shell with
  // a readable message instead of blanking the page.
  defaultErrorComponent: ({ error, reset }) => <ErrorState error={error} reset={reset} />,
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <MantineProvider theme={theme} defaultColorScheme="dark" forceColorScheme="dark">
      <QueryClientProvider client={queryClient}>
        <Notifications position="bottom-right" limit={4} />
        <PWAUpdate />
        <RouterProvider router={router} />
      </QueryClientProvider>
    </MantineProvider>
  </StrictMode>,
)
