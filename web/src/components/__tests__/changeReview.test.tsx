import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChangeReview, ChangesTable } from '~/components/ChangeReview'
import { renderWithProviders } from '~/test/render'
import type { AccessRequest } from '~/types/domain'

/**
 * A change request on Approvals: the table shows how many rows each changes,
 * and the review shows the counts, the first rows and the statements, with
 * the decision for whoever may make it, as an editor too.
 */

const api = vi.hoisted(() => ({ decideRequest: vi.fn() }))
vi.mock('~/lib/api', async (orig) => {
  const m = await orig<typeof import('~/lib/api')>()
  return { ...m, api: { ...m.api, decideRequest: api.decideRequest } }
})

const ask = (over: Partial<AccessRequest> = {}): AccessRequest => ({
  id: 'r1', requesterId: 'u1', requesterEmail: 'ana@example.com', assetIds: [], assetHostnames: ['db-01'],
  principal: 'db:statement', justification: 'close out last week and remove two duplicates', durationMinutes: 60,
  state: 'pending', createdAt: new Date().toISOString(), decidedAt: null, decidedByEmail: null, decisionNote: null,
  expiresAt: null, breakGlass: false, asEditor: true,
  statement: {
    requestId: 'r1', databaseId: 'd1', databaseName: 'orders', databaseLabel: 'production', state: 'pending',
    items: [{ sql: "UPDATE orders SET status = 'paid' WHERE id < 20", rows: 12 }, { sql: 'DELETE FROM orders WHERE id IN (2, 7)', rows: 2 }],
    summary: {
      rows: 18, checked: true,
      tables: [{ name: 'public.orders', updated: 12, deleted: 2 }, { name: 'public.order_items', deleted: 4 }],
      preview: [{ change: 'update', columns: ['id', 'status'], rows: [['1042', 'paid']] }, { change: 'delete', columns: ['id', 'status'], rows: [['2', 'open']] }],
    },
  },
  ...over,
})

const notify = vi.hoisted(() => ({ notifyWarn: vi.fn(), notifyError: vi.fn() }))
vi.mock('~/lib/notify', async (orig) => ({ ...(await orig<typeof import('~/lib/notify')>()), ...notify }))

afterEach(() => vi.clearAllMocks())

describe('Change review', () => {
  it('lists each change with its impact, big ones in red', () => {
    const big = ask({ id: 'r2', statement: { ...ask().statement!, requestId: 'r2', summary: { rows: 48210, tables: [{ name: 'public.billing', deleted: 48210 }] } } })
    const open = vi.fn()
    renderWithProviders(<ChangesTable reqs={[ask(), big]} mine="bo@example.com" onOpen={open} />)
    expect(screen.getByText('18 rows · 2 tables')).toBeInTheDocument()
    expect(screen.getByText('48,210 rows')).toBeInTheDocument()
    expect(screen.getByText('2 waiting for you')).toBeInTheDocument()
    fireEvent.click(screen.getByText('18 rows · 2 tables'))
    expect(open).toHaveBeenCalledWith(expect.objectContaining({ id: 'r1' }))
  })

  it('shows what it changes and approves as an editor', async () => {
    api.decideRequest.mockResolvedValue(ask({ state: 'approved' }))
    const close = vi.fn()
    renderWithProviders(<ChangeReview request={ask()} canDecide onClose={close} />)
    expect(screen.getByText("Approve ana's change on orders?")).toBeInTheDocument()
    expect(screen.getByText(/you can approve as an editor of orders/)).toBeInTheDocument()
    expect(screen.getByText('rows deleted in orders, order_items')).toBeInTheDocument()
    expect(screen.getByText('1042')).toBeInTheDocument()
    expect(screen.getByText(/stops if it would change more than these 18 rows/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /Approve 2 statements, 18 rows/ }))
    await waitFor(() => expect(api.decideRequest).toHaveBeenCalledWith('r1', 'approved', ''))
    await waitFor(() => expect(close).toHaveBeenCalled())
  })

  it('says the values are for readers when they were held back, and offers no buttons on your own', () => {
    const own = ask({ asEditor: false, statement: { ...ask().statement!, summary: { rows: 3, checked: true, tables: [{ name: 'public.orders', updated: 3 }] } } })
    renderWithProviders(<ChangeReview request={own} canDecide={false} onClose={() => {}} />)
    expect(screen.getByText(/Only people who can read orders see the affected rows/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Approve/ })).toBeNull()
  })

  it('says the asker cancelled it, and offers nothing to decide, when opened from a stale alert', () => {
    const gone = ask({ state: 'cancelled', decidedAt: new Date().toISOString(), decidedByEmail: 'ana@example.com' })
    renderWithProviders(<ChangeReview request={gone} canDecide onClose={() => {}} me="bo@example.com" />)
    expect(screen.getByText('Cancelled by ana@example.com, who asked for it')).toBeInTheDocument()
    expect(screen.getByText('There is nothing left to decide.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Approve|Deny/ })).toBeNull()
  })

  it('keeps the review open and warns, rather than erroring, when it was settled meanwhile', async () => {
    api.decideRequest.mockRejectedValue(new Error('ana@example.com cancelled this request; there is nothing left to decide'))
    const close = vi.fn()
    renderWithProviders(<ChangeReview request={ask()} canDecide onClose={close} />)
    fireEvent.click(screen.getByRole('button', { name: /Approve 2 statements/ }))
    await waitFor(() => expect(notify.notifyWarn).toHaveBeenCalledWith('Nothing to decide', expect.stringContaining('cancelled')))
    expect(notify.notifyError).not.toHaveBeenCalled()
    expect(close).not.toHaveBeenCalled()
  })
})
