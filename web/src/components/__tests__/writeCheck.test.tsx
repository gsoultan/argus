import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'
import { setEditor } from '~/test/editor'

/**
 * The guarded write path in the Postgres workspace. A DELETE is counted
 * before it runs (op "check"), the count is shown, and a DELETE without WHERE
 * is only sent on, carrying the agent's phrase, once the person has typed it
 * exactly. A DROP the agent stopped opens the same dialog. The agent is the
 * one that refuses; this pins that the console asks instead of running, and
 * never fills the phrase in for the person.
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
  push(m: unknown) { act(() => { this.onmessage?.({ data: JSON.stringify(m) }) }) }
  answer(ans: Record<string, unknown>) { this.push({ type: 'ans', ans }) }
}

const db: Database = {
  id: 'd1', assetId: 'a1', hostname: 'core-db-01', engine: 'postgres', host: '127.0.0.1', port: 5432,
  poolerHost: '', poolerPort: 0, dbname: 'orders', adminUser: '', label: 'production', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

async function open() {
  renderWithProviders(
    <DatabaseWorkspace database={db} access={{ level: 'owner' }} endpoint="/api/v1/postgres"
      getTicket={async () => ({ ticket: 't', role: 'ana', level: 'owner' })} />,
  )
  await waitFor(() => expect(FakeSocket.last).not.toBeNull())
  const ws = FakeSocket.last!
  ws.push({ type: 'ready' })
  ws.answer({ id: 0, type: 'ready', level: 'owner', role: 'ana@owner', version: '16.4' })
  return ws
}

function type(sql: string) {
  setEditor(sql)
  fireEvent.keyDown(screen.getByLabelText('SQL'), { key: 'Enter', ctrlKey: true })
}

describe('Check before it runs', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.stubGlobal('WebSocket', FakeSocket)
  })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('counts a DELETE first and sends it without WHERE only once the phrase is typed', async () => {
    const ws = await open()
    type('DELETE FROM orders')
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'check')).toBe(true))
    expect(ws.sent.some((r) => r.op === 'run' && r.sql === 'DELETE FROM orders')).toBe(false)
    const chk = ws.sent.find((r) => r.op === 'check')!
    ws.answer({
      id: chk.id, type: 'check', rows: 221480,
      check: {
        class: { verb: 'delete', targets: ['orders'], writes: true }, rows: 221480,
        tables: [{ name: 'public.orders', deleted: 221480, estimate: 221480 }, { name: 'public.order_items', deleted: 612 }],
        guard: { code: 'AR001', verb: 'delete', targets: ['orders'], confirm: 'orders/orders', reason: 'This DELETE has no WHERE, so it changes every row of orders.' },
      },
    })
    expect(await screen.findByText('221,480')).toBeInTheDocument()
    expect(screen.getByText(/public.order_items: 612 deleted/)).toBeInTheDocument()
    const go = screen.getByRole('button', { name: /Run it: 221,480 rows/ })
    expect(go).toBeDisabled()
    const phrase = screen.getByLabelText(/to go ahead/)
    fireEvent.change(phrase, { target: { value: 'orders/order' } })
    expect(go).toBeDisabled()
    fireEvent.change(phrase, { target: { value: 'orders/orders' } })
    expect(go).not.toBeDisabled()
    fireEvent.click(go)
    await waitFor(() => expect(ws.sent.find((r) => r.op === 'run' && r.sql === 'DELETE FROM orders')).toMatchObject({ confirm: 'orders/orders' }))
  })

  it('runs a checked DELETE with a WHERE with no phrase at all', async () => {
    const ws = await open()
    type('DELETE FROM orders WHERE id = 48199')
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'check')).toBe(true))
    const chk = ws.sent.find((r) => r.op === 'check')!
    ws.answer({ id: chk.id, type: 'check', check: { class: { verb: 'delete', targets: ['orders'], hasWhere: true, writes: true }, rows: 1, tables: [{ name: 'public.orders', deleted: 1, estimate: 221480 }] } })
    fireEvent.click(await screen.findByRole('button', { name: 'Run it: 1 row' }))
    await waitFor(() => expect(ws.sent.find((r) => r.op === 'run' && r.sql === 'DELETE FROM orders WHERE id = 48199')).toBeTruthy())
    expect(ws.sent.find((r) => r.op === 'run' && r.sql === 'DELETE FROM orders WHERE id = 48199')!.confirm).toBeUndefined()
  })

  it('opens the dialog when the agent stops a DROP, with what depends on it', async () => {
    const ws = await open()
    type('DROP TABLE orders')
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'run' && r.sql === 'DROP TABLE orders')).toBe(true))
    const run = ws.sent.find((r) => r.op === 'run' && r.sql === 'DROP TABLE orders')!
    expect(run.confirm).toBeUndefined()
    ws.answer({ id: run.id, type: 'error', code: 'AR002', error: 'DROP TABLE removes orders and everything in it.',
      guard: { code: 'AR002', verb: 'drop', object: 'table', targets: ['orders'], confirm: 'orders/orders', reason: 'DROP TABLE removes orders and everything in it.' } })
    expect(await screen.findByText('Drop table orders?')).toBeInTheDocument()
    // The size and dependents are read as the person's role.
    await waitFor(() => expect(ws.sent.some((r) => typeof r.sql === 'string' && (r.sql as string).includes('pg_total_relation_size'))).toBe(true))
    const go = screen.getByRole('button', { name: 'Drop it' })
    expect(go).toBeDisabled()
    fireEvent.change(screen.getByLabelText(/to go ahead/), { target: { value: 'orders/orders' } })
    fireEvent.click(go)
    await waitFor(() => expect(ws.sent.filter((r) => r.op === 'run' && r.sql === 'DROP TABLE orders')).toHaveLength(2))
    expect(ws.sent.filter((r) => r.op === 'run' && r.sql === 'DROP TABLE orders')[1]).toMatchObject({ confirm: 'orders/orders' })
  })

  it('offers no way through for DROP DATABASE', async () => {
    const ws = await open()
    type('DROP DATABASE orders')
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'run' && r.sql === 'DROP DATABASE orders')).toBe(true))
    const run = ws.sent.find((r) => r.op === 'run' && r.sql === 'DROP DATABASE orders')!
    ws.answer({ id: run.id, type: 'error', code: 'AR003', error: 'A database is never dropped from a session.',
      guard: { code: 'AR003', verb: 'drop', object: 'database', targets: ['orders'], reason: 'A database is never dropped from a session.' } })
    expect(await screen.findByText('Not from a session')).toBeInTheDocument()
    expect(screen.queryByLabelText(/to go ahead/)).toBeNull()
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument()
  })
})
