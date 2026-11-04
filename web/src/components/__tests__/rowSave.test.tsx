import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { renderWithProviders } from '~/test/render'
import type { DBLevel, Database } from '~/types/domain'
import { setEditor } from '~/test/editor'
import { TARGET_SQL } from '~/lib/rowEdit'

/**
 * Saving row edits from the grid: one transaction, each statement checked to
 * have touched exactly one row, and rolled back whole when one did not. The
 * agent sees ordinary statements with bind parameters, so each is recorded
 * and guarded like any other.
 */

const live = vi.hoisted(() => ({ askStatements: vi.fn(), statementAsk: vi.fn() }))
vi.mock('~/lib/live', async (orig) => ({ ...(await orig<typeof import('~/lib/live')>()), ...live }))

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

/** A row batch as the agent sends it: id, then uvarint(len*2+1) + bytes per cell. */
function batch(id: number, rows: (string | null)[][]): ArrayBuffer {
  const out: number[] = [(id >>> 24) & 255, (id >>> 16) & 255, (id >>> 8) & 255, id & 255]
  const enc = new TextEncoder()
  for (const r of rows) {
    for (const c of r) {
      if (c === null) { out.push(0); continue }
      const b = enc.encode(c)
      let v = b.length * 2 + 1
      while (v >= 0x80) { out.push((v & 0x7f) | 0x80); v = Math.floor(v / 128) }
      out.push(v)
      out.push(...b)
    }
  }
  return new Uint8Array(out).buffer
}

