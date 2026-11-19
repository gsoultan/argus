import { describe, expect, it } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import {
  MSSQLActivity, MSSQLIndexes, MSSQLJobs, MSSQLSettings, headBlockers, indexAdvice, type MSIndexRow,
} from '~/components/DatabaseMSSQLDBA'
import type { Admin, AdminOp, Collect } from '~/components/DatabaseAdmin'

/*
SQL Server's DBA tools read through fixed operations and change only after a
confirmation that shows the agent's preview: ending a session, rebuilding an
index, starting a job, changing a setting.
*/

const collect: Collect = async () => ({ cols: [], rows: [] })
const base = { collect, levels: {}, self: 'argus-ana-dba-0a1b2c3d', narrow: false, onBack: () => {}, level: 'dba' as const }

function fakeAdmin(data: Record<string, unknown>): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => {
    ops.push(op)
    if (op.preview) return { data: { sql: `-- preview of ${op.kind}` } }
    if (op.kind in data) return { data: data[op.kind] }
    return { data: { sql: 'done' }, tag: 'DONE' }
  }) as Admin
  return { admin, ops }
}

const ran = (ops: AdminOp[], kind: string) => ops.find((o) => o.kind === kind && !o.preview)

describe('SQL Server DBA tools', () => {
  it('finds the head of a blocking chain', () => {
    const s = (session: number, blockedBy?: number) => ({ session, blockedBy, login: 'x', status: 'running' })
    expect([...headBlockers([s(51), s(52, 51), s(53, 52), s(60, 61)])].sort()).toEqual([51, 61])
  })

  it('advises upkeep only for big, fragmented indexes', () => {
    const ix = (pages: number, fragmentation: number, name = 'ix'): MSIndexRow =>
      ({ schema: 'dbo', table: 't', name, type: 'nonclustered', pages, fragmentation, seeks: 0, scans: 0, lookups: 0, updates: 0 })
    expect(indexAdvice(ix(5000, 45))).toBe('rebuild')
    expect(indexAdvice(ix(5000, 12))).toBe('reorganize')
    expect(indexAdvice(ix(5000, 2))).toBe('')
    expect(indexAdvice(ix(200, 90))).toBe('')
    expect(indexAdvice(ix(5000, 90, ''))).toBe('')
  })

  it('shows who blocks whom, and ends a session after its preview', async () => {
    const { admin, ops } = fakeAdmin({ 'ms.activity': { sessions: [
      { session: 61, login: 'app', host: 'web-1', status: 'sleeping', openTran: 1, statement: 'UPDATE orders SET …' },
      { session: 64, login: 'reports', status: 'suspended', command: 'SELECT', wait: 'LCK_M_S', waitMs: 5000, blockedBy: 61, statement: 'SELECT * FROM orders' },
      { session: 70, login: 'argus-ana-dba-0a1b2c3d', status: 'running', command: 'SELECT', yours: true },
    ] } })
    ui(<MSSQLActivity {...base} admin={admin} />)
    expect(await screen.findByText('head blocker')).toBeInTheDocument()
    expect(screen.getByText('LCK_M_S 5.0 s')).toBeInTheDocument()
    // Not your own.
    expect(screen.getAllByRole('button', { name: 'End' })).toHaveLength(2)
    fireEvent.click(screen.getAllByRole('button', { name: 'End' })[0]!)
    expect(await screen.findByText('-- preview of ms.kill')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'End session' }))
    await waitFor(() => expect(ran(ops, 'ms.kill')?.msdba).toEqual({ session: 61 }))
  })

  it('rebuilds an index online, and updates statistics from every row', async () => {
    const { admin, ops } = fakeAdmin({ 'ms.indexes': {
      indexes: [{ schema: 'dbo', table: 'orders', name: 'ix_customer', type: 'nonclustered', pages: 9000, fragmentation: 41, seeks: 10, scans: 0, lookups: 0, updates: 99 }],
      statistics: [{ schema: 'dbo', table: 'orders', name: 'ix_customer', lastUpdated: '2026-11-01T00:00:00Z', rows: 1000, sampled: 300, modified: 900 }],
      missing: [{ schema: 'dbo', table: 'orders', equality: '[status]', seeks: 40, impact: 80, create: 'CREATE INDEX [IX_orders_status] ON [dbo].[orders] ([status])' }],
    } })
    ui(<MSSQLIndexes {...base} admin={admin} />)
    expect(await screen.findByText('41%')).toBeInTheDocument()
    fireEvent.click(screen.getByLabelText('Rebuild online'))
    fireEvent.click(screen.getByRole('button', { name: 'Rebuild' }))
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('-- preview of ms.index.rebuild')).toBeInTheDocument()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Rebuild' }))
    await waitFor(() => expect(ran(ops, 'ms.index.rebuild')?.msdba).toEqual({ schema: 'dbo', table: 'orders', index: 'ix_customer', online: true }))

    fireEvent.click(screen.getByRole('tab', { name: 'Statistics' }))
    fireEvent.click(await screen.findByText('Every row'))
    fireEvent.click(screen.getByRole('button', { name: 'Update' }))
    await screen.findByText('-- preview of ms.stats.update')
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Update' }))
    await waitFor(() => expect(ran(ops, 'ms.stats.update')?.msdba).toEqual({ schema: 'dbo', table: 'orders', index: 'ix_customer', fullScan: true }))
  })

  it('lists this database’s jobs, says the Agent is stopped, and starts one', async () => {
    const { admin, ops } = fakeAdmin({ 'ms.jobs': { agent: 'Stopped', jobs: [
      { name: 'nightly-etl', enabled: true, lastOutcome: 'failed', lastRun: '2026-11-18T02:00:00Z', lastSecs: 75, schedules: 1 },
    ] } })
    ui(<MSSQLJobs {...base} admin={admin} />)
    expect(await screen.findByText(/SQL Server Agent is stopped/)).toBeInTheDocument()
    expect(screen.getByText('failed')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Start' }))
    await screen.findByText('-- preview of ms.job.start')
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Start' }))
    await waitFor(() => expect(ran(ops, 'ms.job.start')?.msdba).toEqual({ job: 'nightly-etl' }))
  })

  it('changes a setting only after confirming, and warns of simple recovery', async () => {
    const { admin, ops } = fakeAdmin({ 'ms.settings': {
      settings: [{ name: 'RECOVERY', value: 'FULL' }, { name: 'COMPATIBILITY_LEVEL', value: '160' }, { name: 'TARGET_RECOVERY_TIME', value: '60' }],
      facts: [{ name: 'State', value: 'ONLINE' }],
      files: [{ name: 'shop', type: 'rows', path: '/var/opt/mssql/data/shop.mdf', sizeMb: 100, usedMb: 40, maxMb: -1, growth: '64 MB' }],
      server: [{ name: 'max degree of parallelism', value: '0' }],
    } })
    ui(<MSSQLSettings {...base} admin={admin} />)
    expect(await screen.findByText(/Simple ends log backups/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Change…' })).toBeNull()
    expect(ops.some((o) => o.kind === 'ms.db.set')).toBe(false)
    fireEvent.change(screen.getByLabelText('Target recovery time (seconds)'), { target: { value: '30' } })
    fireEvent.click(await screen.findByRole('button', { name: 'Change…' }))
    await screen.findByText('-- preview of ms.db.set')
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Change' }))
    await waitFor(() => expect(ran(ops, 'ms.db.set')?.msdba).toEqual({ setting: 'TARGET_RECOVERY_TIME', value: '30' }))
  })
})
