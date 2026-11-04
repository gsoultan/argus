import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'
import { editorView, setEditor } from '~/test/editor'

/**
 * The query editor in the Postgres workspace: :name parameters go as bind
 * values, Postgres's error position is marked in the text, and Format lays
 * the statement out.
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


describe('Query editor', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.stubGlobal('WebSocket', FakeSocket)
  })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('sends :name parameters as bind values, never in the text', async () => {
    const ws = await open()
    setEditor("SELECT * FROM orders WHERE customer = :customer AND note = :note OR customer = :customer")
    const field = await screen.findByLabelText('Value for :customer')
    expect(screen.getByLabelText('Value for :note')).toBeInTheDocument()
    fireEvent.change(field, { target: { value: "x'; DROP TABLE orders; --" } })
    fireEvent.keyDown(screen.getByLabelText('SQL'), { key: 'Enter', ctrlKey: true })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'run' && typeof r.sql === 'string' && (r.sql as string).includes('$1'))).toBe(true))
    const run = ws.sent.find((r) => r.op === 'run' && typeof r.sql === 'string' && (r.sql as string).includes('$1'))!
    expect(run.sql).toBe('SELECT * FROM orders WHERE customer = $1 AND note = $2 OR customer = $1')
    expect(run.params).toEqual(["x'; DROP TABLE orders; --", null])
  })

  it('marks where Postgres says the statement failed, and clears the mark on edit', async () => {
    const ws = await open()
    setEditor('SELECT nope FROM orders')
    fireEvent.keyDown(screen.getByLabelText('SQL'), { key: 'Enter', ctrlKey: true })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'run' && r.sql === 'SELECT nope FROM orders')).toBe(true))
    const run = ws.sent.find((r) => r.op === 'run' && r.sql === 'SELECT nope FROM orders')!
    ws.answer({ id: run.id, type: 'error', code: '42703', error: 'column "nope" does not exist', position: 8 })
    await waitFor(() => expect(document.querySelector('.cm-lintRange-error')).not.toBeNull())
    expect(document.querySelector('.cm-lintRange-error')!.textContent).toBe('nope')
    setEditor('SELECT id FROM orders')
    await waitFor(() => expect(document.querySelector('.cm-lintRange-error')).toBeNull())
  })

  it('marks the right place when only a selection ran with parameters in it', async () => {
    const ws = await open()
    const text = 'SELECT 1;\nSELECT :who, nope FROM orders'
    setEditor(text)
    const from = text.indexOf('SELECT :who')
    act(() => { editorView().dispatch({ selection: { anchor: from, head: text.length } }) })
    fireEvent.keyDown(screen.getByLabelText('SQL'), { key: 'Enter', ctrlKey: true })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'run' && r.sql === 'SELECT $1, nope FROM orders')).toBe(true))
    const run = ws.sent.find((r) => r.op === 'run' && r.sql === 'SELECT $1, nope FROM orders')!
    ws.answer({ id: run.id, type: 'error', code: '42703', error: 'column "nope" does not exist', position: 12 })
    await waitFor(() => expect(document.querySelector('.cm-lintRange-error')?.textContent).toBe('nope'))
  })

  it('formats the statement and keeps its parameters', async () => {
    await open()
    setEditor('select id,total from orders where customer=:customer')
    fireEvent.click(screen.getByRole('button', { name: /Format/ }))
    await waitFor(() => expect(editorView().state.doc.toString()).toContain('\n'))
    const out = editorView().state.doc.toString()
    expect(out).toContain(':customer')
    expect(out.replace(/\s+/g, ' ').trim()).toBe('select id, total from orders where customer = :customer')
  })
})
