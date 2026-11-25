import { describe, expect, it } from 'vitest'
import captured from './mysqlPerformance.fixture.json'
import {
  analyzeMySQL, diagnoseDigest, digestColumns, digests, digestTables, explainDigest, indexes, myIdent, tables,
  type MySQLPerf,
} from '~/lib/mysqlPerformance'

// captured is what the agent's mysql.perf answered from a real MariaDB 10.11
// (internal/agent/mysql_perf_test.go builds the same database): a 300k-row
// orders table with a GROUP BY that scans it, an index-less WHERE on
// customer_id, audit without a primary key and a duplicated index.
const real = captured as MySQLPerf

const DAY = 86400
function perf(over: Partial<MySQLPerf> = {}, status: Record<string, string> = {}, variables: Record<string, string> = {}): MySQLPerf {
  return {
    version: '8.0.36',
    status: { uptime: String(30 * DAY), threads_connected: '5', threads_running: '1', connections: '100', threads_created: '5', ...status },
    variables: {
      max_connections: '151', performance_schema: 'ON', slow_query_log: 'ON', innodb_buffer_pool_size: String(4 * 1024 ** 3),
      tmp_table_size: '16777216', max_heap_table_size: '16777216', table_open_cache: '4000', ...variables,
    },
    ...over,
  }
}
const ids = (p: MySQLPerf) => analyzeMySQL(p).findings.map((f) => f.id)
const DIGEST_COLS = ['DIGEST_TEXT', 'COUNT_STAR', 'SUM_TIMER_WAIT', 'AVG_TIMER_WAIT', 'SUM_LOCK_TIME', 'SUM_ROWS_EXAMINED', 'SUM_ROWS_SENT',
  'SUM_ROWS_AFFECTED', 'SUM_NO_INDEX_USED', 'SUM_NO_GOOD_INDEX_USED', 'SUM_CREATED_TMP_DISK_TABLES', 'SUM_CREATED_TMP_TABLES',
  'SUM_SORT_MERGE_PASSES', 'SUM_SELECT_FULL_JOIN', 'share']
const ps = (msv: number) => String(Math.round(msv * 1e9))

describe('reading what the agent sends', () => {
  it('turns picosecond timers into milliseconds and keeps the share', () => {
    const d = digests(real)[0]!
    expect(d.calls).toBe(40)
    expect(d.totalMs).toBeCloseTo(43356.016, 2)
    expect(d.avgMs).toBeCloseTo(1083.9, 1)
    expect(d.share).toBeCloseTo(0.9235)
  })
  it('groups index columns in order and knows unique from not', () => {
    const st = indexes(real).find((i) => i.name === 'st_total')!
    expect(st.columns).toEqual(['status', 'total'])
    expect(st.unique).toBe(false)
    expect(indexes(real).find((i) => i.name === 'PRIMARY')!.unique).toBe(true)
  })
  it('knows which tables have a primary key', () => {
    expect(tables(real).map((t) => [t.name, t.primary])).toEqual([['orders', true], ['customers', true], ['audit', false]])
  })
})

describe('the captured MariaDB', () => {
  const r = analyzeMySQL(real)
  const byId = new Map(r.findings.map((f) => [f.id, f]))
  it('names the heavy GROUP BY first and says why', () => {
    const top = r.findings[0]!
    expect(top.severity).toBe('high')
    expect(top.title).toContain('GROUP BY STATUS')
    expect(top.why).toContain('92%')
    expect(top.why).toMatch(/full table scan/)
    expect(top.why).toMatch(/temporary tables on disk/)
    expect(top.sql).toMatch(/^EXPLAIN SELECT/)
  })
  it('proposes an online index for the table read without one', () => {
    const f = byId.get('my-scan-orders')!
    expect(f.sql).toBe('ALTER TABLE `orders` ADD INDEX (`customer_id`), ALGORITHM=INPLACE, LOCK=NONE;')
  })
  it('finds the table without a primary key and the duplicate index, and not the PRIMARY ones', () => {
    expect(byId.has('my-nopk-audit')).toBe(true)
    expect(byId.get('my-dup-orders.st2')!.title).toBe('orders.st2 duplicates st')
    expect(byId.has('my-redundant-orders.st')).toBe(true)
    expect([...byId.keys()].some((k) => k.includes('PRIMARY'))).toBe(false)
  })
  it('does not call an index unused after four minutes of counting', () => {
    expect([...byId.keys()].some((k) => k.startsWith('my-unused-'))).toBe(false)
  })
  it('knows it is MariaDB and reports the summary numbers', () => {
    expect(r.mariadb).toBe(true)
    expect(r.connections).toMatchObject({ used: 1, max: 151 })
    expect(r.bufferHit).toBeGreaterThan(0.99)
  })
  it('sorts high before medium before low', () => {
    const order = { high: 0, medium: 1, low: 2 }
    const s = r.findings.map((f) => order[f.severity])
    expect(s).toEqual([...s].sort((a, b) => a - b))
  })
})

