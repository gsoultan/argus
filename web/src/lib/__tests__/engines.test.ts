import { describe, expect, it } from 'vitest'
import { isActive } from '~/components/nav'
import { engineVersion, isSQLEngine, isStatementProtocol, levelName } from '~/lib/engines'
import type { DBLevel } from '~/types/domain'

describe('engines', () => {
  it('treats Postgres and MySQL as SQL, and a missing engine as Postgres', () => {
    expect(isSQLEngine('postgres')).toBe(true)
    expect(isSQLEngine('mysql')).toBe(true)
    expect(isSQLEngine(undefined)).toBe(true)
    expect(isSQLEngine('redis')).toBe(false)
    expect(isSQLEngine('rabbitmq')).toBe(false)
  })

  it('names the server from the version the agent reported', () => {
    expect(engineVersion('postgres', '16.4 (Ubuntu 16.4-1)')).toBe('Postgres 16.4')
    expect(engineVersion(undefined, '15.2')).toBe('Postgres 15.2')
    expect(engineVersion('mysql', '8.0.46')).toBe('MySQL 8.0.46')
    expect(engineVersion('mysql', '8.0.46-0ubuntu0.24.04.1')).toBe('MySQL 8.0.46')
    expect(engineVersion('mysql', '10.11.14-MariaDB-0ubuntu0.24.04.1')).toBe('MariaDB 10.11.14')
  })

  it('records MySQL sessions statement by statement', () => {
    expect(isStatementProtocol('mysql')).toBe(true)
  })
})

describe('levelName', () => {
  it('names broker levels in broker words, not database ones', () => {
    expect(['read', 'write', 'owner', 'dba'].map((l) => levelName(l as DBLevel, 'rabbitmq'))).toEqual(['Read', 'Write', 'Manage', 'Admin'])
    expect(levelName('dba', 'kafka')).toBe('Admin')
    expect(levelName('dba', 'postgres')).toBe('DBA')
    expect(levelName('owner', 'mysql')).toBe('Owner')
  })
})

describe('the menu on a broker’s settings', () => {
  // "Who can connect" on a broker used to open /databases/<id>/settings, so
  // the top menu lit up Databases.
  it('is Brokers, not Databases', () => {
    expect(isActive('/brokers/b1/settings', '/brokers')).toBe(true)
    expect(isActive('/brokers/b1/settings', '/databases')).toBe(false)
  })
})
