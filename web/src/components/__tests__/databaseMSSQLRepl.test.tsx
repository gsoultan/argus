import { describe, expect, it } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { MSSQLReplication, lagWords, type MSReplication } from '~/components/DatabaseMSSQLRepl'
import type { Admin, AdminOp, Collect } from '~/components/DatabaseAdmin'

/*
SQL Server replication reads the database's availability group, publications
and log shipping in one fixed read, and changes them only after a confirmation
that shows the agent's preview.
*/

const collect: Collect = async () => ({ cols: [], rows: [] })
const base = { collect, levels: {}, self: 'argus-ana-dba-0a1b2c3d', narrow: false, onBack: () => {}, level: 'dba' as const }

function fakeAdmin(read: MSReplication): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => {
    ops.push(op)
    if (op.preview) return { data: { sql: `-- preview of ${op.kind}` } }
    if (op.kind === 'ms.replication') return { data: read }
    return { data: { sql: 'done' }, tag: 'DONE' }
  }) as Admin
  return { admin, ops }
}
const ran = (ops: AdminOp[], kind: string) => ops.find((o) => o.kind === kind && !o.preview)
const server = { server: 'mssql1', edition: 'Developer Edition', hadrEnabled: true, recovery: 'FULL', published: false, subscribed: false }

async function confirm(verb: string) {
  const dialog = await screen.findByRole('dialog')
  await within(dialog).findByText(/-- preview of/)
  fireEvent.click(within(dialog).getByRole('button', { name: verb }))
}

