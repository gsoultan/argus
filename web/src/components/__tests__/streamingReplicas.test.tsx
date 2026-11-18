import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ReactNode } from 'react'
import { AskReplica, StreamingReplicas } from '~/components/DatabaseStreamingReplicas'
import { renderWithProviders } from '~/test/render'
import { endpointStatus, replicaHealth, replicaNameFor, replicaRefusal, replicaStage, serverAddresses, targetRefusal, validReplicaName } from '~/lib/managedReplicas'
import type { Asset, Database, DatabaseReplica, ReadEndpoint, ReadEndpointChoice } from '~/types/domain'

/**
 * Streaming replicas on servers Argus manages, on the database's Replication
 * panel: what the form refuses before asking, what it sends, and how a run's
 * progress reads. The control plane decides; these cover what the page promises.
 */

const live = vi.hoisted(() => ({
  databaseReplicas: vi.fn(),
  askReplica: vi.fn(),
  readEndpoint: vi.fn(),
  setReadEndpoint: vi.fn(),
  removeReadEndpoint: vi.fn(),
  assets: vi.fn(),
  me: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  ...live,
}))

vi.mock('~/lib/api', async (orig) => {
  const m = await orig<typeof import('~/lib/api')>()
  return { ...m, api: { ...m.api, assets: live.assets, me: live.me } }
})

vi.mock('~/components/links', () => ({
  ButtonLink: ({ children }: { children: ReactNode }) => <a>{children}</a>,
}))

const db: Database = {
  id: 'db1', assetId: 'a1', hostname: 'orders-db-01', engine: 'postgres', host: '127.0.0.1', port: 5432, poolerHost: '', poolerPort: 0,
  dbname: 'orders', adminUser: 'postgres', label: 'production' as Database['label'], policy: {} as Database['policy'],
  createdBy: 'ann@example.com', createdAt: '2026-10-01T00:00:00Z', tunnelConnected: true,
}

const asset = (id: string, hostname: string, address: string, over: Partial<Asset> = {}): Asset => ({
  id, hostname, address, port: 22, os: 'linux', tags: [], groupId: 'g', kind: 'server', tunnelConnected: true,
  addresses: [{ address, family: 'ipv4' }], ...over,
} as Asset)

const replica = (over: Partial<DatabaseReplica> = {}): DatabaseReplica => ({
  id: 'r1', databaseId: 'db1', dbname: 'orders', primaryHostname: 'orders-db-01', assetId: 'a2', hostname: 'orders-db-02',
  name: 'orders_db_02', replicaAddress: '10.0.2.15', primaryAddress: '10.0.1.10', requestedBy: 'ann@example.com', reason: 'reports',
  state: 'running', stage: 'copying', detail: '120 GiB of 210 GiB', error: '', createdAt: '2026-11-17T08:00:00Z', ...over,
})

const endpoint = (over: Partial<ReadEndpoint> = {}): ReadEndpoint => ({
  databaseId: 'db1', accountId: 'acc1', accountName: 'payments-prod', region: 'ap-southeast-3', targetGroup: 'arn:tg/pg-read',
  targetName: 'pg-read', includePrimary: true, primaryInstance: 'i-p', note: '', setBy: 'ann@example.com',
  setAt: '2026-11-17T08:00:00Z', health: { 'i-p': 'healthy', 'i-2': 'healthy' }, ...over,
})

const choice = (over: Partial<ReadEndpointChoice> = {}): ReadEndpointChoice => ({
  accountId: 'acc1', accountName: 'payments-prod', region: 'ap-southeast-3', targetGroup: 'arn:tg/pg-read', name: 'pg-read',
  balancer: 'pg', ...over,
})

beforeEach(() => {
  live.readEndpoint.mockResolvedValue({ endpoint: null, choices: [] })
  live.me.mockResolvedValue({ email: 'ann@example.com', role: 'admin' })
})

afterEach(() => vi.clearAllMocks())