const db: Database = {
  id: 'd1', assetId: 'a1', hostname: 'core-db-01', engine: 'postgres', host: '127.0.0.1', port: 5432,
  poolerHost: '', poolerPort: 0, dbname: 'orders', adminUser: '', label: 'production', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

const COLS = [
  { name: 'id', type: 'int4', table: 42, attnum: 1 },
  { name: 'note', type: 'text', table: 42, attnum: 2 },
]

async function open(level: DBLevel = 'write') {
  renderWithProviders(
    <DatabaseWorkspace database={db} access={{ level }} endpoint="/api/v1/postgres"
      getTicket={async () => ({ ticket: 't', role: 'ana', level })} />,
  )
  await waitFor(() => expect(FakeSocket.last).not.toBeNull())
  const ws = FakeSocket.last!
  ws.push({ type: 'ready' })
  ws.answer({ id: 0, type: 'ready', level, role: `ana@${level}`, version: '16.4' })
  return ws
}

const runs = (ws: FakeSocket) => ws.sent.filter((r) => r.op === 'run')

/** Answers every run request the grid sends for itself: the table, its foreign keys. */
function answerLookups(ws: FakeSocket, done: Set<unknown>) {
  for (const r of runs(ws)) {
    if (done.has(r.id)) continue
    if (r.sql === TARGET_SQL) {
      done.add(r.id)
      // Cols first, as from the agent: a batch is read by its column count.
      ws.answer({ id: r.id, type: 'cols', cols: Array.from({ length: 9 }, (_, i) => ({ name: `c${i}`, type: 'text' })) })
      ws.push(batch(r.id as number, [
        ['public.orders', '1', 'id', 'integer', 'true', 'true', 'false', null, '1'],
        ['public.orders', '2', 'note', 'text', 'false', 'false', 'false', null, '1'],
      ]))
      ws.answer({ id: r.id, type: 'done', tag: 'SELECT 2' })
    } else if (String(r.sql).includes('pg_constraint')) {
      done.add(r.id)
      ws.answer({ id: r.id, type: 'cols', cols: [] })
      ws.answer({ id: r.id, type: 'done', tag: 'SELECT 0' })
    }
  }
}

async function selectOrders(ws: FakeSocket) {
  setEditor('SELECT * FROM orders')
  fireEvent.keyDown(screen.getByLabelText('SQL'), { key: 'Enter', ctrlKey: true })
  await waitFor(() => expect(runs(ws)).toHaveLength(1))
  const r = runs(ws)[0]!
  ws.answer({ id: r.id, type: 'cols', cols: COLS })
  ws.push(batch(r.id as number, [['1', 'first'], ['2', 'second']]))
  ws.answer({ id: r.id, type: 'done', tag: 'SELECT 2' })
}

/** Waits for the next request and answers it. */
async function next(ws: FakeSocket, n: number, ans: Record<string, unknown>, rows?: (string | null)[][]) {
  await waitFor(() => expect(runs(ws).length).toBeGreaterThanOrEqual(n))
  const r = runs(ws)[n - 1]!
  if (rows) {
    // Cols arrive before the rows, as from the agent.
    ws.answer({ id: r.id, type: 'cols', cols: COLS })
    ws.push(batch(r.id as number, rows))
  }
  ws.answer({ id: r.id, ...ans })
  return r
}

describe('Saving row edits', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.stubGlobal('WebSocket', FakeSocket)
  })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  async function changeNote(ws: FakeSocket) {
    await selectOrders(ws)
    const seen = new Set<unknown>()
    await waitFor(() => { answerLookups(ws, seen); expect(screen.getByText(/double-click a cell/)).toBeInTheDocument() })
    const before = runs(ws).length
    fireEvent.doubleClick(screen.getByText('first').closest('td')!)
    const input = screen.getByLabelText('New value for note')
    fireEvent.change(input, { target: { value: 'changed' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    fireEvent.click(within(screen.getByRole('region', { name: 'Unsaved changes' })).getByRole('button', { name: 'Save' }))
    return before
  }

  it('wraps the edits in a transaction and shows what Postgres returned', async () => {
    const ws = await open()
    const n = await changeNote(ws)
    expect((await next(ws, n + 1, { type: 'done', tag: 'BEGIN' })).sql).toBe('BEGIN')
    const upd = await next(ws, n + 2, { type: 'done', tag: 'UPDATE 1' }, [['1', 'changed']])
    expect(upd.sql).toContain('UPDATE public.orders\nSET note = CAST($1 AS text)')
    expect(upd.params).toEqual(['changed', '1', 'first'])
    expect((await next(ws, n + 3, { type: 'done', tag: 'COMMIT' })).sql).toBe('COMMIT')
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Unsaved changes' })).toBeNull())
    expect(screen.getByText('changed')).toBeInTheDocument()
  })

  it('rolls back when the row changed since it was read', async () => {
    const ws = await open()
    const n = await changeNote(ws)
    await next(ws, n + 1, { type: 'done', tag: 'BEGIN' })
    await next(ws, n + 2, { type: 'done', tag: 'UPDATE 0' }, [])
    expect((await next(ws, n + 3, { type: 'done', tag: 'ROLLBACK' })).sql).toBe('ROLLBACK')
    expect(await screen.findByText(/Nothing was saved\..*someone changed it since you read it/)).toBeInTheDocument()
    await new Promise((r) => setTimeout(r, 30))
    expect(runs(ws).some((r) => r.sql === 'COMMIT')).toBe(false)
  })

  it('on production, asks for approval for the whole save and sends it again under it', async () => {
    live.askStatements.mockResolvedValue({ requestId: 'r1', databaseId: 'd1', databaseName: 'orders', items: [], state: 'pending' })
    live.statementAsk.mockResolvedValue({ requestId: 'r1', state: 'approved', decidedBy: 'budi', items: [] })
    const ws = await open()
    const n = await changeNote(ws)
    await next(ws, n + 1, { type: 'done', tag: 'BEGIN' })
    await next(ws, n + 2, { type: 'error', code: 'AR007', error: 'orders is production: a statement that changes it runs once someone else approves it' })
    expect((await next(ws, n + 3, { type: 'done', tag: 'ROLLBACK' })).sql).toBe('ROLLBACK')
    fireEvent.change(await screen.findByLabelText(/Why/), { target: { value: 'fixing the note on order 1 for support' } })
    vi.useFakeTimers({ shouldAdvanceTime: true })
    fireEvent.click(screen.getByRole('button', { name: 'Ask for approval' }))
    await waitFor(() => expect(live.askStatements).toHaveBeenCalled())
    const asked = live.askStatements.mock.calls[0]![1] as { items: { sql: string; params: unknown[] }[] }
    expect(asked.items).toHaveLength(1)
    expect(asked.items[0]!.params).toEqual(['changed', '1', 'first'])
    await act(async () => { await vi.advanceTimersByTimeAsync(3100) })
    vi.useRealTimers()
    await next(ws, n + 4, { type: 'done', tag: 'BEGIN' })
    const upd = await next(ws, n + 5, { type: 'done', tag: 'UPDATE 1' }, [['1', 'changed']])
    expect(upd.approval).toBe('r1')
    expect((await next(ws, n + 6, { type: 'done', tag: 'COMMIT' })).sql).toBe('COMMIT')
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Unsaved changes' })).toBeNull())
  })

  it('offers no editing to read access', async () => {
    const ws = await open('read')
    await selectOrders(ws)
    await waitFor(() => expect(screen.getByText('second')).toBeInTheDocument())
    expect(screen.queryByText(/double-click a cell/)).toBeNull()
    expect(runs(ws).some((r) => r.sql === TARGET_SQL)).toBe(false)
  })
})
