import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AssetTypeSettings, type ConnectRules } from '~/components/AssetTypeSettings'
import { theme } from '~/theme'

/** Settings › Asset types: who is asked before connecting, per type. */

const live = vi.hoisted(() => ({ get: vi.fn(), send: vi.fn() }))
vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  ...live,
}))

const rules: ConnectRules = {
  server: { desktop: 'never', terminal: 'never', seconds: 60 },
  workstation: { desktop: 'ask', terminal: 'tell', seconds: 60 },
}

function renderIt(canEdit: boolean) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={qc}>
      <MantineProvider theme={theme}><AssetTypeSettings canEdit={canEdit} /></MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.clearAllMocks())

describe('AssetTypeSettings', () => {
  it('saves a changed rule', async () => {
    live.get.mockResolvedValue(rules)
    live.send.mockImplementation((_m: string, _p: string, body: ConnectRules) => Promise.resolve(body))
    renderIt(true)
    await screen.findByText('Workstations')
    const save = screen.getByRole('button', { name: 'Save' })
    expect((save as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(screen.getAllByText('Tell them')[0]!)
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(live.send).toHaveBeenCalledWith('PUT', '/api/v1/settings/asset-types',
      { ...rules, server: { ...rules.server, desktop: 'tell' } }))
  })

  it('shows an auditor the rules without a way to change them', async () => {
    live.get.mockResolvedValue(rules)
    renderIt(false)
    await screen.findByText('Workstations')
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull()
  })
})
