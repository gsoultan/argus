import { describe, expect, it } from 'vitest'
import { collectionLine, indexLine } from '~/components/mongo/MongoNew'
import { newKeyCommands, redisArg } from '~/components/redis/RedisNewKey'

describe('New collection and New index write one shell line', () => {
  const base = { name: 'events', kind: 'plain' as const, timeField: 'ts', metaField: '', granularity: '', keepDays: '', cappedMB: 100, cappedDocs: '', validator: '' }
  it('plain, time series, capped', () => {
    expect(collectionLine(base).line).toBe('db.createCollection("events")')
    expect(collectionLine({ ...base, kind: 'timeseries', metaField: 'sensor', keepDays: 30 }).line)
      .toBe('db.createCollection("events", { timeseries: { timeField: "ts", metaField: "sensor" }, expireAfterSeconds: 2592000 })')
    expect(collectionLine({ ...base, kind: 'capped', cappedMB: 1 }).line).toBe('db.createCollection("events", { capped: true, size: 1048576 })')
  })
  it('refuses what MongoDB would', () => {
    expect(collectionLine({ ...base, name: 'system.users' }).line).toBe('')
    expect(collectionLine({ ...base, validator: '{not json' }).error).not.toBe('')
    expect(collectionLine({ ...base, kind: 'timeseries', timeField: '' }).line).toBe('')
  })
  it('an index, quoting a collection name with a space', () => {
    expect(indexLine('order items', [{ field: 'email', dir: '1' }], { unique: true, sparse: false, ttlS: '', name: '' }))
      .toBe('db.getCollection("order items").createIndex({ "email": 1 }, { unique: true })')
    expect(indexLine('orders', [{ field: 'body', dir: 'text' }], { unique: false, sparse: false, ttlS: '', name: '' }))
      .toBe('db.orders.createIndex({ "body": "text" })')
  })
})

describe('New key never writes over a key and quotes what it sends', () => {
  it('quotes', () => {
    expect(redisArg('user:42')).toBe('user:42')
    expect(redisArg('a b')).toBe('"a b"')
    expect(redisArg('say "hi"\n')).toBe('"say \\"hi\\"\\n"')
    expect(redisArg('')).toBe('""')
  })
  it('each type', () => {
    expect(newKeyCommands({ key: 'k', type: 'string', value: 'v', ttlS: 60, group: '' }).lines).toEqual(['SET k v NX EX 60'])
    expect(newKeyCommands({ key: 'k', type: 'hash', value: 'name Ana Lima\nplan pro', ttlS: '', group: '' }).lines).toEqual(['HSET k name "Ana Lima" plan pro'])
    expect(newKeyCommands({ key: 'k', type: 'zset', value: '10 ana', ttlS: 5, group: '' }).lines).toEqual(['ZADD k 10 ana', 'EXPIRE k 5'])
    expect(newKeyCommands({ key: 'q', type: 'stream', value: '', ttlS: '', group: 'workers' }).lines).toEqual(['XGROUP CREATE q workers $ MKSTREAM'])
    expect(newKeyCommands({ key: 'q', type: 'stream', value: 'event signup', ttlS: '', group: 'workers' }).lines)
      .toEqual(['XADD q * event signup', 'XGROUP CREATE q workers 0 MKSTREAM'])
    expect(newKeyCommands({ key: 'k', type: 'zset', value: 'x ana', ttlS: '', group: '' }).error).not.toBe('')
  })
})

describe('pgLevelRole matches the agent', async () => {
  const { pgLevelRole } = await import('~/lib/pgRoles')
  it('plain and hashed names', async () => {
    expect(await pgLevelRole('owner', 'invoices')).toBe('argus_owner_invoices')
    // The same names tunnel.PGLevelRole gives.
    expect(await pgLevelRole('owner', 'Orders')).toBe('argus_owner_orders_b7e8acdd')
    expect(await pgLevelRole('owner', 'café')).toBe('argus_owner_caf___850f7dc4')
  })
})
