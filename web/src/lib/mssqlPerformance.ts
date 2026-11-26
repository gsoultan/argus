/*
Why a SQL Server database is heavy, and what to change: the analysis behind
the Performance panel for the mssql engine. Pure, so it is tested without a
server.

The facts are four read-only DBA operations the agent runs as the admin login
(every SQL Server panel is DBA only): ms.performance (the server's waits, this
database's costly cached statements), ms.indexes (size, fragmentation, use,
statistics, the indexes SQL Server says are missing), ms.settings (database
options, files, server options) and ms.health (memory, counters, tempdb, the
log's VLFs, the plan cache, Query Store). See internal/agent/mssql_dba.go and
mssql_health.go.

The shape of a finding, the formatting and staying quiet on small numbers
are shared with the Postgres and MySQL analyses.
*/

import { bytes, count, duration, ms, oneLine, type Finding, type Severity } from '~/lib/pgPerformance'

export interface MSWait { wait: string; tasks: number; waitMs: number; signalMs: number; percent: number }
export interface MSQuery { statement: string; executions: number; cpuMs: number; avgCpuMs: number; avgMs: number; avgReads: number; lastRun?: string }
export interface MSIndex {
  schema: string; table: string; name?: string; type: string; primary?: boolean; unique?: boolean; disabled?: boolean
  pages: number; fragmentation: number; seeks: number; scans: number; lookups: number; updates: number; lastUsed?: string; keys?: string[]
}
export interface MSStatistic { schema: string; table: string; name: string; auto?: boolean; lastUpdated?: string; rows: number; sampled: number; modified: number }
export interface MSMissing { schema: string; table: string; equality?: string; range?: string; include?: string; seeks: number; impact: number; create: string }
export interface MSFile { name: string; type: string; path: string; sizeMb: number; usedMb: number; maxMb: number; growth: string; readonly?: boolean }
export interface MSNameValue { name: string; value: string }

export interface MSHealth {
  version: string; edition: string; since: string; uptimeS: number; cpus: number; memoryMb: number
  targetMb: number; committedMb: number; sqlMemoryMb: number
  pageLife: number; grantsPending: number; batches: number; compilations: number; recompiles: number
  deadlocks: number; pageSplits: number; fullScans: number
  tempdbFiles: number; tempdbSizes: number[]; vlfs: number; planCacheMb: number; singleUseMb: number
  blocked: number; oldestTranS: number
  queryStore?: string; queryStoreReason?: number; queryStoreUsedMb?: number; queryStoreMaxMb?: number
  missing?: string[]
}

export interface MSFacts {
  performance: { since: string; waits: MSWait[]; queries: MSQuery[] }
  indexes: { indexes: MSIndex[]; statistics: MSStatistic[]; missing: MSMissing[]; truncated?: boolean }
  settings: { settings: MSNameValue[]; facts: MSNameValue[]; files: MSFile[]; server: { name: string; value: string }[] }
  health: MSHealth
}

export interface MSReport {
  findings: Finding[]
  countingS: number
  /** The share of all waiting that was for a CPU, 0 to 1. */
  signalShare: number
  /** The share of the plan cache held by ad hoc plans run once. */
  singleUseShare: number
}

const ORDER: Record<Severity, number> = { high: 0, medium: 1, low: 2 }
const pct = (x: number) => `${(x * 100).toFixed(x < 0.1 ? 1 : 0)}%`
const mb = (v: number) => bytes(v * 1024 * 1024)
/** SQL Server's estimate of an index's gain, never shown as a full 100%. */
const impact = (m: MSMissing) => String(Math.min(99, Math.round(m.impact)))
/** A bracketed SQL Server name. */
export const msIdent = (s: string) => `[${s.replace(/]/g, ']]')}]`
const table = (schema: string, name: string) => `${msIdent(schema)}.${msIdent(name)}`

/** The table names a statement reads, without schema or brackets. */
export function statementTables(text: string): string[] {
  const out = new Set<string>()
  for (const m of text.matchAll(/\b(?:FROM|JOIN|UPDATE|INTO)\s+(?:(?:\[[^\]]+\]|\w+)\s*\.\s*)?(?:\[([^\]]+)\]|(\w+))/gi)) {
    const n = m[1] ?? m[2] ?? ''
    if (n && !/^(select|sys|@)/i.test(n)) out.add(n.toLowerCase())
  }
  return [...out]
}

