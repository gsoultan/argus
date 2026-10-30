import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { Notifications } from '@mantine/notifications'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApproveServer } from '~/components/ApproveServer'
import { api } from '~/lib/api'
import { currentUser, users } from '~/lib/seed'
import { theme } from '~/theme'
import type { DiscoveredHost } from '~/types/domain'

/**
 * Approving a server asks who may use it, and sends what was picked.
 *
 * The admin who added a server could not reach it, because approval granted no
 * accounts and the Connect page then told them to ask an admin. These pin the
 * fix: the approval carries the accounts and people chosen, the agent's own
 * report is only an offer, and "Add and connect" lands in a session.
 */

const host: DiscoveredHost = {
  hostname: 'web-04',
  os: 'Ubuntu 24.04',
  addresses: ['10.0.4.12'],
  sshPorts: [22],
  accounts: ['root', 'ubuntu', 'deploy'],
  version: '0.6.0',
  firstSeenAt: '2026-10-30T07:00:00Z',
  lastSeenAt: '2026-10-30T07:00:00Z',
  state: 'unreviewed',
}

function enrolCalls() {
  const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
    if (String(input).includes('/api/v1/discovered/web-04/enrol')) {
      return new Response(
        JSON.stringify({ assetId: 'a-1', hostname: 'web-04', assetHostname: 'web-04.example' }),
        { status: 200 },
      )
    }
    throw new Error(`unexpected fetch ${String(input)}`)
  })
  return () =>
    spy.mock.calls
      .filter(([u]) => String(u).includes('/enrol'))
      .map(([, init]) => JSON.parse(String(init!.body)) as { principals: string[]; people: string[] })
}

async function renderDialog() {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role: 'admin' })
  vi.spyOn(api, 'users').mockResolvedValue(users)
  const onClose = vi.fn()
  const root = createRootRoute()
  const page = createRoute({
    getParentRoute: () => root,
    path: '/',
    component: () => <ApproveServer host={host} onClose={onClose} />,
  })
  const connect = createRoute({
    getParentRoute: () => root,
    path: '/connect',
    validateSearch: (s: Record<string, unknown>) => s,
    component: () => <p>connect page</p>,
  })
  const router = createRouter({
    routeTree: root.addChildren([page, connect]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  await router.load()
  render(
    <MantineProvider theme={theme}>
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <Notifications />
        <RouterProvider router={router as never} />
      </QueryClientProvider>
    </MantineProvider>,
  )
  return { router, onClose }
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('approving a server', () => {
  it('offers the reported accounts with an ordinary one ticked, never root', async () => {
    await renderDialog()
    expect(await screen.findByRole('checkbox', { name: 'ubuntu' })).toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'root' })).not.toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'deploy' })).not.toBeChecked()
  }, 15_000)

  it('sends the accounts picked, and opens the session on "Add and connect"', async () => {
    const calls = enrolCalls()
    const { router, onClose } = await renderDialog()
    fireEvent.click(await screen.findByRole('checkbox', { name: 'deploy' }))
    fireEvent.click(screen.getByRole('button', { name: 'Add and connect' }))
    await waitFor(() => expect(calls()).toHaveLength(1))
    expect(calls()[0]).toEqual({ principals: ['ubuntu', 'deploy'], people: [] })
    await waitFor(() => expect(router.state.location.pathname).toBe('/connect'))
    expect(router.state.location.search).toEqual({ host: 'web-04.example', as: 'ubuntu', open: true })
    expect(onClose).toHaveBeenCalled()
  })

  it('cannot connect with no account ticked', async () => {
    const calls = enrolCalls()
    await renderDialog()
    fireEvent.click(await screen.findByRole('checkbox', { name: 'ubuntu' }))
    expect(screen.getByRole('button', { name: 'Add and connect' })).toBeDisabled()
    // Adding with nothing granted is still allowed, and says so.
    fireEvent.click(screen.getByRole('button', { name: 'Add server' }))
    await waitFor(() => expect(calls()).toEqual([{ principals: [], people: [] }]))
    expect(await screen.findByText(/nobody can log in yet/i)).toBeInTheDocument()
  })

  it('takes an account the server did not report', async () => {
    const calls = enrolCalls()
    await renderDialog()
    const input = await screen.findByLabelText('Another account')
    fireEvent.change(input, { target: { value: 'gembit' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(screen.getByRole('checkbox', { name: 'gembit' })).toBeChecked()
    fireEvent.click(screen.getByRole('button', { name: 'Add server' }))
    await waitFor(() => expect(calls()[0]?.principals).toEqual(['ubuntu', 'gembit']))
  })
})
