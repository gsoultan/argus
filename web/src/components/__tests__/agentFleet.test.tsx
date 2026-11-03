import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactNode } from 'react'
import { AgentFleetCard } from '~/components/AgentFleet'
import { versionBefore, type AgentFleetRow, type AgentFleetView } from '~/lib/live'
import { theme } from '~/theme'

/**
 * The Servers page's Agents card: which servers run an older agent than the
 * newest release, Update all for the ones that can, and the automatic switch.
 */

const live = vi.hoisted(() => ({
  agentFleet: vi.fn(),
  updateAgentFleet: vi.fn(),
  setAgentAutoUpdate: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  ...live,
}))

vi.mock('@tanstack/react-router', async (orig) => ({
  ...(await orig<typeof import('@tanstack/react-router')>()),
  Link: ({ children }: { children: ReactNode }) => <a>{children}</a>,
}))

const row = (over: Partial<AgentFleetRow>): AgentFleetRow => ({
  assetId: over.hostname ?? 'a', hostname: 'web-01', agentVersion: '1.0.0', agentLastSeenAt: null,
  activeSessions: 0, status: { supported: true }, request: null, state: 'behind', ...over,
})

const fleet: AgentFleetView = {
  latest: '1.1.0', consoleVersion: '1.0.0',
  release: { version: '1.1.0', publishedAt: '2026-11-03T01:15:57Z', page: 'https://example/v1.1.0' },
  checkedAt: '2026-11-03T06:00:00Z', automatic: false, behind: 2, updating: 0,
  servers: [
    row({ hostname: 'web-01' }),
    row({ hostname: 'web-02', activeSessions: 2 }),
    row({ hostname: 'mac-01', state: 'cannot', why: 'this server does not run systemd' }),
    row({ hostname: 'db-01', agentVersion: '1.1.0', state: 'current' }),
  ],
}

function renderCard(canEdit: boolean) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MantineProvider theme={theme}>
        <AgentFleetCard canEdit={canEdit} />
      </MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.clearAllMocks())

describe('AgentFleetCard', () => {
  it('lists the servers behind the newest release and updates them all', async () => {
    live.agentFleet.mockResolvedValue(fleet)
    live.updateAgentFleet.mockResolvedValue({ version: '1.1.0', asked: ['web-01', 'web-02'], skipped: [] })
    renderCard(true)

    expect(await screen.findByText('web-01')).toBeTruthy()
    expect(screen.getByText('this server does not run systemd')).toBeTruthy()
    expect(screen.getByText('1 other server is up to date.')).toBeTruthy()
    expect(screen.queryByText('db-01')).toBeNull()
    expect(screen.getByText(/newer than this console \(1\.0\.0\)/)).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Update 2 servers to 1.1.0' }))
    expect(await screen.findByText(/1 of them has people connected now/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Update when free' }))
    await waitFor(() => expect(live.updateAgentFleet).toHaveBeenCalledWith({}))
  })

  it('updates one server, and turns on automatic updates', async () => {
    live.agentFleet.mockResolvedValue(fleet)
    live.updateAgentFleet.mockResolvedValue({ version: '1.1.0', asked: ['web-01'], skipped: [] })
    live.setAgentAutoUpdate.mockResolvedValue({ on: true })
    renderCard(true)

    const [first] = await screen.findAllByRole('button', { name: 'Update' })
    fireEvent.click(first!)
    await waitFor(() => expect(live.updateAgentFleet).toHaveBeenCalledWith({ hostnames: ['web-01'] }))

    fireEvent.click(screen.getByRole('switch'))
    await waitFor(() => expect(live.setAgentAutoUpdate).toHaveBeenCalledWith(true))
  })

  it('offers nothing to change to an auditor', async () => {
    live.agentFleet.mockResolvedValue(fleet)
    renderCard(false)
    await screen.findByText('web-01')
    expect(screen.queryByRole('button', { name: /Update/ })).toBeNull()
    expect(screen.queryByRole('switch')).toBeNull()
  })
})

describe('versionBefore', () => {
  it('orders releases by number, not by text', () => {
    expect(versionBefore('1.9.0', '1.10.0')).toBe(true)
    expect(versionBefore('1.10.0', '1.9.0')).toBe(false)
    expect(versionBefore('1.2.0-rc.1', '1.2.0')).toBe(true)
    expect(versionBefore('1.2.0', '1.2.0')).toBe(false)
  })
})
