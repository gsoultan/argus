/*
Why a MongoDB database is heavy, and what to change: the analysis behind the
MongoDB Performance panel. Pure, so it is tested without a server.

The facts are the agent's perf operation (internal/agent/mongo_perf.go):
serverStatus cut to what is read here, dbStats, each collection's storage and
index use ($collStats, $indexStats), the profiler's slow operations grouped by
collection and plan, and the operations running for 5 seconds or more. It is
DBA only and read as the person's own user, whose roles at DBA (dbOwner and
clusterMonitor) cover exactly these reads.

Fixes are mongosh-shaped commands the console's command line takes
(createIndex, or a command document), put on the line and never run from the
panel.
*/

import { bytes, count, duration, ms, type Finding, type Severity } from '~/lib/pgPerformance'

type Doc = Record<string, unknown>

export interface MongoIndex {
  name: string; key: Record<string, unknown>; accesses?: { ops?: number; since?: string }
  unique?: boolean; ttl?: boolean; partial?: boolean
}
export interface MongoCollection {
  name: string; count: number; size: number; storage: number; free: number
  indexes: MongoIndex[]; indexSize?: Record<string, number>
}
export interface MongoSlow {
  _id: { ns: string; op: string; plan?: string; shape?: string }
  count: number; totalMs: number; maxMs: number; examined: number; keys: number; returned: number; sorts: number
  example?: { filter?: Doc; sort?: Doc; pipeline?: Doc[]; q?: Doc }
}
export interface MongoPerf {
  version: string
  server?: Doc
  db?: Doc
  collections: MongoCollection[]
  profile?: { was?: number; slowms?: number }
  slow: MongoSlow[]
  running: { opid?: unknown; op?: string; ns?: string; secs_running?: number; planSummary?: string; waitingForLock?: boolean }[]
  missing?: string[]
}

export interface MongoReport {
  findings: Finding[]
  countingS: number
  /** The WiredTiger cache's fill, 0 to 1, when known. */
  cacheFill: number | null
  connections: { current: number; available: number }
  /** Documents examined for each one returned, server-wide. */
  examinedPerReturned: number | null
}

const ORDER: Record<Severity, number> = { high: 0, medium: 1, low: 2 }
const pct = (x: number) => `${(x * 100).toFixed(x < 0.1 ? 1 : 0)}%`
const IDENT = /^[A-Za-z_$][\w$]*$/

/** A number out of a serverStatus path, 0 when absent. */
export function at(d: unknown, ...path: string[]): number {
  let v: unknown = d
  for (const p of path) {
    if (!v || typeof v !== 'object') return 0
    v = (v as Doc)[p]
  }
  if (typeof v === 'number') return v
  if (v && typeof v === 'object' && '$numberLong' in (v as Doc)) return Number((v as Doc).$numberLong)
  if (v && typeof v === 'object' && '$numberDouble' in (v as Doc)) return Number((v as Doc).$numberDouble)
  return 0
}

/** How the command line names a collection. */
export function coll(name: string): string {
  return IDENT.test(name) ? `db.${name}` : `db.getCollection(${JSON.stringify(name)})`
}

/** The fields a slow operation filters on, from its filter or first $match. */
export function filterFields(ex: MongoSlow['example']): string[] {
  const f = ex?.filter ?? ex?.q ?? (ex?.pipeline?.[0] && '$match' in ex.pipeline[0] ? ex.pipeline[0].$match as Doc : undefined)
  const out: string[] = []
  const walk = (d: Doc) => {
    for (const [k, v] of Object.entries(d)) {
      if (k === '$and' && Array.isArray(v)) { v.forEach((x) => x && typeof x === 'object' && walk(x as Doc)); continue }
      if (k.startsWith('$')) continue
      if (!out.includes(k)) out.push(k)
    }
  }
  if (f && typeof f === 'object') walk(f)
  return out.slice(0, 3)
}
/** The fields a slow operation sorts on, in order. */
export function sortFields(ex: MongoSlow['example']): [string, number][] {
  const s = ex?.sort ?? (ex?.pipeline?.find((x) => '$sort' in x)?.$sort as Doc | undefined)
  if (!s || typeof s !== 'object') return []
  return Object.entries(s).filter(([k]) => !k.startsWith('$')).map(([k, v]) => [k, Number(v) < 0 ? -1 : 1] as [string, number]).slice(0, 2)
}

