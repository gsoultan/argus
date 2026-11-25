import { describe, expect, it } from 'vitest'
import {
  analyzePostgres, diagnoseStatement, explainFor, filterColumns, poolSummaries, settingBytes, statementsOn,
  type PgPerfFacts, type PgStatement, type PoolerStats,
} from './pgPerformance'

const NOW = new Date('2026-11-25T12:00:00Z')

function facts(over: Partial<PgPerfFacts> = {}): PgPerfFacts {
  return {
    database: 'shop',
    overview: {
      versionNum: 160004, maxConnections: 100, reservedConnections: 3, blksHit: 99_000_000, blksRead: 100_000,
      commits: 1_000_000, rollbacks: 1000, deadlocks: 0, tempFiles: 0, tempBytes: 0, statsReset: '2026-11-01 00:00:00+00',
      uptimeS: 30 * 86400, dbBytes: 20 * 1024 ** 3, statements: true, preload: 'pg_stat_statements', replica: false, xidAge: 1000,
    },
    activity: [{ state: 'active', waitType: '', count: 3, longestTxS: 1, longestActiveS: 1, here: true }],
    clients: [],
    settings: [
      { name: 'shared_buffers', setting: '524288', unit: '8kB', source: 'configuration file' },
      { name: 'work_mem', setting: '4096', unit: 'kB', source: 'default' },
      { name: 'track_io_timing', setting: 'on', unit: '', source: 'configuration file' },
      { name: 'log_min_duration_statement', setting: '1000', unit: 'ms', source: 'configuration file' },
      { name: 'random_page_cost', setting: '1.1', unit: '', source: 'configuration file' },
    ],
    statements: [],
    tables: [],
    indexes: [],
    foreignKeys: [],
    slots: [],
    ...over,
  }
}

const stmt = (over: Partial<PgStatement>): PgStatement => ({
  queryid: '1', query: 'SELECT 1', calls: 100, totalMs: 1000, meanMs: 10, stddevMs: 1, rows: 100,
  hit: 1000, read: 0, tempWritten: 0, role: 'app', share: 0.01, ...over,
})

const ids = (f: PgPerfFacts) => analyzePostgres(f, NOW).findings.map((x) => x.id)

describe('a healthy database', () => {
  it('says nothing', () => {
    expect(analyzePostgres(facts(), NOW).findings).toEqual([])
  })
})

describe('settings', () => {
  it('reads sizes in their units', () => {
    expect(settingBytes({ name: 'shared_buffers', setting: '16384', unit: '8kB', source: '' })).toBe(128 * 1024 ** 2)
    expect(settingBytes({ name: 'work_mem', setting: '4096', unit: 'kB', source: '' })).toBe(4 * 1024 ** 2)
  })
  it('flags the default shared_buffers on a big database, with the statement', () => {
    const f = facts({ settings: [{ name: 'shared_buffers', setting: '16384', unit: '8kB', source: 'default' }] })
    const r = analyzePostgres(f, NOW).findings.find((x) => x.id === 'shared-buffers-default')
    expect(r?.sql).toContain('ALTER SYSTEM SET shared_buffers')
  })
})

describe('connections', () => {
  it('warns near max_connections and recommends a pooler when there is none', () => {
    const f = facts({
      activity: [{ state: 'idle', waitType: 'Client', count: 90, longestTxS: 0, longestActiveS: 0, here: true }],
      pooler: { found: false, through: false, running: false },
    })
    const r = analyzePostgres(f, NOW)
    const near = r.findings.find((x) => x.id === 'conn-near-limit')
    expect(near?.severity).toBe('medium')
    expect(near?.fix).toContain('PgBouncer')
    expect(r.findings.map((x) => x.id)).toContain('pooler-none')
  })
  it('names a transaction left open, and offers the timeout', () => {
    const f = facts({ activity: [{ state: 'idle in transaction', waitType: 'Client', count: 2, longestTxS: 1800, longestActiveS: 0, here: true }] })
    const r = analyzePostgres(f, NOW).findings.find((x) => x.id === 'idle-in-tx')
    expect(r?.severity).toBe('high')
    expect(r?.sql).toBe("ALTER DATABASE shop SET idle_in_transaction_session_timeout = '5min';")
  })
})

