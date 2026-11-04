import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseWorkspace } from '~/components/DatabaseWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'
import { setEditor } from '~/test/editor'

/**
 * A pasted script runs one statement per request, in order, each waiting for
 * the one before: the agent's extended protocol takes one statement, and each
 * one is recorded, guarded and checked on its own. The script stops at the
 * first failure, so a COMMIT after a failed statement is never sent, and a
 * write to a real table still stops to be counted and confirmed.
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

const runs = (ws: FakeSocket) => ws.sent.filter((r) => r.op === 'run')

/** Waits for the next run request, checks its text, and answers it. */
async function step(ws: FakeSocket, n: number, sql: string, ans: Record<string, unknown>) {
  await waitFor(() => expect(runs(ws)).toHaveLength(n))
  const r = runs(ws)[n - 1]!
  expect(r.sql).toBe(sql)
  if (ans.cols) ws.answer({ id: r.id, type: 'cols', cols: ans.cols })
  ws.answer({ id: r.id, ...ans, cols: undefined })
}

const script = `-- Clean up, then roll back.
BEGIN;
CREATE TEMP TABLE input_class (id uuid PRIMARY KEY) ON COMMIT DROP;
INSERT INTO input_class SELECT id FROM core.classes WHERE code = 'x;y';
CREATE FUNCTION pg_temp.f() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END $$;
SELECT pg_temp.f() AS one;
ROLLBACK;`

describe('Running a script', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.stubGlobal('WebSocket', FakeSocket)
  })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('sends each statement on its own, in order, and shows every answer', async () => {
    const ws = await open()
    type(script)
    // step waits for exactly n requests: nothing goes ahead of the answer before it.
    await step(ws, 1, 'BEGIN', { type: 'done', tag: 'BEGIN' })
    await step(ws, 2, 'CREATE TEMP TABLE input_class (id uuid PRIMARY KEY) ON COMMIT DROP', { type: 'done', tag: 'CREATE TABLE' })

    // The INSERT … SELECT is counted first; it filled only the session's
    // temp table, so the script goes on without asking.
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'check')).toBe(true))
    const chk = ws.sent.find((r) => r.op === 'check')!
    expect(chk.sql).toBe("INSERT INTO input_class SELECT id FROM core.classes WHERE code = 'x;y'")
    ws.answer({ id: chk.id, type: 'check', check: { class: { verb: 'insert', writes: true }, rows: 3, tables: [{ name: 'pg_temp_3.input_class', inserted: 3 }] } })
    await step(ws, 3, "INSERT INTO input_class SELECT id FROM core.classes WHERE code = 'x;y'", { type: 'done', tag: 'INSERT 0 3', rows: 3 })
    expect(screen.queryByText(/Check before it runs/)).toBeNull()

    await step(ws, 4, 'CREATE FUNCTION pg_temp.f() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END $$', { type: 'done', tag: 'CREATE FUNCTION' })
    await step(ws, 5, 'SELECT pg_temp.f() AS one', { type: 'done', tag: 'SELECT 0', cols: [{ name: 'one', type: 'int4' }] })
    await step(ws, 6, 'ROLLBACK', { type: 'done', tag: 'ROLLBACK' })

    expect(await screen.findByText('All 6 statements ran.')).toBeInTheDocument()
    expect(screen.getByLabelText('Statement 1: BEGIN')).toBeInTheDocument()
    expect(screen.getByLabelText('Statement 6: ROLLBACK')).toBeInTheDocument()
    // It ends showing the last answer with columns, the SELECT.
    expect(screen.getByText('one')).toBeInTheDocument()
    fireEvent.click(screen.getByLabelText('Statement 2: CREATE TABLE'))
    expect(screen.queryByText('one')).toBeNull()
  })

  it('stops at the first failure and never sends what comes after, a COMMIT least of all', async () => {
    const ws = await open()
    type('BEGIN;\nSELECT nope;\nDELETE FROM orders WHERE id = 1;\nCOMMIT;')
    await step(ws, 1, 'BEGIN', { type: 'done', tag: 'BEGIN' })
    await step(ws, 2, 'SELECT nope', { type: 'error', code: '42703', error: 'column "nope" does not exist' })
    expect(await screen.findByText(/Stopped at statement 2 of 4: Postgres refused it. The 2 statements after it did not run. Your transaction is still open/)).toBeInTheDocument()
    expect(screen.getByText(/column "nope" does not exist/)).toBeInTheDocument()
    await new Promise((r) => setTimeout(r, 50))
    expect(runs(ws)).toHaveLength(2)
    expect(ws.sent.some((r) => r.op === 'check')).toBe(false)
  })

  it('stops to count a write to a real table, and "Stop the script" sends nothing more', async () => {
    const ws = await open()
    type('BEGIN;\nDELETE FROM orders WHERE created_at < now();\nCOMMIT;')
    await step(ws, 1, 'BEGIN', { type: 'done', tag: 'BEGIN' })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'check')).toBe(true))
    const chk = ws.sent.find((r) => r.op === 'check')!
    ws.answer({ id: chk.id, type: 'check', check: { class: { verb: 'delete', targets: ['orders'], hasWhere: true, writes: true }, rows: 40, tables: [{ name: 'public.orders', deleted: 40, estimate: 1000 }] } })
    expect(await screen.findByText(/Statement 2 of 3 in your script/)).toBeInTheDocument()
    expect(runs(ws)).toHaveLength(1)
    fireEvent.click(screen.getByRole('button', { name: 'Stop the script' }))
    expect(await screen.findByText(/Stopped before statement 2 of 3, as you chose/)).toBeInTheDocument()
    expect(runs(ws)).toHaveLength(1)
  })

  it('goes on after the person confirms the count', async () => {
    const ws = await open()
    type('BEGIN;\nDELETE FROM orders WHERE created_at < now();\nROLLBACK;')
    await step(ws, 1, 'BEGIN', { type: 'done', tag: 'BEGIN' })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'check')).toBe(true))
    const chk = ws.sent.find((r) => r.op === 'check')!
    ws.answer({ id: chk.id, type: 'check', check: { class: { verb: 'delete', targets: ['orders'], hasWhere: true, writes: true }, rows: 40, tables: [{ name: 'public.orders', deleted: 40, estimate: 1000 }] } })
    fireEvent.click(await screen.findByRole('button', { name: 'Run it: 40 rows' }))
    await step(ws, 2, 'DELETE FROM orders WHERE created_at < now()', { type: 'done', tag: 'DELETE 40', rows: 40 })
    await step(ws, 3, 'ROLLBACK', { type: 'done', tag: 'ROLLBACK' })
    expect(await screen.findByText('All 3 statements ran.')).toBeInTheDocument()
  })

  it('runs a single statement as before, with no script strip', async () => {
    const ws = await open()
    type('-- note\nSELECT 1;')
    await step(ws, 1, 'SELECT 1', { type: 'done', tag: 'SELECT 1', cols: [{ name: '?column?', type: 'int4' }] })
    expect(screen.queryByText('Script')).toBeNull()
  })
})
