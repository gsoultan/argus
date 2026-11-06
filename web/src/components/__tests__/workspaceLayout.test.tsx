import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { renderWithProviders } from '~/test/render'
import type { DBLevel, Database } from '~/types/domain'
import { compactCount } from '~/components/DatabaseWorkspace'

/**
 * The workspace's layout: one header, three tabs, and the administration
 * panels under Manage rather than under the object tree.
 */

class FakeSocket {
  static last: FakeSocket | null = null
  static OPEN = 1
  readyState = 1
  binaryType = ''
  sent: Record<string, unknown>[] = []
  onmessage: ((ev: { data: unknown }) => void) | null = null
  onclose: (() => void) | null = null
  url: string
  constructor(url: string) { this.url = url; FakeSocket.last = this }
  send(frame: string) { this.sent.push((JSON.parse(frame) as { req: Record<string, unknown> }).req) }
  close() {}
  push(m: unknown) { act(() => { this.onmessage?.({ data: typeof m === 'string' || m instanceof ArrayBuffer ? m : JSON.stringify(m) }) }) }
  answer(ans: Record<string, unknown>) { this.push({ type: 'ans', ans }) }
}

const db: Database = {
  id: 'd1', assetId: 'a1', hostname: 'core-db-01', engine: 'postgres', host: '127.0.0.1', port: 5432,
  poolerHost: '', poolerPort: 0, dbname: 'orders', adminUser: '', label: 'staging', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

async function open(level: DBLevel = 'owner', database: Database = db, initialPanel?: string) {
  renderWithProviders(
    <DatabaseWorkspace database={database} access={{ level }} endpoint="/api/v1/postgres" initialPanel={initialPanel}
      getTicket={async () => ({ ticket: 't', role: 'ana', level })} />,
  )
  await waitFor(() => expect(FakeSocket.last).not.toBeNull())
  const ws = FakeSocket.last!
  ws.push({ type: 'ready' })
  ws.answer({ id: 0, type: 'ready', level, role: `ana@${level}`, version: '16.4' })
  // Opened on a Manage panel, the tree is not on screen to ask for anything.
  if (initialPanel) return ws
  // The tree: one schema with one table.
  await waitFor(() => expect(ws.sent.some((r) => r.op === 'schema' && r.kind === 'schemas')).toBe(true))
  for (const r of ws.sent.filter((x) => x.op === 'schema' && x.kind === 'schemas')) ws.answer({ id: r.id, type: 'schema', objects: [{ name: 'public', kind: 'schema' }] })
  await waitFor(() => expect(ws.sent.some((r) => r.op === 'schema' && r.kind === 'objects')).toBe(true))
  for (const r of ws.sent.filter((x) => x.op === 'schema' && x.kind === 'objects')) ws.answer({ id: r.id, type: 'schema', objects: [{ name: 'orders', kind: 'table', detail: '48 MB', count: 214_118 }] })
  return ws
}


describe('Workspace layout', () => {
  beforeEach(() => { localStorage.clear(); vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('keeps the object tree to the schema and puts administration under Manage', async () => {
    await open()
    const tabs = screen.getByRole('tablist', { name: 'View' })
    expect(within(tabs).getByRole('tab', { name: 'Query' })).toHaveAttribute('aria-selected', 'true')
    // The tree has the table, with its estimated rows, and no admin links.
    expect(await screen.findByLabelText('about 214,118 rows')).toHaveTextContent('≈214k')
    expect(screen.queryByText('Administration')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Trash/ })).not.toBeInTheDocument()

    fireEvent.click(within(tabs).getByRole('tab', { name: 'Manage' }))
    expect(within(tabs).getByRole('tab', { name: 'Manage' })).toHaveAttribute('aria-selected', 'true')
    const nav = screen.getByLabelText('Manage')
    // Opens on Activity, and each entry opens its panel without a back arrow.
    expect(within(nav).getByRole('button', { name: /Activity & locks/ })).toHaveAttribute('aria-current', 'page')
    fireEvent.click(within(nav).getByRole('button', { name: /Trash/ }))
    expect(within(nav).getByRole('button', { name: /Trash/ })).toHaveAttribute('aria-current', 'page')
    expect(screen.queryByRole('button', { name: 'Back to the query editor' })).not.toBeInTheDocument()

    // Query goes back to the editor; Manage then reopens the panel last looked at.
    fireEvent.click(within(tabs).getByRole('tab', { name: 'Query' }))
    expect(screen.getByLabelText('SQL')).toBeInTheDocument()
    fireEvent.click(within(tabs).getByRole('tab', { name: 'Manage' }))
    expect(within(screen.getByLabelText('Manage')).getByRole('button', { name: /Trash/ })).toHaveAttribute('aria-current', 'page')
  })

  it('opens on the panel a link names, under Manage', async () => {
    await open('owner', db, 'backups')
    expect(within(screen.getByRole('tablist', { name: 'View' })).getByRole('tab', { name: 'Manage' })).toHaveAttribute('aria-selected', 'true')
    expect(within(screen.getByLabelText('Manage')).getByRole('button', { name: /Backups/ })).toHaveAttribute('aria-current', 'page')
  })

  it('marks a production database across the top of the header', async () => {
    await open('read', { ...db, label: 'production' })
    const header = screen.getByRole('tablist', { name: 'View' }).parentElement!
    expect(header).toHaveAttribute('data-env', 'production')
    expect(header.style.boxShadow).toContain('rose')
    expect(within(header).getByText('orders')).toBeInTheDocument()
    expect(within(header).getByText(/core-db-01 · Postgres 16.4/)).toBeInTheDocument()
  })
})

describe('compactCount', () => {
  it('shortens without rounding up into the next unit', () => {
    expect(compactCount(940)).toBe('940')
    expect(compactCount(12_400)).toBe('12k')
    expect(compactCount(1_240)).toBe('1.2k')
    expect(compactCount(999_400)).toBe('999k')
    expect(compactCount(999_800)).toBe('1M')
    expect(compactCount(1_200_000)).toBe('1.2M')
  })
})
