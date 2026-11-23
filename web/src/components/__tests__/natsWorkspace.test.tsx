import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { NatsWorkspace, type NatsTab } from '~/components/nats/NatsWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'

vi.mock('@tanstack/react-router', () => ({
  Link: ({ children, to }: { children: React.ReactNode; to: string }) => <a href={to}>{children}</a>,
}))
vi.mock('~/components/links', () => ({
  ButtonLink: ({ children }: { children: React.ReactNode }) => <a>{children}</a>,
}))

/**
 * The NATS workspace: it reads the account, its streams and consumers, says
 * what needs attention, opens a stream and reads its newest messages by
 * sequence, and asks for the stream's name before deleting it, which the
 * agent refuses without.
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
  id: 'n1', assetId: 'a1', hostname: 'nats-01', engine: 'nats', host: '127.0.0.1', port: 4222,
  poolerHost: '', poolerPort: 0, dbname: 'ORDERS', adminUser: '', label: 'staging', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

const answerAll = async (ws: FakeSocket, op: string, ans: Record<string, unknown>) => {
  await waitFor(() => expect(ws.sent.some((r) => r.op === op && !(r as { _done?: boolean })._done)).toBe(true))
  const req = ws.sent.find((r) => r.op === op && !(r as { _done?: boolean })._done)!
  ;(req as { _done?: boolean })._done = true
  ws.answer({ id: req.id, ...ans })
  return req
}

const stale = new Date(Date.now() - 3_600_000).toISOString()
const stream = {
  config: { name: 'ORDERS_EVENTS', subjects: ['orders.>'], retention: 'limits', storage: 'file', num_replicas: 1, max_age: 7 * 86_400e9,
    max_bytes: -1, max_msgs: -1, max_msgs_per_subject: -1, discard: 'old', duplicate_window: 120e9 },
  created: '2026-11-01T00:00:00Z',
  state: { messages: 1200, bytes: 4096, first_seq: 1, first_ts: stale, last_seq: 1200, last_ts: stale, consumer_count: 1 },
}
const consumer = {
  stream_name: 'ORDERS_EVENTS', name: 'invoicer', created: stale, config: { durable_name: 'invoicer', deliver_policy: 'all', ack_policy: 'explicit' },
  delivered: { consumer_seq: 10, stream_seq: 10, last_active: stale }, ack_floor: { consumer_seq: 10, stream_seq: 10 },
  num_ack_pending: 0, num_redelivered: 0, num_waiting: 0, num_pending: 1190,
}

function Harness({ start = 'overview', level = 'owner' }: { start?: NatsTab; level?: 'owner' | 'write' }) {
  const [at, setAt] = useState<{ tab: NatsTab; stream?: string; consumer?: string }>({ tab: start })
  return (
    <NatsWorkspace database={db} access={{ level }} endpoint="/api/v1/postgres"
      getTicket={async () => ({ ticket: 't', role: 'ana', level })} tab={at.tab} stream={at.stream} consumer={at.consumer} onNavigate={setAt} />
  )
}

describe('NatsWorkspace', () => {
  beforeEach(() => { vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('reads the account, opens a stream, reads by sequence and confirms a delete by the stream name', async () => {
    renderWithProviders(<Harness />)
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'nats', level: 'owner', user: 'argus-ana-owner',
      ops: ['overview', 'streams', 'stream', 'consumers', 'consumer', 'subjects', 'connections', 'get', 'tail', 'publish', 'stream_create', 'stream_delete', 'consumer_move'] })
    await answerAll(ws, 'overview', { type: 'result', data: { account: 'ORDERS', accountId: 'ADXX', version: '2.11.4', jetstream: true,
      stat: { conns: 4, subscriptions: 12, msgsIn: 5000, msgsOut: 7000, bytesIn: 1, bytesOut: 1, slowConsumers: 0 } } })
    await answerAll(ws, 'streams', { type: 'result', data: [stream] })
    await answerAll(ws, 'consumers', { type: 'result', data: [consumer] })

    expect(await screen.findByText(/ORDERS_EVENTS › invoicer: 1,190 messages wait and no app has pulled/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New stream' })).toBeInTheDocument()

    fireEvent.click(screen.getAllByRole('button', { name: 'Open' })[0]!)
    expect(await screen.findByText(/1,190 messages wait and 0 are out unacked; nothing has pulled/)).toBeInTheDocument()

    act(() => { fireEvent.click(screen.getByRole('tab', { name: /Streams/ })) })
    fireEvent.click(await screen.findByText('ORDERS_EVENTS'))
    await answerAll(ws, 'stream', { type: 'result', data: { info: stream, subjects: { 'orders.eu.created': 1000, 'orders.us.created': 200 } } })
    const get = await answerAll(ws, 'get', { type: 'result', data: [
      { seq: 1200, subject: 'orders.eu.created', time: stale, headers: { 'Nats-Msg-Id': ['ord-1'] }, data: '{"total":12}', size: 12 },
    ] })
    expect(get).toMatchObject({ nats: { stream: 'ORDERS_EVENTS', from: 'newest' }, count: 20 })
    expect(await screen.findByText(/"total": 12/)).toBeInTheDocument()
    expect(screen.getByText('Nats-Msg-Id: ord-1')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /Delete…/ }))
    const name = await screen.findByLabelText('Stream name')
    fireEvent.change(name, { target: { value: 'ORDERS_EVENTS' } })
    fireEvent.click(screen.getByRole('button', { name: 'Do it' }))
    const del = await answerAll(ws, 'stream_delete', { type: 'result', ms: 30 })
    expect(del).toMatchObject({ nats: { stream: 'ORDERS_EVENTS' }, confirm: 'ORDERS_EVENTS' })
  })

  it('refuses a new stream on a subject another stream stores, before asking the agent', async () => {
    renderWithProviders(<Harness start="streams" />)
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'nats', level: 'owner', user: 'argus-ana-owner', ops: ['overview', 'streams', 'consumers', 'stream_create'] })
    await answerAll(ws, 'overview', { type: 'result', data: { account: 'ORDERS', accountId: 'ADXX', jetstream: true } })
    await answerAll(ws, 'streams', { type: 'result', data: [stream] })
    await answerAll(ws, 'consumers', { type: 'result', data: [] })

    fireEvent.click(await screen.findByRole('button', { name: 'New stream' }))
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'EU' } })
    fireEvent.change(screen.getByLabelText('Subject 1'), { target: { value: 'orders.eu.*' } })
    expect(await screen.findByText(/overlaps orders.>, which ORDERS_EVENTS already stores/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Create stream' })).toBeDisabled()

    fireEvent.change(screen.getByLabelText('Subject 1'), { target: { value: 'eu.orders.>' } })
    await waitFor(() => expect(screen.getByRole('button', { name: 'Create stream' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: 'Create stream' }))
    const create = await answerAll(ws, 'stream_create', { type: 'result', data: { ...stream, config: { ...stream.config, name: 'EU', subjects: ['eu.orders.>'] } } })
    expect(create).toMatchObject({ nats: { streamSpec: { name: 'EU', subjects: ['eu.orders.>'], retention: 'limits', maxAgeS: 604800 } } })
  })

  it('lists a bucket\u2019s keys, reads a key\u2019s history and puts a new value at Write', async () => {
    renderWithProviders(<Harness start="kv" level="write" />)
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'nats', level: 'write', user: 'argus-ana-write',
      ops: ['overview', 'streams', 'consumers', 'kv_buckets', 'kv_keys', 'kv_get', 'kv_put', 'kv_del'] })
    await answerAll(ws, 'overview', { type: 'result', data: { account: 'ORDERS', accountId: 'ADXX', jetstream: true } })
    await answerAll(ws, 'streams', { type: 'result', data: [] })
    await answerAll(ws, 'consumers', { type: 'result', data: [] })
    await answerAll(ws, 'kv_buckets', { type: 'result', data: [
      { bucket: 'carts', values: 4, history: 5, ttlS: 86400, bytes: 2048, storage: 'file', replicas: 1, maxBytes: -1, maxValueSize: -1 },
    ] })
    expect(screen.queryByRole('button', { name: /Remove bucket/ })).toBeNull()
    const keys = await answerAll(ws, 'kv_keys', { type: 'result', total: 2, data: [
      { key: 'c1', revision: 3, size: 11, op: 'put', kept: 3, time: stale },
      { key: 'c2', revision: 4, size: 0, op: 'del', kept: 2, time: stale },
    ] })
    expect(keys).toMatchObject({ nats: { bucket: 'carts' } })
    expect(await screen.findByText('c1')).toBeInTheDocument()
    expect(screen.queryByText('c2')).toBeNull()

    fireEvent.click(screen.getByText('c1'))
    const get = await answerAll(ws, 'kv_get', { type: 'result', data: { key: 'c1', entries: [
      { revision: 3, op: 'put', value: { subject: '$KV.carts.c1', data: '{"items":3}', size: 11, time: stale } },
      { revision: 1, op: 'put', value: { subject: '$KV.carts.c1', data: '{"items":1}', size: 11, time: stale } },
    ] } })
    expect(get).toMatchObject({ nats: { bucket: 'carts', key: 'c1' } })
    expect(await screen.findByText(/"items": 3/)).toBeInTheDocument()
    expect(screen.getByText('current')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Put a new value' }))
    const value = await screen.findByLabelText('Value')
    fireEvent.change(value, { target: { value: '{"items":4}' } })
    fireEvent.click(screen.getByRole('button', { name: 'Put' }))
    const put = await answerAll(ws, 'kv_put', { type: 'result', data: { revision: 5 } })
    expect(put).toMatchObject({ nats: { bucket: 'carts', key: 'c1' }, payload: '{"items":4}' })
  })
})
