import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'
import { setEditor } from '~/test/editor'

/**
 * A value too long for a row batch arrives cut, and its rest follows as
 * part answers: the console asks for them and joins them into the cell.
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
  push(m: unknown) { act(() => { this.onmessage?.({ data: m instanceof ArrayBuffer ? m : JSON.stringify(m) }) }) }
  answer(ans: Record<string, unknown>) { this.push({ type: 'ans', ans }) }
}

/** A row batch; a cell given as [text] is marked cut. */
function batch(id: number, rows: (string | null | [string])[][]): ArrayBuffer {
  const out: number[] = [(id >>> 24) & 255, (id >>> 16) & 255, (id >>> 8) & 255, id & 255]
  const enc = new TextEncoder()
  for (const r of rows) for (const c of r) {
    if (c === null) { out.push(0); continue }
    const cut = Array.isArray(c)
    const b = enc.encode(cut ? c[0] : c)
    let v = b.length * 2 + (cut ? 1 : 0) + 1
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

describe('A long value', () => {
  beforeEach(() => { localStorage.clear(); vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('arrives whole: the batch cuts it, the parts complete it', async () => {
    renderWithProviders(
      <DatabaseWorkspace database={db} access={{ level: 'read' }} endpoint="/api/v1/postgres"
        getTicket={async () => ({ ticket: 't', role: 'ana', level: 'read' })} />,
    )
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', level: 'read', role: 'ana@read', version: '16.4' })

    setEditor('SELECT id, rules FROM approval_rules')
    fireEvent.keyDown(screen.getByLabelText('SQL'), { key: 'Enter', ctrlKey: true })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'run')).toBe(true))
    const run = ws.sent.find((r) => r.op === 'run')!
    expect(run.whole).toBe(true)
    const id = run.id as number

    const doc = JSON.stringify(Array.from({ length: 3 }, (_, i) => ({ name: `rule ${i}`, then: ['SUBMIT'] })))
    ws.answer({ id, type: 'cols', cols: [{ name: 'id', type: 'integer' }, { name: 'rules', type: 'jsonb' }] })
    ws.push(batch(id, [['1', [doc.slice(0, 20)]]]))
    expect(await screen.findByText('cut')).toBeInTheDocument()
    ws.answer({ id, type: 'part', part: { row: 0, col: 1, text: doc.slice(20, 50) } })
    ws.answer({ id, type: 'part', part: { row: 0, col: 1, text: doc.slice(50), last: true } })
    ws.answer({ id, type: 'done', tag: 'SELECT 1', rows: 1, ms: 3 })
    await waitFor(() => expect(screen.queryByText('cut')).not.toBeInTheDocument())

    fireEvent.click(screen.getByRole('button', { name: 'Open row 1' }))
    const dialog = await screen.findByRole('dialog')
    // Each item of the list is named in the tree; as a table, an item a row.
    expect(await within(dialog).findByText('rule 2')).toBeInTheDocument()
    fireEvent.click(within(dialog).getByText('Table'))
    const table = await within(dialog).findByRole('table', { name: 'As a table' })
    expect(within(table).getByText('rule 2')).toBeInTheDocument()
  })
})
