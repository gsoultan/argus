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

function open(tab: BrokerTab, queue?: string, ops = ['overview', 'queues', 'exchanges', 'bindings', 'queue', 'peek', 'move'], data: Record<string, unknown> = DATA) {
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
    ws.serve(data)
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

  it('refreshes on its poll, not on every answer', async () => {
    const ws = await open('overview')()
    await screen.findByText('invoice.render has no consumers')
    // Answering whatever was asked again must not set off another round:
    // the agent allows a session so many requests a minute.
    for (let i = 0; i < 5; i++) {
      ws.serve(DATA)
      await new Promise((r) => setTimeout(r, 10))
    }
    expect(ws.sent.filter((r) => r.op === 'queues')).toHaveLength(1)
  })

  it('offers no move without the operation', async () => {
    const ws = await open('queues', 'orders.failed', ['overview', 'queues', 'exchanges', 'bindings', 'queue', 'peek'])()
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'queue')).toBe(true))
    ws.serve(DATA)
    await screen.findByText('Messages')
    expect(screen.queryByRole('button', { name: /Move/ })).toBeNull()
  })

  it('makes an app user on the vhost: only the hash goes with it, then it signs in and the details show once', async () => {
    const ws = await open('users', undefined, ['overview', 'queues', 'exchanges', 'bindings', 'queue', 'users', 'user_set', 'user_del', 'user_scope_del', 'user_login'])()
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'users')).toBe(true))
    ws.serve({ users: { vhost: '/orders', amqpPort: 5672, notShown: 1, sessions: ['argus-ana-dba'], users: [
      { name: 'guest', tags: ['administrator'], perms: { configure: '.*', write: '.*', read: '.*' }, topicPerms: [], limits: {}, connections: 0, hasPassword: true,
        locked: 'tagged administrator', defaultPassword: true },
      { name: 'billing', tags: [], perms: { configure: '^$', write: '^billing\\.', read: '^$' }, topicPerms: [], limits: {}, connections: 2, hasPassword: true, elsewhere: ['/billing'] },
    ] } })
    expect(await screen.findByText('guest still signs in with its default password. Give it a new one or delete it.')).toBeInTheDocument()
    expect(screen.getByText('starting billing.')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'New user' }))
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'invoices-app' } })
    fireEvent.change(screen.getByLabelText('Reads queues starting with'), { target: { value: 'orders.' } })
    fireEvent.change(screen.getByLabelText('Publishes to exchanges starting with'), { target: { value: 'orders.' } })
    expect(screen.getByText(/Reads: orders.created, orders.failed/)).toBeInTheDocument()
    const create = screen.getByRole('button', { name: 'Create invoices-app' })
    await waitFor(() => expect(create).toBeEnabled())
    fireEvent.click(create)
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'user_set')).toBe(true))
    const set = ws.sent.find((r) => r.op === 'user_set') as { password?: string; brokerUser: { passwordHash: string; perms: Record<string, string>; tags: string[] } }
    expect(set.password).toBeUndefined()
    expect(atob(set.brokerUser.passwordHash)).toHaveLength(36)
    expect(set.brokerUser.perms).toEqual({ configure: '^orders\\.', write: '^orders\\.', read: '^orders\\.' })
    expect(set.brokerUser.tags).toEqual([])
    ws.serve({ user_set: { user: 'invoices-app' } })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'user_login')).toBe(true))
    const login = ws.sent.find((r) => r.op === 'user_login') as { password: string }
    expect(login.password.length).toBeGreaterThanOrEqual(12)
    ws.serve({ user_login: { user: 'invoices-app', address: '127.0.0.1:5672', ok: true } })
    expect(await screen.findByText('Opens /orders on 127.0.0.1:5672')).toBeInTheDocument()
    expect(screen.getByText(/amqp:\/\/invoices-app:.*@mq-01:5672\/%2Forders/)).toBeInTheDocument()
  })
  it('reads a stream from an offset, pages to newer, and lists its readers', async () => {
    const data = { ...DATA, queue: { queue: { name: 'orders.events', type: 'stream', messages_ready: 50, consumers: 1 }, bindings: [] } }
    const ws = await open('queues', 'orders.events', ['overview', 'queues', 'exchanges', 'bindings', 'queue', 'stream_read', 'stream_readers'], data)()
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'queue')).toBe(true))
    ws.serve(data)
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'stream_readers')).toBe(true))
    ws.serve({ stream_readers: { readers: [{ name: 'ledger-sync', protocol: 'stream', offset: 30, behind: 19, from: '10.20.0.61:5552' },
      { name: 'amq.ctag-1', protocol: 'amqp' }], publishers: [] } })
    expect(await screen.findByText('ledger-sync')).toBeInTheDocument()
    expect(screen.getByText('AMQP readers keep no place on the server, so how far they are is not known.')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('radio', { name: 'Offset' }))
    fireEvent.change(screen.getByLabelText('Start at offset'), { target: { value: '10' } })
    fireEvent.click(screen.getByRole('button', { name: 'Read' }))
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'stream_read')).toBe(true))
    expect(ws.sent.find((r) => r.op === 'stream_read')).toMatchObject({ name: 'orders.events', count: 20, stream: { from: 'offset', offset: 10 } })
    const msgs = Array.from({ length: 20 }, (_, i) => ({ offset: 10 + i, body: `{"n":${10 + i}}`, encoding: 'string', size: 8 }))
    ws.serve({ stream_read: { messages: msgs, first: 0, last: 49 } })
    expect(await screen.findByText('offset 10')).toBeInTheDocument()
    expect(screen.getByText('10 to 29 of 0 to 49')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Newer' }))
    await waitFor(() => expect(ws.sent.filter((r) => r.op === 'stream_read')).toHaveLength(2))
    expect(ws.sent.filter((r) => r.op === 'stream_read')[1]).toMatchObject({ stream: { from: 'offset', offset: 30 } })
  })

  it('makes a super stream from the routing tab', async () => {
    const ws = await open('routing', undefined, ['overview', 'queues', 'exchanges', 'bindings', 'exchange', 'super_streams', 'declare_super_stream'])()
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'super_streams')).toBe(true))
    ws.serve({ super_streams: [{ name: 'invoices', messages: 40, partitions: [{ stream: 'invoices-0', routingKey: '0', order: 0, messages: 40 }] }] })
    expect(await screen.findByText('invoices')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'New super stream' }))
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'payments' } })
    fireEvent.change(screen.getByLabelText('Partitions'), { target: { value: '4' } })
    fireEvent.change(screen.getByLabelText('Keep for, days'), { target: { value: '30' } })
    expect(screen.getByLabelText('What runs').textContent).toContain('payments-0 … payments-3')
    fireEvent.click(screen.getByRole('button', { name: 'Create payments' }))
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'declare_super_stream')).toBe(true))
    expect(ws.sent.find((r) => r.op === 'declare_super_stream')).toMatchObject({ name: 'payments', superStream: { partitions: 4, maxAgeS: 2592000 } })
  })
})
