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
  it('offers a way to sign in when the control plane says "not signed in"', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, { authenticated: false, loginUrl: '/auth/login', oidcEnabled: true }),
    ))
    await renderGate()

    const link = await screen.findByRole('link', { name: /sign in with sso/i })
    expect(link).toHaveAttribute('href', expect.stringContaining('/auth/login'))
    // And does not tell the operator the control plane is down, which is what
    // sent people to debug an identity provider that was working.
    expect(screen.queryByText(/could not be reached/i)).not.toBeInTheDocument()
  })

  it('reports an outage when nothing answers, and offers no button', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await renderGate()

    expect(await screen.findByText(/could not be reached/i)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /sign in with sso/i })).not.toBeInTheDocument()
  })

  it('says so when the control plane has no identity provider', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, { authenticated: false, oidcEnabled: false }),
    ))
    await renderGate()

    expect(await screen.findByText(/no identity provider configured/i)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /sign in with sso/i })).not.toBeInTheDocument()
  })

  // The signed-in path is not asserted here: past the gate the whole shell
  // renders, nav links and all, which needs a router. The route tests cover
  // that. What this file covers is the branch an unauthenticated operator
  // meets, which nothing else renders.
})
