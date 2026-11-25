/*
Why a MySQL or MariaDB database is heavy, and what to change: the analysis
behind the Performance panel for the mysql engine. Pure, so it is tested
without a server. The facts come from the agent's mysql.perf (read as the
server's administrator, DBA only; see internal/agent/mysql_perf.go), because
the person's own user has, and must have, no access to performance_schema.

The shape of a finding, the formatting and the rule of staying quiet on
small numbers are shared with the Postgres analysis (lib/pgPerformance).
*/

import { bytes, count, duration, ms, oneLine, type Finding, type PoolerTable, type Severity } from '~/lib/pgPerformance'

/** What the agent answers (tunnel.MySQLPerf). */
export interface MySQLPerf {
  version: string
  status: Record<string, string>
  variables: Record<string, string>
  digests?: PoolerTable
  tables?: PoolerTable
  indexes?: PoolerTable
  indexUsage?: PoolerTable
  trx?: PoolerTable
  processes?: PoolerTable
  missing?: string[]
}

export interface MyDigest {
  text: string
  calls: number
  totalMs: number
  avgMs: number
  lockMs: number
  examined: number
  sent: number
  affected: number
  noIndex: number
  noGoodIndex: number
  tmpDisk: number
  tmp: number
  sortMerge: number
  fullJoin: number
  share: number
}

export interface MyTable { name: string; engine: string; rows: number; data: number; index: number; free: number; primary: boolean }
export interface MyIndex { table: string; name: string; unique: boolean; columns: string[] }

export interface MyReport {
  findings: Finding[]
  countingS: number
  bufferHit: number | null
  connections: { used: number; max: number; running: number; sleeping: number }
  digests: MyDigest[]
  mariadb: boolean
}

const n = (s: string | undefined) => {
  const v = Number(s ?? '')
  return Number.isFinite(v) ? v : 0
}
/** performance_schema's timers are picoseconds. */
const psToMs = (s: string | undefined) => n(s) / 1e9
const on = (v: string | undefined) => ['on', '1', 'yes', 'true'].includes((v ?? '').toLowerCase())
const pct = (x: number) => `${(x * 100).toFixed(x < 0.1 ? 1 : 0)}%`
export const myIdent = (s: string) => `\`${s.replace(/`/g, '``')}\``

function rows(t: PoolerTable | undefined): string[][] {
  return t?.rows ?? []
}

export function digests(p: MySQLPerf): MyDigest[] {
  return rows(p.digests).map((r) => ({
    text: r[0] ?? '', calls: n(r[1]), totalMs: psToMs(r[2]), avgMs: psToMs(r[3]), lockMs: psToMs(r[4]), examined: n(r[5]),
    sent: n(r[6]), affected: n(r[7]), noIndex: n(r[8]), noGoodIndex: n(r[9]), tmpDisk: n(r[10]), tmp: n(r[11]),
    sortMerge: n(r[12]), fullJoin: n(r[13]), share: n(r[14]),
  }))
}

export function tables(p: MySQLPerf): MyTable[] {
  return rows(p.tables).map((r) => ({
    name: r[0] ?? '', engine: r[1] ?? '', rows: n(r[2]), data: n(r[3]), index: n(r[4]), free: n(r[5]), primary: n(r[6]) > 0,
  }))
}

export function indexes(p: MySQLPerf): MyIndex[] {
  const by = new Map<string, MyIndex>()
  for (const r of rows(p.indexes)) {
    const key = `${r[0]}\u0000${r[1]}`
    let i = by.get(key)
    if (!i) {
      i = { table: r[0] ?? '', name: r[1] ?? '', unique: r[2] === '0', columns: [] }
      by.set(key, i)
    }
    i.columns.push(`${r[4] ?? ''}${r[5] ? `(${r[5]})` : ''}`)
  }
  return [...by.values()]
}