describe('server-wide rules', () => {
  it('is quiet on a healthy server', () => {
    expect(ids(perf())).toEqual([])
  })
  it('warns near the connection limit, high when almost full', () => {
    expect(analyzeMySQL(perf({}, { threads_connected: '125' })).findings[0]).toMatchObject({ id: 'my-conn-near-limit', severity: 'medium' })
    expect(analyzeMySQL(perf({}, { threads_connected: '148' })).findings[0]).toMatchObject({ id: 'my-conn-near-limit', severity: 'high' })
  })
  it('flags a default buffer pool only when the data is bigger than it', () => {
    const small = perf({}, {}, { innodb_buffer_pool_size: '134217728' })
    expect(ids(small)).not.toContain('my-buffer-default')
    const big = perf({ tables: { cols: [], rows: [['t', 'InnoDB', '9000000', String(2 * 1024 ** 3), '0', '0', '1']] } }, {}, { innodb_buffer_pool_size: '134217728' })
    expect(ids(big)).toContain('my-buffer-default')
  })
  it('says when performance_schema is off and the slow log is off', () => {
    const p = ids(perf({}, {}, { performance_schema: 'OFF', slow_query_log: 'OFF' }))
    expect(p).toContain('my-no-ps')
    expect(p).toContain('my-slow-log')
  })
  it('flags a long open transaction', () => {
    const p = perf({ trx: { cols: [], rows: [['1', 'RUNNING', '900', '10', '0', 'UPDATE t SET a = 1']] } })
    expect(analyzeMySQL(p).findings[0]).toMatchObject({ id: 'my-long-trx', severity: 'high' })
  })
})

describe('statements', () => {
  const digest = (text: string, calls: number, avgMs: number, over: Partial<Record<string, string>> = {}) => {
    const row = [text, String(calls), ps(calls * avgMs), ps(avgMs), '0', '0', String(calls), '0', '0', '0', '0', '0', '0', '0', '0.5']
    for (const [k, v] of Object.entries(over)) row[DIGEST_COLS.indexOf(k)] = v!
    return row
  }
  it('never calls a one-off statement heavy', () => {
    const p = perf({ digests: { cols: DIGEST_COLS, rows: [digest('ALTER TABLE `t` ADD COLUMN `x` INT', 1, 60_000, { share: '0.9' })] } })
    expect(ids(p).some((i) => i.startsWith('my-stmt-'))).toBe(false)
  })
  it('spots a cheap query run very often (N+1)', () => {
    const d = digests(perf({ digests: { cols: DIGEST_COLS, rows: [digest('SELECT * FROM `items` WHERE `id` = ?', 2_000_000, 0.2, { share: '0.4' })] } }))[0]!
    expect(diagnoseDigest(d).fixes.join(' ')).toMatch(/N\+1/)
  })
  it('puts real values back before EXPLAIN', () => {
    const d = digests(real)[2]!
    expect(explainDigest(d)).toBe('-- Put real values in place of each ? before running.\nEXPLAIN SELECT COUNT ( * ) FROM `orders` WHERE `customer_id` = ?')
  })
  it('reads tables and filter columns out of a digest', () => {
    expect(digestTables('SELECT * FROM `shop` . `orders` o JOIN `customers` c ON c.id = o.customer_id')).toEqual(['orders', 'customers'])
    expect(digestColumns('SELECT * FROM `orders` WHERE `customer_id` = ? AND `status` IN (...) ORDER BY `id`')).toEqual(['customer_id', 'status'])
  })
  it('quotes identifiers safely', () => {
    expect(myIdent('we`ird')).toBe('`we``ird`')
  })
})
