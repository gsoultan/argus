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
 * connected is offline: the page says so, with nothing to press but Try again.
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
    expect(await screen.findByText(/server.s own agent opens the\s+session/i)).toBeInTheDocument()
    expect(screen.queryByText('Gateway endpoint')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^connect$/i })).toBeEnabled()
  }, 15_000)

  it('does not open at all when the agent is not connected', async () => {
    withHost(false)
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}`)
    expect(await screen.findByRole('heading', { name: /is offline/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^connect$/i })).not.toBeInTheDocument()
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
    expect(await screen.findByLabelText(/windows password for/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^connect$/i })).toBeEnabled()
    expect(screen.queryByText(/through the gateway/i)).not.toBeInTheDocument()
    expect(screen.queryByText('Gateway endpoint')).not.toBeInTheDocument()
    // Files are the terminal's; a desktop has none to offer yet.
    expect(screen.queryByRole('button', { name: /files only/i })).not.toBeInTheDocument()
  }, 15_000)

  it('does not open when the agent is not connected', async () => {
    withHost(false, rdp)
    await renderRoute(`/connect?host=${encodeURIComponent(rdp.hostname)}`)
    expect(await screen.findByRole('heading', { name: /is offline/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^connect$/i })).not.toBeInTheDocument()
  })
})

describe('console-only', () => {
  it('offers file transfer through the agent, and no native client', async () => {
    withHost(true)
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}`)
    expect(await screen.findByRole('button', { name: /files only/i })).toBeEnabled()
    expect(screen.queryByText(/prefer your own client/i)).not.toBeInTheDocument()
  }, 15_000)

  it('does not offer file transfer without the agent', async () => {
    withHost(false)
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}`)
    expect(await screen.findByRole('heading', { name: /is offline/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /files only/i })).not.toBeInTheDocument()
  })
})

describe('opening straight from a server card', () => {
  function ticketCalls() {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      if (String(input).endsWith('/api/v1/terminal/ticket')) {
        return new Response(JSON.stringify({ error: 'refused for the test' }), { status: 403 })
      }
      throw new Error(`unexpected fetch ${String(input)}`)
    })
    return () =>
      fetchSpy.mock.calls.filter(([u]) => String(u).endsWith('/api/v1/terminal/ticket'))
  }
  const principal = ssh.principals[0]!

  it('asks for the session without a second click when the link says open', async () => {
    withHost(true)
    const calls = ticketCalls()
    await renderRoute(
      `/connect?host=${encodeURIComponent(ssh.hostname)}&as=${principal}&open=true`,
    )
    // The control plane's answer is shown, so the request was made.
    expect(await screen.findByText(/refused for the test/)).toBeInTheDocument()
    expect(calls()).toHaveLength(1)
    expect(JSON.parse(String(calls()[0]![1]!.body))).toEqual({ target: ssh.hostname, principal })
  }, 15_000)

  it('waits for the button without open', async () => {
    withHost(true)
    const calls = ticketCalls()
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}&as=${principal}`)
    expect(await screen.findByRole('button', { name: /^connect$/i })).toBeEnabled()
    expect(calls()).toHaveLength(0)
  })

  it('does not try when the agent is not connected', async () => {
    withHost(false)
    const calls = ticketCalls()
    await renderRoute(
      `/connect?host=${encodeURIComponent(ssh.hostname)}&as=${principal}&open=true`,
    )
    expect(await screen.findByRole('heading', { name: /is offline/i })).toBeInTheDocument()
    expect(calls()).toHaveLength(0)
  })
})

describe('the states around connecting', () => {
  it('offers to ask for a server you were not given', async () => {
    withHost(true)
    await renderRoute('/connect?host=not-mine-01')
    expect(await screen.findByRole('heading', { name: /can.t connect to not-mine-01/i })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /ask for access/i })).toHaveAttribute(
      'href', expect.stringContaining('/approvals?ask=not-mine-01'),
    )
  })

  it('says root needs approval, and how to ask for it', async () => {
    vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role: 'operator' })
    vi.spyOn(api, 'assets').mockResolvedValue([{ ...ssh, tunnelConnected: true, principals: ['ops', 'root'] }])
    vi.spyOn(globalThis, 'fetch').mockImplementation(async () =>
      new Response(JSON.stringify({ error: 'root on this host requires an approved request' }), { status: 403 }))
    await renderRoute(`/connect?host=${encodeURIComponent(ssh.hostname)}&as=root&open=true`)
    expect(await screen.findByRole('heading', { name: /root needs approval/i })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /ask for root/i })).toHaveAttribute(
      'href', expect.stringContaining('as=root'),
    )
  })
})
