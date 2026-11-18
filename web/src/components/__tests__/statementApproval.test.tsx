import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { useStatementApproval } from '~/components/StatementApproval'
import { renderWithProviders } from '~/test/render'
import { api } from '~/lib/api'

/**
 * Where a database's changes need approval, a statement that changes it waits for someone
 * else's approval: the person says why, the ask goes to Approvals, and the
 * statement is sent again with the approval once it is approved.
 */

const live = vi.hoisted(() => ({ askStatements: vi.fn(), statementAsk: vi.fn() }))
vi.mock('~/lib/live', async (orig) => ({ ...(await orig<typeof import('~/lib/live')>()), ...live }))

function Harness() {
  const { ask, dialog } = useStatementApproval('db1', 'orders')
  const [got, setGot] = useState<string>('none')
  return (
    <>
      <button onClick={() => void ask([{ sql: 'DELETE FROM orders WHERE id = $1', params: ['7'] }]).then((id) => setGot(id ?? 'null'))}>go</button>
      <span data-testid="got">{got}</span>
      {dialog}
    </>
  )
}

afterEach(() => { vi.useRealTimers(); vi.clearAllMocks() })

describe('Statement approval', () => {
  it('asks with a reason, waits, and hands back the approval', async () => {
    live.askStatements.mockResolvedValue({ requestId: 'r1', databaseId: 'db1', databaseName: 'orders', items: [], state: 'pending' })
    live.statementAsk.mockResolvedValueOnce({ requestId: 'r1', state: 'pending', items: [] })
      .mockResolvedValue({ requestId: 'r1', state: 'approved', decidedBy: 'budi', items: [] })
    renderWithProviders(<Harness />)
    fireEvent.click(screen.getByText('go'))
    expect(await screen.findByText(/Changes on orders need a second person's approval/)).toBeInTheDocument()
    expect(screen.getByText(/-- values: "7"/)).toBeInTheDocument()
    const askBtn = screen.getByRole('button', { name: 'Ask for approval' })
    expect(askBtn).toBeDisabled()
    fireEvent.change(screen.getByLabelText(/Why/), { target: { value: 'order 7 was imported twice by mistake' } })
    vi.useFakeTimers({ shouldAdvanceTime: true })
    fireEvent.click(askBtn)
    await waitFor(() => expect(live.askStatements).toHaveBeenCalledWith('db1', {
      items: [{ sql: 'DELETE FROM orders WHERE id = $1', params: ['7'] }], summary: undefined,
      justification: 'order 7 was imported twice by mistake',
    }))
    expect(await screen.findByText(/Waiting for someone to approve it/)).toBeInTheDocument()
    await act(async () => { await vi.advanceTimersByTimeAsync(3100) })
    await act(async () => { await vi.advanceTimersByTimeAsync(3100) })
    await waitFor(() => expect(screen.getByTestId('got').textContent).toBe('r1'))
  })

  it('says when it was denied, and nothing runs', async () => {
    live.askStatements.mockResolvedValue({ requestId: 'r2', state: 'pending', items: [] })
    live.statementAsk.mockResolvedValue({ requestId: 'r2', state: 'denied', decidedBy: 'budi', note: 'use the import fix instead', items: [] })
    renderWithProviders(<Harness />)
    fireEvent.click(screen.getByText('go'))
    fireEvent.change(await screen.findByLabelText(/Why/), { target: { value: 'order 7 was imported twice by mistake' } })
    vi.useFakeTimers({ shouldAdvanceTime: true })
    fireEvent.click(screen.getByRole('button', { name: 'Ask for approval' }))
    await screen.findByText(/Waiting for someone/)
    await act(async () => { await vi.advanceTimersByTimeAsync(3100) })
    expect(await screen.findByText(/budi denied it: use the import fix instead/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    await waitFor(() => expect(screen.getByTestId('got').textContent).toBe('null'))
  })

  it('offers approving your own ask only where the control plane marks it', async () => {
    live.askStatements.mockResolvedValue({ requestId: 'r3', state: 'pending', items: [] })
    live.statementAsk.mockResolvedValue({ requestId: 'r3', state: 'pending', items: [] })
    const one = vi.spyOn(api, 'request').mockResolvedValue({ id: 'r3', state: 'pending', approveOwn: true } as never)
    renderWithProviders(<Harness />)
    fireEvent.click(screen.getByText('go'))
    fireEvent.change(await screen.findByLabelText(/Why/), { target: { value: 'order 7 was imported twice by mistake' } })
    fireEvent.click(screen.getByRole('button', { name: 'Ask for approval' }))
    expect(await screen.findByRole('button', { name: 'Approve my own' })).toBeInTheDocument()
    expect(screen.getByText(/no need to go to Approvals/)).toBeInTheDocument()
    expect(one).toHaveBeenCalledWith('r3')
    one.mockRestore()
  })

  it('does not offer it when another editor could decide, whatever the roles say', async () => {
    live.askStatements.mockResolvedValue({ requestId: 'r4', state: 'pending', items: [] })
    live.statementAsk.mockResolvedValue({ requestId: 'r4', state: 'pending', items: [] })
    const one = vi.spyOn(api, 'request').mockResolvedValue({ id: 'r4', state: 'pending' } as never)
    renderWithProviders(<Harness />)
    fireEvent.click(screen.getByText('go'))
    fireEvent.change(await screen.findByLabelText(/Why/), { target: { value: 'order 7 was imported twice by mistake' } })
    fireEvent.click(screen.getByRole('button', { name: 'Ask for approval' }))
    await screen.findByText(/Waiting for someone to approve it in Approvals/)
    await waitFor(() => expect(one).toHaveBeenCalledWith('r4'))
    expect(screen.queryByRole('button', { name: 'Approve my own' })).not.toBeInTheDocument()
    one.mockRestore()
  })
})
