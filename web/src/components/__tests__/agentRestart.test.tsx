import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AgentUpdateCard } from '~/components/AgentUpdate'
import type { AgentUpdateView } from '~/lib/live'
import { theme } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Restarting a server's agent from its agent card: offered only when the
 * agent says it can be, asked once nobody is connected unless told now, and
 * shown with its own steps rather than an update's.
 */

const live = vi.hoisted(() => ({
  agentUpdate: vi.fn(),
  requestAgentRestart: vi.fn(),
  requestAgentUpdate: vi.fn(),
  cancelAgentUpdate: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  ...live,
}))

const asset = { id: 'a1', hostname: 'web-01' } as Asset

const view = (over: Partial<AgentUpdateView>): AgentUpdateView => ({
  hostname: 'web-01', enrolled: true, agentVersion: '1.1.0', agentLastSeenAt: null, activeSessions: 0,
  latest: '1.1.0', consoleVersion: '1.1.0', status: { supported: true, restartable: true }, request: null, ...over,
})

function renderCard(canEdit: boolean) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MantineProvider theme={theme}>
        <AgentUpdateCard asset={asset} canEdit={canEdit} />
      </MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.clearAllMocks())

describe('AgentUpdateCard restart', () => {
  it('asks the agent to restart once nobody is connected, or now', async () => {
    live.agentUpdate.mockResolvedValue(view({ activeSessions: 2 }))
    live.requestAgentRestart.mockResolvedValue({})
    renderCard(true)

    fireEvent.click(await screen.findByRole('button', { name: 'Restart agent' }))
    expect(await screen.findByText(/2 session\(s\) are open on this server now/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Restart when nobody is connected' }))
    await waitFor(() => expect(live.requestAgentRestart).toHaveBeenCalledWith('a1', { now: false }))
  })

  it('shows a restart under way with its own steps', async () => {
    live.agentUpdate.mockResolvedValue(view({
      request: { id: 'r1', version: '1.1.0', now: false, restart: true, requestedAt: '2026-11-04T03:00:00Z', requestedBy: 'ada@example.com' },
      status: { supported: true, restartable: true, restart: true, id: 'r1', state: 'waiting', sessions: 1,
        message: 'waiting for 1 open session(s) to end before restarting' },
    }))
    renderCard(true)

    expect(await screen.findByText(/Restarting the agent/)).toBeTruthy()
    expect(screen.queryByText(/Download the new version/)).toBeNull()
    expect(screen.getByRole('button', { name: 'Restart now, ending 1 session(s)' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Restart agent' })).toBeNull()
  })

  it('is not offered by an agent that cannot restart, nor to an auditor', async () => {
    live.agentUpdate.mockResolvedValue(view({
      status: { supported: false, reason: 'no package', restartable: false, restart_reason: 'this server does not run systemd' },
    }))
    renderCard(true)
    expect(await screen.findByText(/Restarting from the console is not available here: this server does not run systemd/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Restart agent' })).toBeNull()
  })

  it('offers nothing to an auditor', async () => {
    live.agentUpdate.mockResolvedValue(view({}))
    renderCard(false)
    await screen.findByText('1.1.0')
    expect(screen.queryByRole('button', { name: 'Restart agent' })).toBeNull()
  })
})
