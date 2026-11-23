import { describe, expect, it } from 'vitest'
import {
  connectionEnv, connectionString, newOwnerState, ownerName, ownerReady, ownerRequest, passwordProblem, suggestedOwner, userKey,
} from '~/components/OwnerFields'

describe('owner of a new database', () => {
  it('suggests <db>_app and follows the name until one is typed', () => {
    expect(suggestedOwner('invoices')).toBe('invoices_app')
    expect(suggestedOwner('My-Shop')).toBe('my_shop_app')
    expect(suggestedOwner('')).toBe('')
    const s = { ...newOwnerState('new') }
    expect(ownerName(s, 'crm', [])).toBe('crm_app')
    expect(ownerName({ ...s, name: 'app', named: true }, 'crm', [])).toBe('app')
  })

  it('builds the request per engine', () => {
    const s = { ...newOwnerState('new'), connLimit: 20, policy: true }
    expect(ownerRequest(s, 'postgres', 'shop', [])).toEqual({ kind: 'new', name: 'shop_app', connLimit: 20 })
    expect(ownerRequest(s, 'mysql', 'shop', [])).toEqual({ kind: 'new', name: 'shop_app', from: '%' })
    expect(ownerRequest(s, 'mssql', 'shop', [])).toEqual({ kind: 'new', name: 'shop_app', policy: true })
    expect(ownerRequest(newOwnerState(''), 'postgres', 'shop', [])).toBeUndefined()
    const users = [{ name: 'reports', from: 'admin' }]
    expect(ownerRequest({ ...newOwnerState('existing'), picked: userKey(users[0]!) }, 'mongodb', 'shop', users))
      .toEqual({ kind: 'existing', name: 'reports', from: 'admin' })
  })

  it('checks a typed password, never a generated one', () => {
    const typed = { ...newOwnerState('new'), pwMode: 'type' as const }
    expect(passwordProblem({ ...typed, typed: 'short' }, 'postgres')).not.toBe('')
    expect(passwordProblem({ ...typed, typed: 'long-enough-password' }, 'postgres')).toBe('')
    expect(passwordProblem({ ...typed, typed: 'alllowercaseletters', policy: true }, 'mssql')).not.toBe('')
    expect(passwordProblem(newOwnerState('new'), 'postgres')).toBe('')
    expect(ownerReady({ ...typed, typed: 'short' }, 'postgres', 'shop', [])).toBe(false)
    expect(ownerReady(newOwnerState('existing'), 'postgres', 'shop', [])).toBe(false)
  })

  it('writes connection strings with the password escaped', () => {
    const c = { host: 'db-01', port: 6432, database: 'shop', user: 'shop_app' }
    expect(connectionString('postgres', c, 'a@b/c')).toBe('postgresql://shop_app:a%40b%2Fc@db-01:6432/shop')
    expect(connectionString('mongodb', c, 'p')).toBe('mongodb://shop_app:p@db-01:6432/shop?authSource=shop')
    expect(connectionString('mssql', c, 'p')).toBe('sqlserver://shop_app:p@db-01:6432?database=shop')
    expect(connectionEnv('mysql', c, 'p')).toContain('DB_PASSWORD=p\nDATABASE_URL=mysql://shop_app:p@db-01:6432/shop')
  })
})