/** One statement digest: why it costs what it costs. */
export function diagnoseDigest(d: MyDigest): { reasons: string[]; fixes: string[] } {
  const reasons: string[] = []
  const fixes: string[] = []
  if (d.share >= 0.2) reasons.push(`It alone takes ${pct(d.share)} of all the time statements spend in this database.`)
  if (d.avgMs >= 500) reasons.push(`Each run takes ${ms(d.avgMs)} on average.`)
  if (d.noIndex > 0 && d.noIndex / Math.max(1, d.calls) >= 0.5) {
    reasons.push(`${d.noIndex === d.calls ? 'Every run' : `${count(d.noIndex)} of ${count(d.calls)} runs`} read a table without using any index (a full table scan).`)
    fixes.push('Add an index on the columns in its WHERE or JOIN; EXPLAIN shows type ALL for the table that is scanned.')
  } else if (d.noGoodIndex > 0) {
    reasons.push('MySQL found no good index for it and fell back to a poor one.')
    fixes.push('A composite index that matches its WHERE columns in order helps; check it with EXPLAIN.')
  }
  const perSent = d.sent > 0 ? d.examined / d.sent : d.examined / Math.max(1, d.calls)
  if (d.examined >= 100_000 && perSent >= 100) {
    reasons.push(`It examines about ${count(perSent)} rows for each row it returns (${count(d.examined)} examined in all).`)
    if (!fixes.length) fixes.push('Most of the rows it reads are thrown away: an index that narrows the WHERE reads only the ones needed.')
  }
  if (d.tmpDisk > 0) {
    reasons.push(`It built ${count(d.tmpDisk)} temporary tables on disk (for GROUP BY, DISTINCT, UNION or ORDER BY on another table).`)
    fixes.push('An index in the GROUP BY or ORDER BY order avoids the temporary table; TEXT and BLOB columns in it force it to disk whatever tmp_table_size is.')
  }
  if (d.sortMerge > 0) reasons.push(`Its sorts did not fit in sort_buffer_size and merged on disk ${count(d.sortMerge)} times.`)
  if (d.fullJoin > 0) {
    reasons.push(`It joined ${count(d.fullJoin)} times with no index on the joined table.`)
    fixes.push('Index the join columns of the second table.')
  }
  if (d.calls >= 100_000 && d.avgMs < 10 && d.share >= 0.1) {
    reasons.push(`It is cheap (${ms(d.avgMs)}) but runs very often: ${count(d.calls)} times.`)
    fixes.push('This is usually a loop in the application running one query per row (N+1). Fetch the rows in one query, or cache the answer.')
  }
  if (d.lockMs >= 0.2 * d.totalMs && d.lockMs >= 1000) reasons.push(`${pct(d.lockMs / Math.max(1, d.totalMs))} of its time was spent waiting for locks.`)
  if (reasons.length === 0 && d.share >= 0.05) reasons.push(`It takes ${pct(d.share)} of the statement time here, over ${count(d.calls)} runs.`)
  return { reasons, fixes }
}

/** EXPLAIN of a digest: values are ? in a digest and have to be put back. */
export function explainDigest(d: MyDigest): string {
  const text = d.text.trim().replace(/;\s*$/, '')
  return /\?/.test(text) ? `-- Put real values in place of each ? before running.\nEXPLAIN ${text}` : `EXPLAIN ${text}`
}

