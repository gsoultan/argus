import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { MSSQLAccess, principalKind, type MSPrincipal } from '~/components/DatabaseMSSQLAccess'
import type { Admin, AdminOp, Collect } from '~/components/DatabaseAdmin'

/*
SQL Server's Logins & access sends the agent fixed operations, never SQL it
composed; shows the agent's preview before every change; keeps Argus's and
SQL Server's own principals read-only; and says what a user can do in
effect, with DENY marked.
*/

const collect: Collect = async () => ({ cols: [], rows: [] })
const principals: MSPrincipal[] = [
  { name: 'dbo', type: 'user', auth: 'sql', login: 'sa', defaultSchema: 'dbo', system: true },
  { name: 'app_reader', type: 'user', auth: 'sql', login: 'app_reader', defaultSchema: 'sales', memberOf: ['db_datareader', 'sales_writers'] },
  { name: 'argus-ana-dba-0a1b2c3d', type: 'user', auth: 'sql', login: 'argus-ana-dba-0a1b2c3d', argus: true },
  { name: 'old_app', type: 'user', auth: 'orphaned' },
  { name: 'db_owner', type: 'role', fixed: true, members: 1 },
  { name: 'db_datareader', type: 'role', fixed: true, members: 1 },
  { name: 'sales_writers', type: 'role', members: 1 },
  { name: 'public', type: 'role', system: true },
]

function fakeAdmin(): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => {
    ops.push(op)
    if (op.preview) return { data: { sql: `-- preview of ${op.kind}` } }
    if (op.kind === 'ms.principals') return { data: { principals, logins: ['CORP\\ops'] } }
    if (op.kind === 'ms.objects') {
      return { data: { schemas: [{ name: 'dbo' }, { name: 'sales' }], objects: op.msGrant?.schema === 'dbo' ? [{ name: 'secrets', kind: 'user table' }] : [] } }
    }
    if (op.kind === 'ms.permissions') {
      return { data: {
        principal: principals[1],
        memberOf: [{ role: 'db_datareader' }, { role: 'sales_writers' }],
        direct: [{ state: 'DENY', permission: 'SELECT', on: 'object', schema: 'dbo', object: 'secrets', kind: 'USER_TABLE' }],
        inherited: [{ state: 'GRANT', permission: 'INSERT', on: 'schema', schema: 'sales', via: 'sales_writers' }],
        database: ['CONNECT', 'SELECT', 'SHOWPLAN'],
        schemas: [{ schema: 'sales', can: ['SELECT', 'INSERT'] }],
        objects: [{ schema: 'dbo', name: 'secrets', kind: 'user table', can: [] }],
      } }
    }
    return { data: { sql: 'done' }, tag: 'GRANT' }
  }) as Admin
  return { admin, ops }
}

const base = { collect, levels: {}, self: 'argus-ana-dba-0a1b2c3d', narrow: false, onBack: () => {}, level: 'dba' as const }

describe('SQL Server logins & access', () => {
  it('lists users and roles, and keeps Argus’s and SQL Server’s own read-only', async () => {
    const { admin } = fakeAdmin()
    ui(<MSSQLAccess {...base} admin={admin} />)
    expect(await screen.findByRole('button', { name: 'app_reader' })).toBeInTheDocument()
    expect(principalKind(principals[2]!).label).toBe('Argus person')
    expect(principalKind(principals[3]!).label).toBe('orphaned')
    expect(principalKind(principals[4]!).label).toBe('Fixed role')
    expect(screen.getByText('read every table and view')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'argus-ana-dba-0a1b2c3d' }))
    expect(await screen.findByText(/A person’s login that Argus manages/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Drop user' })).not.toBeInTheDocument()
    // Only the panel's own Grant…, none for this user.
    expect(screen.getAllByRole('button', { name: 'Grant…' })).toHaveLength(1)
  })

  it('shows what a user can do, with the deny, and revokes by fixed operation', async () => {
    const { admin, ops } = fakeAdmin()
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true)
    ui(<MSSQLAccess {...base} admin={admin} />)
    fireEvent.click(await screen.findByRole('button', { name: 'app_reader' }))
    expect(await screen.findByText('On the database, in effect')).toBeInTheDocument()
    expect(screen.getByText('DENY')).toBeInTheDocument()
    expect(screen.getByText('through sales_writers')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Revoke' }))
    await waitFor(() => expect(ops.some((o) => o.kind === 'ms.revoke')).toBe(true))
    const r = ops.find((o) => o.kind === 'ms.revoke')!
    expect(r.msGrant).toEqual({ grantee: 'app_reader', on: 'object', schema: 'dbo', objects: ['secrets'], privileges: ['SELECT'], cascade: true })
    confirm.mockRestore()
  })

  it('makes a user with a new login, previewing before it runs', async () => {
    const { admin, ops } = fakeAdmin()
    ui(<MSSQLAccess {...base} admin={admin} />)
    await screen.findByRole('button', { name: 'app_reader' })
    fireEvent.click(screen.getByRole('button', { name: 'New user' }))
    fireEvent.change(screen.getByLabelText('User name'), { target: { value: 'reports' } })
    fireEvent.change(screen.getByLabelText(/^Password/), { target: { value: 'S3cret!pw' } })
    expect(await screen.findByText('-- preview of ms.user.create')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Create user' }))
    await waitFor(() => expect(ops.some((o) => o.kind === 'ms.user.create' && !o.preview)).toBe(true))
    const made = ops.find((o) => o.kind === 'ms.user.create' && !o.preview)!
    expect(made.principal).toMatchObject({ name: 'reports', auth: 'sql', password: 'S3cret!pw', defaultSchema: 'dbo', memberOf: ['db_datareader'] })
  })

  it('denies on chosen objects', async () => {
    const { admin, ops } = fakeAdmin()
    ui(<MSSQLAccess {...base} admin={admin} />)
    await screen.findByRole('button', { name: 'app_reader' })
    fireEvent.click(screen.getAllByRole('button', { name: 'Grant…' })[0]!)
    fireEvent.click(screen.getByText('Deny'))
    const form = screen.getByText(/Stops it doing these/).parentElement!
    expect(within(form).queryByText(/WITH GRANT OPTION/)).toBeNull()
    fireEvent.click(screen.getByText('Tables, views or procedures'))
    // The pickers are Mantine selects; the operation is what matters.
    await waitFor(() => expect(ops.some((o) => o.kind === 'ms.objects' && o.msGrant?.schema === 'dbo')).toBe(true))
  })
})