describe('statements', () => {
  it('explains why one statement is heavy', () => {
    const d = diagnoseStatement(stmt({ share: 0.4, meanMs: 900, read: 50_000, hit: 50_000, tempWritten: 1000 }))
    expect(d.reasons.join(' ')).toMatch(/40% of all the time/)
    expect(d.reasons.join(' ')).toMatch(/read from disk/)
    expect(d.reasons.join(' ')).toMatch(/temporary files/)
    expect(d.fixes.length).toBeGreaterThan(1)
  })
  it('calls a cheap query run very often what it usually is', () => {
    const d = diagnoseStatement(stmt({ calls: 2_000_000, meanMs: 0.4, share: 0.3 }))
    expect(d.fixes.join(' ')).toMatch(/N\+1/)
  })
  it('does not call a one-off bulk load the heavy query', () => {
    const f = facts({ statements: [stmt({ queryid: '9', calls: 1, meanMs: 3000, totalMs: 3000, share: 0.9 })] })
    expect(ids(f)).not.toContain('stmt-9')
  })
  it('opens a plan Postgres 16 can make from $1 placeholders', () => {
    expect(explainFor(stmt({ query: 'select * from t where a = $1' }), 160000)).toBe('EXPLAIN (GENERIC_PLAN)\nselect * from t where a = $1')
    expect(explainFor(stmt({ query: 'select * from t where a = $1;' }), 150000)).toMatch(/^-- Put real values/)
  })
  it('says what to do when pg_stat_statements is missing', () => {
    const f = facts({ statements: null, overview: { ...facts().overview, statements: false, preload: 'auto_explain' } })
    const r = analyzePostgres(f, NOW).findings.find((x) => x.id === 'no-statements')
    expect(r?.sql).toContain("shared_preload_libraries = 'auto_explain,pg_stat_statements'")
  })
})

describe('tables and indexes', () => {
  const orders = {
    schema: 'public', name: 'orders', live: 400_000, dead: 0, seqScan: 188, seqRead: 25_000_000, idxScan: 1,
    lastVacuum: 'x', lastAnalyze: 'x', modSinceAnalyze: 0, totalBytes: 80 * 1024 ** 2, heapBytes: 60 * 1024 ** 2, heapHit: 0, heapRead: 0,
  }
  it('suggests the index from what the queries filter on', () => {
    const q = stmt({ query: 'select count(*) from orders where customer_id = $1', calls: 60 })
    const f = facts({ tables: [orders], statements: [q] })
    const r = analyzePostgres(f, NOW).findings.find((x) => x.id === 'seq-public.orders')
    expect(r?.sql).toContain('CREATE INDEX CONCURRENTLY ON public.orders (customer_id);')
  })
  it('ties a heavy query to the table it reads whole, and names the index', () => {
    const q = stmt({ queryid: '7', query: 'select count(*) from orders where customer_id = $1', calls: 1684, meanMs: 17, share: 0.24 })
    const r = analyzePostgres(facts({ tables: [orders], statements: [q] }), NOW).findings.find((x) => x.id === 'stmt-7')
    expect(r?.why).toContain('public.orders, which is scanned whole')
    expect(r?.fix).toMatch(/^An index on public\.orders \(customer_id\)/)
  })
  it('finds the statements that name a table, and the columns they compare', () => {
    const qs = [stmt({ query: 'SELECT * FROM public.orders o WHERE o.status = $1 AND o.created > $2 ORDER BY id', calls: 5 }),
      stmt({ query: 'select * from order_lines where x = $1' })]
    expect(statementsOn(qs, 'orders')).toHaveLength(1)
    expect(filterColumns(statementsOn(qs, 'orders'))).toEqual(['status', 'created'])
  })
  it('vacuums a table full of dead rows', () => {
    const f = facts({ tables: [{ ...orders, seqScan: 0, dead: 500_000 }] })
    const r = analyzePostgres(f, NOW).findings.find((x) => x.id === 'dead-public.orders')
    expect(r?.severity).toBe('high')
    expect(r?.sql).toContain('VACUUM (ANALYZE, VERBOSE) public.orders;')
  })
  it('reports a duplicate once, not also as unused', () => {
    const idx = (name: string) => ({ schema: 'public', table: 'orders', name, scans: 0, bytes: 4 * 1024 ** 2, unique: false, primary: false,
      def: `CREATE INDEX ${name} ON public.orders USING btree (status)`, constraint: false })
    const f = facts({ indexes: [idx('a_idx'), idx('b_idx')] })
    const got = ids(f)
    expect(got.filter((x) => x.includes('b_idx'))).toEqual(['dup-public.b_idx'])
    expect(got).toContain('unused-public.a_idx')
  })
  it('never offers to drop an index behind a constraint', () => {
    const f = facts({ indexes: [{ schema: 'public', table: 'o', name: 'o_key', scans: 0, bytes: 1024 ** 3, unique: true, primary: false, def: '', constraint: true }] })
    expect(ids(f)).toEqual([])
  })
  it('warns of an abandoned slot holding WAL', () => {
    const f = facts({ slots: [{ name: 'old_cdc', active: false, retainedBytes: 30 * 1024 ** 3 }] })
    expect(analyzePostgres(f, NOW).findings[0]).toMatchObject({ id: 'slot-old_cdc', severity: 'high' })
  })
})

