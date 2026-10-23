import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
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
        authenticated: false, passwordEnabled: true, accountsExist: true,
      }),
    ))
    await renderGate()

    expect(await screen.findByLabelText(/^email$/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/^password$/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^sign in$/i })).toBeInTheDocument()
    expect(screen.queryByText(/could not be reached/i)).not.toBeInTheDocument()
  })

  it('reports an outage when nothing answers, and offers no button', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await renderGate()

    expect(await screen.findByText(/could not be reached/i)).toBeInTheDocument()
    expect(screen.queryByLabelText(/^email$/i)).not.toBeInTheDocument()
  })

  // A brand new deployment. A form that can only ever refuse reads as a bug,
  // so the screen says what to run instead of taking a password nobody set.
  it('tells a fresh install how to create the first account', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, {
        authenticated: false, passwordEnabled: true, accountsExist: false,
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
      json(401, { authenticated: false, passwordEnabled: false }),
    ))
    await renderGate()

    expect(await screen.findByText(/no way to sign anyone in/i)).toBeInTheDocument()
    expect(screen.getByText(/argus-control users add/)).toBeInTheDocument()
    expect(screen.queryByLabelText(/^password$/i)).not.toBeInTheDocument()
  })

  // First run goes straight to the setup page rather than to a sign-in form
  // nobody can use, and the address follows so a reload stays there.
  it('sends a fresh install straight to /setup when the control plane offers it', async () => {
    vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(url.endsWith('/auth/setup')
      ? json(200, { complete: false, available: true, tokenFile: '/var/lib/argus-control/setup-token' })
      : json(401, {
        authenticated: false, passwordEnabled: true, accountsExist: false, setupAvailable: true,
      }))))
    await renderGate()

    expect(await screen.findByLabelText(/setup token/i)).toBeInTheDocument()
    expect(window.location.pathname).toBe('/setup')
    expect(screen.queryByRole('button', { name: /^sign in$/i })).not.toBeInTheDocument()
    window.history.pushState({}, '', '/')
  })

  // No configuration at all: the control plane serves only the setup page,
  // and the page sets up everything, starting with the token.
  it('walks a control plane with no configuration through installing it', async () => {
    window.history.pushState({}, '', '/assets')
    vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(url.endsWith('/auth/setup')
      ? json(200, { complete: false, available: true, mode: 'install', port: 8080, hostname: 'argus.example.com',
        tokenFile: '/var/lib/argus-control/setup-token' })
      : json(401, {
        authenticated: false, passwordEnabled: true, accountsExist: false, setupAvailable: true,
        installing: true,
      }))))
    await renderGate()

    expect(await screen.findByText(/has no configuration yet/i)).toBeInTheDocument()
    expect(window.location.pathname).toBe('/setup')
    for (const step of ['Setup token', 'Database', 'Addresses', 'Certificate', 'Administrator']) {
      expect(screen.getByText(new RegExp(`^\\d\\. ${step}$`))).toBeInTheDocument()
    }
    const next = screen.getByRole('button', { name: /next/i })
    expect(next).toBeDisabled()
    fireEvent.change(screen.getByLabelText(/setup token/i), { target: { value: 'abc' } })
    expect(next).toBeEnabled()
    fireEvent.click(next)
    expect(await screen.findByRole('button', { name: /test connection/i })).toBeInTheDocument()
    // Not past the database until it has answered.
    expect(screen.getByRole('button', { name: /next/i })).toBeDisabled()
    window.history.pushState({}, '', '/')
  })

  describe('at /setup', () => {
    beforeEach(() => window.history.pushState({}, '', '/setup'))
    afterEach(() => window.history.pushState({}, '', '/'))

    const respond = (setup: unknown) =>
      vi.fn((url: string) => Promise.resolve(url.endsWith('/auth/setup')
        ? json(200, setup)
        : json(401, { authenticated: false, passwordEnabled: true, accountsExist: false }),
      ))

    // The token is the part that keeps this page from belonging to whoever
    // finds it first, so the form must ask for it and say where it is.
    it('asks for the token and says which file on the host holds it', async () => {
      vi.stubGlobal('fetch', respond({
        complete: false, available: true, tokenFile: '/var/lib/argus-control/setup-token',
      }))
      await renderGate()

      expect(await screen.findByLabelText(/setup token/i)).toBeInTheDocument()
      expect(screen.getByText(/sudo cat \/var\/lib\/argus-control\/setup-token/)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /create administrator/i })).toBeDisabled()
    })

    // A generated password nobody saw is one nobody saved, so it is shown,
    // and the repeat field is filled rather than asking for it retyped.
    it('generates a password into both fields and shows it', async () => {
      vi.stubGlobal('fetch', respond({ complete: false, available: true }))
      await renderGate()

      fireEvent.click(await screen.findByRole('button', { name: /generate password/i }))
      const pw = screen.getByLabelText(/^password/i) as HTMLInputElement
      const again = screen.getByLabelText(/repeat password/i) as HTMLInputElement
      expect(pw.value).toMatch(/^[A-Za-z2-9]{6}(-[A-Za-z2-9]{6}){3}$/)
      expect(again.value).toBe(pw.value)
      expect(pw.type).toBe('text')
      expect(screen.getByRole('button', { name: /copy password/i })).toBeInTheDocument()
    })

    // A closed setup must not look like a form that is about to work.
    it('draws no form once setup is complete', async () => {
      vi.stubGlobal('fetch', respond({ complete: true, available: false }))
      await renderGate()

      expect(await screen.findByText(/setup is complete/i)).toBeInTheDocument()
      expect(screen.queryByLabelText(/setup token/i)).not.toBeInTheDocument()
    })

    it('points at the host when the browser path is not offered', async () => {
      vi.stubGlobal('fetch', respond({ complete: false, available: false }))
      await renderGate()

      expect(await screen.findByText(/not offering setup in the browser/i)).toBeInTheDocument()
      expect(screen.queryByLabelText(/setup token/i)).not.toBeInTheDocument()
    })
  })

  // The signed-in path is not asserted here: past the gate the whole shell
  // renders, nav links and all, which needs a router. The route tests cover
  // that. What this file covers is the branch an unauthenticated operator
  // meets, which nothing else renders.
})
