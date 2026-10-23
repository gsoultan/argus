import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { assets, currentUser } from '~/lib/seed'
import { terminalEndpoint } from '~/lib/live'

/**
 * Which way a terminal opens, and the page saying so.
 *
 * SSH opens through the server's agent and nothing else. A server whose agent
 * holds a tunnel opens through it, and the page must not describe a gateway or
 * ask for its address. A server whose agent is not connected cannot be opened
 * at all, and the page says why rather than falling back to the gateway.
 */

const ssh = assets.find((a) => a.protocol !== 'rdp')!

function withHost(tunnelConnected: boolean) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role: 'admin' })
  vi.spyOn(api, 'assets').mockResolvedValue([{ ...ssh, tunnelConnected }])
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('opening a terminal', () => {
  it("goes through the server's agent when it is connected", async () => {
    withHost(true)
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}`)
    expect(await screen.findByText(/through the server's Argus agent/i)).toBeInTheDocument()
    expect(screen.queryByText('Gateway endpoint')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /open terminal/i })).toBeEnabled()
  }, 15_000)

  it('does not open at all when the agent is not connected', async () => {
    withHost(false)
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}`)
    expect(await screen.findByText(/agent is not connected/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /open terminal/i })).toBeDisabled()
    expect(screen.queryByText(/through the gateway/i)).not.toBeInTheDocument()
    expect(screen.queryByText('Gateway endpoint')).not.toBeInTheDocument()
  })

  it('connects to the control plane for an agent, and to the gateway otherwise', () => {
    expect(terminalEndpoint(true, 'https://gw.example:8081')).toMatch(/\/api\/v1\/terminal$/)
    expect(terminalEndpoint(false, 'https://gw.example:8081')).toBe('https://gw.example:8081/ws/session')
    // No gateway configured is not a crash on the way to the agent path.
    expect(terminalEndpoint(false, '')).toBe('')
  })
})

describe('console-only', () => {
  it('offers file transfer through the agent, and no native client', async () => {
    withHost(true)
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}`)
    expect(await screen.findByRole('button', { name: /transfer files/i })).toBeEnabled()
    expect(screen.getByText(/this console is the only way in/i)).toBeInTheDocument()
    expect(screen.queryByText(/prefer your own client/i)).not.toBeInTheDocument()
  }, 15_000)

  it('does not offer file transfer without the agent', async () => {
    withHost(false)
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}`)
    expect(await screen.findByText(/agent is not connected/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /transfer files/i })).not.toBeInTheDocument()
  })
})
