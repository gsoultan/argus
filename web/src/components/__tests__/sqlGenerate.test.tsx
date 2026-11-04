import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { renderWithProviders } from '~/test/render'
import type { DBLevel, Database } from '~/types/domain'
import { TABLE_DDL_SQL } from '~/lib/sqlGen'
import { TARGET_BY_NAME_SQL } from '~/lib/rowEdit'

/**
 * The schema tree's generators: an object's DDL, or a statement for a table,
 * opens in a new tab; the table designer writes a CREATE TABLE. Nothing runs
 * but the catalogue reads.
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

function batch(id: number, rows: (string | null)[][]): ArrayBuffer {
  const out: number[] = [(id >>> 24) & 255, (id >>> 16) & 255, (id >>> 8) & 255, id & 255]
  const enc = new TextEncoder()
  for (const r of rows) for (const c of r) {
    if (c === null) { out.push(0); continue }
    const b = enc.encode(c)
    let v = b.length * 2 + 1
    while (v >= 0x80) { out.push((v & 0x7f) | 0x80); v = Math.floor(v / 128) }
    out.push(v, ...b)
  }
  return new Uint8Array(out).buffer
}

const db: Database = {
  id: 'd1', assetId: 'a1', hostname: 'core-db-01', engine: 'postgres', host: '127.0.0.1', port: 5432,
  poolerHost: '', poolerPort: 0, dbname: 'orders', adminUser: '', label: 'staging', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

async function open(level: DBLevel = 'owner') {
  renderWithProviders(
    <DatabaseWorkspace database={db} access={{ level }} endpoint="/api/v1/postgres"
      getTicket={async () => ({ ticket: 't', role: 'ana', level })} />,
  )
  await waitFor(() => expect(FakeSocket.last).not.toBeNull())
  const ws = FakeSocket.last!
  ws.push({ type: 'ready' })
  ws.answer({ id: 0, type: 'ready', level, role: `ana@${level}`, version: '16.4' })
  // The tree: one schema with one table.
  await waitFor(() => expect(ws.sent.some((r) => r.op === 'schema' && r.kind === 'schemas')).toBe(true))
  for (const r of ws.sent.filter((x) => x.op === 'schema' && x.kind === 'schemas')) ws.answer({ id: r.id, type: 'schema', objects: [{ name: 'public', kind: 'schema' }] })
  await waitFor(() => expect(ws.sent.some((r) => r.op === 'schema' && r.kind === 'objects')).toBe(true))
  for (const r of ws.sent.filter((x) => x.op === 'schema' && x.kind === 'objects')) ws.answer({ id: r.id, type: 'schema', objects: [{ name: 'orders', kind: 'table' }] })
  return ws
}

const runOf = (ws: FakeSocket, sql: string) => ws.sent.find((r) => r.op === 'run' && r.sql === sql)

async function answerRows(ws: FakeSocket, sql: string, rows: (string | null)[][], ncols: number) {
  await waitFor(() => expect(runOf(ws, sql)).toBeDefined())
  const r = runOf(ws, sql)!
  ws.answer({ id: r.id, type: 'cols', cols: Array.from({ length: ncols }, (_, i) => ({ name: `c${i}`, type: 'text' })) })
  ws.push(batch(r.id as number, rows))
  ws.answer({ id: r.id, type: 'done', tag: `SELECT ${rows.length}` })
  return r
}

describe('Generators in the schema tree', () => {
  beforeEach(() => { localStorage.clear(); vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('opens a table\'s DDL in a new tab', async () => {
    const ws = await open()
    fireEvent.click(await screen.findByRole('button', { name: 'Generate for orders' }))
    fireEvent.click(await screen.findByText(/DDL \(CREATE TABLE\)/))
    const r = await answerRows(ws, TABLE_DDL_SQL, [['CREATE TABLE public.orders (\n    id bigint\n);']], 1)
    expect(r.params).toEqual(['public.orders'])
    expect(await screen.findByText('orders DDL')).toBeInTheDocument()
  })

  it('writes an UPDATE for the table with parameters for its values', async () => {
    const ws = await open()
    fireEvent.click(await screen.findByRole('button', { name: 'Generate for orders' }))
    fireEvent.click(await screen.findByText('UPDATE'))
    await answerRows(ws, TARGET_BY_NAME_SQL, [
      ['public.orders', '1', 'id', 'bigint', 'true', 'true', 'false', null, '1', 'a'],
      ['public.orders', '2', 'note', 'text', 'false', 'false', 'false', null, '1', ''],
    ], 10)
    expect(await screen.findByText('update orders')).toBeInTheDocument()
    // The tab's parameters show a field for each :name.
    expect(await screen.findByLabelText('Value for :note')).toBeInTheDocument()
    expect(screen.getByLabelText('Value for :id')).toBeInTheDocument()
    expect(ws.sent.some((r) => r.op === 'run' && typeof r.sql === 'string' && /^UPDATE/.test(r.sql))).toBe(false)
  })

  it('designs a new table and opens its CREATE TABLE', async () => {
    await open()
    fireEvent.click(await screen.findByRole('button', { name: 'New table in public' }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByLabelText('Table name'), { target: { value: 'invoices' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add column' }))
    fireEvent.change(within(dialog).getByLabelText('Name of column 2'), { target: { value: 'amount' } })
    expect(within(dialog).getByText((_, el) => el?.tagName === 'PRE' && el.textContent ===
      'CREATE TABLE public.invoices (\n    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,\n    amount text\n);')).toBeInTheDocument()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Open in a new tab' }))
    expect(await screen.findByText('create invoices')).toBeInTheDocument()
  })

  it('offers no designer at read access', async () => {
    await open('read')
    fireEvent.click(await screen.findByRole('button', { name: 'Generate for orders' }))
    expect(await screen.findByText(/DDL \(CREATE TABLE\)/)).toBeInTheDocument()
    expect(screen.queryByText('Design table…')).toBeNull()
    expect(screen.queryByRole('button', { name: 'New table in public' })).toBeNull()
  })
})
