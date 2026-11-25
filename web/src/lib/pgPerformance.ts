/*
Why a Postgres database is heavy, and what to change: the analysis behind the
Performance panel. Pure, so it is tested without a database.

The panel gathers facts with ordinary catalog statements run as the person's
role (PG_PERF_SQL below), so Postgres decides what each level may see, and,
at DBA, PgBouncer's own numbers through the agent (tunnel.PoolerStats). This
file turns those facts into findings. Each finding says what was seen (its
evidence, with the numbers), why that makes the database slow or hungry, and
what to change, with the statement to run where there is one. It never runs
anything itself: the panel only opens a fix in the editor, where it goes
through the same checks and approvals as anything typed.

A finding is only as good as its evidence, so every rule below is written to
say nothing when the numbers are too small to mean anything, and the report
says how long the statistics have been counting (since the last reset or
restart), because "this index was never used" means little after an hour.
*/

export type Severity = 'high' | 'medium' | 'low'
export type Area = 'queries' | 'tables' | 'indexes' | 'memory' | 'connections' | 'pooler' | 'maintenance' | 'setup'

export interface Finding {
  id: string
  severity: Severity
  area: Area
  title: string
  /** Why this makes the database heavy, in a sentence or two. */
  why: string
  /** What was seen, with numbers. */
  evidence: string[]
  /** What to change. */
  fix: string
  /** A statement that carries out (or starts) the fix, for the editor. */
  sql?: string
}

/* ── Facts ─────────────────────────────────────────────────────────────── */

export interface PgOverview {
  versionNum: number
  maxConnections: number
  reservedConnections: number
  blksHit: number
  blksRead: number
  commits: number
  rollbacks: number
  deadlocks: number
  tempFiles: number
  tempBytes: number
  statsReset: string
  /** Seconds since Postgres started. */
  uptimeS: number
  dbBytes: number
  /** pg_stat_statements is installed in this database. */
  statements: boolean
  preload: string
  replica: boolean
  /** age(datfrozenxid) of this database. */
  xidAge: number
}

export interface PgActivityGroup {
  state: string
  waitType: string
  count: number
  /** Longest open transaction and longest running statement, seconds. */
  longestTxS: number
  longestActiveS: number
  /** In this database, rather than another one on the server. */
  here: boolean
}

export interface PgClientGroup {
  application: string
  address: string
  role: string
  count: number
  idle: number
  idleInTx: number
}

export interface PgSetting { name: string; setting: string; unit: string; source: string }

export interface PgStatement {
  queryid: string
  query: string
  calls: number
  totalMs: number
  meanMs: number
  stddevMs: number
  rows: number
  hit: number
  read: number
  tempWritten: number
  role: string
  /** Share of the time all statements here took, 0..1. */
  share: number
}

export interface PgTable {
  schema: string
  name: string
  live: number
  dead: number
  seqScan: number
  seqRead: number
  idxScan: number
  lastVacuum: string
  lastAnalyze: string
  modSinceAnalyze: number
  totalBytes: number
  heapBytes: number
  heapHit: number
  heapRead: number
}

export interface PgIndex {
  schema: string
  table: string
  name: string
  scans: number
  bytes: number
  unique: boolean
  primary: boolean
  def: string
  /** Backs a constraint (primary key, unique, exclusion). */
  constraint: boolean
}

export interface PgForeignKey { table: string; name: string; columns: string; references: string }
export interface PgSlot { name: string; active: boolean; retainedBytes: number }
export interface PgCheckpoints { timed: number; requested: number }

export interface PoolerTable { cols: string[]; rows: string[][] }
/** What the agent read of PgBouncer (tunnel.PoolerStats). */
export interface PoolerStats {
  found: boolean
  through: boolean
  port?: number
  config?: string
  running: boolean
  runsAs?: string
  settings?: Record<string, string>
  settingsFrom?: 'console' | 'file'
  databasePools?: Record<string, Record<string, string>>
  version?: string
  pools?: PoolerTable
  stats?: PoolerTable
  databases?: PoolerTable
  liveError?: string
  others?: number[]
}

export interface PgPerfFacts {
  database: string
  overview: PgOverview
  activity: PgActivityGroup[]
  clients: PgClientGroup[]
  settings: PgSetting[]
  /** null: pg_stat_statements is not there or could not be read. */
  statements: PgStatement[] | null
  statementsError?: string
  tables: PgTable[]
  indexes: PgIndex[]
  foreignKeys: PgForeignKey[]
  slots: PgSlot[]
  checkpoints?: PgCheckpoints
  /** null: not read (below DBA); undefined: not asked. */
  pooler?: PoolerStats | null
  /** The session itself goes through PgBouncer. */
  viaPooler?: boolean
}

/* ── The statements that gather them ───────────────────────────────────── */

const SETTINGS = [
  'shared_buffers', 'effective_cache_size', 'work_mem', 'maintenance_work_mem', 'max_connections',
  'random_page_cost', 'effective_io_concurrency', 'checkpoint_timeout', 'max_wal_size', 'checkpoint_completion_target',
  'autovacuum', 'autovacuum_vacuum_scale_factor', 'autovacuum_analyze_scale_factor', 'autovacuum_max_workers',
  'autovacuum_vacuum_cost_limit', 'track_io_timing', 'log_min_duration_statement', 'idle_in_transaction_session_timeout',
  'statement_timeout', 'default_statistics_target', 'jit', 'huge_pages', 'max_parallel_workers_per_gather',
  'track_activity_query_size', 'superuser_reserved_connections', 'server_version',
]

