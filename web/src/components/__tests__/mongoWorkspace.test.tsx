import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { MongoWorkspace, formatMongo, shellValue } from '~/components/MongoWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'

/**
 * The MongoDB workspace: it lists the collections it may see, opens one by
 * asking for its documents, and asks for the server's name before a command
 * the agent runs only confirmed, sending the same command again with it.
 */

class FakeSocket {
  static last: FakeSocket | null = null
  static OPEN = 1
  readyState = 1
  sent: Record<string, unknown>[] = []
  onmessage: ((ev: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  url: string
  constructor(url: string) { this.url = url; FakeSocket.last = this }
  send(frame: string) { this.sent.push((JSON.parse(frame) as { req: Record<string, unknown> }).req) }
  close() {}
  push(m: unknown) { act(() => { this.onmessage?.({ data: JSON.stringify(m) }) }) }
  answer(ans: Record<string, unknown>) { this.push({ type: 'ans', ans }) }
}

const db: Database = {
  id: 'm1', assetId: 'a1', hostname: 'docs-01', engine: 'mongodb', host: '127.0.0.1', port: 27017,
  poolerHost: '', poolerPort: 0, dbname: 'shop', adminUser: '', label: 'production', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

describe('MongoWorkspace', () => {
  beforeEach(() => { vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('lists collections, opens one, and confirms a wipe by the server name', async () => {
    renderWithProviders(
      <MongoWorkspace database={db} access={{ level: 'write' }} endpoint="/api/v1/postgres"
        getTicket={async () => ({ ticket: 't', role: 'ana', level: 'write' })} />,
    )
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'mongodb', level: 'write', user: 'argus-ana-write', version: '8.0.4', ops: ['run', 'scan', 'value'] })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'scan')).toBe(true))
    const scan = ws.sent.find((r) => r.op === 'scan')!
    ws.answer({ id: scan.id, type: 'result', keys: [{ name: 'orders', type: 'collection', ttl: -1, count: 2140 }], total: 1 })

    fireEvent.click(await screen.findByText('orders'))
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'value')).toBe(true))
    const value = ws.sent.find((r) => r.op === 'value')!
    expect(value).toMatchObject({ key: 'orders' })
    ws.answer({ id: value.id, type: 'result', data: [{ _id: { $oid: '65a1b2c3d4e5f60718293a4b' }, number: 'NW-1' }], total: 2140, truncated: true })
    expect(await screen.findByText('2,140 documents · showing the first 1')).toBeInTheDocument()

    const input = screen.getByLabelText('MongoDB command')
    fireEvent.change(input, { target: { value: 'db.orders.deleteMany({})' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'run')).toBe(true))
    const first = ws.sent.find((r) => r.op === 'run')!
    expect(first.confirm).toBeUndefined()
    ws.answer({ id: first.id, type: 'error', needsConfirm: true, error: 'Deleting every document needs the server’s name' })

    const name = await screen.findByLabelText('Server name')
    fireEvent.change(name, { target: { value: 'docs-01' } })
    fireEvent.click(screen.getByRole('button', { name: 'Do it' }))
    await waitFor(() => expect(ws.sent.filter((r) => r.op === 'run')).toHaveLength(2))
    const second = ws.sent.filter((r) => r.op === 'run')[1]!
    expect(second).toMatchObject({ command: 'db.orders.deleteMany({})', confirm: 'docs-01' })
    ws.answer({ id: second.id, type: 'result', data: [{ n: 2140, ok: 1 }], tag: 'delete 2140', ms: 41 })
    expect(await screen.findByText('delete 2140 · 41 ms')).toBeInTheDocument()
  })

  it('shows documents as mongosh does', () => {
    expect(shellValue({ _id: { $oid: '65a1b2c3d4e5f60718293a4b' }, at: { $date: '2026-11-06T05:58:11Z' }, 'a b': [1, 2] }))
      .toBe('{ _id: ObjectId("65a1b2c3d4e5f60718293a4b"), at: ISODate("2026-11-06T05:58:11Z"), "a b": [ 1, 2 ] }')
    const long = shellValue({ note: 'x'.repeat(120), n: 1 })
    expect(long.split('\n')).toHaveLength(4)
  })

  it('formats documents and refusals for the terminal', () => {
    expect(formatMongo({ id: 1, type: 'result', data: [] })).toBe('(no documents)')
    expect(formatMongo({ id: 1, type: 'result', data: [{ a: 1 }], truncated: true })).toContain('more documents than shown')
    expect(formatMongo({ id: 1, type: 'error', error: 'not authorized on shop to execute command { insert: "orders" }' }))
      .toBe('(refused) not authorized on shop to execute command { insert: "orders" }')
  })
})
