import { describe, expect, it } from 'vitest'
import type { CloudItem } from './cloud'
import { nextReplicaName, replicaFamilies, replicaProblem, validDBName } from './cloudReplicas'

const db = (name: string, details: Record<string, string> = {}, state = 'available', accountId = 'a1'): CloudItem => ({
  accountId, accountName: 'payments', environment: 'production', firstSeen: '', seenAt: '',
  resource: { kind: 'database', id: `arn:${name}`, name, region: 'ap-southeast-3', state, size: 'db.r6i.large',
    details: { engine: 'postgres', backup_days: '7', ...details } },
}) as CloudItem

describe('read replicas', () => {
  it('groups replicas under their primary, and keeps an orphan on its own', () => {
    const f = replicaFamilies([db('orders-r1', { replica_of: 'orders' }), db('orders'), db('lone-r1', { replica_of: 'gone' }), db('billing')])
    expect(f.map((x) => [x.primary.resource.name, x.replicas.map((r) => r.resource.name)])).toEqual([
      ['billing', []], ['lone-r1', []], ['orders', ['orders-r1']],
    ])
  })

  it('says why a replica cannot be added', () => {
    expect(replicaProblem(db('orders'))).toBeNull()
    expect(replicaProblem(db('o', { backup_days: '0' }))).toMatch(/backups, which are off/)
    expect(replicaProblem(db('o', { engine: 'aurora-postgresql', cluster: 'c' }))).toMatch(/Aurora cluster/)
    expect(replicaProblem(db('o', { engine: 'sqlserver-ee' }))).toMatch(/not sqlserver-ee/)
    expect(replicaProblem(db('o', { replica_of: 'x' }))).toMatch(/itself a replica of x/)
    expect(replicaProblem(db('o', {}, 'modifying'))).toMatch(/is modifying/)
  })

  it('names the next replica and checks names', () => {
    expect(nextReplicaName(db('orders'), [db('orders'), db('orders-r1'), db('orders-r2', {}, 'available', 'other')])).toBe('orders-r2')
    expect(validDBName('orders-r1')).toBe(true)
    expect(validDBName('1orders')).toBe(false)
    expect(validDBName('a--b')).toBe(false)
    expect(validDBName('trailing-')).toBe(false)
  })
})
