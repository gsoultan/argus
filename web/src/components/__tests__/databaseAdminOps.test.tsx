import { describe, expect, it, vi } from 'vitest'
import { screen, waitFor, fireEvent } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { ExtensionsPanel, updateTarget } from '~/components/DatabaseExtensions'
import { ReplicationPanel, fmtLag, subscriptionStatus, type PGSubscription } from '~/components/DatabaseReplication'
import { parseRecording } from '~/components/PostgresReplay'
import type { Admin, AdminOp, Collect } from '~/components/DatabaseAdmin'

/*
The two panels are gated at DBA, send the agent fixed operations rather than
SQL, and never show a publisher's password. The agent is the authority on all
three; these tests cover what the console promises on top.
*/

const collect: Collect = async () => ({ cols: [], rows: [] })
const sub: PGSubscription = {
  name: 'orders_from_eu', enabled: true, publisher: 'replicator@eu-db-01:5432/orders',
  conninfo: 'host=eu-db-01 port=5432 dbname=orders user=replicator password=[redacted] sslmode=require',
  publications: ['orders_pub'], slot: 'orders_from_eu', owner: 'postgres', setUpBy: 'r.hakim@corp.example',
  workerPid: 48213, applyLagS: 1, tablesReady: 4, tablesSyncing: 0, tablesWaiting: 0, applyErrors: 0, syncErrors: 0,
}

function fakeAdmin(answers: (op: AdminOp) => unknown): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => {
    ops.push(op)
    if (op.preview) return { data: { sql: `-- preview of ${op.kind}` } }
    return { data: answers(op), tag: 'OK' }
  }) as Admin
  return { admin, ops }
}

const base = { collect, levels: {}, self: 'r_hakim', narrow: false, onBack: () => {} }

describe('Extensions panel', () => {
  it('is labelled, not hidden, below DBA and asks the agent for nothing', async () => {
    const { admin, ops } = fakeAdmin(() => [])
    const ask = vi.fn()
    ui(<ExtensionsPanel {...base} admin={admin} level="owner" onAskForAccess={ask} />)
    expect(screen.getByText('Needs DBA access')).toBeInTheDocument()
    fireEvent.click(screen.getByText('Ask for access'))
    expect(ask).toHaveBeenCalled()
    expect(ops).toHaveLength(0)
  })

  it('lists installed and available extensions from the helper, with the update it offers', async () => {
    const { admin, ops } = fakeAdmin(() => [
      { name: 'plpgsql', installedVersion: '1.0', defaultVersion: '1.0', versions: ['1.0'], schema: 'pg_catalog' },
      { name: 'pg_stat_statements', installedVersion: '1.10', defaultVersion: '1.11', versions: ['1.8', '1.9', '1.10', '1.11'], schema: 'public' },
      { name: 'hstore', defaultVersion: '1.8', versions: ['1.8'], trusted: true, comment: 'key/value pairs' },
    ])
    ui(<ExtensionsPanel {...base} admin={admin} level="dba" />)
    await waitFor(() => expect(screen.getByText('hstore')).toBeInTheDocument())
    expect(ops[0]).toEqual({ kind: 'extension.list' })
    expect(screen.getByText('update to 1.11')).toBeInTheDocument()
    expect(screen.getByText('trusted')).toBeInTheDocument()
    expect(screen.getByText('built in')).toBeInTheDocument()
    // Install opens the form, which shows the agent's preview before the button.
    fireEvent.click(screen.getByText('Install'))
    await waitFor(() => expect(screen.getByText('-- preview of extension.create')).toBeInTheDocument())
    expect(ops.at(-1)).toMatchObject({ kind: 'extension.create', name: 'hstore', preview: true })
  })

  it('offers an update only when the server has a newer version', () => {
    expect(updateTarget({ name: 'a', installedVersion: '1.10', versions: ['1.9', '1.10', '1.11'] })).toBe('1.11')
    expect(updateTarget({ name: 'a', installedVersion: '1.11', versions: ['1.9', '1.10', '1.11'] })).toBeUndefined()
    expect(updateTarget({ name: 'a', versions: ['1.0'] })).toBeUndefined()
  })
})