export const PG_PERF_SQL = {
  overview: `SELECT current_setting('server_version_num'),
  current_setting('max_connections'), current_setting('superuser_reserved_connections'),
  d.blks_hit::text, d.blks_read::text, d.xact_commit::text, d.xact_rollback::text, d.deadlocks::text,
  d.temp_files::text, d.temp_bytes::text, coalesce(d.stats_reset::text, ''),
  extract(epoch FROM now() - pg_postmaster_start_time())::bigint::text,
  pg_database_size(current_database())::text,
  (SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements')::text,
  current_setting('shared_preload_libraries'),
  pg_is_in_recovery()::text,
  (SELECT age(datfrozenxid) FROM pg_database WHERE datname = current_database())::text
FROM pg_stat_database d WHERE d.datname = current_database()`,

  activity: `SELECT coalesce(state, ''), coalesce(wait_event_type, ''), count(*)::text,
  coalesce(max(extract(epoch FROM now() - xact_start))::bigint::text, '0'),
  coalesce(max(CASE WHEN state = 'active' THEN extract(epoch FROM now() - query_start) END)::bigint::text, '0'),
  (datname IS NOT DISTINCT FROM current_database())::text
FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()
GROUP BY 1, 2, 6`,

  clients: `SELECT coalesce(application_name, ''), coalesce(host(client_addr), 'local'), coalesce(usename, ''), count(*)::text,
  count(*) FILTER (WHERE state = 'idle')::text, count(*) FILTER (WHERE state LIKE 'idle in transaction%')::text
FROM pg_stat_activity WHERE backend_type = 'client backend' AND datname = current_database() AND pid <> pg_backend_pid()
GROUP BY 1, 2, 3 ORDER BY count(*) DESC LIMIT 50`,

  settings: `SELECT name, setting, coalesce(unit, ''), source FROM pg_settings
WHERE name IN (${SETTINGS.map((s) => `'${s}'`).join(', ')})`,

  /** pg_stat_statements renamed its time columns in Postgres 13. */
  statements: (versionNum: number) => {
    const t = versionNum >= 130000 ? 'exec_time' : 'time'
    return `SELECT s.queryid::text, left(s.query, 4000), s.calls::text, s.total_${t}::text, s.mean_${t}::text, s.stddev_${t}::text,
  s.rows::text, s.shared_blks_hit::text, s.shared_blks_read::text, s.temp_blks_written::text, coalesce(r.rolname, ''),
  coalesce((s.total_${t} / nullif(sum(s.total_${t}) OVER (), 0))::text, '0')
FROM pg_stat_statements s LEFT JOIN pg_roles r ON r.oid = s.userid
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
ORDER BY s.total_${t} DESC LIMIT 25`
  },

  tables: `SELECT t.schemaname, t.relname, t.n_live_tup::text, t.n_dead_tup::text, coalesce(t.seq_scan, 0)::text,
  coalesce(t.seq_tup_read, 0)::text, coalesce(t.idx_scan, 0)::text,
  coalesce(greatest(t.last_autovacuum, t.last_vacuum)::text, ''), coalesce(greatest(t.last_autoanalyze, t.last_analyze)::text, ''),
  t.n_mod_since_analyze::text, pg_total_relation_size(t.relid)::text, pg_relation_size(t.relid)::text,
  coalesce(io.heap_blks_hit, 0)::text, coalesce(io.heap_blks_read, 0)::text
FROM pg_stat_user_tables t LEFT JOIN pg_statio_user_tables io ON io.relid = t.relid
ORDER BY pg_total_relation_size(t.relid) DESC LIMIT 200`,

  indexes: `SELECT s.schemaname, s.relname, s.indexrelname, s.idx_scan::text, pg_relation_size(s.indexrelid)::text,
  i.indisunique::text, i.indisprimary::text, pg_get_indexdef(s.indexrelid),
  EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = s.indexrelid)::text
FROM pg_stat_user_indexes s JOIN pg_index i ON i.indexrelid = s.indexrelid
ORDER BY pg_relation_size(s.indexrelid) DESC LIMIT 300`,

  /** Foreign keys whose columns lead no index on the referencing table. */
  foreignKeys: `SELECT c.conrelid::regclass::text, c.conname,
  (SELECT string_agg(quote_ident(a.attname), ', ' ORDER BY x.n) FROM unnest(c.conkey) WITH ORDINALITY x(k, n)
     JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = x.k),
  c.confrelid::regclass::text
FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
WHERE c.contype = 'f' AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.conrelid
    AND (string_to_array(i.indkey::text, ' ')::int2[])[1:cardinality(c.conkey)] @> c.conkey)
LIMIT 100`,

  slots: `SELECT slot_name, active::text, coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn), 0)::bigint::text
FROM pg_replication_slots`,

  /** Postgres 17 moved checkpoint counts to pg_stat_checkpointer. */
  checkpoints: (versionNum: number) => versionNum >= 170000
    ? 'SELECT num_timed::text, num_requested::text FROM pg_stat_checkpointer'
    : 'SELECT checkpoints_timed::text, checkpoints_req::text FROM pg_stat_bgwriter',
}

/* ── Parsing what came back ────────────────────────────────────────────── */

type Row = (string | null)[]
const n = (s: string | null | undefined) => {
  const v = Number(s ?? '')
  return Number.isFinite(v) ? v : 0
}
const s = (v: string | null | undefined) => v ?? ''
const b = (v: string | null | undefined) => v === 'true' || v === 't'

export const parse = {
  overview: (r: Row): PgOverview => ({
    versionNum: n(r[0]), maxConnections: n(r[1]), reservedConnections: n(r[2]), blksHit: n(r[3]), blksRead: n(r[4]),
    commits: n(r[5]), rollbacks: n(r[6]), deadlocks: n(r[7]), tempFiles: n(r[8]), tempBytes: n(r[9]), statsReset: s(r[10]),
    uptimeS: n(r[11]), dbBytes: n(r[12]), statements: n(r[13]) > 0, preload: s(r[14]), replica: b(r[15]), xidAge: n(r[16]),
  }),
  activity: (r: Row): PgActivityGroup => ({
    state: s(r[0]), waitType: s(r[1]), count: n(r[2]), longestTxS: n(r[3]), longestActiveS: n(r[4]), here: b(r[5]),
  }),
  clients: (r: Row): PgClientGroup => ({
    application: s(r[0]), address: s(r[1]), role: s(r[2]), count: n(r[3]), idle: n(r[4]), idleInTx: n(r[5]),
  }),
  settings: (r: Row): PgSetting => ({ name: s(r[0]), setting: s(r[1]), unit: s(r[2]), source: s(r[3]) }),
  statements: (r: Row): PgStatement => ({
    queryid: s(r[0]), query: s(r[1]), calls: n(r[2]), totalMs: n(r[3]), meanMs: n(r[4]), stddevMs: n(r[5]), rows: n(r[6]),
    hit: n(r[7]), read: n(r[8]), tempWritten: n(r[9]), role: s(r[10]), share: n(r[11]),
  }),
  tables: (r: Row): PgTable => ({
    schema: s(r[0]), name: s(r[1]), live: n(r[2]), dead: n(r[3]), seqScan: n(r[4]), seqRead: n(r[5]), idxScan: n(r[6]),
    lastVacuum: s(r[7]), lastAnalyze: s(r[8]), modSinceAnalyze: n(r[9]), totalBytes: n(r[10]), heapBytes: n(r[11]),
    heapHit: n(r[12]), heapRead: n(r[13]),
  }),
  indexes: (r: Row): PgIndex => ({
    schema: s(r[0]), table: s(r[1]), name: s(r[2]), scans: n(r[3]), bytes: n(r[4]), unique: b(r[5]), primary: b(r[6]),
    def: s(r[7]), constraint: b(r[8]),
  }),
  foreignKeys: (r: Row): PgForeignKey => ({ table: s(r[0]), name: s(r[1]), columns: s(r[2]), references: s(r[3]) }),
  slots: (r: Row): PgSlot => ({ name: s(r[0]), active: b(r[1]), retainedBytes: n(r[2]) }),
  checkpoints: (r: Row): PgCheckpoints => ({ timed: n(r[0]), requested: n(r[1]) }),
}

/* ── Formatting ────────────────────────────────────────────────────────── */

export function bytes(v: number): string {
  if (v < 1024) return `${v} B`
  const u = ['KB', 'MB', 'GB', 'TB']
  let x = v / 1024
  let i = 0
  while (x >= 1024 && i < u.length - 1) { x /= 1024; i++ }
  return `${x >= 100 ? Math.round(x) : x.toFixed(1)} ${u[i]}`
}
export function count(v: number): string {
  if (v >= 1e9) return `${(v / 1e9).toFixed(1)}B`
  if (v >= 1e6) return `${(v / 1e6).toFixed(1)}M`
  if (v >= 1e4) return `${(v / 1e3).toFixed(0)}k`
  return String(Math.round(v))
}
export function ms(v: number): string {
  if (v >= 3_600_000) return `${(v / 3_600_000).toFixed(1)} h`
  if (v >= 60_000) return `${(v / 60_000).toFixed(1)} min`
  if (v >= 1000) return `${(v / 1000).toFixed(1)} s`
  if (v >= 10) return `${Math.round(v)} ms`
  return `${v.toFixed(2)} ms`
}
export function duration(sec: number): string {
  if (sec >= 86400) return `${(sec / 86400).toFixed(1)} days`
  if (sec >= 3600) return `${(sec / 3600).toFixed(1)} hours`
  const plural = (v: number, w: string) => `${v} ${w}${v === 1 ? '' : 's'}`
  if (sec >= 60) return plural(Math.round(sec / 60), 'minute')
  return plural(Math.round(sec), 'second')
}
const pct = (x: number) => `${(x * 100).toFixed(x < 0.1 ? 1 : 0)}%`
const qi = (name: string) => (/^[a-z_][a-z0-9_]*$/.test(name) ? name : `"${name.replace(/"/g, '""')}"`)
const qualified = (schema: string, name: string) => `${qi(schema)}.${qi(name)}`

