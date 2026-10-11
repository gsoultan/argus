import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { assets, currentUser } from '~/lib/seed'
import { terminalEndpoint } from '~/lib/live'

/**
 * Which way a terminal opens, and the page saying so.
 *
 * A server whose agent holds a tunnel opens through the agent, and the page
 * must not then describe a gateway, ask for its address, or refuse to connect
 * because none is configured. A server whose agent is not connected still
 * goes through the gateway, and the page says that instead.
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

  it('goes through the gateway when the agent is not connected', async () => {
    withHost(false)
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}`)
    expect(await screen.findByText(/through the gateway\./i)).toBeInTheDocument()
    expect(screen.getByText('Gateway endpoint')).toBeInTheDocument()
  })

  it('connects to the control plane for an agent, and to the gateway otherwise', () => {
    expect(terminalEndpoint(true, 'https://gw.example:8081')).toMatch(/\/api\/v1\/terminal$/)
    expect(terminalEndpoint(false, 'https://gw.example:8081')).toBe('https://gw.example:8081/ws/session')
    // No gateway configured is not a crash on the way to the agent path.
    expect(terminalEndpoint(false, '')).toBe('')
  })
})
