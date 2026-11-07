import { screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { assets, currentUser } from '~/lib/seed'
import type { AccessRequest, FleetStats, HomeSnapshot, Session, User } from '~/types/domain'

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

const snapshot = (o: Partial<HomeSnapshot> = {}): HomeSnapshot => ({
  servers: { total: 12, answering: 12, quiet: 0, down: 0 },
  live: { count: 1, root: 1, database: 0, silent: 1 },
  sessions: { last24h: 40, weekAgo: 35, hours: Array.from({ length: 24 }, (_, i) => [i % 3, i % 2, 0] as [number, number, number]), kinds: [23, 12, 0] },
  waiting: { count: 0, oldestAt: null },
  processor: { average: 31, reporting: 2, busiest: [{ assetId: 'a-1', name: 'web-07', busy: 88 }, { assetId: 'a-2', name: 'pay-01', busy: 12 }] },
  disks: [],
  signIns: { last24h: 9, failed24h: 0, failed15m: 0, hours: new Array<number>(24).fill(0) },
  liveList: [{ id: 's-1', user: 'siti.n@northwind.id', principal: 'root', host: 'pay-01', kind: 'terminal', startedAt: new Date().toISOString() }],
  audit: [],
  trends: { since: null, answering: [12, 12, 12], live: [0, 1, 1], waiting: [0, 0, 0], processor: [30, 31, null] },
  unreviewedHosts: 0,
  at: new Date().toISOString(),
  ...o,
})

function fleetHome(role: User['role'], snap: HomeSnapshot) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role, displayName: 'Dewi Pratama' })
  return vi.spyOn(api, 'home').mockResolvedValue(snap)
}

describe('Home for whoever sees the whole fleet', () => {
  it('greets by first name and lays out the live dashboard', async () => {
    const home = fleetHome('admin', snapshot())
    await renderRoute('/')
    expect(await screen.findByRole('heading', { name: HOME }, { timeout: 5_000 })).toHaveTextContent(/Dewi$/)

    // Six numbers, each with what it means.
    const servers = await screen.findByTestId('tile-servers', {}, { timeout: 5_000 })
    expect(servers).toHaveTextContent('12of 12')
    expect(servers).toHaveTextContent('Every server answers')
    expect(screen.getByTestId('tile-live')).toHaveTextContent('1 as root')
    expect(screen.getByTestId('tile-sessions')).toHaveTextContent('+14% on a week ago')
    expect(screen.getByTestId('tile-cpu')).toHaveTextContent('busiest web-07 · 88%')

    // Live now is the control plane's list, with Watch.
    const rows = await screen.findByTestId('live-now')
    expect(within(rows).getByText('siti.n')).toBeInTheDocument()
    expect(within(rows).getByRole('link', { name: 'Watch' })).toHaveAttribute('href', '/sessions/s-1')
    expect(screen.getByTestId('fleet-health')).toHaveTextContent('Every disk has more than 10% free.')

    // No control plane here: the page says its numbers are a sample.
    expect(screen.getByTestId('home-live')).toHaveTextContent('Sample data')
    // One read of the whole dashboard, not one poll per card.
    expect(home).toHaveBeenCalledTimes(1)
    expect(screen.queryByText('Your servers')).toBeNull()
  }, 15_000)

  it('lists what needs someone and links each to where it is dealt with', async () => {
    fleetHome('admin', snapshot({
      waiting: { count: 2, oldestAt: new Date(Date.now() - 22 * 60_000).toISOString() },
      disks: [{ assetId: 'a-9', name: 'core-db-01', mount: '/var/lib/postgresql', used: 96 }, { assetId: 'a-3', name: 'logs-01', mount: '/var/log', used: 91 }],
      signIns: { last24h: 51, failed24h: 7, failed15m: 7, hours: new Array<number>(24).fill(1) },
      unreviewedHosts: 2,
    }))
    await renderRoute('/')
    const needs = await screen.findByTestId('needs-you', {}, { timeout: 5_000 })
    expect(within(needs).getByText('2 requests are waiting on you')).toBeInTheDocument()
    expect(within(needs).getByRole('link', { name: /Review/ })).toHaveAttribute('href', '/approvals')
    // Only a disk at 95% or fuller needs someone; 91% is on Fleet health.
    expect(within(needs).getByText('core-db-01: /var/lib/postgresql is 96% full')).toBeInTheDocument()
    expect(within(needs).queryByText(/logs-01/)).toBeNull()
    expect(within(needs).getByRole('link', { name: /Open/ })).toHaveAttribute('href', '/assets/a-9')
    expect(within(needs).getByText('7 failed sign-ins in 15 minutes')).toBeInTheDocument()
    expect(within(needs).getByText('2 servers are waiting to be added')).toBeInTheDocument()
    expect(screen.getByTestId('tile-signins')).toHaveAttribute('data-tone', 'denied')
    expect(screen.getByTestId('tile-servers')).toHaveAttribute('data-tone', 'calm')
  }, 15_000)

  it('says so in one line when nothing is waiting', async () => {
    fleetHome('owner', snapshot())
    await renderRoute('/')
    expect(await screen.findByTestId('needs-you-clear', {}, { timeout: 5_000 })).toHaveTextContent('Nothing is waiting on you.')
    expect(screen.queryByTestId('needs-you')).toBeNull()
  }, 15_000)
})

describe('Home for an approver', () => {
  it('keeps their own Needs you, from the approvals they decide', async () => {
    const waiting: AccessRequest = {
      id: 'r-1', requesterEmail: 'kevin.tan@northwind.id', assetHostnames: ['pay-01'], principal: 'root',
      justification: 'deploy', state: 'pending', createdAt: new Date().toISOString(), expiresAt: null,
      decidedByEmail: null, breakGlass: false,
    } as unknown as AccessRequest
    signedInAs('approver', { stats: { requestsPending: 2 } })
    vi.spyOn(api, 'requests').mockResolvedValue([waiting])
    vi.spyOn(api, 'assets').mockResolvedValue([])
    const home = vi.spyOn(api, 'home')
    await renderRoute('/')
    const needs = await screen.findByTestId('needs-you', {}, { timeout: 5_000 })
    expect(within(needs).getByText('2 requests are waiting on you')).toBeInTheDocument()
    expect(await within(needs).findByText(/kevin\.tan@northwind\.id asks for root on pay-01/)).toBeInTheDocument()
    // The fleet's dashboard is not read for someone who may not see it.
    expect(home).not.toHaveBeenCalled()
    expect(screen.queryByTestId('home-tiles')).toBeNull()
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
    expect(screen.queryByText('Happening now')).toBeNull()
    expect(screen.queryByTestId('home-tiles')).toBeNull()
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
