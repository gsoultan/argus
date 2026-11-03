import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { PostgresReplay, STATEMENT_PREVIEW_CHARS } from '~/components/PostgresReplay'
import { StatementWatch } from '~/components/StatementWatch'
import { renderWithProviders } from '~/test/render'

/**
 * Watching a database session shows its statements as they are recorded.
 * It used to open a terminal, which drew the result rows as text and never
 * a statement. And a replay shows a long statement whole when asked: it was
 * cut at 4,000 characters with no way to see the rest.
 */

class FakeSocket {
  static all: FakeSocket[] = []
  sent: unknown[] = []
  onmessage: ((ev: { data: unknown }) => void) | null = null
  onerror: (() => void) | null = null
  onclose: (() => void) | null = null
  url: URL
  constructor(url: string) { this.url = new URL(url); FakeSocket.all.push(this) }
  send(frame: unknown) { this.sent.push(frame) }
  close() {}
  push(m: unknown) { act(() => { this.onmessage?.({ data: JSON.stringify(m) }) }) }
  pushBinary(b: ArrayBuffer) { act(() => { this.onmessage?.({ data: b }) }) }
}

const line = (o: Record<string, unknown>) => JSON.stringify({ t: '2026-11-03T10:00:00Z', prev: '', hash: 'h', ...o })

describe('StatementWatch', () => {
  beforeEach(() => {
    FakeSocket.all = []
    vi.stubGlobal('WebSocket', FakeSocket)
  })
  afterEach(() => vi.unstubAllGlobals())

  it('shows each statement and its answer as it is recorded, and nothing it returned', async () => {
    renderWithProviders(
      <StatementWatch endpoint="/api/v1/sessions/s1/watch" sessionId="s1" getTicket={async () => 'tk'} />,
    )
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    const ws = FakeSocket.all[0]!
    expect(ws.url.searchParams.get('ticket')).toBe('tk')
    ws.push({ type: 'ready', session: 's1' })
    expect(await screen.findByText('read only · live')).toBeTruthy()

    ws.push({ type: 'record', data: line({ seq: 1, kind: 'open', role: 'ana_sari', level: 'read' }) })
    ws.push({ type: 'record', data: line({ seq: 2, kind: 'stmt', req: 1, sql: 'SELECT email FROM customers' }) })
    expect(await screen.findByText('SELECT email FROM customers')).toBeTruthy()
    expect(screen.getByText('running')).toBeTruthy()

    // Binary is terminal output or result rows; neither is drawn here.
    ws.pushBinary(new TextEncoder().encode('alice@example.com').buffer)
    ws.push({ type: 'record', data: line({ seq: 3, kind: 'done', req: 1, tag: 'SELECT 2', rows: 2 }) })
    expect(await screen.findByText('SELECT 2')).toBeTruthy()
    expect(screen.queryByText('running')).toBeNull()
    expect(screen.queryByText(/alice@example.com/)).toBeNull()
    expect(ws.sent).toHaveLength(0)

    ws.push({ type: 'closed', data: 'the session ended' })
    expect(await screen.findByText('the session ended')).toBeTruthy()
  })

  it('says why when the view is refused', async () => {
    renderWithProviders(
      <StatementWatch endpoint="/api/v1/sessions/s1/watch" sessionId="s1" getTicket={async () => 'tk'} />,
    )
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    FakeSocket.all[0]!.push({ type: 'error', data: 'not authorised to watch this session' })
    expect(await screen.findByText('not authorised to watch this session')).toBeTruthy()
  })
})

describe('PostgresReplay', () => {
  it('shows a long statement whole when asked', () => {
    const sql = 'INSERT INTO t VALUES ' + "('x'),".repeat(2000) + "('end')"
    renderWithProviders(
      <PostgresReplay text={[
        line({ seq: 1, kind: 'open' }),
        line({ seq: 2, kind: 'stmt', req: 1, sql }),
      ].join('\n')} />,
    )
    expect(screen.queryByText(sql)).toBeNull()
    fireEvent.click(screen.getByText(`Show all ${sql.length.toLocaleString()} characters`))
    expect(screen.getByText(sql)).toBeTruthy()
    expect(sql.length).toBeGreaterThan(STATEMENT_PREVIEW_CHARS)
  })
})