/** A setting in bytes, from pg_settings' value and unit ("16384" "8kB"). */
export function settingBytes(st: PgSetting | undefined): number {
  if (!st) return 0
  const v = n(st.setting)
  const m = /^(\d+)?\s*(B|kB|MB|GB|TB)$/.exec(st.unit)
  if (!m) return v
  const unit = { B: 1, kB: 1024, MB: 1024 ** 2, GB: 1024 ** 3, TB: 1024 ** 4 }[m[2] as 'B'] ?? 1
  return v * (m[1] ? Number(m[1]) : 1) * unit
}
/** A setting in milliseconds (unit "ms", "s" or "min"). */
function settingMs(st: PgSetting | undefined): number {
  if (!st) return 0
  const v = n(st.setting)
  return st.unit === 's' ? v * 1000 : st.unit === 'min' ? v * 60_000 : v
}

/* ── One statement: why it costs what it costs ─────────────────────────── */

export interface StatementDiagnosis { reasons: string[]; fixes: string[] }

export function diagnoseStatement(q: PgStatement): StatementDiagnosis {
  const reasons: string[] = []
  const fixes: string[] = []
  const blocks = q.hit + q.read
  const perCall = q.calls > 0 ? q.rows / q.calls : 0
  if (q.share >= 0.2) reasons.push(`It alone takes ${pct(q.share)} of all the time statements spend running in this database.`)
  if (q.meanMs >= 500) {
    reasons.push(`Each run takes ${ms(q.meanMs)} on average.`)
    fixes.push('Look at its plan (Explain) for a sequential scan on a large table, a nested loop over many rows, or a sort that spills; an index on the columns in its WHERE, JOIN or ORDER BY is the usual fix.')
  }
  if (q.calls >= 100_000 && q.meanMs < 10 && q.share >= 0.1) {
    reasons.push(`It is cheap (${ms(q.meanMs)}) but runs very often: ${count(q.calls)} times.`)
    fixes.push('This is usually a loop in the application running one query per row (N+1). Fetch the rows in one query, or cache the answer.')
  }
  if (blocks >= 10_000 && q.read / blocks > 0.1) {
    reasons.push(`${pct(q.read / blocks)} of the pages it needs are not in memory and are read from disk.`)
    fixes.push('Read fewer pages: an index that matches its filter, or selecting only the columns needed. If the data it touches is simply larger than shared_buffers, more memory helps.')
  }
  if (q.tempWritten > 0) {
    reasons.push(`It writes ${bytes(q.tempWritten * 8192)} of temporary files in total: a sort or hash that does not fit in work_mem goes to disk.`)
    fixes.push('An index in the ORDER BY or GROUP BY order avoids the sort; otherwise raise work_mem for this statement (SET LOCAL work_mem) rather than for every connection.')
  }
  if (perCall >= 10_000) {
    reasons.push(`It returns ${count(perCall)} rows per call on average.`)
    fixes.push('Page through the result (LIMIT with a key) or aggregate in the database instead of sending every row.')
  }
  if (q.meanMs >= 50 && q.stddevMs > 2 * q.meanMs) {
    reasons.push(`Its time varies a lot (average ${ms(q.meanMs)}, deviation ${ms(q.stddevMs)}): it sometimes waits on locks, or on data that is not cached, or its plan depends on the values passed.`)
  }
  if (reasons.length === 0 && q.share >= 0.05) reasons.push(`It takes ${pct(q.share)} of the statement time here, over ${count(q.calls)} runs.`)
  return { reasons, fixes }
}

/**
 * A statement to see a recorded query's plan. pg_stat_statements keeps the
 * query with its values replaced by $1, $2…; Postgres 16 can plan that as it
 * is (GENERIC_PLAN). Before 16 the values have to be put back by hand.
 */
export function explainFor(q: PgStatement, versionNum: number): string {
  const text = q.query.trim().replace(/;\s*$/, '')
  const params = /\$\d+/.test(text)
  if (versionNum >= 160000 && params) return `EXPLAIN (GENERIC_PLAN)\n${text}`
  if (params) return `-- Put real values in place of $1, $2 … before running.\nEXPLAIN\n${text}`
  return `EXPLAIN\n${text}`
}

/** A large table read whole again and again: the missing-index signature. */
function scannedWhole(t: PgTable): boolean {
  const avg = t.seqScan > 0 ? t.seqRead / t.seqScan : 0
  return t.live >= 100_000 && t.seqScan >= 50 && avg >= 50_000 && t.seqScan > t.idxScan
}

/* ── The whole database ────────────────────────────────────────────────── */

export interface PerfReport {
  findings: Finding[]
  /** How long the counters have been counting, for the report's header. */
  countingS: number
  cacheHit: number | null
  connections: { used: number; max: number; idle: number; idleInTx: number; active: number; waitingOnLock: number }
}

const ORDER: Record<Severity, number> = { high: 0, medium: 1, low: 2 }

/** How long the database's counters have been counting, in seconds. */
function countingSeconds(o: PgOverview, now: Date): number {
  if (o.statsReset) {
    const t = Date.parse(o.statsReset.replace(' ', 'T'))
    if (Number.isFinite(t)) return Math.max(0, (now.getTime() - t) / 1000)
  }
  return o.uptimeS
}

