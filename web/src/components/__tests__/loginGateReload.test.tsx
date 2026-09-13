import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRouteWithContext,
  createRoute, createRouter,
} from '@tanstack/react-router'
import { theme } from '~/theme'

/**
 * Signing in must re-run what failed before there was a session.
 *
 * Route loaders do not wait for LoginGate. `/` called ensureQueryData while
 * nobody was signed in, took a 401, and the router committed that error to the
 * match -- so the first screen after a successful sign-in was the error
 * boundary, and only a manual reload got past it. Nothing caught this: the unit
 * harness has no router to commit a loader error, and the e2e suite builds
 * against the in-memory fixture, where LoginGate is bypassed entirely.
 *
 * So this builds the smallest thing that reproduces it -- a real router whose
 * loader fails while unauthenticated -- and asserts the loader runs again.
 */
const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': 'application/json' },
  })

let authenticated = false
let loaderRuns = 0

async function renderApp() {
  vi.resetModules()
  vi.stubEnv('VITE_CONTROL_URL', '/')
  const { Shell } = await import('~/components/Shell')

  const rootRoute = createRootRouteWithContext<{ queryClient: QueryClient }>()({
    component: () => <Shell><Outlet /></Shell>,
  })
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    loader: () => {
      loaderRuns++
      // Exactly what ensureQueryData does with a 401 from the control plane.
      if (!authenticated) throw new Error('/api/v1/stats -> 401')
      return null
    },
    component: () => <div>the console</div>,
    errorComponent: () => <div>this page could not be rendered</div>,
  })

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute]),
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: ['/'] }),
    defaultPreload: false,
  })
  await router.load()

  return render(
    <MantineProvider theme={theme} defaultColorScheme="dark" forceColorScheme="dark">
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router as never} />
      </QueryClientProvider>
    </MantineProvider>,
  )
}

beforeEach(() => {
  vi.restoreAllMocks()
  authenticated = false
  loaderRuns = 0
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.includes('/auth/password')) {
      authenticated = true
      return json(200, { email: 'dev@northwind.id', role: 'admin' })
    }
    if (url.includes('/auth/me')) {
      return authenticated
        ? json(200, {
            authenticated: true, email: 'dev@northwind.id',
            displayName: 'Dev Admin', role: 'admin',
          })
        : json(401, { authenticated: false, passwordEnabled: true, accountsExist: true })
    }
    return authenticated ? json(200, {}) : json(401, { error: 'unauthorized' })
  }))
})
afterEach(() => {
  vi.unstubAllEnvs()
  vi.unstubAllGlobals()
})

describe('LoginGate', () => {
  it('re-runs loaders that failed before sign-in, without a reload', async () => {
    await renderApp()

    // The loader has already run once, unauthenticated, and the router has
    // committed its error to the match. The gate hides that behind the form,
    // so the failure only becomes visible once the children render.
    expect(loaderRuns).toBe(1)
    expect(await screen.findByLabelText(/^email$/i)).toBeInTheDocument()

    fireEvent.change(await screen.findByLabelText(/^email$/i), {
      target: { value: 'dev@northwind.id' },
    })
    fireEvent.change(screen.getByLabelText(/^password$/i), {
      target: { value: 'correct-horse' },
    })
    fireEvent.click(screen.getByRole('button', { name: /^sign in$/i }))

    // Without invalidating the router, this stays at 1 and the boundary stays up.
    expect(await screen.findByText('the console')).toBeInTheDocument()
    await waitFor(() => expect(loaderRuns).toBeGreaterThan(1))
    expect(screen.queryByText(/this page could not be rendered/i)).not.toBeInTheDocument()
  })
})
