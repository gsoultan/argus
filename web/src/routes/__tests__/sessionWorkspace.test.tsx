import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { assets, currentUser } from '~/lib/seed'

/**
 * The session workspace: each connection a tab over the whole window, kept
 * running while you look at another tab or another page.
 *
 * The views themselves (xterm, the desktop canvas) are stubbed: what is tested
 * here is which sessions exist, which is in front, and that opening one still
 * goes through exactly one ticket request.
 */

vi.mock('~/components/LiveTerminal', () => ({
  LiveTerminal: ({ target, principal, onStateChange }: {
    target: string; principal: string; onStateChange?: (s: string) => void
  }) => {
    queueMicrotask(() => onStateChange?.('live'))
    return <div data-testid="terminal">{principal}@{target}</div>
  },
}))
vi.mock('~/components/FileBrowser', () => ({
  FileBrowser: ({ target }: { target: string }) => <div data-testid="files">files on {target}</div>,
}))

const [ssh, ssh2] = assets.filter((a) => a.protocol !== 'rdp')

function fleet() {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role: 'admin' })
  vi.spyOn(api, 'assets').mockResolvedValue([
    { ...ssh!, tunnelConnected: true }, { ...ssh2!, tunnelConnected: true },
  ])
}

function tickets() {
  const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
    if (String(input).endsWith('/api/v1/terminal/ticket')) {
      return new Response(JSON.stringify({ ticket: 't-' + Math.random() }), { status: 200 })
    }
    throw new Error(`unexpected fetch ${String(input)}`)
  })
  return () => spy.mock.calls.filter(([u]) => String(u).endsWith('/api/v1/terminal/ticket'))
}

afterEach(() => {
  vi.restoreAllMocks()
  window.localStorage.clear()
})

describe('the session workspace', () => {
  it('opens a session as a tab over the page, with one ticket', async () => {
    fleet()
    const calls = tickets()
    await renderRoute(`/connect?host=${encodeURIComponent(ssh!.hostname)}`)
    fireEvent.click(await screen.findByRole('button', { name: /^open terminal$/i }))
    const ws = await screen.findByTestId('session-workspace')
    expect(ws).toBeVisible()
    const tabs = within(ws).getByRole('tablist', { name: /open sessions/i })
    expect(within(tabs).getAllByRole('tab')).toHaveLength(1)
    expect(await within(ws).findByTestId('terminal')).toHaveTextContent(`${ssh!.principals[0]}@${ssh!.hostname}`)
    expect(calls()).toHaveLength(1)
  }, 15_000)

  it('keeps the first session running while a second opens beside it', async () => {
    fleet()
    tickets()
    const { router } = await renderRoute(`/connect?host=${encodeURIComponent(ssh!.hostname)}`)
    fireEvent.click(await screen.findByRole('button', { name: /^open terminal$/i }))
    await screen.findByTestId('terminal')

    await router.navigate({ to: '/connect', search: { host: ssh2!.hostname, open: true } })
    const ws = await screen.findByTestId('session-workspace')
    await waitFor(() => expect(within(ws).getAllByTestId('terminal')).toHaveLength(2))
    const tabs = within(ws).getAllByRole('tab', { selected: false }).filter((t) => t.closest('.argus-ws-tab'))
    expect(tabs).toHaveLength(1)
    // The first is still mounted, behind the second.
    expect(within(ws).getAllByTestId('terminal')[0]).toHaveTextContent(ssh!.hostname)
  }, 15_000)

  it('hides on other pages without ending, and the header leads back', async () => {
    fleet()
    tickets()
    const { router } = await renderRoute(`/connect?host=${encodeURIComponent(ssh!.hostname)}`)
    fireEvent.click(await screen.findByRole('button', { name: /^open terminal$/i }))
    await screen.findByTestId('terminal')
    await router.navigate({ to: '/assets' })
    await waitFor(() => expect(screen.getByTestId('session-workspace')).not.toBeVisible())
    expect(screen.getByTestId('terminal')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /1 session open/i })).toHaveAttribute('href', '/connect')
  }, 15_000)

  it('asks before ending a live session from its tab', async () => {
    fleet()
    tickets()
    await renderRoute(`/connect?host=${encodeURIComponent(ssh!.hostname)}`)
    fireEvent.click(await screen.findByRole('button', { name: /^open terminal$/i }))
    await screen.findByTestId('terminal')
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const end = await screen.findByRole('button', { name: /end session on/i })
    await waitFor(() => {
      fireEvent.click(end)
      expect(confirm).toHaveBeenCalled()
    })
    expect(screen.getByTestId('terminal')).toBeInTheDocument()
    confirm.mockReturnValue(true)
    fireEvent.click(end)
    await waitFor(() => expect(screen.queryByTestId('session-workspace')).not.toBeInTheDocument())
  }, 15_000)

  it('leads back to the asset once its last session ends, not to an empty Connect page', async () => {
    fleet()
    tickets()
    const { router } = await renderRoute(`/connect?host=${encodeURIComponent(ssh!.hostname)}`)
    fireEvent.click(await screen.findByRole('button', { name: /^open terminal$/i }))
    await screen.findByTestId('terminal')
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fireEvent.click(await screen.findByRole('button', { name: /end session on/i }))
    await waitFor(() => expect(router.state.location.pathname).toBe(`/assets/${ssh!.id}`))
    expect(screen.queryByText('Type a server or device name')).not.toBeInTheDocument()
  }, 15_000)

  it('sends a bare /connect with nothing open to Assets', async () => {
    fleet()
    const { router } = await renderRoute('/connect')
    await waitFor(() => expect(router.state.location.pathname).toBe('/assets'))
  }, 15_000)
})

describe('remembering how you connect', () => {
  it('makes the asset page Connect one click once asked to', async () => {
    fleet()
    const calls = tickets()
    await renderRoute(`/connect?host=${encodeURIComponent(ssh!.hostname)}`)
    fireEvent.click(await screen.findByRole('radio', { name: /^Files/ }))
    fireEvent.click(screen.getByRole('checkbox', { name: /one click/i }))
    fireEvent.click(screen.getByRole('button', { name: /^open files$/i }))
    await screen.findByTestId('files')
    expect(calls()).toHaveLength(1)

    const saved = JSON.parse(window.localStorage.getItem('argus.connect.last') ?? '{}')
    expect(saved[ssh!.hostname]).toEqual({ way: 'files', principal: ssh!.principals[0], oneClick: true })
  }, 15_000)
})
