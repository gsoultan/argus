import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { currentUser } from '~/lib/seed'
import type { DirectLogin, User } from '~/types/domain'

/**
 * Direct SSH logins held for approval, on Access requests. Someone is waiting
 * at a terminal for each one, so it must show, say who and from where, and let
 * only the roles the control plane accepts decide.
 */

const waiting = (o: Partial<DirectLogin> = {}): DirectLogin => ({
  id: 'dl-1',
  hostname: 'web-01.prod.northwind.id',
  principal: 'ops',
  clientAddr: '198.51.100.7',
  command: '',
  state: 'pending',
  createdAt: new Date().toISOString(),
  expiresAt: new Date(Date.now() + 90_000).toISOString(),
  decidedAt: null,
  decidedByEmail: null,
  decisionNote: null,
  ...o,
})

function signedInAs(role: User['role'], logins: DirectLogin[]) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role })
  vi.spyOn(api, 'requests').mockResolvedValue([])
  vi.spyOn(api, 'directLogins').mockResolvedValue(logins)
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('direct SSH logins waiting for approval', () => {
  it('shows who is waiting and sends the approval with its note', async () => {
    signedInAs('admin', [waiting()])
    const decide = vi.spyOn(api, 'decideDirectLogin').mockResolvedValue(
      waiting({ state: 'approved' }),
    )
    await renderRoute('/requests')
    const card = (await screen.findByText(/198\.51\.100\.7/)).closest('.argus-direct-login') as HTMLElement
    expect(card).toHaveTextContent('web-01.prod.northwind.id')
    expect(card).toHaveTextContent(/interactive shell/)

    fireEvent.change(within(card).getByPlaceholderText(/note for the audit log/i), {
      target: { value: 'on call' },
    })
    fireEvent.click(within(card).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(decide).toHaveBeenCalledWith('dl-1', true, 'on call'))
  }, 15_000)

  it('leaves out logins that are decided or out of time', async () => {
    signedInAs('admin', [
      waiting({ id: 'a', state: 'denied' }),
      waiting({ id: 'b', expiresAt: new Date(Date.now() - 1_000).toISOString() }),
    ])
    await renderRoute('/requests')
    await screen.findByText(/nothing waiting/i)
    expect(screen.queryByText(/direct ssh logins waiting/i)).toBeNull()
  })

  it('shows an auditor the queue without the buttons', async () => {
    signedInAs('auditor', [waiting()])
    await renderRoute('/requests')
    await screen.findByText(/198\.51\.100\.7/)
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull()
  })
})
