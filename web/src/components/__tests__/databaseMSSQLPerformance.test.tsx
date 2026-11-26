import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { MSSQLPerformance } from '~/components/DatabaseMSSQLDBA'
import type { Admin, AdminOp, Collect } from '~/components/DatabaseAdmin'
import captured from '~/lib/mssqlPerformance.fixture.json'

/*
SQL Server's Performance panel reads four fixed, read-only operations and
nothing else, explains what it found, and hands a fix to the editor instead
of running it.
*/

const reads: Record<string, unknown> = {
  'ms.performance': captured.performance, 'ms.indexes': captured.indexes, 'ms.settings': captured.settings, 'ms.health': captured.health,
}
function fakeAdmin(fail?: string): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => {
    ops.push(op)
    if (op.kind === fail) return { error: { error: 'VIEW SERVER STATE permission was denied', code: '300' } }
    return { data: reads[op.kind] }
  }) as Admin
  return { admin, ops }
}
const noSql = vi.fn<Collect>(async () => ({ cols: [], rows: [] }))
const base = { levels: {}, self: 'argus-ana-dba-0a1b2c3d', narrow: false, level: 'dba' as const, collect: noSql }

describe('SQL Server Performance', () => {
  it('reads only, explains, and opens a fix in the editor', async () => {
    const { admin, ops } = fakeAdmin()
    const open = vi.fn()
    ui(<MSSQLPerformance {...base} admin={admin} onOpenSql={open} />)
    expect(await screen.findByText('Auto shrink is on')).toBeInTheDocument()
    expect(screen.getByText('orders.st2 duplicates st')).toBeInTheDocument()
    expect(screen.getByText(/A heavy query: SELECT @n = COUNT/)).toBeInTheDocument()
    expect(ops.map((o) => o.kind).sort()).toEqual(['ms.health', 'ms.indexes', 'ms.performance', 'ms.settings'])
    expect(noSql).not.toHaveBeenCalled()

    fireEvent.click(screen.getAllByText('Open in editor')[0]!)
    expect(open).toHaveBeenCalledTimes(1)
    expect(String(open.mock.calls[0]![0])).toContain('CREATE INDEX [IX_orders_customer_id]')
    expect(ops).toHaveLength(4)
  })

  it('says what SQL Server refused', async () => {
    const { admin } = fakeAdmin('ms.health')
    ui(<MSSQLPerformance {...base} admin={admin} />)
    expect(await screen.findByText(/VIEW SERVER STATE permission was denied/)).toBeInTheDocument()
  })
})
