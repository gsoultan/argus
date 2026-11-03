import { screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { assets, currentUser, sessions } from '~/lib/seed'
import type { AccessRequest, FleetStats, Session, User } from '~/types/domain'

/**
 * Home: where the console opens.
 *
 * Whoever sees the whole fleet gets the fleet; everyone else gets their own
 * servers and nothing about the rest. "Needs you" shows only what is waiting,
 * and says plainly when nothing is.
 */

const HOME = /^Good (morning|afternoon|evening)/

const calm: FleetStats = {
  assetsTotal: 12, assetsUnreachable: 0, hostKeysUnpinned: 0, sessionsActive: 2, sessionsSilent: 0,
  sessionsToday: 40, requestsPending: 0, credentialsOverdue: 0, standingCredentialAssets: 0,
  sessionsDirectToday: 0, assetsUnmonitored: 0, agentsStale: 0,
}

const live = (o: Partial<Session>): Session => ({
  ...sessions[0]!, state: 'active', endedAt: null, silent: false, ...o,
})

function signedInAs(role: User['role'], o: { stats?: Partial<FleetStats>; active?: Session[] } = {}) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role, displayName: 'Dewi Pratama' })
  vi.spyOn(api, 'stats').mockResolvedValue({ ...calm, ...o.stats })
  vi.spyOn(api, 'directLogins').mockResolvedValue([])
  const active = o.active ?? []
  vi.spyOn(api, 'sessions').mockImplementation(async (state) => (state === 'active' ? active : active))
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('Home for whoever sees the whole fleet', () => {
  it('greets by first name and lays out the fleet, live sessions and the audit log', async () => {
    signedInAs('admin', {
      active: [
        live({ id: 's-1', userEmail: 'siti.n@northwind.id', principal: 'root', assetHostname: 'pay-01' }),
        live({ id: 's-2', userEmail: 'kevin.tan@northwind.id', principal: 'deploy', assetHostname: 'web-02', silent: true }),
      ],
    })
    await renderRoute('/')
    expect(await screen.findByRole('heading', { name: HOME }, { timeout: 5_000 })).toHaveTextContent(/Dewi$/)
    expect(await screen.findByText('12 of 12')).toBeInTheDocument()

    // A silent session is not "live now": the tile says it went silent instead.
    const rows = await screen.findByTestId('live-now')
    expect(within(rows).getByText('siti.n')).toBeInTheDocument()
    expect(within(rows).queryByText('kevin.tan')).toBeNull()
    expect(within(rows).getByRole('link', { name: 'Watch' })).toHaveAttribute('href', '/sessions/s-1')

    expect(await screen.findByTestId('recent-audit', {}, { timeout: 5_000 })).toBeInTheDocument()
    // Operators' cards are not on an admin's Home.
    expect(screen.queryByText('Your servers')).toBeNull()
  }, 15_000)

  it('lists what is waiting and links each to where it is dealt with', async () => {
    const waiting: AccessRequest = {
      id: 'r-1', requesterEmail: 'kevin.tan@northwind.id', assetHostnames: ['pay-01'], principal: 'root',
      justification: 'deploy', state: 'pending', createdAt: new Date().toISOString(), expiresAt: null,
      decidedByEmail: null, breakGlass: false,
    } as unknown as AccessRequest
    signedInAs('admin', { stats: { requestsPending: 2 } })
    vi.spyOn(api, 'requests').mockResolvedValue([waiting])
    await renderRoute('/')
    const needs = await screen.findByTestId('needs-you', {}, { timeout: 5_000 })
    expect(within(needs).getByText('2 requests are waiting on you')).toBeInTheDocument()
    expect(await within(needs).findByText(/kevin\.tan@northwind\.id asks for root on pay-01/)).toBeInTheDocument()
    expect(within(needs).getByRole('link', { name: /Review/ })).toHaveAttribute('href', '/approvals')
    expect(screen.queryByTestId('needs-you-clear')).toBeNull()
  }, 15_000)

  it('says so in one line when nothing is waiting', async () => {
    signedInAs('owner')
    vi.spyOn(api, 'requests').mockResolvedValue([])
    await renderRoute('/')
    expect(await screen.findByTestId('needs-you-clear', {}, { timeout: 5_000 })).toHaveTextContent('Nothing is waiting on you.')
    expect(screen.queryByTestId('needs-you')).toBeNull()
  }, 15_000)
})

describe('Home for someone who is given servers', () => {
  it('shows their servers with Connect and nothing about the fleet', async () => {
    const mine = assets.filter((a) => a.protocol !== 'rdp').slice(0, 2)
    signedInAs('operator')
    vi.spyOn(api, 'assets').mockResolvedValue(mine)
    vi.spyOn(api, 'requests').mockResolvedValue([])
    await renderRoute('/')
    await screen.findByRole('heading', { name: HOME }, { timeout: 5_000 })
    const grid = await screen.findByTestId('your-servers', {}, { timeout: 5_000 })
    expect(within(grid).getAllByRole('heading', { level: 3 })).toHaveLength(2)
    expect(within(grid).getAllByRole('link', { name: /^Connect to / }).length).toBeGreaterThan(0)

    expect(screen.queryByText('Fleet')).toBeNull()
    expect(screen.queryByText('Live now')).toBeNull()
    expect(screen.queryByText('Latest in the audit log')).toBeNull()
    // An operator approves nothing, so there is no "Needs you" either way.
    expect(screen.queryByTestId('needs-you-clear')).toBeNull()
    expect(screen.getByText('You have not asked for access')).toBeInTheDocument()
  }, 15_000)

  it('says how to get a server when none is given yet', async () => {
    signedInAs('operator')
    vi.spyOn(api, 'assets').mockResolvedValue([])
    vi.spyOn(api, 'requests').mockResolvedValue([])
    await renderRoute('/')
    expect(await screen.findByText('No server is given to you yet', {}, { timeout: 5_000 })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Ask for access' })).toHaveAttribute('href', '/approvals')
  }, 15_000)
})
