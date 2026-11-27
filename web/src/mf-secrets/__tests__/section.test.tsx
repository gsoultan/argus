import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import { renderWithProviders } from '~/test/render'
import { b64urlDecode, readAAD, sealTo, stdB64Encode } from '../seal'

const live = vi.hoisted(() => ({ get: vi.fn(), send: vi.fn() }))
vi.mock('~/lib/live', async (orig) => ({ ...(await orig<typeof import('~/lib/live')>()), ...live }))

const { SectionSlot } = await import('~/mf/host')

const secret = {
  name: 'prod/db', type: 'text', versions: [{ n: 3, createdAt: '2026-11-01T00:00:00Z', createdBy: 'a@x' }], labels: { current: 3 },
  createdAt: '2026-11-01T00:00:00Z', createdBy: 'a@x', updatedAt: '2026-11-01T00:00:00Z', server: 'Vault', serverId: 's1',
}

function mount(role = 'operator', tab?: string) {
  return renderWithProviders(
    <SectionSlot name="secrets" fallback={(e) => <p>failed: {e.message}</p>}
      context={{ person: { email: 'me@x', name: 'Me', role }, tab, setTab: () => {}, navigate: () => {} }} />,
  )
}

beforeEach(() => {
  live.get.mockReset()
  live.send.mockReset()
  live.get.mockImplementation(async (path: string) => {
    if (path.startsWith('/api/v1/secrets?')) return { secrets: [secret], unreachable: [{ server: 'Edge', error: 'Edge is not connected to this argus-control' }] }
    if (path === '/api/v1/secrets/requests') return []
    throw new Error(`unexpected ${path}`)
  })
})

describe('the secrets section, mounted by the console', () => {
  it('lists what the person may see, and names a server that did not answer', async () => {
    mount()
    expect(await screen.findByText('prod/db')).toBeInTheDocument()
    expect(screen.getByText('Edge did not answer')).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1, name: 'Secrets' })).toBeInTheDocument()
    // Rules and app tokens are for admins.
    expect(screen.queryByRole('tab', { name: 'Rules' })).toBeNull()
  })

  it('opens a value sealed to this tab, and only after a reason', async () => {
    live.send.mockImplementation(async (_m: string, path: string, body: { name: string; recipient: string; reason: string }) => {
      expect(path).toBe('/api/v1/secrets/read')
      expect(body.reason).toBe('deploy')
      const sealed = await sealTo(b64urlDecode(body.recipient), new TextEncoder().encode('hunter2'), readAAD(body.name, 3, ''))
      return { name: body.name, version: 3, sealed: stdB64Encode(sealed) }
    })
    mount()
    fireEvent.click(await screen.findByRole('button', { name: 'Open' }))
    const dialog = await screen.findByRole('dialog')
    const show = within(dialog).getByRole('button', { name: 'Show value' })
    expect(show).toBeDisabled()
    fireEvent.change(within(dialog).getByLabelText(/Why do you need it/), { target: { value: 'deploy' } })
    fireEvent.click(show)
    expect(await within(dialog).findByText('hunter2')).toBeInTheDocument()
  })

  it('refuses an answer sealed for another secret', async () => {
    live.send.mockImplementation(async (_m: string, _p: string, body: { recipient: string }) => {
      const sealed = await sealTo(b64urlDecode(body.recipient), new TextEncoder().encode('other'), readAAD('prod/other', 3, ''))
      return { name: 'prod/db', version: 3, sealed: stdB64Encode(sealed) }
    })
    mount()
    fireEvent.click(await screen.findByRole('button', { name: 'Open' }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByLabelText(/Why do you need it/), { target: { value: 'deploy' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Show value' }))
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Show value' })).not.toHaveAttribute('data-loading'))
    expect(within(dialog).queryByText('other')).toBeNull()
  })

  it('offers to ask when the rule needs an approval', async () => {
    live.send.mockRejectedValueOnce(new Error('reading this secret needs an approval first'))
    mount()
    fireEvent.click(await screen.findByRole('button', { name: 'Open' }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByLabelText(/Why do you need it/), { target: { value: 'incident 42' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Show value' }))
    expect(await within(dialog).findByText('This secret needs an approval')).toBeInTheDocument()
    live.send.mockResolvedValueOnce({ id: 'r1' })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Ask for approval' }))
    await waitFor(() => expect(live.send).toHaveBeenLastCalledWith('POST', '/api/v1/secrets/requests', { name: 'prod/db', reason: 'incident 42', minutes: 60 }))
  })

  it('shows admins the rules', async () => {
    live.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/secrets/rules') return [{ id: 'r', path: 'prod/', subjects: [{ kind: 'team', id: 'ops' }], can: ['read'], needsApproval: true, note: '', createdBy: 'a', createdAt: '' }]
      return { secrets: [], unreachable: [] }
    })
    mount('admin', 'rules')
    expect(await screen.findByText('Team: ops')).toBeInTheDocument()
    expect(screen.getByText('Needs one')).toBeInTheDocument()
  })
})
