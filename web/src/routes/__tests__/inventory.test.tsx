import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { currentUser } from '~/lib/seed'
import type { User } from '~/types/domain'

/**
 * The inventory is an administrator's to edit, and an operator's to be given.
 *
 * Both halves matter. A console that offers an operator an "Add by hand" button
 * the control plane will refuse is lying about what they can do; a console that
 * shows an operator every host in the fleet has handed them the map whether or
 * not the buttons work.
 */

function signedInAs(role: User['role']) {
  vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role })
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('who may edit the inventory', () => {
  // The longer timeout is for the first render in this file, which pays for
  // loading the Servers page and Mantine's modal code; on CI that alone took
  // most of the default five seconds.
  it('offers an administrator a way to add a host', async () => {
    signedInAs('admin')
    await renderRoute('/assets')
    expect(await screen.findByRole('button', { name: /add by hand/i })).toBeInTheDocument()
  }, 15_000)

  // The inventory is not an operator's page at all: their console is My
  // servers, so a typed address lands there instead of on a read-only copy.
  it('sends an operator to their own servers instead', async () => {
    signedInAs('operator')
    await renderRoute('/assets')
    expect(await screen.findByRole('heading', { name: 'My servers' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /add by hand/i })).not.toBeInTheDocument()
  })

  // An auditor answers "who can reach this host", so the panel is theirs to
  // read -- and nothing on the page is theirs to change.
  it('lets an auditor see who is assigned but not edit', async () => {
    signedInAs('auditor')
    await renderRoute('/assets')
    // Waits for a row rather than the heading: the actions are per host, so
    // asserting before the fixture resolves would pass for the wrong reason.
    const seeWhoCan = await screen.findAllByLabelText(/^Who can reach/)
    expect(seeWhoCan.length).toBeGreaterThan(0)
    expect(screen.queryByLabelText(/^Edit /)).not.toBeInTheDocument()
  })
})

describe('what the pages say they are about', () => {
  it('frames the Overview as the fleet for an administrator', async () => {
    signedInAs('admin')
    await renderRoute('/')
    expect(await screen.findByText(/fleet posture/i)).toBeInTheDocument()
  })

  // Fleet posture is the wrong page to open on for someone who was only given
  // servers: they land on the servers instead.
  it('sends an operator to their own servers', async () => {
    signedInAs('operator')
    await renderRoute('/')
    expect(await screen.findByRole('heading', { name: 'My servers' })).toBeInTheDocument()
    expect(screen.queryByText(/fleet posture/i)).not.toBeInTheDocument()
  })

  it('keeps an operator on My servers rather than the session log', async () => {
    signedInAs('operator')
    await renderRoute('/sessions')
    expect(await screen.findByRole('heading', { name: 'My servers' })).toBeInTheDocument()
  })
})

describe('connecting with nothing assigned', () => {
  it('says so, rather than showing an empty search box', async () => {
    signedInAs('operator')
    vi.spyOn(api, 'assets').mockResolvedValue([])
    await renderRoute('/connect')
    const alert = await screen.findByText(/no hosts are assigned to you/i)
    // And says what to do about it. A dead form with no next step is a support
    // ticket rather than a control.
    expect(alert.closest('.mantine-Alert-root')).toHaveTextContent(/access request/i)
  })
})
