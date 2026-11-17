import type { CloudItem } from './cloud'

/*
 * RDS read replicas on Cloud › Scaling: each database with the replicas it
 * has, and why one cannot have a replica added from Argus. The control plane
 * decides (cloud_replica_routes.go); replicaProblem mirrors its words so the
 * button says why before anyone asks.
 */

export const REPLICA_ENGINES: Record<string, string> = { postgres: 'Postgres', mysql: 'MySQL', mariadb: 'MariaDB' }

export interface ReplicaFamily {
  primary: CloudItem
  replicas: CloudItem[]
}

/** Databases with their replicas, primaries first by name; a replica whose primary was not read stands alone. */
export function replicaFamilies(items: CloudItem[] | undefined): ReplicaFamily[] {
  const all = items ?? []
  const key = (it: CloudItem, name: string) => `${it.accountId}/${it.resource.region}/${name}`
  const byName = new Map(all.map((it) => [key(it, it.resource.name), it]))
  const out = new Map<string, ReplicaFamily>()
  for (const it of all) {
    const of = it.resource.details?.replica_of
    if (of && byName.has(key(it, of))) continue
    out.set(key(it, it.resource.name), { primary: it, replicas: [] })
  }
  for (const it of all) {
    const of = it.resource.details?.replica_of
    const fam = of ? out.get(key(it, of)) : undefined
    if (fam) fam.replicas.push(it)
  }
  for (const f of out.values()) f.replicas.sort((a, b) => a.resource.name.localeCompare(b.resource.name))
  return [...out.values()].sort((a, b) => a.primary.resource.name.localeCompare(b.primary.resource.name))
}

/** Why a read replica cannot be added to this database from Argus, or null. */
export function replicaProblem(it: CloudItem): string | null {
  const r = it.resource
  const d = r.details ?? {}
  const engine = d.engine ?? ''
  if (d.cluster || engine.startsWith('aurora')) return `${r.name} is in an Aurora cluster; add a reader to the cluster instead`
  if (!REPLICA_ENGINES[engine]) return `Argus adds read replicas of Postgres, MySQL and MariaDB, not ${engine || 'this engine'}`
  if (d.replica_of) return `${r.name} is itself a replica of ${d.replica_of}; add one to the primary`
  if (r.state !== 'available') return `${r.name} is ${r.state ?? 'not available'}; a replica can be added once it is available`
  if (!d.backup_days || d.backup_days === '0') return `AWS makes a replica from ${r.name}'s backups, which are off; turn on automated backups first`
  return null
}

const NAME = /^[a-zA-Z][a-zA-Z0-9-]{0,62}$/

/** Whether s may name an RDS database: a letter, then letters, digits and single hyphens, at most 63. */
export function validDBName(s: string): boolean {
  return NAME.test(s) && !s.includes('--') && !s.endsWith('-')
}

/** The first free "<primary>-rN" in the primary's account and region. */
export function nextReplicaName(primary: CloudItem, items: CloudItem[] | undefined): string {
  const taken = new Set((items ?? []).filter((it) => it.accountId === primary.accountId && it.resource.region === primary.resource.region)
    .map((it) => it.resource.name.toLowerCase()))
  const base = primary.resource.name.slice(0, 58)
  for (let n = 1; ; n++) if (!taken.has(`${base}-r${n}`.toLowerCase())) return `${base}-r${n}`
}

/** A database's engine and version in words: "Postgres 16.4". */
export function engineWords(it: CloudItem): string {
  const d = it.resource.details ?? {}
  const e = REPLICA_ENGINES[d.engine ?? ''] ?? d.engine ?? 'Database'
  return d.version ? `${e} ${d.version}` : e
}
