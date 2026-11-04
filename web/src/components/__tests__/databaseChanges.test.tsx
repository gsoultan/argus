import { fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ChangesPanel } from '~/components/DatabaseChanges'
import { renderWithProviders } from '~/test/render'
import type { DataChange } from '~/lib/live'

/** The Data changes log: who changed what, newest first, older on request. */

const live = vi.hoisted(() => ({ dataChanges: vi.fn(), isConfigured: () => true }))
vi.mock('~/lib/live', async (orig) => ({ ...(await orig<typeof import('~/lib/live')>()), ...live }))

const change = (seq: number, over: Partial<DataChange> = {}): DataChange => ({
  seq, at: '2026-11-04T06:00:00Z', sessionId: 's1', actor: 'ana@example.com', verb: 'update', objects: ['public.orders'],
  rows: 3, tag: 'UPDATE 3', sql: 'UPDATE public.orders SET paid = true WHERE id = $1', ...over,
})

describe('Data changes', () => {
  it('lists the changes and pages back', async () => {
    live.dataChanges.mockResolvedValueOnce([
      change(200, { verb: 'delete', rows: 1, tag: 'DELETE 1', approvalId: 'r1' }),
      ...Array.from({ length: 99 }, (_, i) => change(199 - i)),
    ]).mockResolvedValueOnce([change(5, { verb: 'drop', rows: -1, objects: ['public.old'] })])
    renderWithProviders(<ChangesPanel databaseId="d1" narrow={false} onBack={vi.fn()} />)
    expect(await screen.findByText('Deleted')).toBeInTheDocument()
    expect(screen.getByText('approved')).toBeInTheDocument()
    expect(screen.getAllByText('3 rows')).toHaveLength(99)
    fireEvent.click(screen.getByRole('button', { name: 'Older changes' }))
    await waitFor(() => expect(live.dataChanges).toHaveBeenLastCalledWith('d1', 101))
    expect(await screen.findByText('Dropped')).toBeInTheDocument()
    expect(screen.getByText('public.old')).toBeInTheDocument()
  })

  it('says when nothing has changed', async () => {
    live.dataChanges.mockResolvedValueOnce([])
    renderWithProviders(<ChangesPanel databaseId="d1" narrow onBack={vi.fn()} />)
    expect(await screen.findByText(/No data has been changed/)).toBeInTheDocument()
  })
})