/** What one wait type means for this server, and what to do about it. */
interface WaitAdvice { title: string; why: string; fix: string; area: Finding['area'] }
const WAITS: { match: RegExp; advice: WaitAdvice }[] = [
  {
    match: /^PAGEIOLATCH_/, advice: {
      area: 'memory', title: 'Queries wait for data pages to be read from disk',
      why: 'The data a query needs is not in memory, so it waits for the disk. Either memory is short for the data in use, or queries read far more pages than they need (scans where an index would seek).',
      fix: 'Index the costly queries below so they read less; then check max server memory and page life expectancy. Faster storage helps last.',
    },
  },
  {
    match: /^WRITELOG$/, advice: {
      area: 'setup', title: 'Commits wait for the transaction log to be written',
      why: 'Every commit waits until its log records are on disk. Slow log storage, or many tiny transactions each committing on their own, make this the bottleneck.',
      fix: 'Put the log on fast storage of its own, and batch small writes into fewer transactions. Delayed durability trades a little safety for speed where that is acceptable.',
    },
  },
  {
    match: /^LCK_M_/, advice: {
      area: 'queries', title: 'Queries are blocked by each other’s locks',
      why: 'A query waits for a lock another session holds: usually a long transaction, or a write that scans a table without an index and so locks far more rows than it changes.',
      fix: 'Keep transactions short, index the columns writes filter on, and consider READ_COMMITTED_SNAPSHOT so readers stop waiting for writers (test first: it uses tempdb).',
    },
  },
  {
    match: /^(CXPACKET|CXCONSUMER|CXSYNC_PORT|CXSYNC_CONSUMER)$/, advice: {
      area: 'setup', title: 'Parallel queries wait on each other',
      why: 'A query split across CPUs waits for its slowest thread. Lots of it usually means small queries go parallel because cost threshold for parallelism is still 5, or a large scan is being parallelized that an index would avoid.',
      fix: 'Raise cost threshold for parallelism (50 is a common start) and set max degree of parallelism to the cores of one NUMA node, at most 8. Index the scans behind the parallel plans.',
    },
  },
  {
    match: /^SOS_SCHEDULER_YIELD$/, advice: {
      area: 'queries', title: 'Queries wait for a CPU',
      why: 'Queries run long enough to give up the CPU and queue for it again: the CPUs are busy, usually with scans, sorts or hash joins of the costly statements below.',
      fix: 'Tune the top statements by CPU below first; more cores come last.',
    },
  },
  {
    match: /^RESOURCE_SEMAPHORE$/, advice: {
      area: 'memory', title: 'Queries wait for memory before they can start',
      why: 'Each sort or hash needs a memory grant; when grants are used up, new queries queue. Large grants come from big sorts and hashes, often over estimated row counts.',
      fix: 'Index to avoid the big sorts and hashes, update statistics so estimates are right, and check max server memory.',
    },
  },
  {
    match: /^ASYNC_NETWORK_IO$/, advice: {
      area: 'connections', title: 'SQL Server waits for clients to take their results',
      why: 'The server has rows ready and the client reads them slowly: it fetches far more rows than it shows, or processes each row before reading the next.',
      fix: 'Have the application ask only for the rows it uses (paging, a WHERE), and read results fully before working on them.',
    },
  },
  {
    match: /^PAGELATCH_(UP|EX|SH)$/, advice: {
      area: 'setup', title: 'Contention on pages in memory, often in tempdb',
      why: 'Many sessions change the same pages at once: allocation pages in tempdb when temp tables are made and dropped quickly, or the last page of a table with an ever-increasing key.',
      fix: 'Give tempdb one data file per core up to 8, all the same size. For a hot last page, OPTIMIZE_FOR_SEQUENTIAL_KEY (2019+) on the index.',
    },
  },
  {
    match: /^THREADPOOL$/, advice: {
      area: 'connections', title: 'SQL Server ran out of worker threads',
      why: 'Every request needs a worker; when all are busy, new connections and queries wait and can time out. Usually a blocking chain holds many workers at once, or very parallel queries take many each.',
      fix: 'Find and end the blocking (Activity), lower max degree of parallelism, and do not raise max worker threads to hide it.',
    },
  },
  {
    match: /^HADR_SYNC_COMMIT$/, advice: {
      area: 'setup', title: 'Commits wait for an availability group replica',
      why: 'In synchronous commit, a transaction waits until a secondary has hardened its log. A slow network or a slow secondary log disk slows every write here.',
      fix: 'Check the replica’s log disk and the network to it, or move a far replica to asynchronous commit.',
    },
  },
]

