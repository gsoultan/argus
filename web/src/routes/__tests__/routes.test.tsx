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
  ['/', 'Overview'],
  ['/sessions', 'Sessions'],
  ['/assets', 'Servers'],
  ['/assets/add', 'Add a server'],
  ['/requests', 'Access requests'],
  ['/audit', 'Audit log'],
  ['/users', 'Users & roles'],
  ['/settings', 'Settings'],
  ['/coverage', 'Coverage'],
  ['/connect', 'Connect'],
  // The old address of My servers, now part of Servers.
  ['/mine', 'Servers'],
]

describe('every route mounts', () => {
  it.each(ROUTES)('%s renders "%s"', async (path, heading) => {
    await renderRoute(path)
    expect(await screen.findByRole('heading', { name: heading })).toBeInTheDocument()
  })
})

describe('overview', () => {
  it('shows fleet counters once the fixture resolves', async () => {
    await renderRoute('/')
    // Twice: the counter, and the table of them further down.
    expect((await screen.findAllByText('Live sessions')).length).toBeGreaterThan(0)
    expect(screen.getByText('Pending approvals')).toBeInTheDocument()
    expect(screen.getByText('Unverified hosts')).toBeInTheDocument()
    // The coverage gap leads the page: an unmonitored host is a session nobody
    // will ever hear about, which outranks one you can see and fix.
    expect(screen.getByText('Bypassed Argus')).toBeInTheDocument()
  })
})

describe('sessions', () => {
  it('renders rows that are reachable by keyboard', async () => {
    const { container } = await renderRoute('/sessions')
    await screen.findByRole('heading', { name: 'Sessions' })

    const rows = await waitFor(() => {
      const found = container.querySelectorAll('tbody tr[role="link"]')
      expect(found.length).toBeGreaterThan(0)
      return found
    })
    // The page copy tells the user to click a row. It said so for a while
    // before any row had a handler.
    for (const row of rows) {
      expect(row).toHaveAttribute('tabindex', '0')
    }
  })

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

  it('opens on how people connect', async () => {
    await renderRoute('/settings')
    expect(await screen.findByText('Through the console')).toBeInTheDocument()
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

    fireEvent.click(screen.getByRole('button', { name: /^hosts and trust/i }))
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
    await renderRoute('/assets?view=manage')
    const search = await screen.findByPlaceholderText(/filter by hostname/i)
    fireEvent.change(search, { target: { value: 'zzz-not-a-host' } })
    expect(await screen.findByText('No servers match.')).toBeInTheDocument()
  })
})