export function analyzePostgres(f: PgPerfFacts, now = new Date()): PerfReport {
  const out: Finding[] = []
  const add = (x: Finding) => out.push(x)
  const o = f.overview
  const set = (name: string) => f.settings.find((x) => x.name === name)
  const countingS = countingSeconds(o, now)
  const days = countingS / 86400
  const counting = `Postgres has been counting for ${duration(countingS)}.`

  /* Connections */
  const all = f.activity
  const used = all.reduce((a, g) => a + g.count, 0)
  const idle = all.filter((g) => g.state === 'idle').reduce((a, g) => a + g.count, 0)
  const idleInTx = all.filter((g) => g.state.startsWith('idle in transaction'))
  const idleInTxCount = idleInTx.reduce((a, g) => a + g.count, 0)
  const active = all.filter((g) => g.state === 'active').reduce((a, g) => a + g.count, 0)
  const lockWait = all.filter((g) => g.waitType === 'Lock').reduce((a, g) => a + g.count, 0)
  const usable = Math.max(1, o.maxConnections - o.reservedConnections)
  const longestIdleTx = Math.max(0, ...idleInTx.map((g) => g.longestTxS))
  const longestActive = Math.max(0, ...all.map((g) => g.longestActiveS))

  if (used / usable >= 0.8) {
    add({
      id: 'conn-near-limit', severity: used / usable >= 0.95 ? 'high' : 'medium', area: 'connections',
      title: 'Connections are close to the limit',
      why: 'When max_connections is reached, new connections are refused with "too many clients". Every connection is also a Postgres process with its own memory, so many of them crowd out the cache.',
      evidence: [`${used} of ${usable} usable connections are open (max_connections ${o.maxConnections}, ${o.reservedConnections} kept for superusers).`],
      fix: f.pooler?.found || f.viaPooler
        ? 'Let PgBouncer hold the clients and keep fewer server connections: lower its pool sizes or max_db_connections so they add up to less than max_connections, and use transaction pooling.'
        : 'Put a connection pooler (PgBouncer, in transaction mode) in front, so hundreds of clients share a few dozen server connections, instead of raising max_connections.',
    })
  }
  if (idle >= 20 && idle / Math.max(1, used) >= 0.6 && !(f.pooler?.found)) {
    add({
      id: 'conn-idle', severity: 'medium', area: 'connections',
      title: 'Most connections sit idle',
      why: 'An idle connection still holds a Postgres process and its memory. Applications that each keep their own pool open many such connections, which is what a pooler is for.',
      evidence: [`${idle} of ${used} connections are idle.`],
      fix: 'Run PgBouncer in transaction mode in front of this server and point the applications at it, or shrink the applications’ own pools.',
    })
  }
  if (idleInTxCount > 0 && longestIdleTx >= 60) {
    const timeout = settingMs(set('idle_in_transaction_session_timeout'))
    add({
      id: 'idle-in-tx', severity: longestIdleTx >= 600 ? 'high' : 'medium', area: 'connections',
      title: 'Transactions are left open doing nothing',
      why: 'A transaction left open keeps its locks, so other statements wait behind it, and stops VACUUM from removing dead rows anywhere in the database, which makes tables and indexes grow. Behind PgBouncer in transaction mode it also holds a server connection the whole time.',
      evidence: [
        `${idleInTxCount} connection${idleInTxCount === 1 ? ' is' : 's are'} idle in a transaction; the longest has been open ${duration(longestIdleTx)}.`,
        timeout ? `idle_in_transaction_session_timeout is ${ms(timeout)}.` : 'idle_in_transaction_session_timeout is off.',
      ],
      fix: 'Find the code that opens a transaction and then waits (on a user, a network call, a long computation) and commit sooner. Activity & locks shows which connection it is. A timeout ends the ones that are forgotten.',
      sql: timeout ? undefined : `ALTER DATABASE ${qi(f.database)} SET idle_in_transaction_session_timeout = '5min';`,
    })
  }
  if (lockWait > 0) {
    add({
      id: 'lock-waits', severity: lockWait >= 5 ? 'high' : 'medium', area: 'connections',
      title: 'Statements are waiting on locks right now',
      why: 'A statement waiting on a lock does nothing until the transaction holding it ends, however fast the statement is.',
      evidence: [`${lockWait} connection${lockWait === 1 ? ' is' : 's are'} waiting on a lock.`],
      fix: 'Open Activity & locks to see who blocks whom. Usually a long or forgotten transaction is at the front; keep transactions short and take locks in the same order everywhere.',
    })
  }
  if (longestActive >= 300) {
    add({
      id: 'long-running', severity: 'medium', area: 'queries',
      title: 'A statement has been running for a long time',
      why: 'A long statement holds a connection, its locks, and the oldest snapshot, which keeps VACUUM from cleaning up behind it.',
      evidence: [`The longest running statement has been going for ${duration(longestActive)}.`],
      fix: 'Check it in Activity & locks. For reporting work, a replica is the better place; a statement_timeout for application roles stops runaways.',
    })
  }

  /* Memory and cache */
  const blocks = o.blksHit + o.blksRead
  const cacheHit = blocks > 0 ? o.blksHit / blocks : null
  const sharedBuffers = set('shared_buffers')
  if (cacheHit !== null && blocks >= 1_000_000 && cacheHit < 0.98) {
    add({
      id: 'cache-hit', severity: cacheHit < 0.9 ? 'high' : 'medium', area: 'memory',
      title: 'Data is read from disk too often',
      why: 'A page Postgres finds in shared_buffers costs microseconds; one it has to read costs far more. Below about 99% for an application database, queries spend their time waiting on the disk.',
      evidence: [
        `${pct(cacheHit)} of pages were found in memory (${count(o.blksRead)} read from disk). ${counting}`,
        sharedBuffers ? `shared_buffers is ${bytes(settingBytes(sharedBuffers))}; the database is ${bytes(o.dbBytes)}.` : `The database is ${bytes(o.dbBytes)}.`,
      ],
      fix: 'First make queries read less: the tables with many sequential scans below, and the statements that read from disk. Then size shared_buffers at about a quarter of the server’s memory (a restart is needed).',
    })
  }
  if (sharedBuffers && sharedBuffers.source === 'default' && settingBytes(sharedBuffers) <= 128 * 1024 ** 2 && o.dbBytes > 1024 ** 3) {
    add({
      id: 'shared-buffers-default', severity: 'medium', area: 'setup',
      title: 'shared_buffers is still at its default',
      why: 'Postgres ships with 128 MB of shared_buffers so it starts anywhere; for a database of several gigabytes that is too little to keep the working set in memory.',
      evidence: [`shared_buffers is ${bytes(settingBytes(sharedBuffers))} (default); the database is ${bytes(o.dbBytes)}.`],
      fix: 'Set it to about 25% of the server’s memory, and effective_cache_size to about 70%. shared_buffers takes effect after a restart.',
      sql: "ALTER SYSTEM SET shared_buffers = '2GB';  -- about a quarter of the server's memory\nALTER SYSTEM SET effective_cache_size = '6GB';  -- about 70%\n-- shared_buffers needs a restart of Postgres",
    })
  }
  const workMem = set('work_mem')
  if (o.tempBytes > 0 && days > 0) {
    const perDay = o.tempBytes / Math.max(days, 1 / 24)
    if (perDay >= 1024 ** 3) {
      add({
        id: 'temp-files', severity: perDay >= 50 * 1024 ** 3 ? 'high' : 'medium', area: 'memory',
        title: 'Sorts and hashes spill to disk',
        why: 'A sort, hash join or aggregate that needs more than work_mem writes temporary files and reads them back, which is many times slower than doing it in memory.',
        evidence: [
          `${count(o.tempFiles)} temporary files, ${bytes(o.tempBytes)} in all (about ${bytes(perDay)} a day). ${counting}`,
          workMem ? `work_mem is ${bytes(settingBytes(workMem))}.` : '',
        ].filter(Boolean),
        fix: 'Find the statements below that write temporary files. An index in the order they sort or group avoids the sort; otherwise raise work_mem for those roles or statements. Raising it for everyone multiplies by every connection and every sort in a plan.',
        sql: `ALTER ROLE app_role IN DATABASE ${qi(f.database)} SET work_mem = '64MB';  -- the role that runs the heavy reports`,
      })
    }
  }

  /* Errors */
  const xacts = o.commits + o.rollbacks
  if (xacts >= 10_000 && o.rollbacks / xacts >= 0.05) {
    add({
      id: 'rollbacks', severity: o.rollbacks / xacts >= 0.2 ? 'medium' : 'low', area: 'queries',
      title: 'Many transactions roll back',
      why: 'Work that is rolled back was done for nothing, and usually means statements are failing (constraint violations, serialization failures, timeouts) and being retried.',
      evidence: [`${pct(o.rollbacks / xacts)} of ${count(xacts)} transactions rolled back. ${counting}`],
      fix: 'Look in the Postgres log for the errors behind them; some frameworks also roll back read-only transactions on purpose, which is harmless.',
    })
  }
  if (o.deadlocks > 0 && o.deadlocks / Math.max(days, 1) >= 1) {
    add({
      id: 'deadlocks', severity: 'medium', area: 'queries',
      title: 'Deadlocks happen',
      why: 'In a deadlock two transactions wait on each other until Postgres cancels one after deadlock_timeout; both lose that time and one has to retry.',
      evidence: [`${o.deadlocks} deadlocks. ${counting}`],
      fix: 'Update rows in a consistent order (for example by primary key) in every code path, and keep transactions short. The Postgres log names the statements of each deadlock.',
    })
  }

  /* Statements */
  if (f.statements === null) {
    const loaded = o.preload.split(',').map((x) => x.trim()).includes('pg_stat_statements')
    add({
      id: 'no-statements', severity: 'medium', area: 'setup',
      title: 'Which queries are heavy cannot be seen yet',
      why: 'pg_stat_statements is the extension that counts the time, calls, rows and disk reads of every query. Without it Postgres keeps no history of which queries cost the most.',
      evidence: [f.statementsError
        ? `Reading pg_stat_statements failed: ${f.statementsError}`
        : o.statements ? 'pg_stat_statements is installed but could not be read.' : 'pg_stat_statements is not installed in this database.',
      loaded ? 'It is already in shared_preload_libraries.' : 'It is not in shared_preload_libraries.'],
      fix: loaded
        ? 'Install it in this database (Extensions, or the statement below), then come back after the application has run for a while.'
        : 'Add it to shared_preload_libraries, restart Postgres, then install it in this database (Extensions, or the statements below).',
      sql: loaded
        ? 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements;'
        : `ALTER SYSTEM SET shared_preload_libraries = '${[...o.preload.split(',').map((x) => x.trim()).filter(Boolean), 'pg_stat_statements'].join(',')}';\n-- restart Postgres, then:\nCREATE EXTENSION IF NOT EXISTS pg_stat_statements;`,
    })
  } else {
    for (const q of f.statements.slice(0, 10)) {
      const d = diagnoseStatement(q)
      // A one-off (a bulk load, a migration) is not what makes a database
      // heavy day to day, however long it took.
      const heavy = q.calls >= 5 && (q.share >= 0.2 || (q.meanMs >= 1000 && q.calls >= 10) || (q.share >= 0.1 && d.fixes.length > 0))
      if (!heavy) continue
      // Tie it to a table it reads whole, when there is one: that is the
      // most likely reason, and the index to try.
      const scanned = f.tables.filter(scannedWhole).find((t) => statementsOn([q], t.name).length > 0)
      if (scanned) {
        const cols = filterColumns([q])
        d.reasons.push(`It reads from ${qualified(scanned.schema, scanned.name)}, which is scanned whole again and again (${count(scanned.live)} rows).`)
        d.fixes.unshift(cols.length
          ? `An index on ${qualified(scanned.schema, scanned.name)} (${cols.map(qi).join(', ')}) is the likely fix; confirm with Explain that it scans the table now.`
          : `Explain it: if it shows a Seq Scan on ${qualified(scanned.schema, scanned.name)}, index the columns it filters on.`)
      }
      add({
        id: `stmt-${q.queryid}`, severity: q.share >= 0.3 || q.meanMs >= 5000 ? 'high' : 'medium', area: 'queries',
        title: `A heavy query: ${oneLine(q.query, 90)}`,
        why: d.reasons.join(' '),
        evidence: [`${count(q.calls)} calls, ${ms(q.totalMs)} in total, ${ms(q.meanMs)} on average, ${count(q.rows)} rows${q.role ? `, run as ${q.role}` : ''}.`],
        fix: d.fixes.join(' ') || 'Look at its plan to see where the time goes.',
        sql: explainFor(q, o.versionNum),
      })
    }
  }

  /* Tables */
  for (const t of f.tables) {
    const name = qualified(t.schema, t.name)
    const avgSeq = t.seqScan > 0 ? t.seqRead / t.seqScan : 0
    if (scannedWhole(t)) {
      const on = statementsOn(f.statements ?? [], t.name)
      const cols = filterColumns(on)
      add({
        id: `seq-${name}`, severity: t.seqRead >= 1e9 ? 'high' : 'medium', area: 'tables',
        title: `${name} is read whole, over and over`,
        why: 'A sequential scan reads every row of the table. On a large table, a query that wants a few rows but has no index to find them reads them all, every time.',
        evidence: [
          `${count(t.seqScan)} sequential scans read ${count(t.seqRead)} rows (about ${count(avgSeq)} each), against ${count(t.idxScan)} index scans; the table has ${count(t.live)} rows, ${bytes(t.totalBytes)}.`,
          ...on.slice(0, 3).map((q) => `${count(q.calls)} × ${ms(q.meanMs)}: ${oneLine(q.query, 120)}`),
        ],
        fix: cols.length
          ? `The queries on ${name} filter on ${cols.map(qi).join(', ')}. Check one with Explain; if it shows a Seq Scan on ${name}, an index on ${cols.length > 1 ? 'those columns' : 'that column'} turns the scan into a lookup. CREATE INDEX CONCURRENTLY builds it while writes go on.`
          : `Find the queries on ${name} among the heavy ones and look at their plans; an index on the columns they filter or join on turns the scan into a lookup. Build it with CREATE INDEX CONCURRENTLY so writes go on.`,
        sql: cols.length
          ? `-- Check first: Explain one of the queries above\nCREATE INDEX CONCURRENTLY ON ${name} (${cols.map(qi).join(', ')});`
          : `-- Replace column_name with the column the queries filter on\nCREATE INDEX CONCURRENTLY ON ${name} (column_name);`,
      })
    }
    if (t.dead >= 10_000 && t.dead / Math.max(1, t.live) >= 0.2) {
      add({
        id: `dead-${name}`, severity: t.dead / Math.max(1, t.live) >= 1 ? 'high' : 'medium', area: 'maintenance',
        title: `${name} carries many dead rows`,
        why: 'An UPDATE or DELETE leaves the old row behind until VACUUM removes it. While they pile up, every scan reads them too and the table and its indexes grow (bloat).',
        evidence: [
          `${count(t.dead)} dead rows against ${count(t.live)} live ones (${pct(t.dead / Math.max(1, t.live))}).`,
          t.lastVacuum ? `Last vacuumed ${t.lastVacuum}.` : 'Never vacuumed since the statistics were reset.',
        ],
        fix: 'Vacuum it now, and make autovacuum visit it sooner: a lower scale factor for this table. If dead rows keep coming back, look for long or forgotten transactions, which stop VACUUM from removing them.',
        sql: `VACUUM (ANALYZE, VERBOSE) ${name};\nALTER TABLE ${name} SET (autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.01);`,
      })
    }
    if (t.live >= 10_000 && (!t.lastAnalyze || t.modSinceAnalyze / Math.max(1, t.live) >= 0.5)) {
      add({
        id: `analyze-${name}`, severity: 'low', area: 'maintenance',
        title: `${name} has stale statistics`,
        why: 'The planner chooses how to run a query from the statistics ANALYZE keeps. When they are old it guesses row counts wrongly and can pick a slow plan.',
        evidence: [t.lastAnalyze
          ? `${count(t.modSinceAnalyze)} rows changed since it was last analyzed (${t.lastAnalyze}), on ${count(t.live)} rows.`
          : `Never analyzed since the statistics were reset; ${count(t.live)} rows.`],
        fix: 'Analyze it now; autovacuum normally does this, so if it keeps falling behind lower autovacuum_analyze_scale_factor for the table.',
        sql: `ANALYZE ${name};`,
      })
    }
  }

  /* Indexes */
  const longEnough = days >= 7
  const byDef = new Map<string, PgIndex[]>()
  for (const i of f.indexes) {
    const key = `${i.schema}.${i.table}|${i.def.replace(/^CREATE (UNIQUE )?INDEX \S+ ON /i, 'CREATE $1INDEX ON ')}`
    byDef.set(key, [...(byDef.get(key) ?? []), i])
  }
  const dupes = new Map<PgIndex, PgIndex>()
  for (const group of byDef.values()) {
    if (group.length < 2) continue
    const keep = group.find((i) => i.primary || i.constraint) ?? [...group].sort((a, b) => b.scans - a.scans || a.name.localeCompare(b.name))[0]!
    for (const i of group) if (i !== keep && !i.primary && !i.constraint) dupes.set(i, keep)
  }
  const unused = f.indexes.filter((i) => i.scans === 0 && !i.unique && !i.primary && !i.constraint && i.bytes >= 1024 ** 2 && !dupes.has(i))
  for (const i of unused.slice(0, 15)) {
    const name = qualified(i.schema, i.name)
    add({
      id: `unused-${name}`, severity: longEnough && i.bytes >= 1024 ** 3 ? 'medium' : 'low', area: 'indexes',
      title: `${name} is never used`,
      why: 'Every INSERT and most UPDATEs write to every index of a table, and the index takes memory the cache could use. An index no query reads is cost with no benefit.',
      evidence: [`No scans of this ${bytes(i.bytes)} index on ${qualified(i.schema, i.table)}. ${counting}`,
        ...(longEnough ? [] : ['That is less than a week, which may miss monthly or rare jobs.'])],
      fix: 'Drop it if no rare job or replica relies on it (each replica keeps its own counts). DROP INDEX CONCURRENTLY does not block writes; keep the definition to put it back.',
      sql: `-- ${i.def}\nDROP INDEX CONCURRENTLY ${name};`,
    })
  }
  for (const [i, keep] of dupes) {
    const name = qualified(i.schema, i.name)
    add({
      id: `dup-${name}`, severity: 'medium', area: 'indexes',
      title: `${name} duplicates ${qi(keep.name)}`,
      why: 'Two indexes with the same definition cost twice on every write and in memory, and the planner only ever needs one.',
      evidence: [`${i.def}`, `${bytes(i.bytes)}, ${count(i.scans)} scans; ${qi(keep.name)} has ${count(keep.scans)}.`],
      fix: `Drop ${name} and keep ${qi(keep.name)}.`,
      sql: `DROP INDEX CONCURRENTLY ${name};`,
    })
  }
  for (const k of f.foreignKeys.slice(0, 15)) {
    add({
      id: `fk-${k.table}-${k.name}`, severity: 'low', area: 'indexes',
      title: `Foreign key ${qi(k.name)} on ${k.table} has no index`,
      why: `Deleting or changing a row in ${k.references} has to find the rows that point at it; without an index on ${k.table} (${k.columns}) each one is a full scan, and joins on that key are slow too.`,
      evidence: [`${k.table} (${k.columns}) references ${k.references}, and no index starts with those columns.`],
      fix: 'Index the referencing columns.',
      sql: `CREATE INDEX CONCURRENTLY ON ${k.table} (${k.columns});`,
    })
  }

  /* Maintenance and safety */
  if (o.xidAge >= 500_000_000) {
    add({
      id: 'xid-age', severity: o.xidAge >= 1_200_000_000 ? 'high' : 'medium', area: 'maintenance',
      title: 'Transaction IDs are getting old',
      why: 'Postgres must freeze old rows before about two billion transactions pass, or it stops accepting writes to protect the data. Autovacuum does it, unless it is held back or too slow, and an emergency freeze of a big table is itself very heavy.',
      evidence: [`The oldest unfrozen transaction in this database is ${count(o.xidAge)} transactions old (autovacuum forces a freeze at 200M by default).`],
      fix: 'Make sure nothing holds an old snapshot (long transactions, abandoned replication slots, prepared transactions), then let autovacuum catch up or run VACUUM (FREEZE) on the largest tables in a quiet hour.',
      sql: "SELECT relname, age(relfrozenxid) FROM pg_class WHERE relkind = 'r' ORDER BY age(relfrozenxid) DESC LIMIT 10;",
    })
  }
  for (const sl of f.slots) {
    if (sl.active || sl.retainedBytes < 1024 ** 3) continue
    add({
      id: `slot-${sl.name}`, severity: sl.retainedBytes >= 20 * 1024 ** 3 ? 'high' : 'medium', area: 'maintenance',
      title: `Replication slot ${sl.name} is not being read`,
      why: 'A slot keeps every WAL file its reader has not had yet. When nobody reads it, WAL piles up until the disk is full, and a logical slot also keeps VACUUM from cleaning the catalogs.',
      evidence: [`Inactive, holding ${bytes(sl.retainedBytes)} of WAL.`],
      fix: 'Reconnect whatever reads it (a replica, a subscription, a CDC tool), or drop the slot if it is no longer used.',
      sql: `SELECT pg_drop_replication_slot('${sl.name.replace(/'/g, "''")}');`,
    })
  }
  if (f.checkpoints && f.checkpoints.timed + f.checkpoints.requested >= 20 && f.checkpoints.requested > f.checkpoints.timed) {
    const wal = set('max_wal_size')
    add({
      id: 'checkpoints', severity: 'medium', area: 'setup',
      title: 'Checkpoints come too often',
      why: 'A checkpoint writes every changed page to disk. When WAL fills max_wal_size before checkpoint_timeout, checkpoints are forced early and close together, and each one causes a burst of writes and a flood of full-page images in WAL.',
      evidence: [`${f.checkpoints.requested} checkpoints were forced by WAL volume against ${f.checkpoints.timed} on schedule.`,
        wal ? `max_wal_size is ${bytes(settingBytes(wal))}.` : ''].filter(Boolean),
      fix: 'Raise max_wal_size so checkpoints happen on the timer (it takes effect on reload).',
      sql: "ALTER SYSTEM SET max_wal_size = '8GB';\nSELECT pg_reload_conf();",
    })
  }
  if (set('autovacuum')?.setting === 'off') {
    add({
      id: 'autovacuum-off', severity: 'high', area: 'setup', title: 'Autovacuum is off',
      why: 'Without autovacuum dead rows are never removed and statistics are never refreshed unless someone does it by hand: tables bloat, plans go bad, and transaction IDs are not frozen.',
      evidence: ['autovacuum = off'], fix: 'Turn it on.', sql: "ALTER SYSTEM SET autovacuum = on;\nSELECT pg_reload_conf();",
    })
  }
  const rpc = set('random_page_cost')
  if (rpc && n(rpc.setting) >= 4 && rpc.source === 'default') {
    add({
      id: 'random-page-cost', severity: 'low', area: 'setup', title: 'The planner assumes spinning disks',
      why: 'random_page_cost 4 tells the planner a random read costs four times a sequential one, as on a hard disk. On SSDs and cloud disks that makes it avoid indexes it should use.',
      evidence: [`random_page_cost is ${rpc.setting} (default).`],
      fix: 'On SSD or network storage, set it to 1.1.',
      sql: "ALTER SYSTEM SET random_page_cost = 1.1;\nSELECT pg_reload_conf();",
    })
  }
  if (set('track_io_timing')?.setting === 'off') {
    add({
      id: 'track-io-timing', severity: 'low', area: 'setup', title: 'Time spent reading from disk is not measured',
      why: 'With track_io_timing on, plans and pg_stat_statements say how much of a query’s time was waiting on the disk, which tells a slow disk from a slow plan. Its own cost is small on modern systems.',
      evidence: ['track_io_timing = off'], fix: 'Turn it on.', sql: "ALTER SYSTEM SET track_io_timing = on;\nSELECT pg_reload_conf();",
    })
  }
  if (n(set('log_min_duration_statement')?.setting ?? '-1') < 0) {
    add({
      id: 'slow-log', severity: 'low', area: 'setup', title: 'Slow statements are not logged',
      why: 'pg_stat_statements says which queries are heavy on average; the log keeps each slow run with its values, which is what reproduces it.',
      evidence: ['log_min_duration_statement = -1 (off)'],
      fix: 'Log statements slower than, say, one second.', sql: "ALTER SYSTEM SET log_min_duration_statement = '1s';\nSELECT pg_reload_conf();",
    })
  }

  /* PgBouncer */
  out.push(...analyzePooler(f, { used, idle, usable }))

  out.sort((a, b) => ORDER[a.severity] - ORDER[b.severity] || AREA_ORDER.indexOf(a.area) - AREA_ORDER.indexOf(b.area))
  return {
    findings: out, countingS, cacheHit,
    connections: { used, max: o.maxConnections, idle, idleInTx: idleInTxCount, active, waitingOnLock: lockWait },
  }
}

