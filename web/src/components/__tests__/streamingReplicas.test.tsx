import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactNode } from 'react'
import { AskReplica, StreamingReplicas } from '~/components/DatabaseStreamingReplicas'
import { renderWithProviders } from '~/test/render'
import { replicaNameFor, replicaRefusal, replicaStage, serverAddresses, targetRefusal, validReplicaName } from '~/lib/managedReplicas'
import type { Asset, Database, DatabaseReplica } from '~/types/domain'

/**
 * Streaming replicas on servers Argus manages, on the database's Replication
 * panel: what the form refuses before asking, what it sends, and how a run's
 * progress reads. The control plane decides; these cover what the page promises.
 */

const live = vi.hoisted(() => ({
  databaseReplicas: vi.fn(),
  askReplica: vi.fn(),
  assets: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  ...live,
}))

vi.mock('~/lib/api', async (orig) => {
  const m = await orig<typeof import('~/lib/api')>()
  return { ...m, api: { ...m.api, assets: live.assets } }
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
        detail: 'Postgres 16, streaming from orders-db-01' }),
      replica({ id: 'r3', hostname: 'orders-db-04', assetId: 'a4', state: 'failed', stage: 'checking',
        error: 'orders-db-01 needs these first, and Argus never restarts a primary: wal_level' }),
    ])
    renderWithProviders(<StreamingReplicas database={db} narrow={false} />)
    await waitFor(() => expect(screen.getAllByTestId('streaming-replica')).toHaveLength(3))
    expect(screen.getByText('Copying with pg_basebackup · 120 GiB of 210 GiB')).toBeInTheDocument()
    expect(screen.getByText('Failed while checking both servers')).toBeInTheDocument()
    expect(screen.getByText(/never restarts a primary/)).toBeInTheDocument()
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
