import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseWorkspace, railFor, withBackupAge } from '~/components/DatabaseWorkspace'
import { renderWithProviders } from '~/test/render'
import type { DBLevel, Database, DatabaseBackup } from '~/types/domain'
import { compactCount } from '~/components/DatabaseWorkspace'

/**
 * The workspace's layout: one header, and one rail holding the editor, the
 * diagram and every administration panel (plans/database-page-layout).
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

  const rail = () => screen.getByRole('navigation', { name: 'Database' })

  it('keeps the object tree to the schema and puts every panel on one rail', async () => {
    await open()
    // On the editor the rail is folded to icons, Query current.
    expect(within(rail()).getByRole('button', { name: 'Query' })).toHaveAttribute('aria-current', 'page')
    expect(screen.queryByRole('tablist', { name: 'View' })).not.toBeInTheDocument()
    // The tree has the table, with its estimated rows, and no admin links.
    expect(await screen.findByLabelText('about 214,118 rows')).toHaveTextContent('≈214k')
    expect(screen.queryByText('Administration')).not.toBeInTheDocument()

    // A panel opens straight from the rail, which then opens wide.
    fireEvent.click(within(rail()).getByRole('button', { name: 'Trash' }))
    expect(within(rail()).getByRole('button', { name: /Trash/ })).toHaveAttribute('aria-current', 'page')
    expect(within(rail()).getByText('Look after')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Back to the query editor' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('about 214,118 rows')).not.toBeInTheDocument()

    // Query goes back to the editor and its tree.
    fireEvent.click(within(rail()).getByRole('button', { name: /Query/ }))
    expect(screen.getByLabelText('SQL')).toBeInTheDocument()
    expect(within(rail()).getByRole('button', { name: 'Query' })).toHaveAttribute('aria-current', 'page')
  })

  it('lists every Postgres panel, Diagram included, in its section', async () => {
    await open('owner', db, 'backups')
    const names = within(rail()).getAllByRole('button').map((b) => b.textContent).filter(Boolean)
    expect(names).toEqual([
      'Query', 'Diagram', 'Activity & locks', 'Performance', 'Backups', 'Data changes', 'Trash',
      'Roles & access', 'Extensions', 'Replication', 'Network',
    ])
    // Below DBA the DBA section says so once, and each of its items carries a lock.
    expect(within(rail()).getByText('Needs DBA')).toBeInTheDocument()
    expect(within(rail()).getAllByLabelText('needs DBA access')).toHaveLength(4)
  })

  it('opens on the panel a link names', async () => {
    await open('owner', db, 'backups')
    expect(within(rail()).getByRole('button', { name: /Backups/ })).toHaveAttribute('aria-current', 'page')
  })

  it('keeps the rail where the person pinned it', async () => {
    await open()
    // Folded on the editor; pinning it open survives a reload.
    fireEvent.click(screen.getByRole('button', { name: 'Open the menu' }))
    expect(within(rail()).getByText('Work')).toBeInTheDocument()
    expect(localStorage.getItem('argus-db-rail')).toBe('open')
  })

  it('marks a production database across the top of the header', async () => {
    await open('read', { ...db, label: 'production' })
    const header = screen.getByRole('banner')
    expect(header).toHaveAttribute('data-env', 'production')
    expect(header.style.boxShadow).toContain('rose')
    expect(within(header).getByText('orders')).toBeInTheDocument()
    expect(within(header).getByText('core-db-01')).toBeInTheDocument()
    expect(within(header).getByText(/Postgres 16\.4/)).toBeInTheDocument()
    // One strip: the approval rule, the level and the recording.
    const strip = within(header).getByTestId('trust-strip')
    expect(strip).toHaveTextContent(/Changes need approval/)
    expect(strip).toHaveTextContent(/You can READ/)
    expect(strip).toHaveTextContent(/Recorded/)
  })
})

describe('MySQL workspace', () => {
  beforeEach(() => { localStorage.clear(); vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('shows the Performance and Network panels it has on the rail', async () => {
    await open('owner', { ...db, engine: 'mysql', port: 3306 }, 'network')
    const names = within(screen.getByRole('navigation', { name: 'Database' })).getAllByRole('button').map((b) => b.textContent).filter(Boolean)
    expect(names).toEqual(['Query', 'Performance', 'Network'])
  })
})

describe('SQL Server workspace', () => {
  beforeEach(() => { localStorage.clear(); vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('lists its own panels, opens dbo, and names the release', async () => {
    const ms: Database = { ...db, engine: 'mssql', port: 1433, dbname: 'sales' }
    renderWithProviders(
      <DatabaseWorkspace database={ms} access={{ level: 'write' }} endpoint="/api/v1/postgres"
        getTicket={async () => ({ ticket: 't', role: 'ana', level: 'write' })} />,
    )
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', level: 'write', role: 'ana@write', version: '16.0.4135.4' })
    expect(screen.getByText(/SQL Server 2022 \(16\.0\.4135\.4\)/)).toBeInTheDocument()
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'schema' && r.kind === 'schemas')).toBe(true))
    for (const r of ws.sent.filter((x) => x.op === 'schema' && x.kind === 'schemas')) ws.answer({ id: r.id, type: 'schema', objects: [{ name: 'dbo', kind: 'schema' }, { name: 'sales', kind: 'schema' }] })
    // dbo opens by itself, as public does on Postgres; other schemas wait for a click.
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'schema' && r.kind === 'objects' && r.schema === 'dbo')).toBe(true))
    expect(ws.sent.some((r) => r.op === 'schema' && r.kind === 'objects' && r.schema === 'sales')).toBe(false)

    // SQL Server's own panels, no Diagram; each but Backups at DBA only.
    const rail = () => screen.getByRole('navigation', { name: 'Database' })
    fireEvent.click(within(rail()).getByRole('button', { name: 'Activity & blocking' }))
    expect(within(rail()).getAllByRole('button').map((b) => b.textContent).filter(Boolean)).toEqual([
      'Query', 'Activity & blocking', 'Performance', 'Integrity', 'Backups',
      'Indexes & statistics', 'Agent jobs', 'Replication', 'Logins & access', 'Settings',
    ])
    expect(within(rail()).getAllByLabelText('needs DBA access')).toHaveLength(8)
    expect(await screen.findByText('Activity & blocking needs DBA access')).toBeInTheDocument()
    fireEvent.click(within(rail()).getByRole('button', { name: /Logins & access/ }))
    expect(await screen.findByText('Logins & access needs DBA access')).toBeInTheDocument()
    expect(ws.sent.some((r) => r.op === 'admin')).toBe(false)
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

describe('withBackupAge', () => {
  const now = Date.parse('2026-11-28T12:00:00Z')
  const items = railFor('postgres')
  const b = (state: string, at: string) => ({ state, startedAt: at, finishedAt: at } as unknown as DatabaseBackup)
  it('shows the age of a recent backup beside Backups only', () => {
    const out = withBackupAge(items, b('done', '2026-11-28T10:00:00Z'), now)
    expect(out.find((i) => i.key === 'backups')?.count).toEqual({ text: '2h ago', tone: undefined })
    expect(out.filter((i) => i.count)).toHaveLength(1)
  })
  it('flags a day-old backup and a failed one', () => {
    expect(withBackupAge(items, b('done', '2026-11-26T12:00:00Z'), now).find((i) => i.key === 'backups')?.count?.tone).toBe('pending')
    expect(withBackupAge(items, b('failed', '2026-11-28T11:00:00Z'), now).find((i) => i.key === 'backups')?.count).toEqual({ text: 'failed', tone: 'denied' })
  })
  it('leaves the rail alone with no backup', () => {
    expect(withBackupAge(items, undefined, now)).toBe(items)
  })
})