const AREA_ORDER: Area[] = ['connections', 'pooler', 'queries', 'memory', 'tables', 'indexes', 'maintenance', 'setup']

/** The recorded statements that name table (as FROM, JOIN, UPDATE or INTO). */
export function statementsOn(statements: PgStatement[], table: string): PgStatement[] {
  const t = table.toLowerCase().replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const re = new RegExp(`\\b(from|join|update|into)\\s+(?:"?[a-z0-9_]+"?\\.)?"?${t}"?(?![a-z0-9_])`, 'i')
  // The ones that run often first: those are what an index would serve.
  return statements.filter((q) => re.test(q.query)).sort((a, b) => b.calls - a.calls)
}

/**
 * The columns the statements compare with a value in their WHERE: the
 * likeliest index. A guess from the text, so the fix says to check it with
 * Explain first; nothing is built from it without the person running it.
 */
export function filterColumns(statements: PgStatement[]): string[] {
  const seen = new Map<string, number>()
  for (const q of statements) {
    const where = /\bwhere\b([\s\S]*?)(\b(group|order|limit|returning|offset|for)\b|$)/i.exec(q.query)
    if (!where) continue
    const re = /(?:^|[\s(,])(?:"?[a-z_][a-z0-9_]*"?\.)?"?([a-z_][a-z0-9_]*)"?\s*(?:=|<=|>=|<|>|\bin\b|=\s*any)\s*(?:\(|\$\d+|'|\d)/gi
    for (const m of (where[1] ?? '').matchAll(re)) {
      const c = (m[1] ?? '').toLowerCase()
      if (['and', 'or', 'not', 'null', 'true', 'false'].includes(c)) continue
      seen.set(c, (seen.get(c) ?? 0) + q.calls)
    }
  }
  return [...seen.entries()].sort((a, b) => b[1] - a[1]).slice(0, 2).map(([c]) => c)
}

export function oneLine(q: string, max: number): string {
  const t = q.replace(/\s+/g, ' ').trim()
  return t.length > max ? `${t.slice(0, max - 1)}…` : t
}

/* ── PgBouncer ─────────────────────────────────────────────────────────── */

/** A SHOW answer as objects, column name to cell. */
export function poolerRows(t: PoolerTable | undefined): Record<string, string>[] {
  if (!t) return []
  return t.rows.map((r) => Object.fromEntries(t.cols.map((c, i) => [c, r[i] ?? ''])))
}

export interface PoolSummary {
  database: string
  user: string
  mode: string
  clActive: number
  clWaiting: number
  svActive: number
  svIdle: number
  svUsed: number
  /** Longest a client in the queue has waited, seconds. */
  maxWaitS: number
  /** The pool's size, from SHOW DATABASES or the settings. */
  size: number
}

export function poolSummaries(p: PoolerStats): PoolSummary[] {
  const dbs = poolerRows(p.databases)
  const def = n(p.settings?.default_pool_size ?? '20')
  return poolerRows(p.pools).filter((r) => r.database !== 'pgbouncer').map((r) => {
    const database = r.database ?? ''
    const d = dbs.find((x) => x.name === database)
    const size = d && n(d.pool_size) > 0 ? n(d.pool_size) : n(p.databasePools?.[database]?.pool_size ?? '') || def
    return {
      database, user: r.user ?? '', mode: r.pool_mode ?? p.settings?.pool_mode ?? '',
      clActive: n(r.cl_active), clWaiting: n(r.cl_waiting), svActive: n(r.sv_active), svIdle: n(r.sv_idle), svUsed: n(r.sv_used),
      maxWaitS: n(r.maxwait) + n(r.maxwait_us) / 1e6, size,
    }
  })
}

function versionOf(v: string | undefined): number {
  const m = /(\d+)\.(\d+)/.exec(v ?? '')
  return m ? Number(m[1]) * 100 + Number(m[2]) : 0
}

export function analyzePooler(f: PgPerfFacts, pg: { used: number; idle: number; usable: number }): Finding[] {
  const out: Finding[] = []
  const p = f.pooler
  if (p === undefined) return out
  if (p === null) {
    if (f.viaPooler) {
      out.push({
        id: 'pooler-unread', severity: 'low', area: 'pooler', title: 'PgBouncer’s own numbers need DBA access',
        why: 'This database is reached through PgBouncer. Its pools, queue and limits are in PgBouncer’s admin console, which the agent reads only for a DBA session.',
        evidence: ['This session is below DBA.'], fix: 'Open the database at DBA to include PgBouncer in the analysis.',
      })
    }
    return out
  }
  if (!p.found) {
    if (pg.used >= 100 || (pg.idle >= 30 && pg.idle / Math.max(1, pg.used) >= 0.6)) {
      out.push({
        id: 'pooler-none', severity: 'medium', area: 'pooler', title: 'No connection pooler in front of Postgres',
        why: 'Each client connection is a Postgres process. With many clients, most of them idle, a pooler such as PgBouncer lets them share a small number of server connections, which saves memory and keeps Postgres from thrashing between hundreds of processes.',
        evidence: [`${pg.used} connections are open, ${pg.idle} of them idle; no PgBouncer was found on this server.`],
        fix: 'Install PgBouncer on this server with pool_mode = transaction and a default_pool_size of a few times the CPU count, and point the applications at its port. Argus can send this database through it (the database’s settings).',
      })
    }
    return out
  }
  const st = p.settings ?? {}
  const mode = (st.pool_mode ?? 'session').toLowerCase()
  const maxClient = n(st.max_client_conn ?? '100')
  const where = `PgBouncer on port ${p.port ?? '?'}`
  if (!p.through) {
    out.push({
      id: 'pooler-bypassed', severity: 'low', area: 'pooler', title: `${where} is not used by this entry`,
      why: 'A PgBouncer runs on this server, but this database’s entry in Argus connects to Postgres directly, so the numbers below are PgBouncer’s, not this entry’s.',
      evidence: [`Found ${p.config ?? 'pgbouncer.ini'}.`],
      fix: 'If the applications reach this database through PgBouncer, set it as the pooler in the database’s settings so Argus sessions go the same way.',
    })
  }
  if (p.liveError) {
    out.push({
      id: 'pooler-console', severity: 'low', area: 'pooler', title: 'PgBouncer’s live numbers could not be read',
      why: 'Pools, waiting clients and wait times come from PgBouncer’s admin console. Without them only the settings in pgbouncer.ini are checked.',
      evidence: [p.liveError],
      fix: 'The agent signs in as the user pgbouncer through PgBouncer’s unix socket, as the account PgBouncer runs as; that needs unix_socket_dir set and PgBouncer running on this server, outside a container.',
    })
  }
  const pools = poolSummaries(p)
  const clients = pools.reduce((a, x) => a + x.clActive + x.clWaiting, 0)

  if (mode === 'session' && (clients >= 50 || maxClient >= 200 || pg.used >= 50)) {
    out.push({
      id: 'pooler-session-mode', severity: 'medium', area: 'pooler', title: 'PgBouncer pools whole sessions',
      why: 'In session mode a client keeps its server connection until it disconnects, so PgBouncer saves almost nothing: as many server connections as connected clients. Transaction mode hands the connection back after every transaction, so a few server connections serve many clients.',
      evidence: [`pool_mode = session; ${clients ? `${clients} clients connected now, ` : ''}max_client_conn ${maxClient}.`],
      fix: 'Switch to pool_mode = transaction, after checking the applications do not rely on session state between transactions: SET without LOCAL, advisory locks, LISTEN/NOTIFY, temporary tables or WITH HOLD cursors. Prepared statements work in transaction mode from PgBouncer 1.21 with max_prepared_statements set. Argus’s own sessions already work in transaction mode.',
    })
  }
  if (mode === 'statement') {
    out.push({
      id: 'pooler-statement-mode', severity: 'low', area: 'pooler', title: 'PgBouncer is in statement mode',
      why: 'Statement mode returns the server connection after every statement, so multi-statement transactions are refused. That only suits autocommit workloads.',
      evidence: ['pool_mode = statement'], fix: 'Use transaction mode unless every client runs single statements only.',
    })
  }
  const waiting = pools.filter((x) => x.clWaiting > 0 || x.maxWaitS >= 1)
  for (const x of waiting) {
    const saturated = x.svActive + x.svUsed >= x.size
    out.push({
      id: `pooler-wait-${x.database}-${x.user}`, severity: x.maxWaitS >= 5 || x.clWaiting >= 10 ? 'high' : 'medium', area: 'pooler',
      title: `Clients queue for a connection in pool ${x.database}/${x.user}`,
      why: 'A client that PgBouncer cannot give a server connection waits in a queue before its query even starts, so the application sees the wait as a slow query. It happens when every connection in the pool is busy: the pool is too small, or the queries and transactions in it hold their connections too long.',
      evidence: [`${x.clWaiting} waiting, the longest for ${x.maxWaitS.toFixed(1)} s; ${x.svActive} of ${x.size} server connections busy.`],
      fix: saturated
        ? `First shorten what holds the connections (the heavy queries and open transactions above). If Postgres has room (it is not CPU-bound), raise pool_size for ${x.database} in [databases] or default_pool_size, and set reserve_pool_size for bursts; keep the total under Postgres's max_connections.`
        : 'The pool is not full, so check max_db_connections and max_user_connections, which cap it below its size, and server_login_retry if Postgres is refusing new connections.',
    })
  }
  for (const r of poolerRows(p.stats).filter((r) => r.database !== 'pgbouncer')) {
    const waitMs = n(r.avg_wait_time) / 1000
    const queryMs = n(r.avg_query_time) / 1000
    if (waitMs >= 50 && waiting.every((w) => w.database !== r.database)) {
      out.push({
        id: `pooler-avgwait-${r.database}`, severity: waitMs >= 500 ? 'high' : 'medium', area: 'pooler',
        title: `Clients of ${r.database} wait for a server connection`,
        why: 'PgBouncer counts how long clients wait for a connection. Waiting at all means the pool was full at times, which adds directly to every query’s time.',
        evidence: [`On average ${ms(waitMs)} of waiting, against ${ms(queryMs)} per query.`],
        fix: `Shorten what holds connections, then raise pool_size for ${r.database} or add a reserve_pool_size.`,
      })
    }
  }
  // Could PgBouncer open more server connections than Postgres allows?
  if (pools.length > 0) {
    const maxDb = n(st.max_db_connections ?? '0')
    const perDb = new Map<string, number>()
    for (const x of pools) perDb.set(x.database, (perDb.get(x.database) ?? 0) + x.size)
    let possible = 0
    for (const v of perDb.values()) possible += maxDb > 0 ? Math.min(v, maxDb) : v
    possible += n(st.reserve_pool_size ?? '0') * perDb.size
    if (possible > pg.usable) {
      out.push({
        id: 'pooler-over-limit', severity: 'medium', area: 'pooler', title: 'PgBouncer may open more connections than Postgres allows',
        why: 'Each database and user pair is a pool of its own size. Together they can ask for more server connections than max_connections; at a busy moment the last ones are refused and their clients get errors instead of a wait.',
        evidence: [`${pools.length} pools could open up to ${possible} server connections; Postgres allows ${pg.usable} (besides superusers).`],
        fix: 'Set max_db_connections (and max_user_connections) so the total fits, or use fewer database users through PgBouncer. Raising max_connections costs memory on every connection.',
      })
    }
  }
  const current = pools.reduce((a, x) => a + x.clActive + x.clWaiting, 0)
  if (maxClient > 0 && current / maxClient >= 0.8) {
    out.push({
      id: 'pooler-max-client', severity: current >= maxClient ? 'high' : 'medium', area: 'pooler',
      title: 'Clients are close to PgBouncer’s max_client_conn',
      why: 'Past max_client_conn PgBouncer refuses new clients outright. Client connections are cheap for PgBouncer, unlike server connections.',
      evidence: [`${current} clients against max_client_conn ${maxClient}.`],
      fix: 'Raise max_client_conn (and the open files limit of PgBouncer’s service to match); it does not add server connections.',
    })
  }
  if (mode === 'transaction' && n(st.idle_transaction_timeout ?? '0') === 0) {
    out.push({
      id: 'pooler-idle-tx', severity: pg.used > 0 && f.activity.some((g) => g.state.startsWith('idle in transaction')) ? 'medium' : 'low', area: 'pooler',
      title: 'PgBouncer lets a transaction hold its connection forever',
      why: 'In transaction mode a client that opens a transaction and goes quiet keeps a server connection away from everyone else until it commits; a few of these empty the pool.',
      evidence: ['idle_transaction_timeout = 0 (off).'],
      fix: 'Set idle_transaction_timeout = 60 (seconds) under [pgbouncer] in pgbouncer.ini, then reload PgBouncer (RELOAD, or systemctl reload pgbouncer).',
    })
  }
  const ver = versionOf(p.version)
  if (mode === 'transaction' && ver >= 121 && n(st.max_prepared_statements ?? '0') === 0) {
    out.push({
      id: 'pooler-prepared', severity: 'low', area: 'pooler', title: 'Prepared statements are not tracked by PgBouncer',
      why: 'Drivers that prepare statements (JDBC, pgx, asyncpg, Npgsql) fail behind transaction pooling with "prepared statement does not exist" unless PgBouncer tracks them, so applications often turn preparing off and pay to plan every query again.',
      evidence: [`${p.version}; max_prepared_statements = 0.`],
      fix: 'Set max_prepared_statements (for example 200) in pgbouncer.ini and RELOAD.',
    })
  } else if (mode === 'transaction' && ver > 0 && ver < 121) {
    out.push({
      id: 'pooler-old', severity: 'low', area: 'pooler', title: 'This PgBouncer cannot carry prepared statements',
      why: 'Before 1.21 PgBouncer could not keep prepared statements across transaction pooling, so applications have to turn them off and plan every query again.',
      evidence: [p.version ?? ''], fix: 'Upgrade PgBouncer to 1.21 or later and set max_prepared_statements.',
    })
  }
  const idleServers = pools.reduce((a, x) => a + x.svIdle, 0)
  const activeServers = pools.reduce((a, x) => a + x.svActive, 0)
  if (idleServers >= 50 && activeServers * 5 < idleServers) {
    out.push({
      id: 'pooler-oversized', severity: 'low', area: 'pooler', title: 'PgBouncer keeps many server connections idle',
      why: 'Idle server connections still use Postgres memory. Pools larger than the work needs give nothing back.',
      evidence: [`${idleServers} idle server connections against ${activeServers} busy.`],
      fix: 'Lower default_pool_size or min_pool_size, or a shorter server_idle_timeout so unused ones are closed.',
    })
  }
  return out
}
