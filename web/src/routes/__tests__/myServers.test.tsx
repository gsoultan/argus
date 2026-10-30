import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { EPOCH, assets, currentUser } from '~/lib/seed'
import { reachOf } from '~/components/myservers'
import type { AccessRequest, Asset, User } from '~/types/domain'

/**
 * My servers: what someone who was given servers sees first.
 *
 * The list is whatever the control plane sent -- it narrows it to assignments
 * on the server -- so these assert the page shows that list faithfully, never
 * claims a server works when Argus would refuse it, and always says what to do
 * when there is nothing to show.
 */

const ssh = assets.find((a) => a.protocol !== 'rdp')!
const host = (name: string, o: Partial<Asset> = {}): Asset => ({
  ...ssh,
  id: `id-${name}`,
  hostname: `${name}.prod.northwind.id`,
  health: 'reachable',
  hostKeyState: 'pinned',
  tunnelConnected: false,
  principals: ['ops', 'deploy'],
  ...o,
})

const MINE = [
  host('web-01'),
  host('db-02', { health: 'unreachable' }),
  host('pay-03', { hostKeyState: 'changed' }),
]

function signedInAs(role: User['role'], list: Asset[] = MINE, approved: AccessRequest[] = []) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role })
  vi.spyOn(api, 'assets').mockResolvedValue(list)
  vi.spyOn(api, 'requests').mockResolvedValue(approved)
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('the servers someone can reach', () => {
  it('lists exactly what the control plane sent, with a way onto each', async () => {
    signedInAs('operator')
    await renderRoute('/mine')
    const cards = await screen.findAllByRole('heading', { level: 3 })
    expect(cards.map((c) => c.textContent)).toEqual(['db-02', 'pay-03', 'web-01'])
    // Clicking an account opens the session, rather than a form that asks again.
    const asDeploy = screen.getByRole('link', { name: 'Open web-01 as deploy' })
    expect(asDeploy.getAttribute('href')).toBe('/connect?host=web-01.prod.northwind.id&as=deploy&open=true')
    // The card itself opens the first account.
    const open = screen.getByRole('link', { name: 'Open web-01' })
    expect(open).toHaveClass('argus-stretch')
    expect(open.getAttribute('href')).toContain('open=true')
  }, 15_000)

  // Argus refuses SSH to a host whose key no longer matches its pin, so a
  // Connect button there would be a control that cannot work.
  it('does not offer to connect to a server Argus will refuse', async () => {
    signedInAs('operator')
    await renderRoute('/mine')
    const card = (await screen.findByRole('heading', { name: 'pay-03' })).closest('.argus-server')!
    expect(within(card as HTMLElement).getByRole('button', { name: 'Open' })).toBeDisabled()
    expect(card).toHaveTextContent(/identity changed/i)
    expect(screen.getByRole('link', { name: 'Open web-01' })).toBeInTheDocument()
  })

  it('narrows by name and by status, and undoes it in one press', async () => {
    signedInAs('operator')
    await renderRoute('/mine')
    await screen.findByRole('heading', { name: 'web-01' })

    fireEvent.change(screen.getByRole('textbox', { name: 'Find a server' }), { target: { value: 'nothing-like-this' } })
    expect(await screen.findByText('No server matches.')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Show all servers' }))
    expect(await screen.findAllByRole('heading', { level: 3 })).toHaveLength(3)

    fireEvent.click(screen.getByText(/^Offline \(/))
    await waitFor(() =>
      expect(screen.getAllByRole('heading', { level: 3 }).map((c) => c.textContent)).toEqual(['db-02', 'pay-03']),
    )
  })

  // The request form can only offer servers the control plane sent, which here
  // is none, so the page names the person who can help instead of a form that
  // would open empty.
  it('tells someone with nothing assigned who can give them access', async () => {
    signedInAs('operator', [])
    await renderRoute('/mine')
    expect(await screen.findByText(/no servers have been given to you yet/i)).toBeInTheDocument()
    expect(screen.getByText(/ask your admin/i)).toBeInTheDocument()
  })

  it('says an administrator is seeing every server, not their assignments', async () => {
    signedInAs('admin')
    await renderRoute('/mine')
    expect(await screen.findByText(/you can open every server/i)).toBeInTheDocument()
  })
})

describe('temporary access', () => {
  const grant = (o: Partial<AccessRequest>): AccessRequest => ({
    id: `r-${Math.random()}`,
    requesterId: currentUser.id,
    assetIds: [],
    justification: 'deploy hotfix',
    durationMinutes: 60,
    createdAt: new Date(EPOCH - 15 * 60_000).toISOString(),
    decidedAt: new Date(EPOCH - 10 * 60_000).toISOString(),
    decidedByEmail: 'tomas.lie@northwind.id',
    decisionNote: null,
    breakGlass: false,
    state: 'approved',
    requesterEmail: currentUser.email,
    assetHostnames: ['batch-09.prod.northwind.id'],
    principal: 'root',
    expiresAt: new Date(EPOCH + 45 * 60_000).toISOString(),
    ...o,
  })

  it('shows my unexpired grants and nobody else’s', async () => {
    signedInAs('operator', MINE, [
      grant({}),
      grant({ requesterEmail: 'someone.else@northwind.id', assetHostnames: ['theirs-01'] }),
      grant({ assetHostnames: ['old-01'], expiresAt: new Date(EPOCH - 60_000).toISOString() }),
      // Already theirs by assignment, so a second card would add nothing.
      grant({ assetHostnames: ['web-01.prod.northwind.id'], principal: 'ops' }),
    ])
    await renderRoute('/mine')
    expect(await screen.findByRole('heading', { name: 'Temporary access' })).toBeInTheDocument()
    const link = screen.getByRole('link', { name: 'Open batch-09 as root' })
    expect(link.getAttribute('href')).toBe('/connect?host=batch-09.prod.northwind.id&as=root&open=true')
    expect(screen.queryByText('theirs-01')).not.toBeInTheDocument()
    expect(screen.queryByText('old-01')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Open web-01 as ops' })).toBeInTheDocument()
    expect(screen.getAllByRole('heading', { name: 'web-01' })).toHaveLength(1)
  })
})

describe('asking for more', () => {
  const pending = (email: string): AccessRequest => ({
    id: `r-${email}`,
    requesterId: 'u',
    assetIds: [],
    justification: 'rotating the replica after the failover',
    durationMinutes: 60,
    createdAt: new Date(EPOCH - 5 * 60_000).toISOString(),
    decidedAt: null,
    decidedByEmail: null,
    decisionNote: null,
    breakGlass: false,
    state: 'pending',
    requesterEmail: email,
    assetHostnames: ['batch-09.prod.northwind.id'],
    principal: 'root',
    expiresAt: null,
  })

  // The API refuses an operator's decision, so an Approve button there would
  // be a control that cannot work.
  it('shows an operator their own requests, without buttons they cannot use', async () => {
    signedInAs('operator', MINE, [pending(currentUser.email)])
    await renderRoute('/mine')
    fireEvent.click(await screen.findByRole('link', { name: /request more access/i }))
    expect(await screen.findByRole('heading', { name: 'My access requests' })).toBeInTheDocument()
    expect(await screen.findByText('Waiting for an approver.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /approve/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /deny/i })).not.toBeInTheDocument()
  })

  it('lets an approver decide someone else’s request but not their own', async () => {
    signedInAs('approver', MINE, [pending('someone.else@northwind.id'), pending(currentUser.email)])
    await renderRoute('/requests')
    expect(await screen.findByRole('heading', { name: 'Access requests' })).toBeInTheDocument()
    expect(await screen.findAllByRole('button', { name: /approve/i })).toHaveLength(1)
    expect(screen.getByText('Waiting for an approver.')).toBeInTheDocument()
  })
})

describe('the menu follows the role', () => {
  // Asked for in so many words: an ordinary user's console is their servers,
  // and nothing else.
  it('shows an operator Servers and nothing else', async () => {
    signedInAs('operator')
    await renderRoute('/mine')
    const nav = await screen.findByRole('navigation')
    await within(nav).findByRole('link', { name: /^servers/i })
    expect(within(nav).getAllByRole('link')).toHaveLength(1)
    expect(within(nav).queryByRole('button', { name: /admin/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /connect to a server/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /search/i })).not.toBeInTheDocument()
    // The inventory view is the fleet's; an operator gets the cards alone.
    expect(screen.queryByRole('radio', { name: /manage/i })).not.toBeInTheDocument()
  })

  // The request queue is an approver's job, so it stays theirs.
  it('keeps Access requests for an approver', async () => {
    signedInAs('approver')
    await renderRoute('/mine')
    const nav = await screen.findByRole('navigation')
    await within(nav).findByRole('link', { name: /access requests/i })
    expect(within(nav).getAllByRole('link')).toHaveLength(2)
  })

  it('shows an administrator all of it', async () => {
    signedInAs('admin')
    await renderRoute('/mine')
    const nav = await screen.findByRole('navigation')
    expect(await within(nav).findByRole('link', { name: /^Overview/ })).toBeInTheDocument()
    expect(within(nav).getByRole('link', { name: /^Servers/ })).toBeInTheDocument()
    fireEvent.click(within(nav).getByRole('button', { name: /admin/i }))
    expect(within(nav).getByRole('link', { name: /users & roles/i })).toBeInTheDocument()
  })

  // People and roles are an administrator's page. An auditor reads the fleet
  // but has nothing to do there.
  it('keeps Users & roles from an auditor, in the menu and at its address', async () => {
    signedInAs('auditor')
    await renderRoute('/mine')
    const nav = await screen.findByRole('navigation')
    fireEvent.click(await within(nav).findByRole('button', { name: /admin/i }))
    expect(within(nav).getByRole('link', { name: /audit log/i })).toBeInTheDocument()
    expect(within(nav).queryByRole('link', { name: /users & roles/i })).not.toBeInTheDocument()
  })

  it.each([
    ['/', 'operator'], ['/assets', 'operator'], ['/assets/add', 'auditor'], ['/sessions', 'approver'],
    ['/coverage', 'operator'], ['/audit', 'operator'], ['/settings', 'operator'], ['/users', 'auditor'],
  ] as const)('sends %s to Servers for an %s', async (path, role) => {
    signedInAs(role)
    await renderRoute(path)
    expect(await screen.findByRole('heading', { name: 'Servers' })).toBeInTheDocument()
  })
})

describe('connecting from a card', () => {
  it('arrives with the account already chosen', async () => {
    signedInAs('operator')
    await renderRoute('/connect?host=web-01.prod.northwind.id&as=deploy')
    expect(await screen.findByRole('radio', { name: 'deploy' })).toBeChecked()
  })

  // Only a preselection: an account the server does not list for this person
  // falls back to one it does, rather than being sent to the control plane.
  it('drops an account the server does not offer', async () => {
    signedInAs('operator')
    await renderRoute('/connect?host=web-01.prod.northwind.id&as=root')
    expect(await screen.findByRole('radio', { name: 'ops' })).toBeChecked()
  })
})

describe('reachOf', () => {
  it('counts a connected agent as online even when the gateway cannot reach the host', () => {
    expect(reachOf(host('x', { health: 'unreachable', tunnelConnected: true }))).toBe('online')
  })
  it('puts a changed key ahead of everything else for SSH', () => {
    expect(reachOf(host('x', { hostKeyState: 'changed', tunnelConnected: true }))).toBe('blocked')
  })
  it('does not apply SSH host keys to Remote Desktop', () => {
    expect(reachOf(host('x', { protocol: 'rdp', hostKeyState: 'changed' }))).toBe('online')
  })
})