describe('users', () => {
  it('states where roles are actually changed', async () => {
    await renderRoute('/users')
    // The page cannot change a role and no longer implies it can.
    expect(
      await screen.findByText(/Roles are set on the host with/i),
    ).toBeInTheDocument()
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
 * The console's information architecture.
 *
 * Nine flat destinations were grouped into three sections without changing a
 * single path, so these assert the grouping exists and that the links inside it
 * still point where they always did.
 */
describe('navigation', () => {
  // "Connect to a server", "My servers" and "Servers" were three ways to the
  // same machines, and people asked which was which. Now there is one.
  it('has one entry for servers, and no separate Connect', async () => {
    await renderRoute('/')
    const nav = await screen.findByRole('navigation')
    const servers = await within(nav).findByRole('link', { name: /^Servers/ })
    expect(servers).toHaveAttribute('href', '/assets')
    expect(within(nav).queryByRole('link', { name: /connect/i })).not.toBeInTheDocument()
    expect(within(nav).queryByRole('link', { name: /my servers/i })).not.toBeInTheDocument()
  })

  it('opens Servers on the cards to connect from, with the inventory one switch away', async () => {
    await renderRoute('/assets')
    expect(await screen.findByRole('textbox', { name: 'Find a server' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('radio', { name: /manage/i }))
    expect(await screen.findByPlaceholderText(/filter by hostname/i)).toBeInTheDocument()
  })

  // A link from Coverage or the Overview carries a filter, and must land on
  // the table it narrows rather than on the cards.
  it('opens the inventory by itself for a filtered link', async () => {
    await renderRoute('/assets?bypass=open')
    expect(await screen.findByPlaceholderText(/filter by hostname/i)).toBeInTheDocument()
  })

  /**
   * Admin pages are a minority of visits. Folded, they stop competing with the
   * four everyday ones, but they must open by themselves on one of their own
   * pages, or a deep link to the audit log would land with no menu item lit.
   */
  it('keeps admin pages folded until they are wanted', async () => {
    await renderRoute('/')
    const nav = await screen.findByRole('navigation')
    expect(within(nav).queryByRole('link', { name: /audit log/i })).not.toBeInTheDocument()
    fireEvent.click(within(nav).getByRole('button', { name: /admin/i }))
    expect(within(nav).getByRole('link', { name: /audit log/i })).toBeInTheDocument()
  })

  it('opens the admin section by itself on one of its pages', async () => {
    await renderRoute('/audit')
    const nav = await screen.findByRole('navigation')
    expect(await within(nav).findByRole('link', { name: /audit log/i })).toHaveAttribute('data-active')
  })

  /** Adding a server is part of Servers, so Servers stays lit while doing it. */
  it('lights exactly one item, and Servers while adding one', async () => {
    await renderRoute('/assets/add')
    await screen.findByRole('heading', { name: 'Add a server' })
    const nav = screen.getByRole('navigation')
    // Servers is an administrator's entry, so it appears once the identity
    // lookup says this is one.
    await within(nav).findByRole('link', { name: /^Servers/ })
    const current = within(nav)
      .getAllByRole('link')
      .filter((a) => a.hasAttribute('data-active'))
    expect(current).toHaveLength(1)
    expect(current[0]).toHaveTextContent(/^Servers/)
  })
})

describe('add a server', () => {
  it('gives the install commands, then an enroll command', async () => {
    await renderRoute('/assets/add')
    expect(await screen.findByText(/sudo dpkg -i argus-agent_/)).toBeInTheDocument()
    expect(screen.getByText(/sudo argus-agent enroll/)).toBeInTheDocument()
  })

  /**
   * This page used to tell the operator to copy the gateways' reporter token
   * onto every server, which let root on any server report as any other. The
   * shared token must not come back as a step here.
   */
  it('never asks for the shared reporter token', async () => {
    await renderRoute('/assets/add')
    await screen.findByText(/sudo argus-agent enroll/)
    expect(document.body.textContent).not.toMatch(/REPORTER[-_]TOKEN|--control-token/)
  })

  /** With no control plane there is nothing to mint a token on, and it says so. */
  it('does not offer a token it cannot make', async () => {
    await renderRoute('/assets/add')
    expect(await screen.findByText(/cannot make\s+an enrollment token/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /make a token/i })).not.toBeInTheDocument()
  })

  /**
   * The waiting list is the one part of this page that could be faked to look
   * finished. With no control plane nothing can check in, and it says so rather
   * than listing sample servers as if their agents had reported.
   */
  it('shows no servers waiting when there is no control plane to report to', async () => {
    await renderRoute('/assets/add')
    expect(await screen.findByText(/no agent can\s+check in to it/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
  })
})

describe('overview alerts', () => {
  /**
   * This button said "Review coverage" and navigated to /assets — the inventory
   * table, which lists hosts and answers nothing about whether an agent is
   * reporting from each one. The alert it sits in is specifically about hosts
   * that would not record a bypass, which is the Coverage page's whole subject.
   */
  it('sends a coverage gap to Coverage rather than to the inventory', async () => {
    await renderRoute('/')
    const link = await screen.findByRole('link', { name: /review coverage/i })
    expect(link).toHaveAttribute('href', '/coverage')
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
    await renderRoute('/')
    fireEvent.keyDown(document, { key: 'k', metaKey: true })

    const input = await screen.findByPlaceholderText(/search pages, hosts/i)
    fireEvent.change(input, { target: { value: 'db-01' } })

    // Within the palette: the Overview's own server picker lists the same name.
    const palette = await screen.findByRole('dialog')
    expect(await within(palette).findByText('db-01.data.northwind.id')).toBeInTheDocument()
  })
})

/**
 * Assets and Coverage answer different questions about the same fleet, and
 * deliberately stayed separate pages: the inventory lists what Argus manages,
 * so a machine nobody enrolled is invisible there by construction, which is
 * exactly what Coverage is for.
 *
 * What they lacked was a way to get from one to the other. Coverage counted
 * hosts with no agent and could not say which ones; the inventory had no agent
 * filter to be pointed at. These cover the join.
 */
describe('assets and coverage', () => {
  it('filters the inventory from the URL, so a finding can be linked to', async () => {
    await renderRoute('/assets?agent=stale')
    await screen.findByRole('heading', { name: 'Servers' })

    // Every row the filter admits says the agent went quiet. The badge reads
    // "silent" -- the word the operator sees for a stale agent.
    const badges = await screen.findAllByText('silent')
    expect(badges.length).toBeGreaterThan(0)
    expect(screen.queryByText('no agent')).not.toBeInTheDocument()
  })

  it('drops a filter value the controls cannot show', async () => {
    // A hand-edited URL must not put the table into a state with no visible
    // cause -- rows filtered by something none of the selects can display.
    await renderRoute('/assets?view=manage&agent=not-a-state')
    await screen.findByRole('heading', { name: 'Servers' })
    expect(await screen.findAllByText('no agent')).not.toHaveLength(0)
  })

  it('says on the inventory that it is not the whole fleet', async () => {
    await renderRoute('/assets?view=manage')
    // Scoped to the page header: the sidebar links to Coverage from every page,
    // and what is under test is that this page points at it too.
    const heading = await screen.findByRole('heading', { name: 'Servers' })
    const header = heading.closest('.argus-pagehead')
    expect(header).not.toBeNull()
    const link = within(header as HTMLElement).getByRole('link', { name: /coverage/i })
    expect(link).toHaveAttribute('href', '/coverage')
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
    await renderRoute('/')
    expect(await screen.findByText('Local fixture')).toBeInTheDocument()
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
    await renderRoute('/')
    await screen.findByText('Local fixture')
    expect(screen.queryByText(/nothing below is your fleet/)).not.toBeInTheDocument()
  })
})
