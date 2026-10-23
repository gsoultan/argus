import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { assets, currentUser } from '~/lib/seed'
import { desktopEndpoint, terminalEndpoint } from '~/lib/live'

/**
 * Which way a terminal opens, and the page saying so.
 *
 * Terminals and desktops open through the server's agent and nothing else. A
 * server whose agent holds a tunnel opens through it, and the page must not
 * describe a gateway or ask for its address. A server whose agent is not
 * connected cannot be opened at all, and the page says why.
 */

const ssh = assets.find((a) => a.protocol !== 'rdp')!
const rdp = assets.find((a) => a.protocol === 'rdp')!

function withHost(tunnelConnected: boolean, host = ssh) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role: 'admin' })
  vi.spyOn(api, 'assets').mockResolvedValue([{ ...host, tunnelConnected }])
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

  it('connects to the control plane, which relays to the agent', () => {
    expect(terminalEndpoint()).toMatch(/\/api\/v1\/terminal$/)
    expect(desktopEndpoint()).toMatch(/\/api\/v1\/desktop$/)
  })
})

describe('opening a desktop', () => {
  it("goes through the Windows server's agent, asking for the account's password", async () => {
    withHost(true, rdp)
    await renderRoute(`/connect?host=${encodeURIComponent(rdp.hostname)}`)
    expect(await screen.findByText(/through the server's Argus agent/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/windows password for/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /open desktop/i })).toBeEnabled()
    expect(screen.queryByText(/through the gateway/i)).not.toBeInTheDocument()
    expect(screen.queryByText('Gateway endpoint')).not.toBeInTheDocument()
    // Files are the terminal's; a desktop has none to offer yet.
    expect(screen.queryByRole('button', { name: /transfer files/i })).not.toBeInTheDocument()
  }, 15_000)

  it('does not open when the agent is not connected', async () => {
    withHost(false, rdp)
    await renderRoute(`/connect?host=${encodeURIComponent(rdp.hostname)}`)
    expect(await screen.findByText(/agent is not connected/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /open desktop/i })).toBeDisabled()
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
