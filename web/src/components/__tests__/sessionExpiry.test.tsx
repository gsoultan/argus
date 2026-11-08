import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRouteWithContext,
  createRoute, createRouter,
} from '@tanstack/react-router'
import { theme } from '~/theme'

/**
 * A session that ends while a page is open.
 *
 * Before, nothing told the sign-in gate: /api/v1/databases answered 401 and the
 * page printed "/api/v1/databases → 401" until someone reloaded. Now any 401
 * from the API sends the gate to /auth/me, and if the session is really gone
 * the sign-in form replaces the page -- at the same address, so signing in
 * again lands back on it.
 */
const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': 'application/json' },
  })

let authenticated = true
/** One /api/v1/databases refusal that is not about the session. */
let refuseOnce = false

async function renderApp() {
  vi.resetModules()
  vi.stubEnv('VITE_CONTROL_URL', '/')
  const { Shell } = await import('~/components/Shell')
  const { get } = await import('~/lib/live')

  function Databases() {
    const q = useQuery({
      queryKey: ['databases'],
      queryFn: () => get<{ name: string }[]>('/api/v1/databases'),
      retry: false,
    })
    return (
      <div>
        <div>databases page</div>
        {q.error && <div>error: {q.error.message}</div>}
        <button onClick={() => void q.refetch()}>refresh</button>
      </div>
    )
  }

  const rootRoute = createRootRouteWithContext<{ queryClient: QueryClient }>()({
    component: () => <Shell><Outlet /></Shell>,
  })
  const databases = createRoute({
    getParentRoute: () => rootRoute, path: '/databases', component: Databases,
  })
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([databases]),
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: ['/databases'] }),
    defaultPreload: false,
  })
  await router.load()

  render(
    <MantineProvider theme={theme} defaultColorScheme="light" forceColorScheme="light">
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router as never} />
      </QueryClientProvider>
    </MantineProvider>,
  )
  return router
}

beforeEach(() => {
  vi.restoreAllMocks()
  authenticated = true
  refuseOnce = false
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
    if (url.includes('/api/v1/databases')) {
      if (refuseOnce) {
        refuseOnce = false
        return json(401, { error: 'unauthorized' })
      }
      return authenticated ? json(200, []) : json(401, { error: 'unauthorized' })
    }
    // Lists the shell polls for its header counts.
    if (url.includes('/api/v1/direct-logins')) {
      return authenticated ? json(200, []) : json(401, { error: 'unauthorized' })
    }
    return authenticated ? json(200, {}) : json(401, { error: 'unauthorized' })
  }))
})
afterEach(() => {
  vi.unstubAllEnvs()
  vi.unstubAllGlobals()
})

describe('an expired session', () => {
  it('brings back the sign-in form, then returns to the same page', async () => {
    const router = await renderApp()
    expect(await screen.findByText('databases page')).toBeInTheDocument()

    // The session ends on the server; the next request finds out.
    authenticated = false
    fireEvent.click(screen.getByRole('button', { name: 'refresh' }))

    expect(await screen.findByLabelText(/^email$/i)).toBeInTheDocument()
    expect(screen.getByText(/your session has ended/i)).toBeInTheDocument()
    expect(screen.queryByText(/→/)).not.toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/databases')

    fireEvent.change(screen.getByLabelText(/^email$/i), { target: { value: 'dev@northwind.id' } })
    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: 'correct-horse' } })
    fireEvent.click(screen.getByRole('button', { name: /^sign in$/i }))

    expect(await screen.findByText('databases page')).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/databases')
    await waitFor(() => expect(screen.queryByText(/^error:/)).not.toBeInTheDocument())
  })

  it('does not sign anyone out when /auth/me says the session is fine', async () => {
    await renderApp()
    expect(await screen.findByText('databases page')).toBeInTheDocument()

    // A 401 that is about something else: the session itself is still good.
    refuseOnce = true
    fireEvent.click(screen.getByRole('button', { name: 'refresh' }))

    expect(await screen.findByText(/^error:/)).toBeInTheDocument()
    expect(screen.getByText(/your session has ended/i)).toBeInTheDocument()
    expect(screen.queryByLabelText(/^email$/i)).not.toBeInTheDocument()
    expect(screen.getByText('databases page')).toBeInTheDocument()
  })
})