describe('streaming replica words', () => {
  it('names a replica from its server', () => {
    expect(replicaNameFor('orders-db-02.internal')).toBe('orders_db_02')
    expect(replicaNameFor('02-db')).toBe('r_02_db')
    expect(validReplicaName('orders_db_02')).toBe(true)
    expect(validReplicaName('Orders')).toBe(false)
    expect(validReplicaName('a'.repeat(41))).toBe(false)
  })

  it('refuses a Postgres behind another server’s PgBouncer, and other engines', () => {
    expect(replicaRefusal(db)).toBeNull()
    expect(replicaRefusal({ ...db, host: '/var/run/postgresql' })).toBeNull()
    expect(replicaRefusal({ ...db, host: '10.0.3.4' })).toMatch(/PgBouncer/)
    expect(replicaRefusal({ ...db, engine: 'mysql' as Database['engine'] })).toMatch(/Postgres/)
    expect(replicaRefusal({ ...db, tunnelConnected: false })).toMatch(/not connected/)
  })

  it('says which servers cannot take a replica', () => {
    const busy = [replica({ assetId: 'a3', state: 'done' }), replica({ assetId: 'a4', state: 'failed' })]
    expect(targetRefusal(asset('a1', 'p', '10.0.0.1'), db, busy)).toMatch(/primary/)
    expect(targetRefusal(asset('a2', 'x', '10.0.0.2', { tunnelConnected: false }), db, busy)).toMatch(/not connected/)
    expect(targetRefusal(asset('a3', 'x', '10.0.0.3'), db, busy)).toMatch(/already/)
    expect(targetRefusal(asset('a4', 'x', '10.0.0.4'), db, busy)).toBeNull()
  })

  it('lists a server’s addresses IPv4 first, without loopback', () => {
    expect(serverAddresses(asset('a', 'x', 'x.internal', {
      addresses: [{ address: 'fd00::5', family: 'ipv6' }, { address: '127.0.0.1', family: 'ipv4' }, { address: '10.0.2.15', family: 'ipv4' }],
    } as Partial<Asset>))).toEqual(['10.0.2.15', 'fd00::5'])
  })

  it('says how a finished replica is doing, from the lag watch', () => {
    const at = '2026-11-17T08:00:00Z'
    expect(replicaHealth({ state: 'done' })).toBeNull()
    expect(replicaHealth({ state: 'done', watchedAt: at, streaming: true, lagS: 0 })).toEqual({ words: 'Streaming, caught up', color: 'teal' })
    expect(replicaHealth({ state: 'done', watchedAt: at, streaming: true, lagS: 200 })).toEqual({ words: 'Streaming, 3 min behind', color: 'amber' })
    expect(replicaHealth({ state: 'done', watchedAt: at, streaming: false, lagS: -1 })?.words).toBe('Not streaming')
  })

  it('says where a finished replica stands in the read endpoint', () => {
    const ep = endpoint()
    expect(endpointStatus({ state: 'done', endpointInstance: 'i-2' }, null)).toBeNull()
    expect(endpointStatus({ state: 'running' }, ep)).toBeNull()
    expect(endpointStatus({ state: 'done', endpointInstance: 'i-2' }, ep)).toEqual({ words: 'In pg-read · healthy', color: 'teal' })
    expect(endpointStatus({ state: 'done', endpointInstance: 'i-3' }, { ...ep, health: { 'i-3': 'unhealthy' } })?.color).toBe('amber')
    expect(endpointStatus({ state: 'done', endpointNote: 'out while it is behind' }, ep)?.words).toBe('Out of pg-read while it is behind')
    expect(endpointStatus({ state: 'done', endpointNote: 'x is not matched to an instance in a connected account' }, ep))
      .toEqual({ words: 'Not in pg-read: x is not matched to an instance in a connected account', color: 'rose' })
  })

  it('puts a run’s stage in words', () => {
    expect(replicaStage({ state: 'pending', stage: '' })).toBe('Waiting for an approver')
    expect(replicaStage({ state: 'running', stage: 'copying' })).toBe('Copying with pg_basebackup')
    expect(replicaStage({ state: 'failed', stage: 'checking' })).toBe('Failed while checking both servers')
    expect(replicaStage({ state: 'done', stage: 'done' })).toBe('Streaming')
  })
})

