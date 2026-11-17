import { fireEvent, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { sharedPeopleApi } from '~/lib/sharedPeople'
import { currentUser } from '~/lib/seed'

/**
 * Adding someone who already works in another organization: picked from the
 * organizations the admin runs, added in one step, and told plainly that what
 * they have elsewhere does not carry over.
 */

function signedInToSecondOrg() {
  vi.spyOn(api, 'me').mockResolvedValue({
    ...currentUser, role: 'owner', multiOrg: true,
    org: { id: 't2', slug: 'mulford-sg', name: 'Mulford Singapore', default: false },
    orgs: [
      { id: 'default', slug: 'default', name: 'Mulford Indonesia', default: true, role: 'owner', home: true },
      { id: 't2', slug: 'mulford-sg', name: 'Mulford Singapore', default: false, role: 'owner', home: false },
    ],
  })
}

describe('add a person from another organization', () => {
  afterEach(() => vi.restoreAllMocks())

  it('picks them from the organizations you run and adds them in one step', async () => {
    signedInToSecondOrg()
    vi.spyOn(sharedPeopleApi, 'fromOrganizations').mockResolvedValue({ organizations: [{
      slug: 'default', name: 'Mulford Indonesia', yourRole: 'owner',
      people: [{ email: 'citra@mulford.id', displayName: 'Citra Lestari', role: 'operator' }],
    }] })
    const add = vi.spyOn(api, 'addSharedAccount').mockResolvedValue({ team: '' })
    await renderRoute('/teams?tab=people')

    fireEvent.click(await screen.findByRole('button', { name: /add person/i }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByText(/from another organization/i))
    fireEvent.click(await within(dialog).findByText('Citra Lestari'))
    fireEvent.click(within(dialog).getByRole('button', { name: /add citra lestari/i }))

    expect(await screen.findByText(/citra lestari is in mulford singapore/i)).toBeInTheDocument()
    expect(add.mock.calls[0]![0]).toEqual(expect.objectContaining({
      email: 'citra@mulford.id', role: 'operator', homeOrg: 'default',
    }))
    // The step that was missing: access here is given here, for every kind.
    expect(screen.getByText(/does not carry over/i)).toBeInTheDocument()
    for (const k of [/^servers$/i, /^databases$/i, /^brokers$/i, /^cloud$/i]) {
      expect(await screen.findByRole('button', { name: k })).toBeInTheDocument()
    }
  }, 20_000)

  it('without an organization you run, asks for an email and says what their organization must do', async () => {
    signedInToSecondOrg()
    vi.spyOn(sharedPeopleApi, 'fromOrganizations').mockResolvedValue({ organizations: [] })
    await renderRoute('/teams?tab=people')
    fireEvent.click(await screen.findByRole('button', { name: /add person/i }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByText(/from another organization/i))
    expect(await within(dialog).findByText(/has to have allowed mulford singapore first/i)).toBeInTheDocument()
    expect(within(dialog).getByLabelText(/^email$/i)).toBeInTheDocument()
  }, 20_000)
})
