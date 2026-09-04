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
import { PWAUpdate } from '~/components/PWAUpdate'
import { routeTree } from './routeTree.gen'
import './app.css'

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
