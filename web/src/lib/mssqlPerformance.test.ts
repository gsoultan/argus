import { describe, expect, it } from 'vitest'
import captured from './mssqlPerformance.fixture.json'
import { analyzeMSSQL, msIdent, statementTables, type MSFacts, type MSHealth } from '~/lib/mssqlPerformance'

// captured is what the agent's ms.performance, ms.indexes, ms.settings and
// ms.health answered from a real SQL Server 2022 (Developer): a 300k-row
// orders table read 400 times by customer_id without an index, a duplicate
// index, a fragmented clustered index, auto shrink on, a log grown 1 MB at a
// time, and ad hoc statements filling the plan cache.
const real = captured as unknown as MSFacts

const DAY = 86400
function facts(over: { health?: Partial<MSHealth>; settings?: Record<string, string>; server?: Record<string, string> } & Partial<Omit<MSFacts, 'health' | 'settings'>> = {}): MSFacts {
  const settings = { RECOVERY: 'SIMPLE', AUTO_SHRINK: 'OFF', AUTO_CLOSE: 'OFF', AUTO_UPDATE_STATISTICS: 'ON', QUERY_STORE: 'ON', READ_COMMITTED_SNAPSHOT: 'OFF', ...over.settings }
  const server = { 'max server memory (MB)': '28000', 'cost threshold for parallelism': '50', 'max degree of parallelism': '8', 'optimize for ad hoc workloads': '1', ...over.server }
  return {
    performance: over.performance ?? { since: '2026-11-01T00:00:00Z', waits: [], queries: [] },
    indexes: over.indexes ?? { indexes: [], statistics: [], missing: [] },
    settings: {
      settings: Object.entries(settings).map(([name, value]) => ({ name, value })),
      facts: [{ name: 'Log waits on', value: 'NOTHING' }],
      files: [{ name: 'd', type: 'rows', path: '/d.mdf', sizeMb: 10240, usedMb: 9000, maxMb: -1, growth: '512 MB' },
        { name: 'l', type: 'log', path: '/l.ldf', sizeMb: 2048, usedMb: 100, maxMb: -1, growth: '1024 MB' }],
      server: Object.entries(server).map(([name, value]) => ({ name, value })),
    },
    health: {
      version: '16.0.4325.4', edition: 'Standard Edition (64-bit)', since: '2026-11-01T00:00:00Z', uptimeS: 30 * DAY, cpus: 8,
      memoryMb: 32768, targetMb: 28000, committedMb: 28000, sqlMemoryMb: 28000, pageLife: 50000, grantsPending: 0,
      batches: 1_000_000, compilations: 20_000, recompiles: 100, deadlocks: 0, pageSplits: 0, fullScans: 0,
      tempdbFiles: 8, tempdbSizes: [1024], vlfs: 40, planCacheMb: 2000, singleUseMb: 100, blocked: 0, oldestTranS: 0,
      queryStore: 'READ_WRITE', ...over.health,
    },
  }
}
const ids = (f: MSFacts) => analyzeMSSQL(f).findings.map((x) => x.id)

