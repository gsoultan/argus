import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { renderWithProviders } from '~/test/render'
import { MySshKeyModal } from '~/components/MySshKey'
import { api } from '~/lib/api'
import { currentUser, users } from '~/lib/seed'
import type { User } from '~/types/domain'

/**
 * Personal server accounts: a team is given a server with an account of each
 * person's own, and chosen teams may SSH straight to it. People removed from
 * Argus are listed with who removed them.
 */

function signedInAs(role: User['role']) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role })
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('giving a team its own accounts on a server', () => {
  it('starts on own accounts and sends direct SSH and sudo as picked', async () => {
    signedInAs('admin')
    const set = vi.spyOn(api, 'setTeamAssignment')
    const asset = (await api.assets())[0]!
    await renderRoute(`/assets/${asset.id}`)

    fireEvent.click(await screen.findByRole('button', { name: /add team or person/i }))
    const dialog = await screen.findByRole('dialog')
    const team = within(dialog).getByLabelText(/^team$/i)
    fireEvent.focus(team)
    fireEvent.change(team, { target: { value: 'Ops' } })
    fireEvent.click(await screen.findByText(/^Ops \(/))

    const own = within(dialog).getByRole('switch', { name: /their own account/i })
    expect(own).toBeChecked()
    fireEvent.click(within(dialog).getByRole('switch', { name: /sudo without a password/i }))
    fireEvent.click(within(dialog).getByRole('radio', { name: /^yes$/i }))
    fireEvent.click(within(dialog).getByRole('button', { name: /give access/i }))

    await waitFor(() => expect(set).toHaveBeenCalled())
    expect(set.mock.calls[0]![2]).toEqual({
      principals: [], personalAccounts: true, sudo: true, directSsh: 'allow',
    })
  }, 20_000)

  it('offers sudo and direct SSH only with own accounts', async () => {
    signedInAs('admin')
    const asset = (await api.assets())[0]!
    await renderRoute(`/assets/${asset.id}`)
    fireEvent.click(await screen.findByRole('button', { name: /add team or person/i }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('switch', { name: /their own account/i }))
    expect(within(dialog).queryByRole('switch', { name: /sudo/i })).not.toBeInTheDocument()
    expect(within(dialog).queryByRole('radio', { name: /ask an admin/i })).not.toBeInTheDocument()
  }, 20_000)
})

describe('people who were removed', () => {
  it('lists them apart, with when and by whom', async () => {
    signedInAs('admin')
    const gone = users.find((u) => u.email !== currentUser.email)!
    vi.spyOn(api, 'users').mockResolvedValue(users.map((u) => u.email === gone.email
      ? { ...u, disabled: true, disabledAt: new Date().toISOString(), disabledBy: 'boss@northwind.id' }
      : u))
    await renderRoute('/teams?tab=people')

    expect(await screen.findByRole('radio', { name: /removed \(1\)/i })).toBeInTheDocument()
    expect(screen.queryByText(gone.email)).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('radio', { name: /removed \(1\)/i }))
    expect(await screen.findByText(gone.email)).toBeInTheDocument()
    expect(screen.getByText(/by boss@northwind\.id/)).toBeInTheDocument()
  }, 20_000)
})

describe('a key Argus makes', () => {
  it('sends only the public half and hands over the private half once', async () => {
    const set = vi.spyOn(api, 'setMySshKey')
    const saved = vi.fn<(blob: Blob) => void>()
    vi.spyOn(URL, 'createObjectURL').mockImplementation((b) => {
      saved(b as Blob)
      return 'blob:key'
    })
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})

    renderWithProviders(<MySshKeyModal opened onClose={() => {}} />)
    fireEvent.click(await screen.findByRole('button', { name: /generate a key for me/i }))

    expect(await screen.findByText(/your new ssh key/i)).toBeInTheDocument()
    expect(set).toHaveBeenCalledTimes(1)
    const sent = set.mock.calls[0]![0]
    expect(sent).toMatch(/^ssh-ed25519 AAAA\S+ \S+@argus$/)
    expect(sent).not.toMatch(/PRIVATE/)

    expect(saved).toHaveBeenCalledTimes(1)
    const file = await saved.mock.calls[0]![0].text()
    expect(file).toMatch(/^-----BEGIN OPENSSH PRIVATE KEY-----\n/)
  }, 20_000)
})

describe('one person, on one server', () => {
  it('gives them their own account, sudo and direct SSH, and says which team it replaces', async () => {
    signedInAs('admin')
    const asset = (await api.assets())[0]!
    const budi = users.find((u) => u.email !== currentUser.email && u.role !== 'admin' && u.role !== 'owner')!
    vi.spyOn(api, 'teams').mockResolvedValue([{
      id: 'team-ops', name: 'Ops', members: [budi.email], servers: [],
      createdBy: currentUser.email, createdAt: new Date().toISOString(),
    }])
    vi.spyOn(api, 'teamAssignments').mockResolvedValue([{
      assetId: asset.id, hostname: asset.hostname, teamId: 'team-ops', teamName: 'Ops', principals: [],
      personalAccounts: true, sudo: false, directSsh: 'approve',
      grantedBy: currentUser.email, grantedAt: new Date().toISOString(),
    }])
    const set = vi.spyOn(api, 'setAssignment')
    await renderRoute(`/assets/${asset.id}`)

    fireEvent.click(await screen.findByRole('button', { name: /add team or person/i }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('radio', { name: /one person/i }))
    const person = within(dialog).getByLabelText(/^person$/i)
    fireEvent.focus(person)
    fireEvent.change(person, { target: { value: budi.email } })
    fireEvent.click(await screen.findByText(new RegExp(budi.email.replace(/[.+]/g, '\\$&'))))

    expect(within(dialog).getByRole('switch', { name: /their own account/i })).toBeChecked()
    expect(await within(dialog).findByText(/this replaces what ops/i)).toBeInTheDocument()
    fireEvent.click(within(dialog).getByRole('switch', { name: /sudo without a password/i }))
    fireEvent.click(within(dialog).getByRole('radio', { name: /^yes$/i }))
    fireEvent.click(within(dialog).getByRole('button', { name: /give access/i }))

    await waitFor(() => expect(set).toHaveBeenCalled())
    expect(set.mock.calls[0]!.slice(1)).toEqual([
      budi.email, [], { personalAccounts: true, sudo: true, directSsh: 'allow' },
    ])
  }, 20_000)

  it('lists what someone can do on each server, and where it comes from', async () => {
    signedInAs('admin')
    const citra = users.find((u) => u.email !== currentUser.email)!
    vi.spyOn(api, 'personServers').mockResolvedValue([
      { assetId: 'a1', hostname: 'prod-01', principals: [], personalAccount: true, sudo: false,
        directSsh: 'off', source: 'own', teams: ['Deploy'], ownSetting: true },
      { assetId: 'a2', hostname: 'web-01', principals: [], personalAccount: true, sudo: true,
        directSsh: 'allow', source: 'teams', teams: ['Deploy'], ownSetting: false },
    ])
    await renderRoute('/teams?tab=people')

    const row = (await screen.findByText(citra.email)).closest('tr')!
    fireEvent.click(within(row).getByRole('button', { name: /^servers$/i }))
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('prod-01')).toBeInTheDocument()
    expect(within(dialog).getByText(/set for them/i)).toBeInTheDocument()
    expect(within(dialog).getByText(/from Deploy/)).toBeInTheDocument()
    expect(within(dialog).getByText(/own account with sudo · direct SSH/)).toBeInTheDocument()
  }, 20_000)
})
