import { describe, expect, it } from 'vitest'
import { screen, waitFor, fireEvent, within } from '@testing-library/react'
import { renderRoute } from '~/test/renderRoute'
import { assets, sessions } from '~/lib/seed'

/**
 * Smoke coverage for every route.
 *
 * The bar is deliberately low and the value is high: each of these asserts the
 * page mounts, reaches its data and renders something identifying. Every defect
 * these would have caught in the console rewrite — a route throwing on mount, a
 * missing provider, a loader that rejects — is one the unit tests could not see.
 */

const ROUTES: Array<[path: string, heading: string]> = [
  ['/assets', 'Servers'],
  ['/assets/add', 'Add server'],
  ['/approvals', 'Approvals'],
  ['/teams', 'Teams'],
  ['/activity', 'Activity'],
  ['/sessions', 'Activity'],
  ['/audit', 'Activity'],
  ['/settings', 'Settings'],
  ['/connect', 'Connect'],
  ['/about', 'About Argus'],
  // Old addresses, kept so a bookmark lands somewhere sensible.
  ['/', 'Servers'],
  ['/mine', 'Servers'],
  ['/coverage', 'Servers'],
  ['/requests', 'Approvals'],
  ['/users', 'Teams'],
]

describe('every route mounts', () => {
  it.each(ROUTES)('%s renders "%s"', async (path, heading) => {
    await renderRoute(path)
    // The redirecting addresses load a second route first, which on a busy
    // machine takes longer than findBy's default second.
    expect(await screen.findByRole('heading', { name: heading }, { timeout: 5_000 })).toBeInTheDocument()
  }, 15_000)
})

describe('sessions', () => {
  it('renders rows that are reachable by keyboard', async () => {
    const { container } = await renderRoute('/sessions')
    await screen.findByRole('heading', { name: 'Activity' }, { timeout: 5_000 })

    const rows = await waitFor(() => {
      const found = container.querySelectorAll('tbody tr[role="link"]')
      expect(found.length).toBeGreaterThan(0)
      return found
    }, { timeout: 5_000 })
    // The page copy tells the user to click a row. It said so for a while
    // before any row had a handler.
    for (const row of rows) {
      expect(row).toHaveAttribute('tabindex', '0')
    }
  }, 15_000)

  it('filters by the search box', async () => {
    await renderRoute('/sessions')
    const search = await screen.findByPlaceholderText(/filter by person, server/i)
    fireEvent.change(search, { target: { value: 'no-such-host-anywhere' } })
    expect(await screen.findByText('No sessions match.')).toBeInTheDocument()
  })
})

describe('settings', () => {
  const EBPF = 'Require eBPF agent for root sessions'

  it('renders switches that reflect stored policy rather than a default prop', async () => {
    await renderRoute('/settings?section=recording')
    const ebpf = await screen.findByRole('switch', { name: EBPF })
    // The closed default, read from the store — not `defaultChecked`, which is
    // what this page shipped with and which silently reverted on navigation.
    expect(ebpf).toBeChecked()
  })

  it('offers no save until something actually changes', async () => {
    await renderRoute('/settings?section=recording')
    await screen.findByRole('switch', { name: EBPF })
    expect(screen.queryByRole('button', { name: /review \d+ change/i })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('switch', { name: EBPF }))
    expect(await screen.findByRole('button', { name: /review 1 change/i })).toBeInTheDocument()
    // On the toggle, and on its section in the list of sections.
    expect(screen.getAllByText('unsaved')).toHaveLength(2)
  })

  it('opens on logins', async () => {
    await renderRoute('/settings')
    expect(await screen.findByText('Through the console')).toBeInTheDocument()
    expect(screen.getByText("Logins that don't go through Argus")).toBeInTheDocument()
    // Direct SSH logins start refused, and opening them up is a change to
    // review, not one that applies as the radio moves.
    expect(screen.getByRole('radio', { name: /^refuse/i })).toBeChecked()
    fireEvent.click(screen.getByRole('radio', { name: /allow and record/i }))
    expect(await screen.findByRole('button', { name: /review 1 change/i })).toBeInTheDocument()
  })

  it('offers no switches for a gateway that no longer exists', async () => {
    await renderRoute('/settings?section=recording')
    await screen.findByRole('switch', { name: EBPF })
    expect(screen.queryByRole('switch', { name: /forwarding/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('switch', { name: /fail closed/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /ssh channels/i })).not.toBeInTheDocument()
  })

  it('opens one section at a time from the list', async () => {
    await renderRoute('/settings?section=recording')
    await screen.findByRole('switch', { name: EBPF })
    expect(screen.queryByText('Threat model, stated plainly')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /^server agents/i }))
    expect(await screen.findByText('Threat model, stated plainly')).toBeInTheDocument()
    expect(screen.queryByRole('switch', { name: EBPF })).not.toBeInTheDocument()
  })

  it('names the loosening before applying it', async () => {
    await renderRoute('/settings?section=recording')
    fireEvent.click(await screen.findByRole('switch', { name: EBPF }))
    fireEvent.click(await screen.findByRole('button', { name: /review 1 change/i }))

    expect(await screen.findByText(/loosens the policy/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /apply policy/i })).toBeInTheDocument()
  })

  it('discards edits without touching the store', async () => {
    await renderRoute('/settings?section=recording')
    const ebpf = await screen.findByRole('switch', { name: EBPF })
    fireEvent.click(ebpf)
    fireEvent.click(await screen.findByRole('button', { name: /discard/i }))

    await waitFor(() => expect(ebpf).toBeChecked())
    expect(screen.queryByRole('button', { name: /review \d+ change/i })).not.toBeInTheDocument()
  })
})