describe('PgBouncer', () => {
  const pools = (rows: string[][]) => ({
    cols: ['database', 'user', 'cl_active', 'cl_waiting', 'sv_active', 'sv_idle', 'sv_used', 'maxwait', 'maxwait_us', 'pool_mode'], rows,
  })
  const bouncer = (over: Partial<PoolerStats> = {}): PoolerStats => ({
    found: true, through: true, port: 6432, running: true, version: 'PgBouncer 1.23.1', settingsFrom: 'console',
    settings: { pool_mode: 'transaction', default_pool_size: '20', max_client_conn: '1000', idle_transaction_timeout: '60', max_prepared_statements: '200' },
    pools: pools([['shop', 'app', '40', '0', '5', '15', '0', '0', '0', 'transaction']]),
    ...over,
  })
  it('is quiet about a healthy PgBouncer', () => {
    expect(ids(facts({ pooler: bouncer() }))).toEqual([])
  })
  it('explains clients queueing for a full pool', () => {
    const f = facts({ pooler: bouncer({ pools: pools([['shop', 'app', '20', '12', '20', '0', '0', '7', '0', 'transaction']]) }) })
    const r = analyzePostgres(f, NOW).findings.find((x) => x.id === 'pooler-wait-shop-app')
    expect(r?.severity).toBe('high')
    expect(r?.evidence[0]).toContain('12 waiting, the longest for 7.0 s; 20 of 20')
    expect(r?.fix).toMatch(/raise pool_size for shop/)
  })
  it('recommends transaction pooling over session pooling', () => {
    const f = facts({ pooler: bouncer({ settings: { pool_mode: 'session', max_client_conn: '500' } }) })
    expect(analyzePostgres(f, NOW).findings.find((x) => x.id === 'pooler-session-mode')?.fix).toMatch(/pool_mode = transaction/)
  })
  it('adds the pools up against max_connections', () => {
    const rows = ['a', 'b', 'c', 'd', 'e', 'f'].map((u) => ['shop', u, '1', '0', '1', '0', '0', '0', '0', 'transaction'])
    const f = facts({ pooler: bouncer({ pools: pools(rows) }) })
    const r = analyzePostgres(f, NOW).findings.find((x) => x.id === 'pooler-over-limit')
    expect(r?.evidence[0]).toBe('6 pools could open up to 120 server connections; Postgres allows 97 (besides superusers).')
  })
  it('uses a database’s own pool_size from SHOW DATABASES', () => {
    const p = bouncer({ databases: { cols: ['name', 'pool_size'], rows: [['shop', '50']] } })
    expect(poolSummaries(p)[0]?.size).toBe(50)
  })
  it('asks for idle_transaction_timeout and prepared statements in transaction mode', () => {
    const f = facts({ pooler: bouncer({ settings: { pool_mode: 'transaction', default_pool_size: '20', max_client_conn: '1000' } }) })
    expect(ids(f)).toEqual(expect.arrayContaining(['pooler-idle-tx', 'pooler-prepared']))
  })
  it('says when the console could not be read, and still checks the file', () => {
    const f = facts({ pooler: bouncer({ pools: undefined, settingsFrom: 'file', liveError: 'FATAL: not allowed', settings: { pool_mode: 'session', max_client_conn: '300' } }) })
    expect(ids(f)).toEqual(expect.arrayContaining(['pooler-console', 'pooler-session-mode']))
  })
  it('says below DBA that PgBouncer was not read', () => {
    expect(ids(facts({ pooler: null, viaPooler: true }))).toEqual(['pooler-unread'])
  })
})
