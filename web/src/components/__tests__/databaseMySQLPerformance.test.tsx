import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { MySQLPerformancePanel } from '~/components/DatabaseMySQLPerformance'
import type { Admin, AdminOp, Collect } from '~/components/DatabaseAdmin'
import captured from '~/lib/mysqlPerformance.fixture.json'

/*
The MySQL Performance panel asks the agent for one read-only operation,
mysql.perf, and only at DBA; it never sends SQL of its own, and a fix goes
to the editor instead of running.
*/

function fakeAdmin(answer: Awaited<ReturnType<Admin>> = { data: captured }): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => { ops.push(op); return answer }) as Admin
  return { admin, ops }
}
const noSql = vi.fn<Collect>(async () => ({ cols: [], rows: [] }))
const base = { levels: {}, self: 'r_hakim', narrow: false, database: 'shop', collect: noSql }

describe('MySQL Performance', () => {
  it('reads mysql.perf at DBA, explains, and opens fixes in the editor', async () => {
    const { admin, ops } = fakeAdmin()
    const open = vi.fn()
    ui(<MySQLPerformancePanel {...base} admin={admin} level="dba" onOpenSql={open} />)
    expect(await screen.findByText('orders is read without an index')).toBeInTheDocument()
    expect(screen.getByText('audit has no primary key')).toBeInTheDocument()
    expect(screen.getByText('Costliest queries')).toBeInTheDocument()
    expect(ops).toEqual([{ kind: 'mysql.perf' }])
    expect(noSql).not.toHaveBeenCalled()

    fireEvent.click(screen.getAllByText('Open in editor')[0]!)
    expect(open).toHaveBeenCalledTimes(1)
    expect(ops).toHaveLength(1)
  })

  it('asks nothing below DBA and says why', async () => {
    const { admin, ops } = fakeAdmin()
    ui(<MySQLPerformancePanel {...base} admin={admin} level="write" onOpenSql={() => {}} />)
    expect(await screen.findByText('Needs DBA access')).toBeInTheDocument()
    expect(screen.getByText(/run through the server’s own administrator account/)).toBeInTheDocument()
    expect(ops).toEqual([])
  })

  it('says what the agent refused', async () => {
    const { admin } = fakeAdmin({ error: { error: 'reading the server’s statistics needs its administrator account', code: '42501' } })
    ui(<MySQLPerformancePanel {...base} admin={admin} level="dba" onOpenSql={() => {}} />)
    expect(await screen.findByText(/needs its administrator account/)).toBeInTheDocument()
  })
})
