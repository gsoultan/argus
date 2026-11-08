import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { SignedIn } from '~/components/connect/SignedIn'
import { connectOutcome, slotsFull, type LogonsView } from '~/lib/logonsApi'
import { theme } from '~/theme'

/**
 * Who is signed in, on the Connect page: the slot meter, what connecting as
 * the picked account will do, and an admin signing out an idle session.
 */

const api = vi.hoisted(() => ({ assetLogons: vi.fn(), signOutLogon: vi.fn() }))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))
vi.mock('~/lib/logonsApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/logonsApi')>()),
  ...api,
}))

const full = (over: Partial<LogonsView> = {}): LogonsView => ({
  state: 'ok', platform: 'windows', edition: 'server', remoteLimit: 2, canSignOut: true,
  logons: [
    { session: 1, account: 'SRV\\admin', state: 'active', console: true },
    { session: 2, account: 'SRV\\ana', state: 'disconnected', remote: true, from: 'LAPTOP-7', idleSeconds: 7200 },
    { session: 3, account: 'SRV\\bo', state: 'active', remote: true, from: 'DESK-2' },
  ],
  ...over,
})

function renderIt(principal = 'ops') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={qc}>
      <MantineProvider theme={theme}><SignedIn assetId="a1" principal={principal} /></MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.clearAllMocks())

describe('what connecting will do', () => {
  it('counts only Remote Desktop sessions against the limit', () => {
    expect(slotsFull(full())).toBe(true)
    expect(slotsFull(full({ remoteLimit: 0 }))).toBe(false)
    expect(slotsFull(full({ platform: 'linux' }))).toBe(false)
  })
  it('says what happens to the picked account', () => {
    expect(connectOutcome(full(), 'ana')).toMatch(/goes back into it/)
    expect(connectOutcome(full(), 'admin')).toMatch(/locks the screen/)
    expect(connectOutcome(full(), 'ops')).toMatch(/Every Remote Desktop session/)
    expect(connectOutcome(full({ logons: [] }), 'ops')).toBeUndefined()
    const pc = full({ edition: 'workstation', remoteLimit: 1, logons: [{ session: 1, account: 'PC\\sam', state: 'active', console: true }] })
    expect(connectOutcome(pc, 'ops')).toMatch(/ask them to let you in/)
    expect(connectOutcome(full({ platform: 'linux' }), 'ops')).toBeUndefined()
  })
})

describe('SignedIn', () => {
  it('shows the sessions and the slot meter', async () => {
    api.assetLogons.mockResolvedValue(full())
    renderIt()
    expect(await screen.findByText('2 of 2 Remote Desktop sessions in use')).toBeTruthy()
    expect(screen.getByText('SRV\\ana')).toBeTruthy()
    expect(screen.getByText(/idle 2 h/)).toBeTruthy()
    // Only the disconnected, non-console session can be signed out.
    expect(screen.getAllByRole('button', { name: /Sign out/ })).toHaveLength(1)
  })

  it('asks before signing out, then signs out the session it showed', async () => {
    api.assetLogons.mockResolvedValue(full())
    api.signOutLogon.mockResolvedValue({ state: 'ok', logons: [], canSignOut: true, signedOut: 2 })
    renderIt()
    fireEvent.click(await screen.findByRole('button', { name: /Sign out/ }))
    expect(screen.getByText(/closes everything left open/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Sign out ana' }))
    await waitFor(() => expect(api.signOutLogon).toHaveBeenCalledWith('a1', 2, 'SRV\\ana'))
  })

  it('offers no sign-out to someone who may not', async () => {
    api.assetLogons.mockResolvedValue(full({ canSignOut: false }))
    renderIt()
    await screen.findByText('SRV\\ana')
    expect(screen.queryByRole('button', { name: /Sign out/ })).toBeNull()
  })

  it('stays quiet when the agent cannot say', async () => {
    api.assetLogons.mockResolvedValue({ state: 'offline', logons: [], canSignOut: false })
    renderIt()
    await waitFor(() => expect(api.assetLogons).toHaveBeenCalled())
    expect(screen.queryByText('Signed in now')).toBeNull()
  })
})