describe('Replication panel', () => {
  it('shows a subscription with its status and lag and never a password', async () => {
    const { admin } = fakeAdmin((op) => (op.kind === 'subscription.list' ? [sub] : []))
    const { container } = ui(<ReplicationPanel {...base} admin={admin} level="dba" />)
    await waitFor(() => expect(screen.getByText('Subscriptions · 1')).toBeInTheDocument())
    fireEvent.click(screen.getByText('Subscriptions · 1'))
    await waitFor(() => expect(screen.getByText('orders_from_eu')).toBeInTheDocument())
    expect(screen.getByText('streaming')).toBeInTheDocument()
    expect(screen.getByText('password not shown')).toBeInTheDocument()
    expect(container.innerHTML).not.toMatch(/password=(?!\[redacted\])/)
  })

  it('requires typing the name before a drop, and sends the publisher-gone flag', async () => {
    const { admin, ops } = fakeAdmin((op) => (op.kind === 'subscription.list' ? [sub] : []))
    ui(<ReplicationPanel {...base} admin={admin} level="dba" />)
    await waitFor(() => expect(screen.getByText('Subscriptions · 1')).toBeInTheDocument())
    fireEvent.click(screen.getByText('Subscriptions · 1'))
    await waitFor(() => expect(screen.getByLabelText('Actions for orders_from_eu')).toBeInTheDocument())
    fireEvent.change(screen.getByLabelText('Actions for orders_from_eu'), { target: { value: 'drop' } })
    const button = await screen.findByRole('button', { name: 'Drop subscription' })
    await waitFor(() => expect(screen.getByText('-- preview of subscription.drop')).toBeInTheDocument())
    expect(button).toBeDisabled()
    fireEvent.click(screen.getByLabelText(/The publisher is gone/))
    fireEvent.change(screen.getByLabelText('Type orders_from_eu to confirm'), { target: { value: 'orders_from_eu' } })
    await waitFor(() => expect(screen.getByRole('button', { name: 'Drop subscription' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: 'Drop subscription' }))
    await waitFor(() => expect(ops.some((o) => o.kind === 'subscription.drop' && !o.preview)).toBe(true))
    expect(ops.find((o) => o.kind === 'subscription.drop' && !o.preview)).toMatchObject({ name: 'orders_from_eu', publisherGone: true })
  })

  it('names the states a subscription can be in', () => {
    expect(subscriptionStatus(sub).label).toBe('streaming')
    expect(subscriptionStatus({ ...sub, enabled: false }).label).toBe('disabled')
    expect(subscriptionStatus({ ...sub, workerPid: 0 }).label).toBe('enabled, no worker')
    expect(subscriptionStatus({ ...sub, applyLagS: 900 }).label).toBe('streaming, behind')
    expect(fmtLag(0)).toBe('0 s')
    expect(fmtLag(840)).toBe('14 min')
    expect(fmtLag(null)).toBe('—')
  })
})

describe('Replay', () => {
  it('shows an admin line as a statement run as the server’s account', () => {
    const text = [
      '{"seq":1,"t":"2026-11-02T07:00:00Z","kind":"open","actor":"a@b","role":"r","level":"dba","prev":""}',
      '{"seq":2,"t":"2026-11-02T07:00:01Z","kind":"admin","req":3,"sql":"CREATE SUBSCRIPTION \\"s\\" CONNECTION E\'host=\'\'h\'\' password=[redacted]\' PUBLICATION \\"p\\"","prev":"x"}',
      '{"seq":3,"t":"2026-11-02T07:00:02Z","kind":"done","req":3,"tag":"CREATE SUBSCRIPTION","ms":40,"prev":"y"}',
    ].join('\n')
    const { statements } = parseRecording(text)
    expect(statements).toHaveLength(1)
    expect(statements[0]).toMatchObject({ admin: true, tag: 'CREATE SUBSCRIPTION', ms: 40 })
    expect(statements[0]!.sql).toContain('password=[redacted]')
  })
})