describe('the captured SQL Server', () => {
  const r = analyzeMSSQL(real)
  const byId = new Map(r.findings.map((f) => [f.id, f]))
  it('names the query read without an index first, with SQL Server’s own index', () => {
    const top = r.findings[0]!
    expect(top.severity).toBe('high')
    expect(top.title).toContain('customer_id = @c')
    expect(top.why).toMatch(/reads 16k pages/)
    expect(top.sql).toBe('CREATE INDEX [IX_orders_customer_id] ON [dbo].[orders] ([customer_id]) WITH (ONLINE = ON);')
    // Tied to the query, so not repeated as a missing index of its own.
    expect([...byId.keys()].some((k) => k.startsWith('ms-missing-'))).toBe(false)
  })
  it('finds the settings and files that hurt', () => {
    for (const id of ['ms-max-memory', 'ms-autoshrink', 'ms-cost-threshold', 'ms-adhoc', 'ms-qs-off', 'ms-vlfs', 'ms-pct-growth']) {
      expect(byId.has(id), id).toBe(true)
    }
    expect(byId.get('ms-autoshrink')!.sql).toBe('ALTER DATABASE CURRENT SET AUTO_SHRINK OFF;')
  })
  it('finds the duplicate index but never offers to drop a primary key', () => {
    expect(byId.get('ms-dup-dbo.orders.st2')!.sql).toBe('DROP INDEX [st2] ON [dbo].[orders];')
    expect(r.findings.some((f) => /DROP INDEX \[PK__/.test(f.sql ?? ''))).toBe(false)
  })
  it('does not call an index unused, or a wait heavy, after minutes of counting', () => {
    expect([...byId.keys()].some((k) => k.startsWith('ms-unused-') || k.startsWith('ms-wait-'))).toBe(false)
  })
  it('sorts high before medium before low', () => {
    const order = { high: 0, medium: 1, low: 2 }
    const s = r.findings.map((f) => order[f.severity])
    expect(s).toEqual([...s].sort((a, b) => a - b))
  })
})

describe('server rules', () => {
  it('is quiet on a healthy server', () => {
    expect(ids(facts())).toEqual([])
  })
  it('explains the top waits, only when they are large', () => {
    const w = (wait: string, percent: number, waitMs: number) => ({ wait, tasks: 100, waitMs, signalMs: 0, percent })
    const big = facts({ performance: { since: '', waits: [w('PAGEIOLATCH_SH', 55, 90_000_000), w('WRITELOG', 20, 30_000_000)], queries: [] } })
    expect(analyzeMSSQL(big).findings.slice(0, 2).map((f) => [f.id, f.severity])).toEqual([['ms-wait-PAGEIOLATCH_SH', 'high'], ['ms-wait-WRITELOG', 'medium']])
    const small = facts({ performance: { since: '', waits: [w('PAGEIOLATCH_SH', 55, 5_000)], queries: [] } })
    expect(ids(small)).toEqual([])
  })
  it('says when the CPUs are the bottleneck', () => {
    const p = facts({ performance: { since: '', waits: [{ wait: 'SOS_SCHEDULER_YIELD', tasks: 1, waitMs: 1_000_000, signalMs: 500_000, percent: 60 }], queries: [] } })
    expect(ids(p)).toContain('ms-signal')
  })
  it('flags low page life only after the server warmed up', () => {
    expect(ids(facts({ health: { pageLife: 100 } }))).toContain('ms-ple')
    expect(ids(facts({ health: { pageLife: 100, uptimeS: 600 } }))).not.toContain('ms-ple')
  })
  it('flags blocking, long transactions and pending grants now', () => {
    const p = ids(facts({ health: { blocked: 6, oldestTranS: 7200, grantsPending: 2 } }))
    expect(p.slice(0, 3).sort()).toEqual(['ms-blocked', 'ms-grants', 'ms-old-tran'])
  })
  it('wants one tempdb file per core up to 8, all the same size', () => {
    expect(ids(facts({ health: { tempdbFiles: 2, tempdbSizes: [8, 1024] } }))).toEqual(expect.arrayContaining(['ms-tempdb-files', 'ms-tempdb-sizes']))
  })
  it('says when the log grows because nobody backs it up', () => {
    const f = facts({ settings: { RECOVERY: 'FULL' } })
    f.settings.facts = [{ name: 'Log waits on', value: 'LOG_BACKUP' }]
    f.settings.files[1]!.sizeMb = 20480
    expect(analyzeMSSQL(f).findings[0]).toMatchObject({ id: 'ms-log-backup', severity: 'high' })
  })
  it('only calls an index unused after a week, and never a unique one', () => {
    const ix = (name: string, unique = false) => ({ schema: 'dbo', table: 't', name, type: 'nonclustered', unique, pages: 100, fragmentation: 0, seeks: 0, scans: 0, lookups: 0, updates: 500, keys: [name] })
    const f = facts({ indexes: { indexes: [ix('a'), ix('b', true)], statistics: [], missing: [] } })
    expect(ids(f)).toEqual(['ms-unused-dbo.t.a'])
    f.health.uptimeS = DAY
    expect(ids(f)).toEqual([])
  })
})

describe('helpers', () => {
  it('reads table names out of a statement', () => {
    expect(statementTables('SELECT * FROM [dbo].[Orders] o JOIN dbo.customers c ON c.id = o.cid WHERE x = @p')).toEqual(['orders', 'customers'])
  })
  it('brackets names safely', () => {
    expect(msIdent('we]ird')).toBe('[we]]ird]')
  })
})
