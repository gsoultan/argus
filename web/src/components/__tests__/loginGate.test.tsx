import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { theme } from '~/theme'

/**
 * The sign-in screen, which had no test at all.
 *
 * The route tests run against the in-memory fixture, where isConfigured() is
 * false and LoginGate waves everything through -- so the branch an operator
 * actually meets was never rendered by anything. That is why a 401 could be
 * misread as an outage and hide the only way into the product.
 */
async function renderGate() {
  vi.resetModules()
  vi.stubEnv('VITE_CONTROL_URL', '/')
  const { Shell } = await import('~/components/Shell')
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <MantineProvider theme={theme} defaultColorScheme="dark" forceColorScheme="dark">
      <QueryClientProvider client={qc}>
        <Shell>
          <div>the console</div>
        </Shell>
      </QueryClientProvider>
    </MantineProvider>,
  )
}

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })

beforeEach(() => vi.restoreAllMocks())
afterEach(() => {
  vi.unstubAllEnvs()
  vi.unstubAllGlobals()
})

describe('LoginGate', () => {
  it('offers a password form when the control plane holds its own accounts', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, {
        authenticated: false, passwordEnabled: true,
        oidcEnabled: false, accountsExist: true,
      }),
    ))
    await renderGate()

    expect(await screen.findByLabelText(/^email$/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/^password$/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^sign in$/i })).toBeInTheDocument()
    // No identity provider is configured, so nothing should suggest one.
    expect(screen.queryByRole('link', { name: /sso/i })).not.toBeInTheDocument()
    expect(screen.queryByText(/could not be reached/i)).not.toBeInTheDocument()
  })

  it('offers SSO when an identity provider is configured', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, { authenticated: false, loginUrl: '/auth/login', oidcEnabled: true }),
    ))
    await renderGate()

    const link = await screen.findByRole('link', { name: /sign in with sso/i })
    expect(link).toHaveAttribute('href', expect.stringContaining('/auth/login'))
    expect(screen.queryByText(/could not be reached/i)).not.toBeInTheDocument()
  })

  // Both doors, when both exist.
  it('offers both when both are available', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, {
        authenticated: false, passwordEnabled: true,
        oidcEnabled: true, accountsExist: true,
      }),
    ))
    await renderGate()
    expect(await screen.findByLabelText(/^email$/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /sign in with sso/i })).toBeInTheDocument()
  })

  it('reports an outage when nothing answers, and offers no button', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await renderGate()

    expect(await screen.findByText(/could not be reached/i)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /sign in with sso/i })).not.toBeInTheDocument()
  })

  // A brand new deployment. A form that can only ever refuse reads as a bug,
  // so the screen says what to run instead of taking a password nobody set.
  it('tells a fresh install how to create the first account', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, {
        authenticated: false, passwordEnabled: true,
        oidcEnabled: false, accountsExist: false,
      }),
    ))
    await renderGate()

    expect(await screen.findByText(/no accounts exist yet/i)).toBeInTheDocument()
    expect(screen.getByText(/argus-control users add/)).toBeInTheDocument()
    expect(screen.queryByLabelText(/^password$/i)).not.toBeInTheDocument()
  })

  // Neither door. Better to say so, and say how to fix it, than to render an
  // empty card someone stares at.
  it('says so when there is no way to sign in at all, and how to fix it', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, { authenticated: false, oidcEnabled: false, passwordEnabled: false }),
    ))
    await renderGate()

    expect(await screen.findByText(/no way to sign anyone in/i)).toBeInTheDocument()
    expect(screen.getByText(/argus-control users add/)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /sign in with sso/i })).not.toBeInTheDocument()
  })

  // The signed-in path is not asserted here: past the gate the whole shell
  // renders, nav links and all, which needs a router. The route tests cover
  // that. What this file covers is the branch an unauthenticated operator
  // meets, which nothing else renders.
})
