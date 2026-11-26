import { describe, expect, it } from 'vitest'
import captured from './mongoPerformance.fixture.json'
import { analyzeMongo, at, coll, filterFields, indexKey, sortFields, type MongoCollection, type MongoPerf } from '~/lib/mongoPerformance'

// captured is what the agent's perf answered from a real MongoDB 7: 20k
// orders, indexes status_1 and status_1_total_1, the profiler at slowms 0,
// and 30 counts by customer with no index for it.
const real = captured as unknown as MongoPerf

const NOW = Date.parse('2026-11-26T00:00:00Z')
const DAY = 86400_000

function perf(over: Partial<MongoPerf> & { server?: Record<string, unknown> } = {}): MongoPerf {
  return {
    version: '7.0.0',
    server: {
      uptime: 30 * 86400,
      connections: { current: 50, available: 50000, totalCreated: 500 },
      wiredTiger: { cache: { 'bytes currently in the cache': 1e9, 'maximum bytes configured': 4e9, 'tracked dirty bytes in the cache': 1e7 } },
      metrics: { queryExecutor: { scannedObjects: 1000 }, document: { returned: 1000 } },
      ...over.server,
    },
    db: { dataSize: 1e9, indexSize: 1e8 },
    collections: [],
    profile: { was: 1, slowms: 100 },
    slow: [],
    running: [],
    ...over,
  }
}

function collection(over: Partial<MongoCollection> = {}): MongoCollection {
  return { name: 'orders', count: 1000, size: 1e6, storage: 1e6, free: 0, indexes: [{ name: '_id_', key: { _id: 1 } }], ...over }
}

const ids = (p: MongoPerf) => analyzeMongo(p, NOW).findings.map((f) => f.id)

describe('analyzeMongo on a real MongoDB 7', () => {
  const r = analyzeMongo(real)
  it('finds the unindexed customer counts and offers the index', () => {
    const slow = r.findings.find((f) => f.id.startsWith('mg-slow-'))!
    expect(slow).toBeDefined()
    expect(slow.title).toContain('orders')
    expect(slow.why).toContain('COLLSCAN')
    expect(slow.sql).toBe('db.orders.createIndex({ customer: 1 })')
  })
  it('says the profiler records everything, and status_1 is covered by status_1_total_1', () => {
    expect(r.findings.map((f) => f.id)).toEqual(expect.arrayContaining(['mg-profiler-all', 'mg-redundant-orders.status_1']))
    expect(r.findings.find((f) => f.id === 'mg-redundant-orders.status_1')!.sql).toBe('{ dropIndexes: "orders", index: "status_1" }')
  })
  it('never offers to drop _id_', () => {
    expect(r.findings.some((f) => f.id.includes('_id_'))).toBe(false)
  })
  it('reads the summary numbers', () => {
    expect(r.countingS).toBeGreaterThan(0)
    expect(r.cacheFill).not.toBeNull()
    expect(r.connections.current).toBeGreaterThan(0)
  })
})

describe('helpers', () => {
  it('reads numbers through paths and extended JSON', () => {
    expect(at({ a: { b: 3 } }, 'a', 'b')).toBe(3)
    expect(at({ a: { $numberLong: '12' } }, 'a')).toBe(12)
    expect(at({}, 'a', 'b')).toBe(0)
  })
  it('names collections the command line takes', () => {
    expect(coll('orders')).toBe('db.orders')
    expect(coll('order-items')).toBe('db.getCollection("order-items")')
  })
  it('builds the index key in equality, sort order', () => {
    const ex = { filter: { status: 'A', $and: [{ customer: 1 }] }, sort: { createdAt: -1, status: 1 } }
    expect(filterFields(ex)).toEqual(['status', 'customer'])
    expect(sortFields(ex)).toEqual([['createdAt', -1], ['status', 1]])
    expect(indexKey(ex)).toBe('{ status: 1, customer: 1, createdAt: -1 }')
    expect(indexKey({ pipeline: [{ $match: { 'a.b': 1 } }, { $sort: { c: 1 } }] })).toBe('{ "a.b": 1, c: 1 }')
    expect(indexKey(undefined)).toBe('')
  })
})

