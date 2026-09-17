import { describe, expect, it } from 'vitest'
import { screen, waitFor, fireEvent } from '@testing-library/react'
import { renderRoute } from '~/test/renderRoute'

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
  ['/assets', 'Assets'],
  ['/requests', 'Access requests'],
  ['/audit', 'Audit log'],
  ['/users', 'Users & roles'],
  ['/settings', 'Settings'],
  ['/coverage', 'Coverage'],
  ['/connect', 'Connect'],
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
    expect(await screen.findByText('LIVE SESSIONS')).toBeInTheDocument()
    expect(screen.getByText('PENDING APPROVALS')).toBeInTheDocument()
    expect(screen.getByText('UNVERIFIED HOSTS')).toBeInTheDocument()
    // The coverage gap leads the page: an unmonitored host is a session nobody
    // will ever hear about, which outranks one you can see and fix.
    expect(screen.getByText('BYPASSED GATEWAY')).toBeInTheDocument()
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
    const search = await screen.findByPlaceholderText(/filter by user, host/i)
    fireEvent.change(search, { target: { value: 'no-such-host-anywhere' } })
    expect(await screen.findByText('No sessions match.')).toBeInTheDocument()
  })
})

describe('settings', () => {
  it('renders switches that reflect stored policy rather than a default prop', async () => {
    await renderRoute('/settings')
    const x11 = await screen.findByRole('switch', { name: 'Allow X11 forwarding' })
    const sftp = await screen.findByRole('switch', { name: 'Proxy SFTP as a subsystem' })
    // The closed defaults, read from the store — not `defaultChecked`, which is
    // what this page shipped with and which silently reverted on navigation.
    expect(x11).not.toBeChecked()
    expect(sftp).toBeChecked()
  })

  it('offers no save until something actually changes', async () => {
    await renderRoute('/settings')
    await screen.findByRole('switch', { name: 'Allow X11 forwarding' })
    expect(screen.queryByRole('button', { name: /review \d+ change/i })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('switch', { name: 'Allow X11 forwarding' }))
    expect(await screen.findByRole('button', { name: /review 1 change/i })).toBeInTheDocument()
    expect(screen.getByText('unsaved')).toBeInTheDocument()
  })

  it('names the loosening before applying it', async () => {
    await renderRoute('/settings')
    const agent = await screen.findByRole('switch', { name: 'Allow SSH agent forwarding' })
    fireEvent.click(agent)
    fireEvent.click(await screen.findByRole('button', { name: /review 1 change/i }))

    expect(await screen.findByText(/loosens the gateway/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /apply policy/i })).toBeInTheDocument()
  })

  it('discards edits without touching the store', async () => {
    await renderRoute('/settings')
    const x11 = await screen.findByRole('switch', { name: 'Allow X11 forwarding' })
    fireEvent.click(x11)
    fireEvent.click(await screen.findByRole('button', { name: /discard/i }))

    await waitFor(() => expect(x11).not.toBeChecked())
    expect(screen.queryByRole('button', { name: /review \d+ change/i })).not.toBeInTheDocument()
  })
})

describe('assets', () => {
  it('filters to nothing when the search matches no host', async () => {
    await renderRoute('/assets')
    const search = await screen.findByPlaceholderText(/filter by hostname/i)
    fireEvent.change(search, { target: { value: 'zzz-not-a-host' } })
    expect(await screen.findByText('No assets match.')).toBeInTheDocument()
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
  it('groups its destinations under named sections', async () => {
    await renderRoute('/')
    for (const section of ['Operate', 'Fleet', 'Governance']) {
      expect(await screen.findByText(section)).toBeInTheDocument()
    }
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
    const search = await screen.findByPlaceholderText(/filter by user, host/i)
    fireEvent.change(search, { target: { value: 'no-such-host-anywhere' } })
    expect(await screen.findByText('No sessions match.')).toBeInTheDocument()
    expect(screen.getByText(/widen it, or clear the search/i)).toBeInTheDocument()
  })
})

describe('command palette', () => {
  it('opens on the keyboard and reaches a host by name from any page', async () => {
    await renderRoute('/')
    fireEvent.keyDown(document, { key: 'k', metaKey: true })

    const input = await screen.findByPlaceholderText(/search pages, hosts/i)
    fireEvent.change(input, { target: { value: 'db-01' } })

    expect(await screen.findByText('db-01.data.northwind.id')).toBeInTheDocument()
  })
})