describe('assets', () => {
  it('filters to nothing when the search matches no host', async () => {
    await renderRoute('/assets')
    const search = await screen.findByRole('textbox', { name: 'Find a server' })
    fireEvent.change(search, { target: { value: 'zzz-not-a-host' } })
    expect(await screen.findByText('No server matches.')).toBeInTheDocument()
  })
})

describe('people', () => {
  // Roles change on the page now. It used to send an administrator to a shell
  // on the control plane host.
  it('changes a role from the list, not from a command line', async () => {
    await renderRoute('/teams?tab=people')
    expect((await screen.findAllByLabelText(/^Role of /)).length).toBeGreaterThan(0)
    expect(screen.queryByText(/argus-control users/)).not.toBeInTheDocument()
  })
})

describe('unknown routes', () => {
  it('render the not-found page instead of blanking', async () => {
    await renderRoute('/no-such-page')
    expect(await screen.findByText('404')).toBeInTheDocument()
    expect(screen.getByText('No such page.')).toBeInTheDocument()
  })
})

/**
 * The console's menu: five words across the top for an administrator, and
 * one of them lit for the page you are on.
 */
describe('navigation', () => {
  it('has one entry for servers, and no separate Connect', async () => {
    await renderRoute('/assets')
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    const servers = await within(nav).findByRole('link', { name: /^Servers/ })
    expect(servers).toHaveAttribute('href', '/assets')
    expect(within(nav).queryByRole('link', { name: /connect/i })).not.toBeInTheDocument()
  })

  it('puts the audit log under Activity', async () => {
    await renderRoute('/audit')
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    expect(await within(nav).findByRole('link', { name: /^Activity/ })).toHaveAttribute('data-active')
    expect(screen.getByRole('tab', { name: /audit log/i })).toHaveAttribute('aria-selected', 'true')
  })

  /** Adding a server is part of Servers, so Servers stays lit while doing it. */
  it('lights exactly one item, and Servers while adding one', async () => {
    await renderRoute('/assets/add')
    await screen.findByRole('heading', { name: 'Add server' })
    const nav = screen.getByRole('navigation', { name: 'Main' })
    await within(nav).findByRole('link', { name: /^Servers/ })
    const current = within(nav)
      .getAllByRole('link')
      .filter((a) => a.hasAttribute('data-active'))
    expect(current).toHaveLength(1)
    expect(current[0]).toHaveTextContent(/^Servers/)
  })
})