describe('streaming replicas tab', () => {
  it('shows each replica with where its run is, and opens a finished one', async () => {
    live.databaseReplicas.mockResolvedValue([
      replica(),
      replica({ id: 'r2', hostname: 'orders-db-03', assetId: 'a3', state: 'done', stage: 'done', replicaDatabaseId: 'db9',
        detail: 'Postgres 16, streaming from orders-db-01', streaming: false, lagS: -1, retainedBytes: 3 * 2 ** 30, watchedAt: new Date().toISOString() }),
      replica({ id: 'r3', hostname: 'orders-db-04', assetId: 'a4', state: 'failed', stage: 'checking',
        error: 'orders-db-01 needs these first, and Argus never restarts a primary: wal_level' }),
    ])
    renderWithProviders(<StreamingReplicas database={db} narrow={false} />)
    await waitFor(() => expect(screen.getAllByTestId('streaming-replica')).toHaveLength(3))
    expect(screen.getByText('Copying with pg_basebackup · 120 GiB of 210 GiB')).toBeInTheDocument()
    expect(screen.getByText('Failed while checking both servers')).toBeInTheDocument()
    expect(screen.getByText(/never restarts a primary/)).toBeInTheDocument()
    expect(screen.getByText(/^Not streaming/)).toBeInTheDocument()
    expect(screen.getByText(/Its slot keeps 3.00 GB of WAL on orders-db-01/)).toBeInTheDocument()
    const rows = screen.getAllByTestId('streaming-replica')
    expect(within(rows[1]!).getByText('Open')).toBeInTheDocument()
    expect(within(rows[0]!).queryByText('Open')).toBeNull()
  })

  it('keeps the button off for a Postgres it cannot copy', async () => {
    live.databaseReplicas.mockResolvedValue([])
    renderWithProviders(<StreamingReplicas database={{ ...db, host: '10.0.3.4' }} narrow={false} />)
    expect(screen.getByRole('button', { name: 'Add a read replica' })).toBeDisabled()
    expect(screen.getByText(/another server’s PgBouncer/)).toBeInTheDocument()
  })
})

describe('read endpoint', () => {
  const done = (over: Partial<DatabaseReplica> = {}) => replica({ state: 'done', stage: 'done', detail: '', ...over })

  it('shows the endpoint, the primary and each replica in or out, and removes it', async () => {
    live.databaseReplicas.mockResolvedValue([
      done({ endpointInstance: 'i-2' }),
      done({ id: 'r2', hostname: 'orders-db-03', assetId: 'a3', endpointNote: 'out while it is behind' }),
    ])
    live.readEndpoint.mockResolvedValue({ endpoint: endpoint(), choices: [choice()] })
    live.removeReadEndpoint.mockResolvedValue(undefined)
    renderWithProviders(<StreamingReplicas database={db} narrow={false} />)
    const section = await screen.findByTestId('read-endpoint')
    await waitFor(() => expect(within(section).getByText(/orders-db-01 \(the primary\) is in · healthy/)).toBeInTheDocument())
    expect(within(section).getByText(/payments-prod ap-southeast-3, kept by Argus/)).toBeInTheDocument()
    await waitFor(() => expect(screen.getAllByTestId('replica-endpoint')).toHaveLength(2))
    const marks = screen.getAllByTestId('replica-endpoint').map((n) => n.textContent)
    expect(marks).toEqual(['In pg-read · healthy', 'Out of pg-read while it is behind'])
    fireEvent.click(await within(section).findByRole('button', { name: 'Remove' }))
    await waitFor(() => expect(live.removeReadEndpoint).toHaveBeenCalledWith('db1'))
  })

  it('sets one from a target group that fits, leaving the primary out', async () => {
    live.databaseReplicas.mockResolvedValue([done()])
    live.readEndpoint.mockResolvedValue({ endpoint: null, choices: [choice(),
      choice({ targetGroup: 'arn:tg/pg-ip', name: 'pg-ip', problem: 'it sends to ip targets; Argus registers instances' })] })
    live.setReadEndpoint.mockResolvedValue(endpoint({ includePrimary: false, primaryInstance: '' }))
    renderWithProviders(<StreamingReplicas database={db} narrow={false} />)
    const open = await screen.findByRole('button', { name: 'Set a read endpoint' })
    await waitFor(() => expect(open).toBeEnabled())
    fireEvent.click(open)
    fireEvent.click(screen.getAllByLabelText('Target group')[0]!)
    const ip = await screen.findByRole('option', { name: /pg-ip/, hidden: true })
    expect(ip).toHaveAttribute('data-combobox-disabled')
    fireEvent.click(await screen.findByRole('option', { name: /^pg-read \(pg\)/, hidden: true }))
    fireEvent.click(screen.getByLabelText('Include orders-db-01, the primary'))
    fireEvent.click(screen.getByRole('button', { name: 'Set read endpoint' }))
    await waitFor(() => expect(live.setReadEndpoint).toHaveBeenCalledWith('db1', {
      accountId: 'acc1', region: 'ap-southeast-3', targetGroup: 'arn:tg/pg-read', includePrimary: false,
    }))
  })

  it('shows a setting that waits for a second person, and asks no other meanwhile', async () => {
    live.databaseReplicas.mockResolvedValue([done()])
    live.readEndpoint.mockResolvedValue({ endpoint: null, choices: [choice()], pending: {
      id: 'c1', state: 'awaiting_approval', resourceName: 'pg-read', environment: 'production', reason: 'hourly reports',
      summary: 'Make pg-read the read endpoint of orders on orders-db-01: Argus keeps its replicas that are not behind in it',
      requestedBy: 'ann@example.com', requestedAt: new Date().toISOString(),
    } })
    renderWithProviders(<StreamingReplicas database={db} narrow={false} />)
    const box = await screen.findByTestId('read-endpoint-pending')
    expect(within(box).getByText(/Waiting for approval: Make pg-read the read endpoint of orders/)).toBeInTheDocument()
    expect(within(box).getByText(/hourly reports/)).toBeInTheDocument()
    const open = screen.getByRole('button', { name: 'Set a read endpoint' })
    await waitFor(() => expect(open).toHaveAttribute('title', 'A change to it already waits for approval'))
    expect(open).toBeDisabled()
  })

  it('keeps setting it to admins', async () => {
    live.me.mockResolvedValue({ email: 'dev@example.com', role: 'developer' })
    live.databaseReplicas.mockResolvedValue([done()])
    live.readEndpoint.mockResolvedValue({ endpoint: null, choices: [choice()] })
    renderWithProviders(<StreamingReplicas database={db} narrow={false} />)
    const open = await screen.findByRole('button', { name: 'Set a read endpoint' })
    await waitFor(() => expect(open).toHaveAttribute('title', expect.stringMatching(/admin or owner/)))
    expect(open).toBeDisabled()
  })
})

