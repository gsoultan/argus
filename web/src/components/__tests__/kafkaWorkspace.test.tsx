import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { KafkaWorkspace, type KafkaTab } from '~/components/kafka/KafkaWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'

vi.mock('@tanstack/react-router', () => ({
  Link: ({ children, to }: { children: React.ReactNode; to: string }) => <a href={to}>{children}</a>,
}))
vi.mock('~/components/links', () => ({
  ButtonLink: ({ children }: { children: React.ReactNode }) => <a>{children}</a>,
}))

/**
 * The Kafka workspace: it reads the cluster, topics and groups it may see,
 * opens a topic and reads its newest messages, and asks for the server's
 * name before deleting a topic, which the agent refuses without it.
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
  id: 'k1', assetId: 'a1', hostname: 'stream-01', engine: 'kafka', host: '127.0.0.1', port: 9092,
  poolerHost: '', poolerPort: 0, dbname: 'orders.', adminUser: '', label: 'production', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

const answerAll = async (ws: FakeSocket, op: string, ans: Record<string, unknown>) => {
  await waitFor(() => expect(ws.sent.some((r) => r.op === op && !(r as { _done?: boolean })._done)).toBe(true))
  const req = ws.sent.find((r) => r.op === op && !(r as { _done?: boolean })._done)!
  ;(req as { _done?: boolean })._done = true
  ws.answer({ id: req.id, ...ans })
  return req
}

function Harness({ level = 'owner', start = 'overview' }: { level?: 'owner' | 'dba'; start?: KafkaTab }) {
  const [at, setAt] = useState<{ tab: KafkaTab; topic?: string }>({ tab: start })
  return (
    <KafkaWorkspace database={{ ...db, label: 'staging' }} access={{ level }} endpoint="/api/v1/postgres"
      getTicket={async () => ({ ticket: 't', role: 'ana', level })} tab={at.tab} topic={at.topic} onNavigate={setAt} />
  )
}

describe('KafkaWorkspace', () => {
  beforeEach(() => { vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('reads the cluster, opens a topic, peeks and confirms a delete by the server name', async () => {
    renderWithProviders(<Harness />)
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'kafka', level: 'owner', user: 'argus-ana-owner',
      ops: ['overview', 'topics', 'topic', 'groups', 'peek', 'publish', 'create_topic', 'delete_topic'] })
    await answerAll(ws, 'overview', { type: 'result', data: { cluster: 'c-1', brokers: [{ id: 1, host: 'localhost', port: 9092, controller: true }], topics: 2, partitions: 4, scope: 'orders.' } })
    await answerAll(ws, 'topics', { type: 'result', data: [
      { name: 'orders.eu', partitions: 3, replication: 1, messages: 1200, underReplicated: 1 },
      { name: 'orders.us', partitions: 1, replication: 1, messages: 4 },
    ] })
    await answerAll(ws, 'groups', { type: 'result', data: [{ group: 'billing', state: 'Empty', members: 0, lag: { 'orders.eu': 340 } }] })

    expect(await screen.findByText('orders.eu: 1 partition is short of in-sync replicas.')).toBeInTheDocument()
    expect(screen.getByText('billing has no members while 340 messages wait for it.')).toBeInTheDocument()

    fireEvent.click(screen.getAllByRole('button', { name: 'Open' })[0]!)
    await answerAll(ws, 'topic', { type: 'result', data: {
      name: 'orders.eu', configs: { 'retention.ms': '86400000' }, groups: [],
      partitions: [{ partition: 0, leader: 1, replicas: [1], isr: [1], start: 0, end: 1200 }],
    } })
    expect(await screen.findByText('retention.ms')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Read' }))
    const peek = await answerAll(ws, 'peek', { type: 'result', data: [
      { partition: 0, offset: 1199, time: '2026-11-06T05:58:11Z', key: 'o-1', value: '{"total":12}', headers: { source: 'web' } },
    ] })
    expect(peek).toMatchObject({ name: 'orders.eu', count: 20 })
    expect(await screen.findByText(/"total": 12/)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Delete topic' }))
    const name = await screen.findByLabelText('Server name')
    fireEvent.change(name, { target: { value: 'stream-01' } })
    fireEvent.click(screen.getByRole('button', { name: 'Do it' }))
    const del = await answerAll(ws, 'delete_topic', { type: 'result', ms: 30 })
    expect(del).toMatchObject({ name: 'orders.eu', confirm: 'stream-01' })
  })

  it('makes an app user at Admin: only a salted hash goes with it, then it signs in and the details show once', async () => {
    renderWithProviders(<Harness level="dba" start="users" />)
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'kafka', level: 'dba', user: 'argus-ana-dba',
      ops: ['overview', 'topics', 'groups', 'users', 'user_set', 'user_del', 'user_scope_del', 'user_login'] })
    await answerAll(ws, 'overview', { type: 'result', data: { cluster: 'c-1', brokers: [], topics: 1, partitions: 1, scope: 'orders.' } })
    await answerAll(ws, 'topics', { type: 'result', data: [{ name: 'orders.invoices', partitions: 1, replication: 1, messages: 0 }] })
    await answerAll(ws, 'groups', { type: 'result', data: [] })
    await answerAll(ws, 'users', { type: 'result', data: { scope: 'orders.', notShown: 2, sessions: ['argus-ana-dba'], users: [
      { name: 'billing', mechanisms: ['SCRAM-SHA-512'], quotas: {}, acls: [{ resource: 'topic', pattern: 'PREFIXED', name: 'orders.', operation: 'READ', permission: 'ALLOW', host: '*' }],
        outside: [{ resource: 'topic', pattern: 'LITERAL', name: 'payments', operation: 'READ', permission: 'ALLOW', host: '*' }] },
    ] } })
    expect(await screen.findByText('billing')).toBeInTheDocument()
    expect(screen.getByText('1 ACL')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'New user' }))
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'invoices-app' } })
    expect(screen.getByText(/Covers 1 topic now: orders.invoices/)).toBeInTheDocument()
    const create = screen.getByRole('button', { name: 'Create invoices-app' })
    await waitFor(() => expect(create).toBeEnabled())
    fireEvent.click(create)
    const set = await answerAll(ws, 'user_set', { type: 'result', data: { user: 'invoices-app', added: 4 } }) as {
      password?: string; brokerUser: { name: string; create: boolean; credential: { salt: string; saltedPassword: string; iterations: number }; acls: unknown[] }
    }
    expect(set.password).toBeUndefined()
    expect(set.brokerUser).toMatchObject({ name: 'invoices-app', create: true, mechanism: 'SCRAM-SHA-512' })
    expect(set.brokerUser.credential.iterations).toBe(8192)
    expect(set.brokerUser.acls).toHaveLength(4)

    const login = await answerAll(ws, 'user_login', { type: 'result', data: { user: 'invoices-app', address: '127.0.0.1:9092', ok: true, topics: ['orders.invoices'], elsewhere: 0 } }) as { password: string }
    expect(login.password.length).toBeGreaterThanOrEqual(12)
    expect(JSON.stringify(set)).not.toContain(login.password)
    expect(await screen.findByText('Signs in on 127.0.0.1:9092')).toBeInTheDocument()
    expect(screen.getByText('shown once')).toBeInTheDocument()
    await answerAll(ws, 'users', { type: 'result', data: { scope: 'orders.', notShown: 2, sessions: [], users: [] } })
  })
})
