import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { assets, currentUser } from '~/lib/seed'
import type { User } from '~/types/domain'

/**
 * Servers are an administrator's to add and give out, and everyone else's to
 * be given.
 *
 * Both halves matter. A console that offers a member an Add server button the
 * control plane will refuse is lying about what they can do; a console that
 * shows a member every host in the fleet has handed them the map whether or
 * not the buttons work.
 */

function signedInAs(role: User['role']) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role })
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('who may add and manage servers', () => {
  it('offers an administrator Add server, and each server’s page', async () => {
    signedInAs('admin')
    await renderRoute('/assets')
    expect(await screen.findByRole('link', { name: /add server/i })).toHaveAttribute('href', '/assets/add')
    expect((await screen.findAllByRole('link', { name: /^Manage / })).length).toBeGreaterThan(0)
  }, 15_000)

  it('gives a member Connect and nothing to manage', async () => {
    signedInAs('operator')
    await renderRoute('/assets')
    expect(await screen.findByRole('textbox', { name: 'Find a server' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /add server/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /^Manage / })).not.toBeInTheDocument()
  })

  // An auditor reads the fleet; nothing on the page is theirs to change.
  it('gives a viewer the servers without the buttons that change them', async () => {
    signedInAs('auditor')
    await renderRoute('/assets')
    expect(await screen.findByText(/every server in argus/i)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /add server/i })).not.toBeInTheDocument()
  })
})

describe('a server’s page', () => {
  const host = assets.find((a) => a.principals.length > 0)!

  it('says who can connect, with admins always first', async () => {
    signedInAs('admin')
    await renderRoute(`/assets/${host.id}`)
    expect(await screen.findByText('Who can connect')).toBeInTheDocument()
    expect(screen.getByText('Always, as any account')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /add team or person/i })).toBeEnabled()
    expect(screen.getByRole('button', { name: /^remove$/i })).toBeInTheDocument()
  }, 15_000)

  it('shows a viewer who can connect without the buttons', async () => {
    signedInAs('auditor')
    await renderRoute(`/assets/${host.id}`)
    expect(await screen.findByText('Who can connect')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /add team or person/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^remove$/i })).not.toBeInTheDocument()
  })
})

describe('connecting with nothing assigned', () => {
  it('says so, rather than showing an empty search box', async () => {
    signedInAs('operator')
    vi.spyOn(api, 'assets').mockResolvedValue([])
    await renderRoute('/assets')
    expect(await screen.findByText(/no servers have been given to you yet/i)).toBeInTheDocument()
    expect(screen.getByText(/ask your admin/i)).toBeInTheDocument()
  })
})