describe('asking for a streaming replica', () => {
  it('fills the name and addresses from the server picked, and sends the ask', async () => {
    live.assets.mockResolvedValue([
      asset('a1', 'orders-db-01', '10.0.1.10'),
      asset('a2', 'orders-db-02', '10.0.2.15'),
      asset('a3', 'orders-db-03', '10.0.3.15', { tunnelConnected: false }),
    ])
    live.askReplica.mockResolvedValue(replica({ state: 'pending' }))
    const onClose = vi.fn()
    renderWithProviders(<AskReplica database={db} replicas={[]} onClose={onClose} />)
    const send = screen.getByRole('button', { name: 'Ask to add replica' })
    expect(send).toBeDisabled()
    const pick = screen.getAllByLabelText('Replica server')[0]!
    await waitFor(() => expect(live.assets).toHaveBeenCalled())
    fireEvent.click(pick)
    const two = await screen.findByRole('option', { name: 'orders-db-02', hidden: true })
    expect(screen.getByRole('option', { name: 'orders-db-03 · agent not connected', hidden: true })).toHaveAttribute('data-combobox-disabled')
    fireEvent.click(two)
    await waitFor(() => expect(screen.getByDisplayValue('orders_db_02')).toBeInTheDocument())
    expect(screen.getByDisplayValue('10.0.2.15')).toBeInTheDocument()
    expect(screen.getByText('Role argus_repl_orders_db_02, slot argus_replica_orders_db_02')).toBeInTheDocument()
    expect(send).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Why (approvers see this)'), { target: { value: 'reports read the whole table' } })
    await waitFor(() => expect(send).toBeEnabled())
    fireEvent.click(send)
    await waitFor(() => expect(live.askReplica).toHaveBeenCalled())
    expect(live.askReplica).toHaveBeenCalledWith('db1', {
      assetId: 'a2', name: 'orders_db_02', replicaAddress: '10.0.2.15', primaryAddress: undefined, reason: 'reports read the whole table',
    })
    await waitFor(() => expect(onClose).toHaveBeenCalled())
  })
})