/** The tables a digest names, and the columns it compares with a value. */
export function digestTables(text: string): string[] {
  const out = new Set<string>()
  for (const m of text.matchAll(/\b(?:FROM|JOIN|UPDATE|INTO)\s+(?:`[^`]+`\s*\.\s*)?`?([A-Za-z0-9_$]+)`?/gi)) out.add(m[1] ?? '')
  out.delete('')
  return [...out]
}
export function digestColumns(text: string): string[] {
  const where = /\bWHERE\b([\s\S]*?)(\b(GROUP|ORDER|LIMIT|HAVING|FOR)\b|$)/i.exec(text)
  if (!where) return []
  const seen: string[] = []
  for (const m of (where[1] ?? '').matchAll(/(?:^|[\s(,])(?:`[^`]+`\s*\.\s*)?`?([A-Za-z_][A-Za-z0-9_]*)`?\s*(?:=|<=|>=|<|>|\bIN\b|\bLIKE\b)\s*(?:\(|\?|'|\d)/gi)) {
    const c = m[1] ?? ''
    if (!['AND', 'OR', 'NOT', 'NULL'].includes(c.toUpperCase()) && !seen.includes(c)) seen.push(c)
  }
  return seen.slice(0, 2)
}

const ORDER: Record<Severity, number> = { high: 0, medium: 1, low: 2 }

export function analyzeMySQL(p: MySQLPerf): MyReport {
  const out: Finding[] = []
  const add = (f: Finding) => out.push(f)
  const st = p.status
  const v = p.variables
  const mariadb = /mariadb/i.test(p.version)
  const countingS = n(st.uptime_since_flush_status) || n(st.uptime)
  const days = countingS / 86400
  const counting = `The server has been counting for ${duration(countingS)}.`
  const ds = digests(p)
  const ts = tables(p)
  const ix = indexes(p)
  const dataBytes = ts.reduce((a, t) => a + t.data + t.index, 0)

  /* Connections */
  const max = n(v.max_connections)
  const used = n(st.threads_connected)
  const running = n(st.threads_running)
  const procs = rows(p.processes)
  const sleeping = procs.filter((r) => r[1] === 'Sleep').reduce((a, r) => a + n(r[3]), 0)
  if (max > 0 && used / max >= 0.8) {
    add({
      id: 'my-conn-near-limit', severity: used / max >= 0.95 ? 'high' : 'medium', area: 'connections',
      title: 'Connections are close to the limit',
      why: 'Past max_connections MySQL refuses new clients with "Too many connections". Each connection also has its own buffers, so many of them take memory from the buffer pool.',
      evidence: [`${used} of ${max} connections are open (${n(st.max_used_connections)} at most since the start).`],
      fix: 'Have the applications share fewer connections (a smaller pool each, or ProxySQL in front) rather than raising max_connections.',
    })
  }
  if (n(st.connection_errors_max_connections) > 0) {
    add({
      id: 'my-conn-refused', severity: 'high', area: 'connections', title: 'Clients have been refused for too many connections',
      why: 'When every connection is taken, MySQL turns new clients away and the application sees errors, not a slow query.',
      evidence: [`${count(n(st.connection_errors_max_connections))} connections refused; at most ${n(st.max_used_connections)} of ${max} were in use. ${counting}`],
      fix: 'Find what holds connections (idle pools, long queries, open transactions) and shrink it; raise max_connections only with memory to spare.',
    })
  }
  if (sleeping >= 50 && sleeping / Math.max(1, used) >= 0.6) {
    const wait = n(v.wait_timeout)
    add({
      id: 'my-conn-idle', severity: 'medium', area: 'connections', title: 'Most connections sit idle',
      why: 'An idle connection still holds a thread and its buffers. Applications that each keep a large pool open many such connections.',
      evidence: [`${sleeping} of ${used} connections are sleeping.`, `wait_timeout is ${duration(wait)}.`],
      fix: 'Shrink the applications’ pools or put ProxySQL in front; a shorter wait_timeout closes ones that are forgotten.',
      sql: wait >= 3600 ? "SET PERSIST wait_timeout = 600;  -- MariaDB: SET GLOBAL, and in the server's config file" : undefined,
    })
  }
  const connections = n(st.connections)
  if (connections >= 10_000 && n(st.threads_created) / connections >= 0.1) {
    add({
      id: 'my-thread-cache', severity: 'low', area: 'setup', title: 'A new thread is made for most connections',
      why: 'Creating a thread for every connection costs time on each connect; the thread cache keeps finished ones for reuse.',
      evidence: [`${count(n(st.threads_created))} threads created for ${count(connections)} connections; thread_cache_size is ${v.thread_cache_size ?? '?'}.`],
      fix: 'Raise thread_cache_size (for example to 100).', sql: 'SET GLOBAL thread_cache_size = 100;',
    })
  }

  /* Transactions and locks */
  const trx = rows(p.trx)
  const longest = Math.max(0, ...trx.map((r) => n(r[2])))
  if (longest >= 60) {
    add({
      id: 'my-long-trx', severity: longest >= 600 ? 'high' : 'medium', area: 'connections', title: 'A transaction has been open for a long time',
      why: 'An open InnoDB transaction keeps its row locks, so other writes wait behind it, and keeps old row versions alive (the history list), which slows every read and grows the undo log.',
      evidence: [`${trx.length} transaction${trx.length === 1 ? '' : 's'} open longer than 5 seconds; the oldest for ${duration(longest)}${trx[0]?.[5] ? `, running ${oneLine(trx[0][5], 100)}` : ''}.`],
      fix: 'Find the client (Activity) and commit sooner: no transaction should wait on a user or a network call. A long report belongs on a replica.',
    })
  }
  const lockWaits = n(st.innodb_row_lock_waits)
  const lockAvg = n(st.innodb_row_lock_time_avg)
  if (lockWaits >= 100 && lockAvg >= 100) {
    add({
      id: 'my-row-locks', severity: lockAvg >= 1000 ? 'high' : 'medium', area: 'queries', title: 'Writes wait on each other’s row locks',
      why: 'A statement that waits for a row lock does nothing until the transaction holding it ends; long transactions and updates without an index (which lock every row they scan) cause most of it.',
      evidence: [`${count(lockWaits)} row lock waits, ${ms(lockAvg)} each on average. ${counting}`],
      fix: 'Keep transactions short, update rows through an index so only those rows are locked, and touch rows in the same order everywhere.',
    })
  }
  if (n(st.innodb_deadlocks) > 0 && n(st.innodb_deadlocks) / Math.max(days, 1) >= 1) {
    add({
      id: 'my-deadlocks', severity: 'medium', area: 'queries', title: 'Deadlocks happen',
      why: 'In a deadlock InnoDB rolls one transaction back; both lose their work so far and one has to retry.',
      evidence: [`${n(st.innodb_deadlocks)} deadlocks. ${counting}`],
      fix: 'Update rows in a consistent order and keep transactions short. SHOW ENGINE INNODB STATUS shows the latest deadlock.',
      sql: 'SHOW ENGINE INNODB STATUS;',
    })
  }

  /* Memory */
  const requests = n(st.innodb_buffer_pool_read_requests)
  const reads = n(st.innodb_buffer_pool_reads)
  const bufferHit = requests > 0 ? 1 - reads / requests : null
  const pool = n(v.innodb_buffer_pool_size)
  if (bufferHit !== null && requests >= 1_000_000 && bufferHit < 0.99) {
    add({
      id: 'my-buffer-hit', severity: bufferHit < 0.95 ? 'high' : 'medium', area: 'memory', title: 'Data is read from disk too often',
      why: 'InnoDB serves pages from its buffer pool; one it has to read from disk costs far more. Below about 99%, queries spend their time waiting on the disk.',
      evidence: [`${pct(bufferHit)} of page reads were served from memory (${count(reads)} from disk). ${counting}`,
        `innodb_buffer_pool_size is ${bytes(pool)}; this database's tables and indexes are ${bytes(dataBytes)}.`],
      fix: 'First make queries read less (the full scans below). Then give the buffer pool about 70% of the memory of a server that runs only MySQL.',
      sql: "SET GLOBAL innodb_buffer_pool_size = 8 * 1024 * 1024 * 1024;  -- about 70% of the server's memory; also set it in the config file",
    })
  } else if (pool > 0 && pool <= 128 * 1024 ** 2 && dataBytes > 1024 ** 3) {
    add({
      id: 'my-buffer-default', severity: 'medium', area: 'setup', title: 'The buffer pool is still at its default',
      why: 'MySQL ships with a 128 MB buffer pool so it starts anywhere; a database of several gigabytes needs more to keep its working set in memory.',
      evidence: [`innodb_buffer_pool_size is ${bytes(pool)}; this database alone is ${bytes(dataBytes)}.`],
      fix: 'Give the buffer pool about 70% of the memory of a server that runs only MySQL.',
      sql: "SET GLOBAL innodb_buffer_pool_size = 8 * 1024 * 1024 * 1024;  -- about 70% of the server's memory; also set it in the config file",
    })
  }
  if (n(st.innodb_buffer_pool_wait_free) > 0) {
    add({
      id: 'my-wait-free', severity: 'medium', area: 'memory', title: 'Queries waited for a free page in the buffer pool',
      why: 'When no clean page is free, a query has to wait for InnoDB to flush one to disk first.',
      evidence: [`Innodb_buffer_pool_wait_free = ${count(n(st.innodb_buffer_pool_wait_free))}.`],
      fix: 'A larger buffer pool, or a higher innodb_io_capacity on fast storage so pages are flushed sooner.',
    })
  }
  const tmp = n(st.created_tmp_tables)
  const tmpDisk = n(st.created_tmp_disk_tables)
  if (tmp >= 1000 && tmpDisk / tmp >= 0.25) {
    const limit = Math.min(n(v.tmp_table_size), n(v.max_heap_table_size))
    add({
      id: 'my-tmp-disk', severity: tmpDisk / tmp >= 0.5 ? 'medium' : 'low', area: 'memory', title: 'Temporary tables go to disk',
      why: 'GROUP BY, DISTINCT, UNION and some ORDER BYs build a temporary table; one larger than the in-memory limit, or with TEXT or BLOB columns, is written to disk.',
      evidence: [`${pct(tmpDisk / tmp)} of ${count(tmp)} temporary tables were made on disk.`,
        `The in-memory limit is ${bytes(limit)} (the smaller of tmp_table_size and max_heap_table_size).`],
      fix: 'Find the statements below that make them and index their GROUP BY or ORDER BY, or select fewer TEXT columns. Raising both limits together helps the rest.',
      sql: 'SET GLOBAL tmp_table_size = 64 * 1024 * 1024;\nSET GLOBAL max_heap_table_size = 64 * 1024 * 1024;',
    })
  }
  if (n(st.sort_merge_passes) / Math.max(days, 1 / 24) >= 1000) {
    add({
      id: 'my-sort-merge', severity: 'low', area: 'memory', title: 'Sorts spill to disk',
      why: 'A sort larger than sort_buffer_size is done in pieces on disk and merged.',
      evidence: [`${count(n(st.sort_merge_passes))} merge passes; sort_buffer_size is ${bytes(n(v.sort_buffer_size))}. ${counting}`],
      fix: 'An index in the ORDER BY order avoids the sort. Raise sort_buffer_size for the session that needs it, not globally: it is given to every sort.',
    })
  }
  if (on(v.query_cache_type) && n(v.query_cache_size) > 0) {
    add({
      id: 'my-query-cache', severity: 'low', area: 'setup', title: 'The query cache is on',
      why: 'Every write to a table empties its cached results under one global lock, so with writes the query cache costs more than it saves. MySQL 8 removed it.',
      evidence: [`query_cache_type = ${v.query_cache_type}, query_cache_size = ${bytes(n(v.query_cache_size))}.`],
      fix: 'Turn it off.', sql: 'SET GLOBAL query_cache_size = 0;\nSET GLOBAL query_cache_type = OFF;',
    })
  }
  if (n(st.innodb_log_waits) > 0) {
    add({
      id: 'my-log-waits', severity: 'medium', area: 'setup', title: 'Writes waited for the redo log buffer',
      why: 'A transaction that fills innodb_log_buffer_size has to wait for it to be written to disk before it can go on.',
      evidence: [`Innodb_log_waits = ${count(n(st.innodb_log_waits))}; innodb_log_buffer_size is ${bytes(n(v.innodb_log_buffer_size))}.`],
      fix: 'Raise innodb_log_buffer_size (for example to 64 MB; a restart is needed on older versions).',
    })
  }
  const opened = n(st.opened_tables)
  if (opened / Math.max(days, 1 / 24) >= 10_000 && n(v.table_open_cache) < 4000) {
    add({
      id: 'my-table-cache', severity: 'low', area: 'setup', title: 'Tables are opened over and over',
      why: 'Each table a query uses must be open; when table_open_cache is too small they are closed and reopened all the time.',
      evidence: [`${count(opened)} tables opened (${count(opened / Math.max(days, 1 / 24))} a day); table_open_cache is ${v.table_open_cache}.`],
      fix: 'Raise table_open_cache.', sql: 'SET GLOBAL table_open_cache = 4000;',
    })
  }
  if (!on(v.slow_query_log)) {
    add({
      id: 'my-slow-log', severity: 'low', area: 'setup', title: 'Slow statements are not logged',
      why: 'The slow query log keeps each slow run with its values, which is what reproduces it.',
      evidence: ['slow_query_log = OFF'], fix: 'Turn it on and log statements slower than one second.',
      sql: 'SET GLOBAL slow_query_log = ON;\nSET GLOBAL long_query_time = 1;',
    })
  }

  /* Statements */
  if (!on(v.performance_schema)) {
    add({
      id: 'my-no-ps', severity: 'medium', area: 'setup', title: 'Which queries are heavy cannot be seen',
      why: 'performance_schema counts the time, rows examined and full scans of every statement. Without it the server keeps no history of which queries cost the most.',
      evidence: ['performance_schema = OFF'],
      fix: `Set performance_schema = ON in the server's config file and restart ${mariadb ? 'MariaDB' : 'MySQL'}. It costs a little memory and a few percent of CPU.`,
    })
  }
  const scanned = new Map<string, MyDigest[]>()
  for (const d of ds.slice(0, 10)) {
    if (d.calls >= 5 && (d.noIndex > 0 || d.fullJoin > 0)) {
      for (const t of digestTables(d.text)) scanned.set(t, [...(scanned.get(t) ?? []), d])
    }
    const dx = diagnoseDigest(d)
    const heavy = d.calls >= 5 && (d.share >= 0.2 || (d.avgMs >= 1000 && d.calls >= 10) || (d.share >= 0.1 && dx.fixes.length > 0))
    if (!heavy) continue
    const cols = digestColumns(d.text)
    const tbl = digestTables(d.text)[0]
    if (d.noIndex > 0 && cols.length && tbl) dx.fixes.unshift(`An index on ${tbl} (${cols.join(', ')}) is the likely fix; confirm with EXPLAIN.`)
    add({
      id: `my-stmt-${oneLine(d.text, 60)}`, severity: d.share >= 0.3 || d.avgMs >= 5000 ? 'high' : 'medium', area: 'queries',
      title: `A heavy query: ${oneLine(d.text, 90)}`, why: dx.reasons.join(' '),
      evidence: [`${count(d.calls)} calls, ${ms(d.totalMs)} in total, ${ms(d.avgMs)} on average, ${count(d.examined)} rows examined, ${count(d.sent)} sent.`],
      fix: dx.fixes.join(' ') || 'EXPLAIN it to see where the time goes.', sql: explainDigest(d),
    })
  }
  for (const [t, list] of scanned) {
    const tb = ts.find((x) => x.name === t)
    if (tb && tb.rows < 10_000) continue
    const cols = [...new Set(list.flatMap((d) => digestColumns(d.text)))].slice(0, 2)
    add({
      id: `my-scan-${t}`, severity: 'medium', area: 'tables', title: `${t} is read without an index`,
      why: 'A full table scan reads every row. On a large table, a query that wants a few rows but has no index to find them reads them all, every time.',
      evidence: [`${tb ? `${count(tb.rows)} rows, ${bytes(tb.data + tb.index)}. ` : ''}Read without an index by:`,
        ...list.slice(0, 3).map((d) => `${count(d.calls)} × ${ms(d.avgMs)}: ${oneLine(d.text, 120)}`)],
      fix: cols.length
        ? `The queries filter on ${cols.join(', ')}. EXPLAIN one; if it shows type ALL for ${t}, an index on ${cols.length > 1 ? 'those columns' : 'that column'} turns the scan into a lookup. On InnoDB, ALGORITHM=INPLACE, LOCK=NONE builds it while writes go on.`
        : 'EXPLAIN the queries above and index the columns they filter or join on.',
      sql: `ALTER TABLE ${myIdent(t)} ADD INDEX (${cols.length ? cols.map(myIdent).join(', ') : '`column_name`'}), ALGORITHM=INPLACE, LOCK=NONE;`,
    })
  }

  /* Tables */
  for (const t of ts) {
    if (t.engine.toLowerCase() === 'innodb' && !t.primary && t.rows >= 1000) {
      add({
        id: `my-nopk-${t.name}`, severity: 'medium', area: 'tables', title: `${t.name} has no primary key`,
        why: 'InnoDB stores a table in primary key order; without one it uses a hidden key shared across all such tables, and row-based replication has to scan the whole table on the replica for every changed row.',
        evidence: [`${count(t.rows)} rows, no PRIMARY KEY.`],
        fix: 'Add a primary key: an existing unique NOT NULL column, or a new auto-increment one.',
        sql: `ALTER TABLE ${myIdent(t.name)} ADD COLUMN id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY FIRST;`,
      })
    }
    if (t.engine.toLowerCase() === 'myisam' && t.rows >= 1000) {
      add({
        id: `my-myisam-${t.name}`, severity: 'medium', area: 'tables', title: `${t.name} uses MyISAM`,
        why: 'MyISAM locks the whole table for every write, so writers and readers queue behind each other; it also has no transactions and no crash safety.',
        evidence: [`ENGINE=MyISAM, ${count(t.rows)} rows.`], fix: 'Convert it to InnoDB.',
        sql: `ALTER TABLE ${myIdent(t.name)} ENGINE=InnoDB;`,
      })
    }
    if (t.free >= 100 * 1024 ** 2 && t.free / Math.max(1, t.data + t.index) >= 0.2) {
      add({
        id: `my-free-${t.name}`, severity: 'low', area: 'maintenance', title: `${t.name} has much unused space`,
        why: 'Deleted rows leave free pages behind; scans still read past them and the file takes disk and cache.',
        evidence: [`${bytes(t.free)} free in a table of ${bytes(t.data + t.index)}.`],
        fix: 'Rebuild it in a quiet hour (online on InnoDB, but it copies the table).',
        sql: `OPTIMIZE TABLE ${myIdent(t.name)};`,
      })
    }
  }

  /* Indexes */
  const groups = new Map<string, MyIndex[]>()
  for (const i of ix) {
    const k = `${i.table}\u0000${i.columns.join(',')}`
    groups.set(k, [...(groups.get(k) ?? []), i])
  }
  const dup = new Set<MyIndex>()
  for (const g of groups.values()) {
    if (g.length < 2) continue
    const keep = g.find((i) => i.name === 'PRIMARY') ?? g.find((i) => i.unique) ?? g[0]!
    for (const i of g) {
      if (i === keep || i.unique || i.name === 'PRIMARY') continue
      dup.add(i)
      add({
        id: `my-dup-${i.table}.${i.name}`, severity: 'medium', area: 'indexes', title: `${i.table}.${i.name} duplicates ${keep.name}`,
        why: 'Two indexes on the same columns cost twice on every write and in the buffer pool, and the optimizer only needs one.',
        evidence: [`Both on (${i.columns.join(', ')}).`], fix: `Drop ${i.name}.`,
        sql: `ALTER TABLE ${myIdent(i.table)} DROP INDEX ${myIdent(i.name)};`,
      })
    }
  }
  // An index whose columns lead another index on the same table is
  // redundant (MySQL can use the longer one), unless it is unique.
  for (const i of ix) {
    if (i.unique || i.name === 'PRIMARY' || dup.has(i)) continue
    const longer = ix.find((o) => o !== i && o.table === i.table && o.columns.length > i.columns.length
      && i.columns.every((c, k) => o.columns[k] === c))
    if (longer) {
      add({
        id: `my-redundant-${i.table}.${i.name}`, severity: 'low', area: 'indexes', title: `${i.table}.${i.name} is covered by ${longer.name}`,
        why: 'An index on the leading columns of another index adds write cost and memory; the longer index serves the same lookups.',
        evidence: [`(${i.columns.join(', ')}) leads (${longer.columns.join(', ')}).`],
        fix: `Drop ${i.name} once you have checked no query hints it by name.`,
        sql: `ALTER TABLE ${myIdent(i.table)} DROP INDEX ${myIdent(i.name)};`,
      })
    }
  }
  if (days >= 7) {
    const usage = rows(p.indexUsage)
    const unused = usage.filter((r) => n(r[2]) === 0 && r[1] !== 'PRIMARY')
    for (const r of unused.slice(0, 15)) {
      const i = ix.find((x) => x.table === r[0] && x.name === r[1])
      if (!i || i.unique || dup.has(i)) continue
      add({
        id: `my-unused-${r[0]}.${r[1]}`, severity: 'low', area: 'indexes', title: `${r[0]}.${r[1]} is never used`,
        why: 'Every write updates every index of a table, and the index takes buffer pool memory. An index no query reads is cost with no benefit.',
        evidence: [`No reads or writes through it. ${counting}`],
        fix: 'Drop it if no rare job relies on it; keep its definition to put it back.',
        sql: `ALTER TABLE ${myIdent(r[0] ?? '')} DROP INDEX ${myIdent(r[1] ?? '')};`,
      })
    }
  }
  out.sort((a, b) => ORDER[a.severity] - ORDER[b.severity])
  return { findings: out, countingS, bufferHit, connections: { used, max, running, sleeping }, digests: ds, mariadb }
}