/** The ESR order: equality fields, then sort fields, as an index key. */
export function indexKey(ex: MongoSlow['example']): string {
  const parts: string[] = []
  const seen = new Set<string>()
  for (const f of filterFields(ex)) { seen.add(f); parts.push(`${IDENT.test(f) ? f : JSON.stringify(f)}: 1`) }
  for (const [f, d] of sortFields(ex)) if (!seen.has(f)) parts.push(`${IDENT.test(f) ? f : JSON.stringify(f)}: ${d}`)
  return parts.length ? `{ ${parts.join(', ')} }` : ''
}

const keyText = (k: Record<string, unknown>) => `{ ${Object.entries(k).map(([f, d]) => `${f}: ${typeof d === 'string' ? JSON.stringify(d) : d}`).join(', ')} }`
const shapeText = (s: MongoSlow) => {
  const ex = s.example
  const f = ex?.filter ?? ex?.q
  if (f) return `${s._id.op} ${JSON.stringify(f)}`.slice(0, 120)
  if (ex?.pipeline) return `aggregate ${JSON.stringify(ex.pipeline)}`.slice(0, 120)
  return s._id.op
}

export function analyzeMongo(p: MongoPerf, now = Date.now()): MongoReport {
  const out: Finding[] = []
  const add = (f: Finding) => out.push(f)
  const s = p.server ?? {}
  const up = at(s, 'uptime')
  const counting = `MongoDB has been counting for ${duration(up)}, since it started.`
  const name = (ns: string) => ns.slice(ns.indexOf('.') + 1)

  /* Connections and queues */
  const current = at(s, 'connections', 'current')
  const available = at(s, 'connections', 'available')
  if (current + available > 0 && current / (current + available) >= 0.8) {
    add({
      id: 'mg-conn-limit', severity: current / (current + available) >= 0.95 ? 'high' : 'medium', area: 'connections',
      title: 'Connections are close to the limit',
      why: 'Each connection costs mongod a thread and about 1 MB. Near the limit new clients are refused, and usually it means clients open connections without a shared pool.',
      evidence: [`${count(current)} open, ${count(available)} still available.`],
      fix: 'Use one MongoClient per application process with a bounded pool (maxPoolSize), and check for clients that leak connections.',
    })
  }
  const created = at(s, 'connections', 'totalCreated')
  if (up >= 3600 && created / (up / 60) >= 60) {
    add({
      id: 'mg-conn-churn', severity: 'low', area: 'connections', title: 'Clients connect and disconnect all the time',
      why: 'Opening a connection costs a TLS handshake and authentication; doing it per request wastes CPU on both sides.',
      evidence: [`${count(created)} connections made, about ${count(Math.round(created / (up / 60)))} a minute. ${counting}`],
      fix: 'Create the MongoClient once and reuse it; do not connect per request.',
    })
  }
  const queued = at(s, 'globalLock', 'currentQueue', 'total')
  if (queued >= 10) {
    add({
      id: 'mg-queue', severity: queued >= 50 ? 'high' : 'medium', area: 'connections', title: 'Operations are queueing right now',
      why: 'Operations wait for a storage engine ticket because others hold them: usually slow scans or a disk that cannot keep up.',
      evidence: [`${count(queued)} operations queued now.`],
      fix: 'Find what is running long (below) and index it; check the disk.',
    })
  }

  /* The WiredTiger cache */
  const inCache = at(s, 'wiredTiger', 'cache', 'bytes currently in the cache')
  const maxCache = at(s, 'wiredTiger', 'cache', 'maximum bytes configured')
  const dirty = at(s, 'wiredTiger', 'cache', 'tracked dirty bytes in the cache')
  const appEvict = at(s, 'wiredTiger', 'cache', 'pages evicted by application threads')
  const cacheFill = maxCache > 0 ? inCache / maxCache : null
  if (cacheFill !== null && cacheFill >= 0.95 && appEvict > 0) {
    add({
      id: 'mg-cache-full', severity: 'high', area: 'memory', title: 'The working set does not fit in the cache',
      why: 'The WiredTiger cache is full and queries have to evict pages themselves before they can read: every read waits on the disk and on eviction.',
      evidence: [`Cache ${bytes(inCache)} of ${bytes(maxCache)} (${pct(cacheFill)}); ${count(appEvict)} pages evicted by application threads.`],
      fix: 'Make queries read less (indexes for the scans below), drop unused indexes that take cache, or give the server more memory.',
    })
  }
  if (maxCache > 0 && dirty / maxCache >= 0.2) {
    add({
      id: 'mg-cache-dirty', severity: 'medium', area: 'memory', title: 'Too much of the cache is waiting to be written',
      why: 'Above 20% dirty, WiredTiger makes application threads help write it out, so writes slow down.',
      evidence: [`${bytes(dirty)} dirty in a ${bytes(maxCache)} cache (${pct(dirty / maxCache)}).`],
      fix: 'The disk cannot keep up with the writes: faster storage, or fewer and smaller writes (fewer indexes to update).',
    })
  }

  /* Server-wide query efficiency */
  const scanned = at(s, 'metrics', 'queryExecutor', 'scannedObjects')
  const returned = at(s, 'metrics', 'document', 'returned')
  const perReturned = returned > 0 ? scanned / returned : null
  if (scanned >= 1_000_000 && (perReturned === null || perReturned >= 100)) {
    add({
      id: 'mg-scanned', severity: perReturned === null || perReturned >= 1000 ? 'medium' : 'low', area: 'queries',
      title: 'Queries read far more documents than they return',
      why: 'MongoDB examines every document a query cannot skip with an index. A high ratio means collection scans or indexes that do not match the filters.',
      evidence: [`${count(scanned)} documents examined for ${count(returned)} returned${perReturned ? ` (${count(Math.round(perReturned))} each)` : ''}; ${count(at(s, 'metrics', 'queryExecutor', 'collectionScans', 'total'))} collection scans. ${counting}`],
      fix: 'The slow operations below show which collections and filters; index them.',
    })
  }
  const inMemorySorts = at(s, 'metrics', 'operation', 'scanAndOrder')
  if (inMemorySorts >= 1000) {
    add({
      id: 'mg-sorts', severity: 'low', area: 'queries', title: 'Sorts are done in memory',
      why: 'A sort no index provides is done in memory, limited to 100 MB, and costs CPU on every run.',
      evidence: [`${count(inMemorySorts)} in-memory sorts. ${counting}`],
      fix: 'Add the sort fields to the index after the equality fields (equality, sort, range).',
    })
  }
  const conflicts = at(s, 'metrics', 'operation', 'writeConflicts')
  if (conflicts >= 10_000) {
    add({
      id: 'mg-conflicts', severity: 'low', area: 'queries', title: 'Writes conflict and are retried',
      why: 'Two operations changed the same document at once; one is retried. Many retries mean a hot document every writer touches (a counter, say).',
      evidence: [`${count(conflicts)} write conflicts. ${counting}`],
      fix: 'Spread hot counters over several documents, or batch their updates.',
    })
  }
  const timedOut = at(s, 'metrics', 'cursor', 'timedOut')
  if (timedOut >= 100) {
    add({
      id: 'mg-cursors', severity: 'low', area: 'connections', title: 'Cursors are left open until they time out',
      why: 'A client that does not read a cursor to the end or close it leaves it holding memory for 10 minutes.',
      evidence: [`${count(timedOut)} cursors timed out. ${counting}`],
      fix: 'Close cursors in the application, or ask for only the documents it uses (limit).',
    })
  }

  /* The profiler */
  const was = p.profile?.was ?? 0
  const slowms = p.profile?.slowms ?? 100
  if (p.profile && was === 0) {
    add({
      id: 'mg-profiler-off', severity: 'low', area: 'setup', title: 'Slow operations are not kept here',
      why: 'With the profiler off, slow operations go only to the server log. Level 1 keeps those slower than slowms in system.profile, where this panel can show which queries are slow and why.',
      evidence: [`Profiler level 0; slowms ${slowms}.`], fix: 'Turn on level 1 for slow operations only.',
      sql: '{ profile: 1, slowms: 100 }',
    })
  } else if (p.profile && (was === 2 || slowms === 0)) {
    add({
      id: 'mg-profiler-all', severity: 'medium', area: 'setup', title: 'The profiler records every operation',
      why: 'Writing every operation to system.profile adds a write to each one and fills the capped collection so fast that slow ones roll out within minutes.',
      evidence: [`Profiler level ${was}; slowms ${slowms}.`], fix: 'Record only slow operations.',
      sql: '{ profile: 1, slowms: 100 }',
    })
  }

  /* Slow operations from the profiler */
  const byColl = new Map(p.collections.map((c) => [c.name, c]))
  const totalSlow = p.slow.reduce((a, x) => a + x.totalMs, 0)
  const proposed = new Set<string>()
  for (const x of p.slow.slice(0, 10)) {
    const c = name(x._id.ns)
    const scan = x._id.plan === 'COLLSCAN'
    const ratio = x.returned > 0 ? x.examined / x.returned : x.examined / Math.max(1, x.count)
    const share = totalSlow > 0 ? x.totalMs / totalSlow : 0
    if (x.count < 5 || (!scan && ratio < 100 && x.sorts === 0) || x.totalMs < 100) continue
    const key = indexKey(x.example)
    const reasons: string[] = []
    if (scan) reasons.push('Every run reads the whole collection (COLLSCAN): no index matches its filter.')
    else if (ratio >= 100) reasons.push(`It uses ${x._id.plan ?? 'an index'}, but still examines about ${count(Math.round(ratio))} documents for each one it returns: the index does not match the whole filter.`)
    if (x.examined > 0) reasons.push(`${count(x.examined)} documents examined for ${count(x.returned)} returned.`)
    if (x.sorts > 0) reasons.push(`${count(x.sorts)} of its runs sorted in memory.`)
    if (share >= 0.2) reasons.push(`It is ${pct(share)} of the time of all the slow operations recorded.`)
    const cp = byColl.get(c)
    const fix = key
      ? `Create an index on ${c} with ${key} (equality fields first, then the sort). ${cp && cp.count >= 1_000_000 ? 'On a large collection it builds in the background but takes I/O; do it in a quiet hour.' : ''}`.trim()
      : 'Run explain() on it to see where the time goes.'
    const sql = key && !proposed.has(`${c}${key}`) ? `${coll(c)}.createIndex(${key})` : undefined
    if (sql) proposed.add(`${c}${key}`)
    add({
      id: `mg-slow-${x._id.ns}-${x._id.shape ?? x._id.plan ?? x._id.op}`, severity: scan && ((share >= 0.3 && x.totalMs >= 10_000) || (cp?.count ?? 0) >= 100_000) ? 'high' : 'medium',
      area: 'queries', title: `A heavy query on ${c}: ${shapeText(x)}`, why: reasons.join(' '),
      evidence: [`${count(x.count)} runs, ${ms(x.totalMs)} in all, up to ${ms(x.maxMs)}; plan ${x._id.plan ?? 'none recorded'}.`],
      fix, sql,
    })
  }

  /* Indexes and storage */
  for (const c of p.collections) {
    const ix = c.indexes.filter((i) => i.name !== '_id_')
    for (const i of ix) {
      if (i.unique || i.ttl || i.partial) continue
      const k = Object.entries(i.key)
      const longer = ix.find((o) => o !== i && Object.keys(o.key).length > k.length && k.every(([f, d], n) => Object.entries(o.key)[n]?.[0] === f && Object.entries(o.key)[n]?.[1] === d))
      if (longer && !longer.partial) {
        add({
          id: `mg-redundant-${c.name}.${i.name}`, severity: 'low', area: 'indexes', title: `${c.name}.${i.name} is covered by ${longer.name}`,
          why: 'An index on the leading fields of another index serves no query the longer one cannot, but every write updates both and both take cache.',
          evidence: [`${keyText(i.key)} leads ${keyText(longer.key)}${c.indexSize?.[i.name] ? `; ${bytes(c.indexSize[i.name]!)}` : ''}.`],
          fix: `Drop ${i.name} once you are sure no query hints it by name.`,
          sql: `{ dropIndexes: ${JSON.stringify(c.name)}, index: ${JSON.stringify(i.name)} }`,
        })
        continue
      }
      const since = i.accesses?.since ? Date.parse(i.accesses.since) : NaN
      if ((i.accesses?.ops ?? 0) === 0 && Number.isFinite(since) && now - since >= 7 * 86400_000) {
        add({
          id: `mg-unused-${c.name}.${i.name}`, severity: 'low', area: 'indexes', title: `${c.name}.${i.name} is never used`,
          why: 'Every insert and update maintains it and it takes cache, but no query has used it since it was last loaded.',
          evidence: [`No uses since ${i.accesses!.since!.slice(0, 10)}${c.indexSize?.[i.name] ? `; ${bytes(c.indexSize[i.name]!)}` : ''}.`],
          fix: 'Drop it if no rare job relies on it; hide it first ({ collMod, index: { name, hidden: true } }) to try without losing it.',
          sql: `{ collMod: ${JSON.stringify(c.name)}, index: { name: ${JSON.stringify(i.name)}, hidden: true } }`,
        })
      }
    }
    const indexBytes = Object.values(c.indexSize ?? {}).reduce((a, x) => a + x, 0)
    if (indexBytes >= 1024 ** 3 && indexBytes > c.size) {
      add({
        id: `mg-index-heavy-${c.name}`, severity: 'low', area: 'indexes', title: `${c.name}'s indexes are bigger than its data`,
        why: 'Indexes compete with documents for the cache. Many wide or overlapping indexes push data out of memory.',
        evidence: [`${bytes(indexBytes)} of indexes for ${bytes(c.size)} of documents, in ${c.indexes.length} indexes.`],
        fix: 'Drop the unused and redundant ones; merge overlapping ones into one compound index.',
      })
    }
    if (c.free >= 1024 ** 3 && c.free / Math.max(1, c.storage) >= 0.5) {
      add({
        id: `mg-free-${c.name}`, severity: 'low', area: 'maintenance', title: `${c.name} holds much free space`,
        why: 'Deleted documents leave space WiredTiger reuses for new ones but does not give back to the disk.',
        evidence: [`${bytes(c.free)} free in ${bytes(c.storage)} on disk.`],
        fix: 'Only if the space is needed elsewhere: compact it in a quiet hour (it blocks writes to it on older versions).',
        sql: `{ compact: ${JSON.stringify(c.name)} }`,
      })
    }
  }

  /* Now */
  const long = p.running.filter((r) => (r.secs_running ?? 0) >= 60)
  if (long.length) {
    add({
      id: 'mg-long-ops', severity: long.some((r) => (r.secs_running ?? 0) >= 600) ? 'high' : 'medium', area: 'connections',
      title: 'Operations have been running for a long time',
      why: 'A long operation holds a storage ticket and its snapshot, and pins old data in the cache.',
      evidence: long.slice(0, 5).map((r) => `${r.op ?? 'op'} on ${r.ns ?? '?'}: ${duration(r.secs_running ?? 0)}${r.planSummary ? `, ${r.planSummary}` : ''}${r.waitingForLock ? ', waiting for a lock' : ''}.`),
      fix: 'Check it is meant to run that long; db.killOp(opid) ends one (from the mongosh, as an administrator).',
    })
  }

  // Within a severity, the profiler's own setup comes after what is heavy.
  out.sort((a, b) => ORDER[a.severity] - ORDER[b.severity] || Number(a.area === 'setup') - Number(b.area === 'setup'))
  return { findings: out, countingS: up, cacheFill, connections: { current, available }, examinedPerReturned: perReturned }
}
