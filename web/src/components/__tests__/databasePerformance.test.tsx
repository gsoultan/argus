import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { PerformancePanel } from '~/components/DatabasePerformance'
import type { Admin, AdminOp, Collect } from '~/components/DatabaseAdmin'

/*
The Performance panel reads only (every statement it sends is a SELECT, so
the recording holds reads), asks the agent for PgBouncer only at DBA, and
never runs a fix: it hands it to the editor.
*/

const overview = ['160004', '100', '3', '99000000', '100000', '1000000', '1000', '0', '0', '0', '2026-11-01 00:00:00+00',
  '2592000', '21474836480', '1', 'pg_stat_statements', 'false', '1000']
const orders = ['public', 'orders', '400000', '0', '5046', '672000000', '1', 'x', 'x', '0', '82000000', '60000000', '0', '0']
const statement = ['7', 'select count(*) from orders where customer_id = $1', '1684', '28200', '17', '3', '1684', '1000', '0', '0', 'app', '0.24']

function fakeCollect(): { collect: Collect; sent: string[] } {
  const sent: string[] = []
  const collect: Collect = async (sql) => {
    sent.push(sql)
    if (sql.includes('pg_stat_database d')) return { cols: [], rows: [overview] }
    if (sql.includes('pg_stat_statements s')) return { cols: [], rows: [statement] }
    if (sql.includes('pg_stat_user_tables t')) return { cols: [], rows: [orders] }
    return { cols: [], rows: [] }
  }
  return { collect, sent }
}

const waitingPool = {
  found: true, through: true, port: 6432, running: true, version: 'PgBouncer 1.23.1', settingsFrom: 'console',
  settings: { pool_mode: 'transaction', default_pool_size: '4', max_client_conn: '100', idle_transaction_timeout: '60', max_prepared_statements: '100' },
  pools: { cols: ['database', 'user', 'cl_active', 'cl_waiting', 'sv_active', 'sv_idle', 'sv_used', 'maxwait', 'maxwait_us', 'pool_mode'],
    rows: [['shop', 'app', '4', '20', '4', '0', '0', '3', '0', 'transaction']] },
}

function fakeAdmin(): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => { ops.push(op); return { data: waitingPool } }) as Admin
  return { admin, ops }
}

const base = { levels: {}, self: 'r_hakim', narrow: false, database: 'shop', viaPooler: true }

describe('Performance', () => {
  it('reads with SELECTs only, explains, and reads PgBouncer at DBA', async () => {
    const { collect, sent } = fakeCollect()
    const { admin, ops } = fakeAdmin()
    const open = vi.fn()
    ui(<PerformancePanel {...base} collect={collect} admin={admin} level="dba" onOpenSql={open} />)
    expect(await screen.findByText('Clients queue for a connection in pool shop/app')).toBeInTheDocument()
    expect(screen.getByText('public.orders is read whole, over and over')).toBeInTheDocument()
    expect(sent.length).toBeGreaterThan(5)
    for (const s of sent) expect(s.trimStart()).toMatch(/^SELECT/)
    expect(ops).toEqual([{ kind: 'pooler.stats' }])

    // The fix goes to the editor, never runs from here.
    const before = sent.length
    fireEvent.click(screen.getAllByText('Open in editor')[0]!)
    expect(open).toHaveBeenCalledTimes(1)
    expect(String(open.mock.calls[0]![0])).toMatch(/CREATE INDEX CONCURRENTLY ON public\.orders \(customer_id\)|EXPLAIN/)
    expect(sent.length).toBe(before)
  })

  it('does not ask the agent for PgBouncer below DBA, and says so', async () => {
    const { collect } = fakeCollect()
    const { admin, ops } = fakeAdmin()
    ui(<PerformancePanel {...base} collect={collect} admin={admin} level="read" onOpenSql={() => {}} />)
    await waitFor(() => expect(screen.getByText('PgBouncer’s own numbers need DBA access')).toBeInTheDocument())
    expect(ops).toEqual([])
  })

  it('says what Postgres refused', async () => {
    const collect: Collect = async () => ({ cols: [], rows: [], error: { error: 'permission denied for view pg_stat_database', code: '42501' } })
    ui(<PerformancePanel {...base} collect={collect} admin={fakeAdmin().admin} level="read" onOpenSql={() => {}} />)
    expect(await screen.findByText(/Postgres refused this: permission denied/)).toBeInTheDocument()
  })
})