describe('add a server', () => {
  it('asks for a name first, then shows one line to paste', async () => {
    await renderRoute('/assets/add')
    fireEvent.change(await screen.findByRole('textbox', { name: /name/i }), { target: { value: 'web-01' } })
    fireEvent.click(screen.getByRole('button', { name: /next: install/i }))
    expect(await screen.findByText(/curl -fsSL .*\/api\/v1\/install\/agent\.sh.*\| sudo sh/)).toBeInTheDocument()
  })

  /**
   * This page used to tell the operator to copy the gateways' reporter token
   * onto every server, which let root on any server report as any other. The
   * shared token must not come back as a step here.
   */
  it('never asks for the shared reporter token', async () => {
    await renderRoute('/assets/add')
    fireEvent.click(await screen.findByRole('button', { name: /next: install/i }))
    await screen.findByText(/install\/agent\.sh/)
    expect(document.body.textContent).not.toMatch(/REPORTER[-_]TOKEN|--control-token/)
  })

  it('gives Windows its own block, for PowerShell', async () => {
    await renderRoute('/assets/add')
    fireEvent.click(await screen.findByText('Windows'))
    fireEvent.click(screen.getByRole('button', { name: /next: install/i }))
    expect(await screen.findByText(/argus-agent\.exe" service install/)).toBeInTheDocument()
  })

  /**
   * Waiting is the one part of this page that could be faked to look finished.
   * With no control plane nothing can check in, and it says so rather than
   * moving on as if an agent had reported.
   */
  it('does not pretend a server arrived when there is nothing to report to', async () => {
    await renderRoute('/assets/add')
    fireEvent.click(await screen.findByRole('button', { name: /next: install/i }))
    expect(await screen.findByText(/no agent can check in to sample data/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /who can use it/i })).not.toBeInTheDocument()
  })
})

describe('empty states', () => {
  /**
   * "No sessions match." on its own reads the same whether the filter is too
   * narrow, the fleet is genuinely idle, or the query failed. The guidance is
   * the part that tells them apart.
   */
  it('say what to do next, not only that there is nothing', async () => {
    await renderRoute('/sessions')
    const search = await screen.findByPlaceholderText(/filter by person, server/i)
    fireEvent.change(search, { target: { value: 'no-such-host-anywhere' } })
    expect(await screen.findByText('No sessions match.')).toBeInTheDocument()
    expect(screen.getByText(/widen it, or clear the search/i)).toBeInTheDocument()
  })
})

describe('sessions by server', () => {
  /**
   * "What happened on this server" had no direct answer: the search box
   * matched hostnames as a substring of everything else in the row.
   */
  it('narrows the table and the counts to one server from the URL', async () => {
    const host = sessions[0]!.assetHostname
    const others = sessions.find((s) => s.assetHostname !== host)!.assetHostname
    await renderRoute(`/sessions?host=${encodeURIComponent(host)}`)
    expect((await screen.findAllByDisplayValue(host)).length).toBeGreaterThan(0)
    const table = screen.getByRole('table')
    expect(within(table).getAllByText(host.split('.')[0]!, { exact: false }).length).toBeGreaterThan(0)
    expect(within(table).queryByText(others, { exact: false })).not.toBeInTheDocument()
  })

  it('says which server came up empty', async () => {
    await renderRoute('/sessions?host=nowhere.example')
    expect(await screen.findByText(/no session on nowhere\.example/i)).toBeInTheDocument()
  })

  it('is one click from a server\'s own page', async () => {
    const asset = assets.find((a) => sessions.some((s) => s.assetId === a.id))!
    await renderRoute(`/assets/${asset.id}`)
    const link = await screen.findByRole('link', { name: /all sessions on this server/i })
    expect(link.getAttribute('href')).toContain(`host=${encodeURIComponent(asset.hostname)}`)
  })
})

describe('command palette', () => {
  it('opens on the keyboard and reaches a host by name from any page', async () => {
    await renderRoute('/assets')
    fireEvent.keyDown(document, { key: 'k', metaKey: true })

    const input = await screen.findByPlaceholderText(/search pages, hosts/i)
    fireEvent.change(input, { target: { value: 'db-01' } })

    // Within the palette: the Servers page lists the same name.
    const palette = await screen.findByRole('dialog')
    expect(await within(palette).findByText('db-01.data.northwind.id')).toBeInTheDocument()
  })
})

describe('the header says which data is on screen', () => {
  /**
   * The slot this covers used to read a hard-coded "northwind-prod ·
   * ap-southeast-3". Argus has no tenant and no region — neither word appears
   * in the domain — so it was an invented deployment name presented as fact, in
   * the one place an operator would look to check which deployment they were
   * about to act on.
   *
   * It matters more than a label usually would: live.orFallback serves the
   * in-memory fixture whenever the control plane cannot be reached, so a
   * console showing invented numbers is otherwise indistinguishable from one
   * showing a real fleet.
   */
  it('names the fixture as the fixture when no control plane is configured', async () => {
    await renderRoute('/assets')
    expect(await screen.findByText('Sample data')).toBeInTheDocument()
    expect(screen.queryByText(/northwind-prod/)).not.toBeInTheDocument()
  })

  /**
   * The banner that says "nothing below is your fleet" belongs to one state:
   * a control plane that was configured and then stopped answering. Running
   * against the fixture is not that — nobody configured anything, the badge
   * already says so, and a warning shown in every state is not a warning.
   *
   * Without this, a banner that rendered unconditionally would pass every other
   * test in this file.
   */
  it('does not cry fallback when there is no control plane to have lost', async () => {
    await renderRoute('/assets')
    await screen.findByText('Sample data')
    expect(screen.queryByText(/nothing below is your fleet/)).not.toBeInTheDocument()
  })
})