describe('SQL Server replication', () => {
  it('words how far behind a replica is', () => {
    expect(lagWords({ role: 'PRIMARY', lagS: 0 })).toBe('')
    expect(lagWords({ role: 'SECONDARY' })).toBe('—')
    expect(lagWords({ role: 'SECONDARY', lagS: 0 })).toBe('up to date')
    expect(lagWords({ role: 'SECONDARY', lagS: 45 })).toBe('45 s behind')
    expect(lagWords({ role: 'SECONDARY', lagS: 600 })).toBe('10 min behind')
  })

  it('shows the group’s replicas and suspends data movement', async () => {
    const { admin, ops } = fakeAdmin({ ...server, distributor: false, ag: { group: 'ag1', clusterType: 'NONE', localRole: 'PRIMARY', replicas: [
      { server: 'mssql1', local: true, role: 'PRIMARY', mode: 'SYNCHRONOUS_COMMIT', failover: 'MANUAL', health: 'HEALTHY', connected: 'CONNECTED', syncState: 'SYNCHRONIZED', sendKb: 0, redoKb: 0 },
      { server: 'mssql2', role: 'SECONDARY', mode: 'ASYNCHRONOUS_COMMIT', failover: 'MANUAL', health: 'HEALTHY', connected: 'CONNECTED', syncState: 'SYNCHRONIZING', sendKb: 2048, redoKb: 12, lagS: 90 },
    ] } })
    ui(<MSSQLReplication {...base} admin={admin} />)
    expect(await screen.findByText('90 s behind')).toBeInTheDocument()
    expect(screen.getByText('2.0 MB')).toBeInTheDocument()
    // A primary is not failed over from here.
    expect(screen.queryByRole('button', { name: 'Fail over…' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Suspend…' }))
    await confirm('Suspend')
    await waitFor(() => expect(ran(ops, 'ms.ag.suspend')).toBeTruthy())
  })

  it('fails a no-cluster group over only by force', async () => {
    const { admin, ops } = fakeAdmin({ ...server, distributor: false, ag: { group: 'ag1', clusterType: 'NONE', localRole: 'SECONDARY', replicas: [
      { server: 'mssql1', local: true, role: 'SECONDARY', mode: 'SYNCHRONOUS_COMMIT', failover: 'MANUAL', health: 'HEALTHY', connected: 'CONNECTED', syncState: 'SYNCHRONIZED', sendKb: 0, redoKb: 0, lagS: 0 },
    ] } })
    ui(<MSSQLReplication {...base} admin={admin} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Fail over…' }))
    await confirm('Fail over')
    await waitFor(() => expect(ran(ops, 'ms.ag.failover')?.msrepl).toEqual({ group: 'ag1', force: true }))
  })

  it('adds the database to a group, but not under simple recovery', async () => {
    const { admin } = fakeAdmin({ ...server, recovery: 'SIMPLE', distributor: false, groups: [{ name: 'ag1', clusterType: 'NONE' }] })
    ui(<MSSQLReplication {...base} admin={admin} />)
    expect(await screen.findByText(/needs full recovery/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Add…' })).toBeDisabled()
  })

  it('sets the server up as its distributor in the snapshot folder', async () => {
    const { admin, ops } = fakeAdmin({ ...server, distributor: false, replDir: '/var/opt/mssql/ReplData' })
    ui(<MSSQLReplication {...base} admin={admin} />)
    fireEvent.click(await screen.findByRole('tab', { name: 'Publications' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Set up…' }))
    await confirm('Set up')
    await waitFor(() => expect(ran(ops, 'ms.repl.distributor')?.msrepl).toEqual({ workingDir: '/var/opt/mssql/ReplData' }))
  })

  it('publishes a table with a primary key and subscribes another server with its login', async () => {
    const { admin, ops } = fakeAdmin({ ...server, distributor: true, published: true, databases: ['reports'],
      tables: [{ schema: 'dbo', name: 'orders', pk: true }, { schema: 'dbo', name: 'audit', pk: false }, { schema: 'dbo', name: 'customers', pk: true }],
      publications: [{ name: 'shop', kind: 'transactional', active: true,
        articles: [{ name: 'customers', schema: 'dbo', table: 'customers' }],
        subscriptions: [{ subscriber: 'MSSQL1', db: 'reports', status: 'active', agent: { status: 'idle', latencyMs: 40 } }],
        snapshot: { status: 'succeeded' } }],
    })
    ui(<MSSQLReplication {...base} admin={admin} />)
    fireEvent.click(await screen.findByRole('tab', { name: 'Publications' }))
    expect(await screen.findByText('40 ms latency')).toBeInTheDocument()
    expect(screen.getByText('(this server)')).toBeInTheDocument()

    fireEvent.click(screen.getAllByLabelText('Table to publish')[0]!)
    expect(await screen.findByRole('option', { name: 'dbo.audit (no primary key)', hidden: true })).toHaveAttribute('data-combobox-disabled')
    fireEvent.click(await screen.findByRole('option', { name: 'dbo.orders', hidden: true }))
    fireEvent.click(screen.getAllByRole('button', { name: 'Add…' })[0]!)
    await confirm('Add article')
    await waitFor(() => expect(ran(ops, 'ms.repl.article.add')?.msrepl).toEqual({ publication: 'shop', schema: 'dbo', table: 'orders' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    fireEvent.change(screen.getByLabelText('Subscriber server'), { target: { value: 'replica.example.com,1433' } })
    fireEvent.change(screen.getByLabelText('Database there'), { target: { value: 'shop_copy' } })
    expect(screen.getByRole('button', { name: 'Subscribe…' })).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Login there'), { target: { value: 'repl' } })
    fireEvent.change(screen.getByLabelText('Its password'), { target: { value: 's3cret!' } })
    fireEvent.click(screen.getByRole('button', { name: 'Subscribe…' }))
    await confirm('Subscribe')
    await waitFor(() => expect(ran(ops, 'ms.repl.sub.add')?.msrepl).toEqual({
      publication: 'shop', subscriber: 'replica.example.com,1433', subscriberDb: 'shop_copy', login: 'repl', password: 's3cret!',
    }))

    // The local subscription is dropped without naming the server.
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    const row = screen.getByText('reports', { selector: 'td' }).closest('tr')!
    fireEvent.click(within(row).getByRole('button', { name: 'Drop' }))
    await confirm('Drop subscription')
    await waitFor(() => expect(ran(ops, 'ms.repl.sub.drop')?.msrepl).toEqual({ publication: 'shop', subscriberDb: 'reports' }))
  })

  it('runs a log shipping secondary’s jobs', async () => {
    const { admin, ops } = fakeAdmin({ ...server, distributor: false, logShipping: {
      role: 'secondary', primary: 'mssql0', lastCopied: '2026-11-19T08:00:00Z', lastRestored: '2026-11-19T07:45:00Z', restoreLagMin: 75, restoreAlertMin: 45,
    } })
    ui(<MSSQLReplication {...base} admin={admin} />)
    fireEvent.click(await screen.findByRole('tab', { name: 'Log shipping' }))
    expect(await screen.findByText(/75 min behind/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Run restore now…' }))
    await confirm('Run')
    await waitFor(() => expect(ran(ops, 'ms.ls.restore')).toBeTruthy())
  })
})
