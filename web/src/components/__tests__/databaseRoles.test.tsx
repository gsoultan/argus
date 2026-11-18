import { describe, expect, it } from 'vitest'
import { screen, waitFor, fireEvent, within } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { RolesManager, roleKind, type PGRole, type PGRolePrivileges } from '~/components/DatabaseRoles'
import { RolesPanel, type Admin, type AdminOp, type Collect } from '~/components/DatabaseAdmin'

/*
Roles & access sends the agent fixed operations, never SQL it composed; shows
the agent's preview before every change; keeps Argus's own roles read-only;
and says what a role can do, so a grant that did not take is visible.
*/

const collect: Collect = async () => ({ cols: [], rows: [] })
const role = (r: Partial<PGRole> & { name: string }): PGRole => ({
  superuser: false, login: false, inherit: true, createrole: false, createdb: false, replication: false, bypassrls: false,
  connLimit: -1, hasPassword: false, connections: 0, owns: 0, ...r,
})
const roles: PGRole[] = [
  role({ name: 'postgres', superuser: true, login: true, hasPassword: false, helper: true }),
  role({ name: 'hermod', login: true, hasPassword: true, comment: 'ETL loader', memberOf: ['etl_readers'] }),
  role({ name: 'etl_owner', createrole: true }),
  role({ name: 'etl_readers' }),
  role({ name: 'ana_sari', comment: 'argus-managed owner=ana@corp.example', memberOf: ['argus_people'] }),
  role({ name: 'argus_read_shop', comment: 'argus-managed level' }),
]
const hermodPrivs: PGRolePrivileges = {
  role: 'hermod', superuser: false,
  database: { name: 'athena_etl_db', owner: 'postgres', effective: ['CONNECT', 'TEMPORARY'] },
  schemas: [
    { name: 'etl', owner: 'etl_owner', effective: [], objects: 14 },
    { name: 'public', owner: 'pg_database_owner', effective: ['USAGE'], objects: 2 },
  ],
  tables: [{ schema: 'etl', name: 'jobs', kind: 'table', owner: 'etl_owner', effective: ['SELECT'], direct: ['SELECT'] }],
  sequences: [], functions: [], defaults: [],
}

function fakeAdmin(): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => {
    ops.push(op)
    if (op.preview) return { data: { sql: `-- preview of ${op.kind}` } }
    if (op.kind === 'role.list') return { data: roles }
    if (op.kind === 'role.privileges') return { data: hermodPrivs }
    if (op.kind === 'grant.objects') {
      return { data: { schemas: [{ name: 'etl', owner: 'etl_owner' }, { name: 'public', owner: 'postgres' }],
        tables: op.grant?.schema === 'etl' ? [{ name: 'jobs', kind: 'table', owner: 'etl_owner' }] : [],
        owners: op.grant?.schema === 'etl' ? ['etl_owner'] : [] } }
    }
    return { data: { sql: 'done' }, tag: 'GRANT' }
  }) as Admin
  return { admin, ops }
}

const base = { collect, levels: { read: 'argus_read_shop' }, self: 'r_hakim', narrow: false, onBack: () => {} }

describe('Roles & access', () => {
  it('is labelled, not hidden, below DBA', () => {
    ui(<RolesPanel {...base} admin={fakeAdmin().admin} level="write" />)
    expect(screen.getByText('Needs DBA access')).toBeInTheDocument()
  })

  it('lists every role with who owns it', async () => {
    const { admin, ops } = fakeAdmin()
    ui(<RolesManager {...base} admin={admin} level="dba" />)
    await waitFor(() => expect(screen.getByText('hermod')).toBeInTheDocument())
    expect(ops[0]).toEqual({ kind: 'role.list' })
    expect(screen.getByText('ana@corp.example')).toBeInTheDocument()
    expect(screen.getByText('Argus helper')).toBeInTheDocument()
    expect(roleKind('argus-managed level').kind).toBe('level')
    expect(roleKind('ETL loader').kind).toBe('app')
  })

  it('shows what a role can do, and that a schema without USAGE blocks its tables', async () => {
    const { admin, ops } = fakeAdmin()
    ui(<RolesManager {...base} admin={admin} level="dba" />)
    await waitFor(() => expect(screen.getByText('hermod')).toBeInTheDocument())
    fireEvent.click(screen.getByText('hermod'))
    await waitFor(() => expect(screen.getByText('etl.jobs')).toBeInTheDocument())
    expect(ops.at(-1)).toEqual({ kind: 'role.privileges', role: { name: 'hermod' } })
    expect(screen.getByText(/no USAGE on the schema/)).toBeInTheDocument()
  })

  it('grants on every table in a schema, with schema usage and tables made later, through the helper', async () => {
    const { admin, ops } = fakeAdmin()
    ui(<RolesManager {...base} admin={admin} level="dba" />)
    await waitFor(() => expect(screen.getByText('hermod')).toBeInTheDocument())
    fireEvent.click(screen.getByText('hermod'))
    await waitFor(() => expect(screen.getByText('etl.jobs')).toBeInTheDocument())
    // The schema row's Grant opens the form for every table in etl.
    const etlRow = screen.getAllByText('etl').find((el) => el.closest('tr'))!.closest('tr')!
    fireEvent.click(within(etlRow).getByText('Grant…'))
    await waitFor(() => expect(screen.getByText('-- preview of grant')).toBeInTheDocument())
    const preview = ops.filter((o) => o.kind === 'grant' && o.preview).at(-1)!
    expect(preview.grant).toMatchObject({ grantee: 'hermod', on: 'all-tables', schema: 'etl', privileges: ['SELECT'],
      schemaUsage: true, future: true, forRoles: ['etl_owner'] })
    fireEvent.click(screen.getByRole('button', { name: 'Grant' }))
    await waitFor(() => expect(ops.some((o) => o.kind === 'grant' && !o.preview)).toBe(true))
  })

  it('keeps Argus’s own roles read-only', async () => {
    const { admin } = fakeAdmin()
    ui(<RolesManager {...base} admin={admin} level="dba" />)
    await waitFor(() => expect(screen.getByText('ana_sari')).toBeInTheDocument())
    fireEvent.click(screen.getByText('ana_sari'))
    await waitFor(() => expect(screen.getByText(/person’s role that Argus manages/)).toBeInTheDocument())
    expect(screen.queryByText('Drop role')).not.toBeInTheDocument()
    expect(screen.queryByText('Change role')).not.toBeInTheDocument()
  })

  it('creates a role, sending the password only to the agent and showing its redacted preview', async () => {
    const { admin, ops } = fakeAdmin()
    ui(<RolesManager {...base} admin={admin} level="dba" />)
    await waitFor(() => expect(screen.getByText('hermod')).toBeInTheDocument())
    fireEvent.click(screen.getByText('New role'))
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'loki' } })
    fireEvent.change(screen.getByLabelText(/^Password$/), { target: { value: 'pw-loki-123' } })
    await waitFor(() => expect(screen.getByText('-- preview of role.create')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: 'Create role' }))
    await waitFor(() => expect(ops.some((o) => o.kind === 'role.create' && !o.preview)).toBe(true))
    expect(ops.find((o) => o.kind === 'role.create' && !o.preview)!.role).toMatchObject({ name: 'loki', login: true, password: 'pw-loki-123' })
  })
})