function waitFinding(w: MSWait, uptimeS: number, signalShare: number): Finding | null {
  const a = WAITS.find((x) => x.match.test(w.wait))?.advice
  if (!a) return null
  // Quiet on small numbers: a wait that is a sliver of the time the server
  // has been up is not what makes it slow.
  if (w.percent < 10 || w.waitMs < 60_000 || w.waitMs < uptimeS * 1000 * 0.005) return null
  const sev: Severity = w.percent >= 40 ? 'high' : 'medium'
  return {
    id: `ms-wait-${w.wait}`, severity: sev, area: a.area, title: a.title, why: a.why,
    evidence: [`${w.wait}: ${w.percent.toFixed(0)}% of all waiting, ${ms(w.waitMs)} in ${count(w.tasks)} waits.` +
      (w.wait === 'SOS_SCHEDULER_YIELD' ? ` ${pct(signalShare)} of all waiting is for a CPU.` : '')],
    fix: a.fix,
  }
}

export function analyzeMSSQL(f: MSFacts): MSReport {
  const out: Finding[] = []
  const add = (x: Finding) => out.push(x)
  const h = f.health
  const up = h.uptimeS
  const days = up / 86400
  const counting = `SQL Server has been counting for ${duration(up)}, since it started.`
  const setting = (n: string) => f.settings.settings.find((s) => s.name === n)?.value ?? ''
  const fact = (n: string) => f.settings.facts.find((s) => s.name === n)?.value ?? ''
  const server = (n: string) => Number(f.settings.server.find((s) => s.name === n)?.value ?? NaN)
  const waits = f.performance.waits
  const waitTotal = waits.reduce((a, w) => a + w.waitMs, 0)
  const signalShare = waitTotal > 0 ? waits.reduce((a, w) => a + w.signalMs, 0) / waitTotal : 0
  const singleUseShare = h.planCacheMb > 0 ? h.singleUseMb / h.planCacheMb : 0
  const enterprise = /enterprise|developer/i.test(h.edition)

  /* What the server waits on */
  for (const w of waits.slice(0, 5)) {
    const x = waitFinding(w, up, signalShare)
    if (x && !out.some((o) => o.title === x.title)) add(x)
  }
  if (signalShare >= 0.25 && waitTotal >= 60_000) {
    add({
      id: 'ms-signal', severity: signalShare >= 0.4 ? 'high' : 'medium', area: 'queries', title: 'The CPUs are the bottleneck',
      why: 'Signal wait is the time a query was ready to run and waited for a CPU. When it is a large share of all waiting, there is more work than CPU.',
      evidence: [`${pct(signalShare)} of all waiting was for a CPU; ${h.cpus} CPUs.`],
      fix: 'Tune the statements that cost the most CPU (below), and stop small queries going parallel (cost threshold for parallelism).',
    })
  }

  /* Memory */
  const maxMem = server('max server memory (MB)')
  if (Number.isFinite(maxMem) && (maxMem >= 2_147_483_647 || maxMem > h.memoryMb) && h.memoryMb > 0) {
    const leave = Math.max(4096, Math.round(h.memoryMb * 0.1))
    const target = Math.max(1024, h.memoryMb - leave)
    add({
      id: 'ms-max-memory', severity: 'medium', area: 'memory', title: 'max server memory is not set',
      why: 'Left at its default, SQL Server grows its cache until the machine has almost nothing left, and Windows or Linux then pages or trims it under pressure: everything slows at once.',
      evidence: [`max server memory = ${maxMem >= 2_147_483_647 ? 'the default (unlimited)' : `${count(maxMem)} MB`}; the machine has ${mb(h.memoryMb)}.`],
      fix: `Leave the operating system and anything else on the machine its share: about ${mb(leave)} here.`,
      sql: `EXEC sys.sp_configure N'show advanced options', 1; RECONFIGURE;\nEXEC sys.sp_configure N'max server memory (MB)', ${target}; RECONFIGURE;`,
    })
  }
  // Page life expectancy: the old 300 s rule, scaled by the buffer pool's
  // size (300 s per 4 GB), and only once the server has warmed up.
  const pleFloor = Math.max(300, (h.committedMb / 4096) * 300)
  if (up >= 3600 && h.pageLife > 0 && h.pageLife < pleFloor) {
    add({
      id: 'ms-ple', severity: h.pageLife < pleFloor / 3 ? 'high' : 'medium', area: 'memory', title: 'Data does not stay in memory for long',
      why: 'Page life expectancy is how long a page read from disk stays in the buffer pool. A low value means pages are pushed out soon after they are read, so the same data is read from disk again and again.',
      evidence: [`Page life expectancy ${count(h.pageLife)} s; about ${count(Math.round(pleFloor))} s would be healthy for a ${mb(h.committedMb)} buffer pool.`],
      fix: 'First make the big scans read less (the missing indexes and costly statements here); add memory if it stays low.',
    })
  }
  if (h.grantsPending > 0) {
    add({
      id: 'ms-grants', severity: 'high', area: 'memory', title: 'Queries are waiting for memory right now',
      why: 'Memory Grants Pending counts queries that cannot start until another one gives its sort or hash memory back.',
      evidence: [`${h.grantsPending} memory grants pending now.`],
      fix: 'Find the queries with the largest grants (Activity) and index them so they sort and hash less; check max server memory.',
    })
  }

  /* Parallelism and plans */
  const cost = server('cost threshold for parallelism')
  const maxdop = server('max degree of parallelism')
  if (cost === 5 && h.cpus > 1) {
    const cx = waits.find((w) => /^CX/.test(w.wait))
    add({
      id: 'ms-cost-threshold', severity: cx && cx.percent >= 20 && cx.waitMs >= 60_000 ? 'medium' : 'low', area: 'setup',
      title: 'Small queries can still go parallel',
      why: 'Cost threshold for parallelism is still 5, the default from the 1990s. Today even cheap queries pass it, and a parallel plan for a small query costs more in coordination than it saves.',
      evidence: [`cost threshold for parallelism = 5${cx ? `; ${cx.wait} is ${cx.percent.toFixed(0)}% of waiting` : ''}.`],
      fix: 'Raise it to 50 and watch the CXPACKET waits and the costly statements.',
      sql: `EXEC sys.sp_configure N'show advanced options', 1; RECONFIGURE;\nEXEC sys.sp_configure N'cost threshold for parallelism', 50; RECONFIGURE;`,
    })
  }
  if (maxdop === 0 && h.cpus > 8) {
    add({
      id: 'ms-maxdop', severity: 'medium', area: 'setup', title: 'One query can use every CPU',
      why: 'max degree of parallelism 0 lets a single query use all cores, starving everything else while it runs.',
      evidence: [`max degree of parallelism = 0 on ${h.cpus} CPUs.`],
      fix: 'Set it to the cores of one NUMA node, at most 8.',
      sql: `EXEC sys.sp_configure N'show advanced options', 1; RECONFIGURE;\nEXEC sys.sp_configure N'max degree of parallelism', 8; RECONFIGURE;`,
    })
  }
  if (server('optimize for ad hoc workloads') === 0 && singleUseShare >= 0.3 && h.singleUseMb >= 10) {
    add({
      id: 'ms-adhoc', severity: h.singleUseMb >= 1024 ? 'medium' : 'low', area: 'memory', title: 'The plan cache is full of plans used once',
      why: 'Each statement sent with its values written in gets its own plan, used once and kept. They crowd out the plans that are reused and take memory from data.',
      evidence: [`${mb(h.singleUseMb)} of the ${mb(h.planCacheMb)} plan cache (${pct(singleUseShare)}) is ad hoc plans run once.`],
      fix: 'Turn on optimize for ad hoc workloads (keeps a small stub until a statement runs twice), and have the application send parameters instead of values.',
      sql: `EXEC sys.sp_configure N'show advanced options', 1; RECONFIGURE;\nEXEC sys.sp_configure N'optimize for ad hoc workloads', 1; RECONFIGURE;`,
    })
  }
  if (h.batches >= 1000 && h.compilations / h.batches >= 0.2) {
    add({
      id: 'ms-compiles', severity: h.compilations / h.batches >= 0.5 ? 'medium' : 'low', area: 'queries', title: 'Statements are compiled over and over',
      why: 'Compiling a plan costs CPU. When a large share of batches compile, plans are not being reused: values are written into the statements, or plans are thrown out (recompiles, memory pressure).',
      evidence: [`${count(h.compilations)} compilations for ${count(h.batches)} batches (${pct(h.compilations / h.batches)}); ${count(h.recompiles)} recompilations. ${counting}`],
      fix: 'Send parameterized statements (sp_executesql, prepared statements). PARAMETERIZATION FORCED on the database is the blunt fix when the application cannot change.',
    })
  }

  /* Blocking and transactions now */
  if (h.blocked > 0) {
    add({
      id: 'ms-blocked', severity: h.blocked >= 5 ? 'high' : 'medium', area: 'connections', title: 'Requests are blocked right now',
      why: 'A blocked request waits on a lock another session holds and does no work until it is released.',
      evidence: [`${h.blocked} request${h.blocked === 1 ? '' : 's'} in this database wait on another session now.`],
      fix: 'Open Activity & blocking to see who blocks whom and what the head of the chain runs.',
    })
  }
  if (h.oldestTranS >= 300) {
    add({
      id: 'ms-old-tran', severity: h.oldestTranS >= 3600 ? 'high' : 'medium', area: 'connections', title: 'A transaction has been open for a long time',
      why: 'An open transaction holds its locks, and the log cannot be reused past its start: the log grows and others block on it.',
      evidence: [`The oldest open transaction in this database started ${duration(h.oldestTranS)} ago.`],
      fix: 'Find it in Activity & blocking; commit or end it, and fix the code path that leaves it open.',
    })
  }
  if (h.deadlocks > 0 && h.deadlocks / Math.max(days, 1 / 24) >= 1) {
    add({
      id: 'ms-deadlocks', severity: h.deadlocks / Math.max(days, 1 / 24) >= 50 ? 'medium' : 'low', area: 'queries', title: 'Deadlocks happen',
      why: 'Two sessions each hold a lock the other needs; SQL Server ends one of them with error 1205 and its work is lost.',
      evidence: [`${count(h.deadlocks)} deadlocks. ${counting}`],
      fix: 'The system_health session keeps the deadlock graphs: touch tables in the same order everywhere, keep transactions short, and index the lookups inside them.',
    })
  }

  /* tempdb */
  const wantFiles = Math.min(h.cpus, 8)
  if (h.tempdbFiles > 0 && h.tempdbFiles < wantFiles) {
    add({
      id: 'ms-tempdb-files', severity: waits.some((w) => /^PAGELATCH_/.test(w.wait) && w.percent >= 10) ? 'medium' : 'low', area: 'setup',
      title: 'tempdb has fewer data files than it should',
      why: 'Sessions that make temp tables, sort or spill all allocate in tempdb. With few files they queue on the same allocation pages.',
      evidence: [`${h.tempdbFiles} tempdb data file${h.tempdbFiles === 1 ? '' : 's'} for ${h.cpus} CPUs.`],
      fix: `Use ${wantFiles} data files of the same size and growth.`,
    })
  }
  if (h.tempdbSizes.length > 1) {
    add({
      id: 'ms-tempdb-sizes', severity: 'low', area: 'setup', title: 'tempdb’s data files differ in size',
      why: 'SQL Server spreads tempdb allocations by free space, so the largest file takes most of them and the others do not share the load.',
      evidence: [`File sizes: ${h.tempdbSizes.map((x) => mb(x)).join(', ')}.`],
      fix: 'Give every tempdb data file the same size and the same fixed growth.',
    })
  }

  /* This database's settings and files */
  if (setting('AUTO_SHRINK') === 'ON') {
    add({
      id: 'ms-autoshrink', severity: 'medium', area: 'maintenance', title: 'Auto shrink is on',
      why: 'The database shrinks its files whenever there is free space and grows them again when it needs it. Shrinking moves pages and leaves indexes badly fragmented, and both steps cost I/O while people work.',
      evidence: ['AUTO_SHRINK = ON.'], fix: 'Turn it off. Shrink by hand only after deleting a large part of the data for good.',
      sql: 'ALTER DATABASE CURRENT SET AUTO_SHRINK OFF;',
    })
  }
  if (setting('AUTO_CLOSE') === 'ON') {
    add({
      id: 'ms-autoclose', severity: 'medium', area: 'setup', title: 'Auto close is on',
      why: 'The database is closed when the last user leaves and opened again by the next one, throwing away its cached data and plans each time.',
      evidence: ['AUTO_CLOSE = ON.'], fix: 'Turn it off.', sql: 'ALTER DATABASE CURRENT SET AUTO_CLOSE OFF;',
    })
  }
  if (setting('AUTO_UPDATE_STATISTICS') === 'OFF') {
    add({
      id: 'ms-autostats', severity: 'high', area: 'maintenance', title: 'Statistics are not updated automatically',
      why: 'The optimizer chooses plans from statistics. Without automatic updates they go stale as data changes, and plans built on wrong row counts can be badly slow.',
      evidence: ['AUTO_UPDATE_STATISTICS = OFF.'], fix: 'Turn it on, unless a maintenance job updates every table’s statistics often.',
      sql: 'ALTER DATABASE CURRENT SET AUTO_UPDATE_STATISTICS ON;',
    })
  }
  const qs = h.queryStore ?? ''
  if (qs === 'READ_ONLY' || qs === 'ERROR') {
    add({
      id: 'ms-qs-readonly', severity: 'medium', area: 'setup', title: `Query Store has stopped recording (${qs})`,
      why: 'Query Store went read-only, usually because it reached its size limit; new plans and regressions are no longer captured.',
      evidence: [`${mb(h.queryStoreUsedMb ?? 0)} used of ${mb(h.queryStoreMaxMb ?? 0)}${h.queryStoreReason ? `; read-only reason ${h.queryStoreReason}` : ''}.`],
      fix: 'Raise its size limit or shorten how long it keeps data, then set it back to READ_WRITE.',
      sql: `ALTER DATABASE CURRENT SET QUERY_STORE (OPERATION_MODE = READ_WRITE, MAX_STORAGE_SIZE_MB = ${Math.max(1000, (h.queryStoreMaxMb ?? 0) * 2)});`,
    })
  } else if (setting('QUERY_STORE') === 'OFF' && Number(h.version.split('.')[0]) >= 13) {
    add({
      id: 'ms-qs-off', severity: 'low', area: 'setup', title: 'Query Store is off',
      why: 'Query Store keeps each statement’s plans and run times across restarts. Without it, a query that suddenly got slower cannot be traced to a plan change, and the good plan cannot be forced back.',
      evidence: ['QUERY_STORE = OFF.'], fix: 'Turn it on; its cost is small.',
      sql: 'ALTER DATABASE CURRENT SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE);',
    })
  }
  const logWait = fact('Log waits on')
  const log = f.settings.files.filter((x) => x.type === 'log')
  const data = f.settings.files.filter((x) => x.type === 'rows')
  const logMb = log.reduce((a, x) => a + x.sizeMb, 0)
  const dataMb = data.reduce((a, x) => a + x.sizeMb, 0)
  if (logWait === 'LOG_BACKUP' && setting('RECOVERY') !== 'SIMPLE' && logMb >= 1024 && logMb >= dataMb * 0.5) {
    add({
      id: 'ms-log-backup', severity: logMb >= dataMb ? 'high' : 'medium', area: 'maintenance', title: 'The log keeps growing: no log backups',
      why: `In ${setting('RECOVERY')} recovery the log is only reused after a log backup. Without regular ones it grows until the disk is full, and every growth stalls writes.`,
      evidence: [`Log ${mb(logMb)} for ${mb(dataMb)} of data; log reuse waits on LOG_BACKUP.`],
      fix: 'Schedule log backups (every 15 minutes is common), or switch to SIMPLE recovery if point-in-time restore is not needed.',
    })
  }
  if (h.vlfs >= 200) {
    add({
      id: 'ms-vlfs', severity: h.vlfs >= 1000 ? 'medium' : 'low', area: 'maintenance', title: 'The log is cut into too many pieces',
      why: 'The log is made of virtual log files, a few for each growth. Many small growths leave hundreds of them, which slows startup, restores, log backups and replication.',
      evidence: [`${count(h.vlfs)} virtual log files; log growth ${log.map((x) => x.growth).join(', ') || 'unknown'}.`],
      fix: 'After a log backup, shrink the log and grow it back in a few large steps, and set a fixed growth of a few GB.',
      sql: log[0] ? `-- After a log backup:\nDBCC SHRINKFILE (${msIdent(log[0].name)}, 1);\nALTER DATABASE CURRENT MODIFY FILE (NAME = ${msIdent(log[0].name)}, SIZE = ${Math.max(1024, Math.ceil(logMb / 1024) * 1024)}MB, FILEGROWTH = 1024MB);` : undefined,
    })
  }
  const pctGrowth = f.settings.files.filter((x) => /%$/.test(x.growth))
  if (pctGrowth.length) {
    add({
      id: 'ms-pct-growth', severity: 'low', area: 'setup', title: 'Files grow by a percentage',
      why: 'Percent growth makes each growth larger than the last; on a big file one growth can stall writes for a long time (and log growth is never instant).',
      evidence: pctGrowth.map((x) => `${x.name}: grows by ${x.growth} of ${mb(x.sizeMb)}.`),
      fix: 'Use a fixed growth: a few hundred MB to a few GB, by the file’s size.',
      sql: pctGrowth.map((x) => `ALTER DATABASE CURRENT MODIFY FILE (NAME = ${msIdent(x.name)}, FILEGROWTH = ${x.type === 'log' ? 1024 : 512}MB);`).join('\n'),
    })
  }
  const lockWait = waits.find((w) => /^LCK_M_(S|IS)$/.test(w.wait) && w.percent >= 10 && w.waitMs >= 60_000)
  if (lockWait && setting('READ_COMMITTED_SNAPSHOT') === 'OFF') {
    add({
      id: 'ms-rcsi', severity: 'medium', area: 'queries', title: 'Readers wait for writers',
      why: 'Under plain READ COMMITTED a reader waits for any row a writer holds. READ_COMMITTED_SNAPSHOT lets readers see the last committed version instead.',
      evidence: [`${lockWait.wait} is ${lockWait.percent.toFixed(0)}% of all waiting; READ_COMMITTED_SNAPSHOT = OFF.`],
      fix: 'Turn on READ_COMMITTED_SNAPSHOT after testing: row versions live in tempdb, and code that relies on readers blocking must be checked.',
      sql: 'ALTER DATABASE CURRENT SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE;',
    })
  }

  /* Costly statements */
  const qsList = f.performance.queries
  const cpuAll = qsList.reduce((a, q) => a + q.cpuMs, 0)
  const missingByTable = new Map<string, MSMissing>()
  for (const m of [...f.indexes.missing].sort((a, b) => b.seeks * b.impact - a.seeks * a.impact)) {
    if (!missingByTable.has(m.table.toLowerCase())) missingByTable.set(m.table.toLowerCase(), m)
  }
  const tied = new Set<MSMissing>()
  for (const q of qsList.slice(0, 10)) {
    const share = cpuAll > 0 ? q.cpuMs / cpuAll : 0
    if (q.executions < 5 || share < 0.2 || q.cpuMs < 1000) continue
    const reasons = [`It takes ${pct(share)} of the CPU spent by this database’s cached statements.`]
    if (q.avgReads >= 10_000) reasons.push(`Each run reads ${count(q.avgReads)} pages (${bytes(q.avgReads * 8192)}), far more than a lookup needs.`)
    if (q.avgCpuMs > q.avgMs * 1.5 && q.avgMs > 0) reasons.push('It uses more CPU time than wall time: it runs in parallel.')
    if (q.executions >= 100_000 && q.avgCpuMs < 5) reasons.push(`It is cheap but runs very often: ${count(q.executions)} times. This is usually one query per row in a loop (N+1).`)
    const m = statementTables(q.statement).map((t) => missingByTable.get(t)).find(Boolean)
    if (m) tied.add(m)
    add({
      id: `ms-stmt-${oneLine(q.statement, 60)}`, severity: share >= 0.4 ? 'high' : 'medium', area: 'queries',
      title: `A heavy query: ${oneLine(q.statement, 90)}`, why: reasons.join(' '),
      evidence: [`${count(q.executions)} runs, ${ms(q.cpuMs)} CPU in all, ${ms(q.avgCpuMs) || '<1 ms'} CPU and ${ms(q.avgMs) || '<1 ms'} each, ${count(q.avgReads)} reads each.`],
      fix: m
        ? `SQL Server itself asks for an index on ${m.schema}.${m.table} (${[m.equality, m.range].filter(Boolean).join(', ')}) that it estimates would cut this kind of query’s cost by ${impact(m)}%. Create it${enterprise ? ' online' : ''} and compare.`
        : q.avgReads >= 10_000 ? 'Look at its actual execution plan in SSMS for a scan, and index the columns in its WHERE and JOIN.' : 'Look at its actual execution plan in SSMS to see where the time goes.',
      sql: m ? missingSql(m, enterprise) : undefined,
    })
  }

  /* Indexes */
  for (const m of [...f.indexes.missing].sort((a, b) => b.seeks * b.impact - a.seeks * a.impact).slice(0, 5)) {
    if (tied.has(m) || m.seeks < 10 || m.impact < 50) continue
    add({
      id: `ms-missing-${m.schema}.${m.table}.${m.equality ?? ''}${m.range ?? ''}`, severity: m.seeks * m.impact >= 100_000 ? 'medium' : 'low', area: 'indexes',
      title: `${m.schema}.${m.table} is missing an index SQL Server wants`,
      why: 'While planning queries, SQL Server noted that an index on these columns would have let them seek instead of scan.',
      evidence: [`On (${[m.equality, m.range].filter(Boolean).join(', ')})${m.include ? `, including ${m.include}` : ''}: wanted by ${count(m.seeks)} seeks, ${impact(m)}% cheaper by its estimate.`],
      fix: 'Create it, but check first that it does not nearly duplicate an index already there: SQL Server suggests one per query shape and never merges them.',
      sql: missingSql(m, enterprise),
    })
  }
  const ix = f.indexes.indexes
  const groups = new Map<string, MSIndex[]>()
  for (const i of ix) {
    if (!i.name || !i.keys?.length || i.type === 'heap') continue
    const k = `${i.schema}.${i.table}\u0000${i.keys.join(',')}`
    groups.set(k, [...(groups.get(k) ?? []), i])
  }
  const dup = new Set<MSIndex>()
  for (const g of groups.values()) {
    if (g.length < 2) continue
    const keep = g.find((i) => i.primary) ?? g.find((i) => i.unique) ?? g.find((i) => i.type === 'clustered') ?? g[0]!
    for (const i of g) {
      if (i === keep || i.primary || i.unique || i.type === 'clustered') continue
      dup.add(i)
      add({
        id: `ms-dup-${i.schema}.${i.table}.${i.name}`, severity: 'medium', area: 'indexes', title: `${i.table}.${i.name} duplicates ${keep.name}`,
        why: 'Two indexes with the same keys cost twice on every write and in memory; the optimizer needs only one (included columns aside).',
        evidence: [`Both on (${i.keys!.join(', ')}).`], fix: `Drop ${i.name} once you have checked its included columns are covered.`,
        sql: `DROP INDEX ${msIdent(i.name!)} ON ${table(i.schema, i.table)};`,
      })
    }
  }
  if (days >= 7) {
    for (const i of ix.filter((x) => x.type === 'nonclustered' && x.name && !x.primary && !x.unique && !x.disabled && !dup.has(x)
      && x.seeks + x.scans + x.lookups === 0 && x.updates > 0).sort((a, b) => b.updates - a.updates).slice(0, 10)) {
      add({
        id: `ms-unused-${i.schema}.${i.table}.${i.name}`, severity: 'low', area: 'indexes', title: `${i.table}.${i.name} is never read`,
        why: 'Every insert, update and delete maintains it, and it takes memory and backup space, but no query has used it.',
        evidence: [`${count(i.updates)} writes, no seeks, scans or lookups; ${mb(i.pages * 8 / 1024)}. ${counting}`],
        fix: 'Drop it if no rare job (month-end, a report) relies on it; script it first so it can be put back.',
        sql: `DROP INDEX ${msIdent(i.name!)} ON ${table(i.schema, i.table)};`,
      })
    }
  }
  for (const i of ix.filter((x) => x.type === 'heap' && x.pages >= 1000)) {
    add({
      id: `ms-heap-${i.schema}.${i.table}`, severity: 'medium', area: 'tables', title: `${i.schema}.${i.table} has no clustered index`,
      why: 'A heap keeps rows in no order. Updates that make a row bigger leave forwarding pointers that every scan follows, deleted space is not reused well, and every lookup goes through a row ID.',
      evidence: [`${mb(i.pages * 8 / 1024)} in ${count(i.pages)} pages, stored as a heap.`],
      fix: 'Add a clustered index: on its primary key, or a narrow, increasing column it is usually read by.',
    })
  }
  const frag = ix.filter((x) => x.name && !x.disabled && x.pages >= 1000 && x.fragmentation >= 30).sort((a, b) => b.pages - a.pages).slice(0, 3)
  for (const i of frag) {
    add({
      id: `ms-frag-${i.schema}.${i.table}.${i.name}`, severity: 'low', area: 'maintenance', title: `${i.table}.${i.name} is ${i.fragmentation.toFixed(0)}% fragmented`,
      why: 'Its pages are out of order and part empty, so range scans read more pages than the data needs. On SSDs the order matters less than the empty space.',
      evidence: [`${count(i.pages)} pages, ${i.fragmentation.toFixed(0)}% fragmented.`],
      fix: 'Rebuild it in a quiet hour (Indexes & statistics). If it fragments again quickly, a lower fill factor helps.',
      sql: `ALTER INDEX ${msIdent(i.name!)} ON ${table(i.schema, i.table)} REBUILD${enterprise ? ' WITH (ONLINE = ON)' : ''};`,
    })
  }
  const stale = f.indexes.statistics.filter((s) => s.rows >= 10_000 && s.modified / s.rows >= 0.2).sort((a, b) => b.modified - a.modified).slice(0, 5)
  for (const s of stale) {
    add({
      id: `ms-stats-${s.schema}.${s.table}.${s.name}`, severity: s.modified / s.rows >= 0.5 ? 'medium' : 'low', area: 'maintenance',
      title: `Statistics ${s.table}.${s.name} are out of date`,
      why: 'The optimizer estimates row counts from statistics. When much of the table has changed since they were taken, estimates go wrong and so do plans.',
      evidence: [`${count(s.modified)} of ${count(s.rows)} rows changed since ${s.lastUpdated ? s.lastUpdated.slice(0, 16).replace('T', ' ') : 'never'}.`],
      fix: 'Update them now, and have a maintenance job keep them current.',
      sql: `UPDATE STATISTICS ${table(s.schema, s.table)} ${msIdent(s.name)};`,
    })
  }

  out.sort((a, b) => ORDER[a.severity] - ORDER[b.severity])
  return { findings: out, countingS: up, signalShare, singleUseShare }
}

/** The CREATE INDEX for a missing index, online where the edition allows. */
export function missingSql(m: MSMissing, enterprise: boolean): string {
  return `${m.create}${enterprise ? ' WITH (ONLINE = ON)' : ''};`
}
