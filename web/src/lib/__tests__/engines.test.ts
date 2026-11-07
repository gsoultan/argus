import { describe, expect, it } from 'vitest'
import { engineVersion, isSQLEngine, isStatementProtocol } from '~/lib/engines'

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
