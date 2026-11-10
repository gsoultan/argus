/*
Creating databases, vhosts and topics on a server, and changing a Postgres
database's owner. Kept out of the shared live module: only an admin creating
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
  mongo?: { collection: string; kind?: 'plain' | 'timeseries' | 'capped'; timeField?: string; expireAfterS?: number; cappedBytes?: number }
  rabbit?: { description?: string; queueType?: '' | 'classic' | 'quorum' | 'stream'; maxConnections?: number; maxQueues?: number }
  kafka?: { partitions: number; replicas?: number; retentionMs?: number; cleanup?: '' | 'delete' | 'compact' }
}

export interface CreateRequest extends CreateFields {
  engine: DBEngine; name: string; host?: string; port?: number; poolerPort?: number; configPath?: string
  label?: DBLabel; addEntry?: boolean; dryRun?: boolean
}

/** What Create new did. state: ok, partial (made, a later step failed), exists, offline, unsupported, failed. */
export interface CreateAnswer {
  state: 'ok' | 'partial' | 'exists' | 'offline' | 'unsupported' | 'failed'
  error?: string
  statements: string[]
  steps: CreateStep[]
  database?: Database
  coveredBy?: Database
}

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
  brokers?: number
  taken?: string[]
  truncated?: boolean
}

export interface CreateOptionsAnswer {
  state: 'ok' | 'offline' | 'unsupported' | 'failed'
  error?: string
  options?: CreateOptions
}

export async function createOptions(assetId: string, q: {
  engine: DBEngine; port?: number; host?: string; configPath?: string
}): Promise<CreateOptionsAnswer> {
  const p = new URLSearchParams({ engine: q.engine })
  if (q.port) p.set('port', String(q.port))
  if (q.host) p.set('host', q.host)
  if (q.configPath) p.set('configPath', q.configPath)
  return get<CreateOptionsAnswer>(`/api/v1/assets/${encodeURIComponent(assetId)}/databases/create-options?${p}`)
}

/** Make a database, vhost or topic on the server and add it to Argus; dryRun only says what would run. */
export async function createOnServer(assetId: string, req: CreateRequest): Promise<CreateAnswer> {
  return post<CreateAnswer>(`/api/v1/assets/${encodeURIComponent(assetId)}/databases/create`, req)
}

/** Change a Postgres database's owner on the server. owner '' is Argus's Owner level. */
export async function setDatabaseOwner(id: string, owner: string): Promise<CreateAnswer> {
  return post<CreateAnswer>(`/api/v1/databases/${encodeURIComponent(id)}/owner`, { owner })
}

