import { fireEvent, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { currentUser } from '~/lib/seed'
import type { User } from '~/types/domain'

/**
 * Teams: a server is given to a team once, and joining the team is all a new
 * person needs. The page is about who is in which team; servers are given out
 * on each server's own page.
 */

function signedInAs(role: User['role']) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role })
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('teams', () => {
  it('lists each team with its people and servers', async () => {
    signedInAs('admin')
    await renderRoute('/teams')
    expect(await screen.findByText('Ops')).toBeInTheDocument()
    expect(screen.getByText('Developers')).toBeInTheDocument()
    expect(screen.getAllByText(/^\d+ (person|people) · \d+ servers?$/).length).toBe(2)
  }, 15_000)

  it('makes a team from a name', async () => {
    signedInAs('admin')
    await renderRoute('/teams')
    fireEvent.click(await screen.findByRole('button', { name: /new team/i }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByRole('textbox', { name: /name/i }), { target: { value: 'Night shift' } })
    fireEvent.click(within(dialog).getByRole('button', { name: /make team/i }))
    expect(await screen.findByText('Night shift')).toBeInTheDocument()
  })

  it('gives a viewer the teams without the buttons that change them', async () => {
    signedInAs('auditor')
    await renderRoute('/teams')
    expect(await screen.findByText('Ops')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /new team/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^Remove .* from Ops$/ })).not.toBeInTheDocument()
  })
})

describe('asking for access from a server you cannot open', () => {
  it('opens the request with that server already picked', async () => {
    signedInAs('operator')
    await renderRoute('/approvals?ask=pay-01.prod.northwind.id&as=root')
    const dialog = await screen.findByRole('dialog', { name: /ask for access/i })
    expect(within(dialog).getByText('pay-01.prod.northwind.id')).toBeInTheDocument()
    expect(within(dialog).getByRole('radio', { name: /root/ })).toBeChecked()
  })
})
