import type { Asset, Database, DatabaseReplica, ReadEndpoint } from '~/types/domain'

/*
 * Streaming replicas of a Postgres on a server Argus manages (db_replica.go).
 * The control plane decides; these mirror its refusals so the form says why
 * before anyone asks, and put a run's stage in words.
 */

const NAME = /^[a-z][a-z0-9_]{0,39}$/

/** Whether s may name a replica: the role is argus_repl_<s> and the slot argus_replica_<s>. */
export function validReplicaName(s: string): boolean {
  return NAME.test(s)
}

/** A replica name from the replica server's hostname: "orders-db-02.internal" → "orders_db_02". */
export function replicaNameFor(hostname: string): string {
  let s = (hostname.split('.')[0] ?? '').toLowerCase().replace(/[^a-z0-9_]+/g, '_').replace(/^_+|_+$/g, '')
  if (!/^[a-z]/.test(s)) s = 'r_' + s
  return s.slice(0, 40).replace(/_+$/, '') || 'replica'
}

/** Whether Argus reaches this Postgres on its own server, the only kind it copies. */
export function localPostgres(db: Pick<Database, 'host'>): boolean {
  return ['127.0.0.1', '::1', 'localhost'].includes(db.host) || (db.host.startsWith('/') && !db.host.includes('..'))
}

/** Why Argus cannot make a streaming replica of this database's server, or null. */
export function replicaRefusal(db: Pick<Database, 'engine' | 'host' | 'hostname' | 'tunnelConnected'>): string | null {
  if (db.engine !== 'postgres') return 'Argus makes streaming replicas of Postgres'
  if (!localPostgres(db)) return 'This database is reached through another server’s PgBouncer; Argus copies only a Postgres it reaches on its own server'
  if (!db.tunnelConnected) return `The agent on ${db.hostname} is not connected`
  return null
}

const BUSY = new Set<DatabaseReplica['state']>(['pending', 'running', 'done'])

/** Why a server cannot take a replica of db, or null. */
export function targetRefusal(a: Asset, db: Pick<Database, 'assetId'>, replicas: DatabaseReplica[]): string | null {
  if (a.id === db.assetId) return 'the primary’s own server'
  if (a.kind && a.kind !== 'server') return 'not a server'
  if (!a.tunnelConnected) return 'agent not connected'
  if (replicas.some((r) => r.assetId === a.id && BUSY.has(r.state))) return 'already a replica'
  return null
}

/** The IP addresses a server's agent reported, IPv4 first, loopback left out. */
export function serverAddresses(a: Asset): string[] {
  const out = (a.addresses ?? []).filter((x) => !x.address.startsWith('127.') && x.address !== '::1')
    .sort((x, y) => (x.family === y.family ? 0 : x.family === 'ipv4' ? -1 : 1)).map((x) => x.address)
  if (isIP(a.address) && !out.includes(a.address)) out.unshift(a.address)
  return [...new Set(out)]
}

/** Whether s is an IPv4 or IPv6 address, loosely. */
export function isIP(s: string): boolean {
  return /^(\d{1,3}\.){3}\d{1,3}$/.test(s) || (s.includes(':') && /^[0-9a-fA-F:.]+$/.test(s))
}

const STAGES: Record<string, string> = {
  approval: 'Waiting for an approver',
  checking: 'Checking both servers',
  reading: 'Reading the replica server’s Postgres',
  verifier: 'Making the replication password',
  preparing: 'Making the replication role and slot on the primary',
  allowing: 'Letting the replica in through pg_hba.conf',
  stopping: 'Stopping Postgres on the replica server',
  copying: 'Copying with pg_basebackup',
  starting: 'Starting Postgres as a replica',
  'adding to Argus': 'Adding the replica to Argus',
  done: 'Streaming',
}

/** Where a replica is, in words. */
export function replicaStage(r: Pick<DatabaseReplica, 'state' | 'stage'>): string {
  switch (r.state) {
    case 'pending': return 'Waiting for an approver'
    case 'denied': return 'Denied'
    case 'cancelled': return 'Cancelled'
    case 'done': return 'Streaming'
    case 'failed': return r.stage ? `Failed while ${(STAGES[r.stage] ?? r.stage).toLowerCase()}` : 'Failed'
    default: return STAGES[r.stage] ?? (r.stage ? r.stage.charAt(0).toUpperCase() + r.stage.slice(1) : 'Starting')
  }
}

export const REPLICA_STATE_COLOR: Record<DatabaseReplica['state'], string> = {
  pending: 'amber', running: 'azure', done: 'teal', failed: 'rose', denied: 'slate', cancelled: 'slate',
}

/** Seconds in words: "4 s", "3 min", "2 h 5 min". */
export function lagWords(s: number): string {
  const n = Math.floor(s)
  if (n < 60) return `${n} s`
  if (n < 3600) return `${Math.floor(n / 60)} min`
  return `${Math.floor(n / 3600)} h ${Math.floor((n % 3600) / 60)} min`
}

/** How a finished replica is doing, from the lag watch: words and a colour. Null before the first reading. */
export function replicaHealth(r: Pick<DatabaseReplica, 'state' | 'streaming' | 'lagS' | 'watchedAt'>): { words: string; color: string } | null {
  if (r.state !== 'done' || !r.watchedAt) return null
  if (!r.streaming || r.lagS === undefined || r.lagS < 0) return { words: 'Not streaming', color: 'rose' }
  if (r.lagS === 0) return { words: 'Streaming, caught up', color: 'teal' }
  return { words: `Streaming, ${lagWords(r.lagS)} behind`, color: r.lagS > 60 ? 'amber' : 'teal' }
}

/**
 * Where a finished replica stands in the database's read endpoint: words and a colour.
 * Null with no endpoint, or for a replica that is not made yet.
 */
export function endpointStatus(
  r: Pick<DatabaseReplica, 'state' | 'endpointInstance' | 'endpointNote'>,
  endpoint: Pick<ReadEndpoint, 'targetName' | 'health'> | null | undefined,
): { words: string; color: string } | null {
  if (!endpoint || r.state !== 'done') return null
  if (r.endpointInstance) {
    const health = endpoint.health[r.endpointInstance]
    const bad = health === 'unhealthy' || health === 'draining'
    return { words: `In ${endpoint.targetName}${health ? ` · ${health}` : ''}`, color: bad ? 'amber' : 'teal' }
  }
  if (r.endpointNote === 'out while it is behind') return { words: `Out of ${endpoint.targetName} while it is behind`, color: 'amber' }
  if (r.endpointNote) return { words: `Not in ${endpoint.targetName}: ${r.endpointNote}`, color: 'rose' }
  return { words: `Not in ${endpoint.targetName} yet`, color: 'dimmed' }
}
