/*
Creating databases, vhosts and topics on a server, and changing a
database's owner and its password. Kept out of the shared live module: only an admin creating
something pays for it.
*/

import { get, post } from '~/lib/live'
import type { Database, DBEngine, DBLabel } from '~/types/domain'

/** One step the agent took making a database, vhost or topic. */
export interface CreateStep { text: string; ok: boolean; error?: string }

/** Fields for a new database, vhost or topic, per engine. */
export interface CreateFields {
  postgres?: { owner?: string; encoding?: string; locale?: string; extensions?: string[] }
  mysql?: { charset?: string; collation?: string }
  mssql?: { collation?: string; recovery?: '' | 'FULL' | 'BULK_LOGGED' | 'SIMPLE' }
  mongo?: { collection: string; kind?: 'plain' | 'timeseries' | 'capped'; timeField?: string; expireAfterS?: number; cappedBytes?: number }
  rabbit?: { description?: string; queueType?: '' | 'classic' | 'quorum' | 'stream'; maxConnections?: number; maxQueues?: number }
  kafka?: { partitions: number; replicas?: number; retentionMs?: number; cleanup?: '' | 'delete' | 'compact' }
}

/** Who owns a new database: '' Argus's, an existing user, or one made in the same step. */
export type OwnerKind = '' | 'existing' | 'new'

/**
 * The owner of a database. from is MySQL's host part ('%' by default), or the
 * MongoDB database an existing user lives in. policy is SQL Server's Windows
 * password policy for a new login; connLimit a new Postgres role's.
 */
export interface CreateOwner { kind: OwnerKind; name?: string; from?: string; policy?: boolean; connLimit?: number }

export interface CreateRequest extends CreateFields {
  engine: DBEngine; name: string; host?: string; port?: number; poolerPort?: number; configPath?: string
  label?: DBLabel; addEntry?: boolean; dryRun?: boolean
  owner?: CreateOwner
  /** A new owner's password: sent to the agent once, kept nowhere. */
  password?: string
}

/** Where the owner user connects, shown once after it is made. The password is the browser's own copy. */
export interface OwnerConnection {
  host: string; port: number; database: string; user: string
  /** Postgres straight, when port is PgBouncer's. */
  directPort?: number
  pooled?: boolean; poolMode?: string
  /** MongoDB: the database the user signs in against. */
  authSource?: string
}

/** What Create new did. state: ok, partial (made, a later step failed), exists, offline, unsupported, failed. */
export interface CreateAnswer {
  state: 'ok' | 'partial' | 'exists' | 'offline' | 'unsupported' | 'failed'
  error?: string
  statements: string[]
  steps: CreateStep[]
  database?: Database
  coveredBy?: Database
  connection?: OwnerConnection
  /** Lines a person must add by hand (PgBouncer the agent could not change). */
  manual?: string[]
}

/** A user the dialog offers as an owner; all: it reaches every database already (greyed). */
export interface OwnerUser { name: string; from?: string; reach?: string; all?: boolean }

/** The PgBouncer in front of a Postgres. auth: argus, file, query or trust. */
export interface PoolerInfo { port: number; config: string; covers?: boolean; auth: string; mode?: string; writable: boolean }

/** What the create dialog may offer, from the agent. */
export interface CreateOptions {
  version?: string
  extensions?: string[]
  roles?: string[]
  encoding?: string
  locale?: string
  charsets?: Record<string, string[]>
  defaultCharset?: string
  defaultCollation?: string
  /** SQL Server: the server's collations, and the model database's recovery model. */
  collations?: string[]
  defaultRecovery?: string
  brokers?: number
  taken?: string[]
  truncated?: boolean
  users?: OwnerUser[]
  pooler?: PoolerInfo
}

export interface CreateOptionsAnswer {
  state: 'ok' | 'offline' | 'unsupported' | 'failed'
  error?: string
  options?: CreateOptions
}

export async function createOptions(assetId: string, q: {
  engine: DBEngine; port?: number; host?: string; configPath?: string; poolerPort?: number
}): Promise<CreateOptionsAnswer> {
  const p = new URLSearchParams({ engine: q.engine })
  if (q.poolerPort) p.set('poolerPort', String(q.poolerPort))
  if (q.port) p.set('port', String(q.port))
  if (q.host) p.set('host', q.host)
  if (q.configPath) p.set('configPath', q.configPath)
  return get<CreateOptionsAnswer>(`/api/v1/assets/${encodeURIComponent(assetId)}/databases/create-options?${p}`)
}

/** Make a database, vhost or topic on the server and add it to Argus; dryRun only says what would run. */
export async function createOnServer(assetId: string, req: CreateRequest): Promise<CreateAnswer> {
  return post<CreateAnswer>(`/api/v1/assets/${encodeURIComponent(assetId)}/databases/create`, req)
}

/** Change a database's owner on the server; kind '' is Argus's. A new user needs password. */
export async function setDatabaseOwner(id: string, owner: CreateOwner & { password?: string; dryRun?: boolean }): Promise<CreateAnswer> {
  return post<CreateAnswer>(`/api/v1/databases/${encodeURIComponent(id)}/owner`, { owner: owner.kind === 'existing' ? owner.name ?? '' : '', ...owner })
}

/** Give the database's owner user a new password. */
export async function setOwnerPassword(id: string, user: { name: string; from?: string; password: string }): Promise<CreateAnswer> {
  return post<CreateAnswer>(`/api/v1/databases/${encodeURIComponent(id)}/owner/password`, user)
}

