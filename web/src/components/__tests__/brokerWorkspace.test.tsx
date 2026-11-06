import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { BrokerWorkspace, type BrokerTab } from '~/components/broker/BrokerWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'

vi.mock('@tanstack/react-router', () => ({
  Link: ({ children, to }: { children: React.ReactNode; to: string }) => <a href={to}>{children}</a>,
}))
vi.mock('~/components/links', () => ({
  ButtonLink: ({ children }: { children: React.ReactNode }) => <a>{children}</a>,
}))

/**
 * The broker workspace against a fake session: the overview says which queue
 * has nobody consuming it, and "Send them back" on a dead-letter queue asks
 * the agent to move its messages to where they died -- one recorded
 * operation, named as the agent expects it, and only at a level that has it.
 */

class FakeSocket {
  static last: FakeSocket | null = null
  static OPEN = 1
  readyState = 1
  sent: Record<string, unknown>[] = []
  answered = new Set<unknown>()
  onmessage: ((ev: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  url: string
  constructor(url: string) { this.url = url; FakeSocket.last = this }
  send(frame: string) { this.sent.push((JSON.parse(frame) as { req: Record<string, unknown> }).req) }
  close() {}
  push(m: unknown) { act(() => { this.onmessage?.({ data: JSON.stringify(m) }) }) }
  answer(ans: Record<string, unknown>) { this.push({ type: 'ans', ans }) }
  /** Answers every request not answered yet from a table of answers by op. */
  serve(by: Record<string, unknown>) {
    for (const r of this.sent) {
      if (this.answered.has(r.id) || !(String(r.op) in by)) continue
      this.answered.add(r.id)
      this.answer({ id: r.id, type: 'result', data: by[String(r.op)] })
    }
  }
}

const db: Database = {
  id: 'm1', assetId: 'a1', hostname: 'mq-01', engine: 'rabbitmq', host: '127.0.0.1', port: 15672,
  poolerHost: '', poolerPort: 0, dbname: '/orders', adminUser: '', label: 'production', policy: {},
  createdBy: '', createdAt: '2026-11-06T00:00:00Z', tunnelConnected: true,
}

const DATA = {
  queues: [
    { name: 'orders.created', type: 'quorum', messages_ready: 3, consumers: 4, arguments: { 'x-dead-letter-exchange': 'dlx.orders' } },
    { name: 'orders.failed', type: 'quorum', messages_ready: 12, consumers: 0 },
    { name: 'invoice.render', type: 'classic', messages_ready: 900, consumers: 0 },
  ],
  exchanges: [{ name: 'dlx.orders', type: 'fanout' }],
  bindings: [{ source: 'dlx.orders', destination: 'orders.failed', destination_type: 'queue', routing_key: '' }],
  overview: { vhost: { messages_ready: 915 }, nodes: null },
  queue: { queue: { name: 'orders.failed', type: 'quorum', messages_ready: 12, consumers: 0 }, bindings: [] },
}

function open(tab: BrokerTab, queue?: string, ops = ['overview', 'queues', 'exchanges', 'bindings', 'queue', 'peek', 'move']) {
  renderWithProviders(
    <BrokerWorkspace database={db} access={{ level: 'owner' }} endpoint="/api/v1/postgres" tab={tab} queue={queue} onNavigate={() => {}}
      getTicket={async () => ({ ticket: 't', role: 'ana', level: 'owner' })} />,
  )
  return async () => {
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'rabbitmq', level: 'owner', user: 'argus-ana-owner', version: '3.13.7', ops })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'overview')).toBe(true))
    ws.serve(DATA)
    return ws
  }
}

describe('BrokerWorkspace', () => {
  beforeEach(() => { vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('names the queue nobody consumes, and the dead-letter queue filling up', async () => {
    await open('overview')()
    expect(await screen.findByText('invoice.render has no consumers')).toBeInTheDocument()
    expect(screen.getByText('orders.failed is collecting dead letters')).toBeInTheDocument()
    expect(screen.queryByText('orders.failed has no consumers')).toBeNull()
  })

  it('moves a dead-letter queue’s messages back to where they died', async () => {
    const ws = await open('queues', 'orders.failed')()
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'queue')).toBe(true))
    ws.serve(DATA)
    const go = await screen.findByRole('button', { name: /Move 12 messages/ })
    await waitFor(() => expect(go).not.toBeDisabled())
    fireEvent.click(go)
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'move')).toBe(true))
    expect(ws.sent.find((r) => r.op === 'move')).toMatchObject({ name: 'orders.failed', target: 'orders.created', count: 0 })
  })

  it('offers no move without the operation', async () => {
    const ws = await open('queues', 'orders.failed', ['overview', 'queues', 'exchanges', 'bindings', 'queue', 'peek'])()
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'queue')).toBe(true))
    ws.serve(DATA)
    await screen.findByText('Messages')
    expect(screen.queryByRole('button', { name: /Move/ })).toBeNull()
  })
})
