import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import { renderWithProviders } from '~/test/render'

const live = vi.hoisted(() => ({ get: vi.fn(), send: vi.fn(), isConfigured: () => true }))
vi.mock('~/lib/live', async (orig) => ({ ...(await orig<typeof import('~/lib/live')>()), ...live }))
const { SecretsServers } = await import('~/components/SecretsServers')

const base = {
  tlsPin: '', sealKey: '', version: '', secretCount: 0, rulesSerial: 0, logSeq: 0, createdBy: 'a@x', createdAt: '', enrolled: false, connected: false,
}
const view = {
  organization: 'acme', controlPublicKey: 'ed25519:KEY',
  servers: [
    { ...base, id: 's1', name: 'Core', mode: 'direct', url: 'https://core:8460', folders: ['prod'], secretCount: 12, rulesSerial: 4, logSeq: 90,
      lastContactAt: '2026-11-27T10:00:00Z', enrolled: false },
    { ...base, id: 's2', name: 'Edge', mode: 'tunnel', url: '', folders: ['edge'], logGap: 'entry 7 does not follow on from entry 6' },
  ],
}

beforeEach(() => {
  live.get.mockReset().mockResolvedValue(view)
  live.send.mockReset()
})

describe('Settings › Secrets servers', () => {
  it('says which servers are in step, which are not enrolled, and which log has a gap', async () => {
    renderWithProviders(<SecretsServers />)
    expect(await screen.findByText('In step')).toBeInTheDocument()
    expect(screen.getByText('Not enrolled yet')).toBeInTheDocument()
    expect(screen.getByText('Read log has a gap')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Resume log' })).toBeInTheDocument()
    expect(screen.getByText('ed25519:KEY')).toBeInTheDocument()
  })

  it('adds a server that dials Argus and shows its enrolment command once', async () => {
    live.send.mockResolvedValue({ ...base, id: 's3', name: 'Branch', mode: 'tunnel', url: '', folders: ['branch'], enrollToken: 'argus-secret-enrol-acme.TOKEN' })
    renderWithProviders(<SecretsServers />)
    fireEvent.click(await screen.findByRole('button', { name: 'Add server' }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: 'Branch' } })
    const folders = within(dialog).getByLabelText('Top-level folders it keeps')
    const tag = (v: string) => fireEvent.paste(folders, { clipboardData: { getData: () => v } })
    tag('prod')
    // prod is Core's: refused before anything is sent.
    expect(await within(dialog).findByText('prod is already kept by another server')).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Add and make token' })).toBeDisabled()
    fireEvent.keyDown(folders, { key: 'Backspace' })
    tag('branch')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add and make token' }))
    await waitFor(() => expect(live.send).toHaveBeenCalledWith('POST', '/api/v1/secrets/servers',
      { name: 'Branch', mode: 'tunnel', url: '', tlsPin: '', folders: ['branch'] }))
    expect(await screen.findByText(/--token argus-secret-enrol-acme\.TOKEN/)).toBeInTheDocument()
  })
})