describe('analyzeMongo rules', () => {
  it('stays quiet on a healthy server', () => {
    expect(ids(perf())).toEqual([])
  })
  it('warns when connections near the limit', () => {
    const r = analyzeMongo(perf({ server: { connections: { current: 970, available: 30 } } }), NOW)
    expect(r.findings[0]!.id).toBe('mg-conn-limit')
    expect(r.findings[0]!.severity).toBe('high')
  })
  it('flags a full cache only when application threads evict', () => {
    const full = { 'bytes currently in the cache': 3.9e9, 'maximum bytes configured': 4e9 }
    expect(ids(perf({ server: { wiredTiger: { cache: full } } }))).not.toContain('mg-cache-full')
    expect(ids(perf({ server: { wiredTiger: { cache: { ...full, 'pages evicted by application threads': 500 } } } }))).toContain('mg-cache-full')
  })
  it('flags documents examined far beyond those returned, not small numbers', () => {
    expect(ids(perf({ server: { metrics: { queryExecutor: { scannedObjects: 50_000 }, document: { returned: 10 } } } }))).not.toContain('mg-scanned')
    expect(ids(perf({ server: { metrics: { queryExecutor: { scannedObjects: 50_000_000 }, document: { returned: 10_000 } } } }))).toContain('mg-scanned')
  })
  it('suggests turning the profiler on, and off from everything', () => {
    expect(analyzeMongo(perf({ profile: { was: 0, slowms: 100 } }), NOW).findings[0]!.sql).toBe('{ profile: 1, slowms: 100 }')
    expect(ids(perf({ profile: { was: 2, slowms: 100 } }))).toContain('mg-profiler-all')
  })
  it('makes a heavy scan on a large collection high, and ignores rare or cheap ones', () => {
    const slow = { _id: { ns: 'shop.orders', op: 'query', plan: 'COLLSCAN', shape: 'A' }, count: 50, totalMs: 20_000, maxMs: 900, examined: 5e7, keys: 0, returned: 50, sorts: 0, example: { filter: { customer: 7 } } }
    const big = [collection({ count: 1_000_000 })]
    const f = analyzeMongo(perf({ collections: big, slow: [slow] }), NOW).findings.find((x) => x.id.startsWith('mg-slow-'))!
    expect(f.severity).toBe('high')
    expect(f.sql).toBe('db.orders.createIndex({ customer: 1 })')
    expect(f.fix).toContain('quiet hour')
    expect(ids(perf({ collections: big, slow: [{ ...slow, count: 2 }] })).some((x) => x.startsWith('mg-slow-'))).toBe(false)
    expect(ids(perf({ collections: big, slow: [{ ...slow, totalMs: 50 }] })).some((x) => x.startsWith('mg-slow-'))).toBe(false)
  })
  it('offers the same index once for two shapes that need it', () => {
    const a = { _id: { ns: 'shop.orders', op: 'query', plan: 'COLLSCAN', shape: 'A' }, count: 50, totalMs: 2000, maxMs: 90, examined: 5e6, keys: 0, returned: 50, sorts: 0, example: { filter: { customer: 7 } } }
    const fs = analyzeMongo(perf({ slow: [a, { ...a, _id: { ...a._id, shape: 'B' } }] }), NOW).findings.filter((x) => x.id.startsWith('mg-slow-'))
    expect(fs).toHaveLength(2)
    expect(fs.filter((x) => x.sql)).toHaveLength(1)
  })
  it('keeps unique, TTL and partial indexes, and waits 7 days before calling one unused', () => {
    const old = new Date(NOW - 10 * DAY).toISOString()
    const fresh = new Date(NOW - 2 * DAY).toISOString()
    const c = collection({ indexes: [
      { name: '_id_', key: { _id: 1 }, accesses: { ops: 0, since: old } },
      { name: 'a_1', key: { a: 1 }, accesses: { ops: 0, since: old } },
      { name: 'b_1', key: { b: 1 }, accesses: { ops: 0, since: fresh } },
      { name: 'email_1', key: { email: 1 }, unique: true, accesses: { ops: 0, since: old } },
      { name: 'exp_1', key: { exp: 1 }, ttl: true, accesses: { ops: 0, since: old } },
    ] })
    const r = analyzeMongo(perf({ collections: [c] }), NOW).findings
    expect(r.map((f) => f.id)).toEqual(['mg-unused-orders.a_1'])
    expect(r[0]!.sql).toBe('{ collMod: "orders", index: { name: "a_1", hidden: true } }')
  })
  it('does not call a prefix redundant when the longer index is partial', () => {
    const c = collection({ indexes: [{ name: 'a_1', key: { a: 1 } }, { name: 'a_1_b_1', key: { a: 1, b: 1 }, partial: true }] })
    expect(ids(perf({ collections: [c] }))).not.toContain('mg-redundant-orders.a_1')
  })
  it('flags much free space with compact, and long operations by age', () => {
    const c = collection({ storage: 4 * 1024 ** 3, free: 3 * 1024 ** 3 })
    expect(analyzeMongo(perf({ collections: [c] }), NOW).findings.find((f) => f.id === 'mg-free-orders')!.sql).toBe('{ compact: "orders" }')
    expect(analyzeMongo(perf({ running: [{ op: 'query', ns: 'shop.orders', secs_running: 30 }] }), NOW).findings).toEqual([])
    expect(analyzeMongo(perf({ running: [{ op: 'query', ns: 'shop.orders', secs_running: 900 }] }), NOW).findings[0]).toMatchObject({ id: 'mg-long-ops', severity: 'high' })
  })
  it('puts high findings first', () => {
    const r = analyzeMongo(perf({ profile: { was: 0 }, running: [{ secs_running: 900 }] }), NOW).findings
    expect(r.map((f) => f.severity)).toEqual(['high', 'low'])
  })
})
