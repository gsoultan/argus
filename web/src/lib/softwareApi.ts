import { get, send, type PackageRun } from '~/lib/live'

/*
 * Installing and removing software on a Linux server. Here rather than in
 * live.ts, which every page loads, so only the Apps tab pays for it.
 *
 * The installed list, a search and a preview are questions put to the
 * server's agent while the person waits; the install or removal itself is a
 * package run, like an update, and shows its progress the same way.
 */

export interface InstalledPackage {
  name: string
  version: string
  arch?: string
  origin?: string
  summary?: string
  size_kb?: number
  /** Unix seconds, when the package database says. */
  installed_at?: number
  /** Asked for by someone, rather than pulled in by another package. */
  manual: boolean
  /** Why it cannot be removed from the console, when it cannot. */
  protected?: string
  /** A database or the like: removing it never deletes its data here. */
  keeps_data?: boolean
}

export interface FoundPackage {
  name: string
  version: string
  origin?: string
  summary?: string
  installed?: string
  versions?: string[]
}

export interface SoftwareChange {
  kind: 'add' | 'upgrade' | 'downgrade' | 'remove'
  name: string
  from?: string
  to?: string
  asked?: boolean
}

export interface SoftwareItem {
  name: string
  version?: string
}

export interface SoftwareAnswer {
  state: 'ok' | 'offline' | 'unsupported' | 'failed'
  error?: string
  manager?: string
  installed: InstalledPackage[]
  total?: number
  manual?: number
  found: FoundPackage[]
  truncated?: boolean
}

export interface SoftwarePreview {
  id: string
  assetId: string
  action: 'install' | 'remove'
  items: SoftwareItem[]
  purge: boolean
  startServices: boolean
  result: {
    action: string
    changes: SoftwareChange[]
    download_kb?: number
    disk_kb?: number
    blocked?: string
    warnings?: string[]
    services?: string[]
    purge_allowed: boolean
    hash: string
  }
  blocked?: string
  needsApproval: boolean
  undoOf?: string | null
  createdBy: string
  createdAt: string
  expiresAt: string
}

const base = (assetId: string) => `/api/v1/assets/${encodeURIComponent(assetId)}/software`

export async function installedSoftware(assetId: string, all: boolean): Promise<SoftwareAnswer> {
  return get(`${base(assetId)}${all ? '?all=1' : ''}`)
}

export async function searchSoftware(assetId: string, q: string): Promise<SoftwareAnswer> {
  return get(`${base(assetId)}/search?q=${encodeURIComponent(q)}`)
}

export async function previewSoftware(assetId: string, body: {
  action: 'install' | 'remove'
  items: SoftwareItem[]
  purge?: boolean
  startServices?: boolean
}): Promise<SoftwarePreview> {
  return send('POST', `${base(assetId)}/preview`, body)
}

export async function startSoftwareRun(assetId: string, previewId: string): Promise<PackageRun> {
  return send('POST', `${base(assetId)}/runs`, { previewId })
}

export async function softwareRuns(assetId: string): Promise<{ runs: PackageRun[]; needsApproval: boolean }> {
  return get(`${base(assetId)}/runs`)
}

export async function undoSoftwareRun(runId: string): Promise<SoftwarePreview> {
  return send('POST', `/api/v1/package-runs/${encodeURIComponent(runId)}/undo`)
}

/** "1.4 MB", from kilobytes. */
export function sizeText(kb: number | undefined): string {
  if (!kb) return '—'
  if (kb < 1024) return `${kb} KB`
  if (kb < 1024 * 1024) return `${(kb / 1024).toFixed(kb < 10 * 1024 ? 1 : 0)} MB`
  return `${(kb / 1024 / 1024).toFixed(1)} GB`
}

/** What a finished or pending install or removal did, in a sentence. */
export function softwareRunText(r: PackageRun): string {
  const items = (r.items ?? []).map((i) => i.name)
  const what = items.length <= 3 ? items.join(', ') : `${items.length} packages`
  const verb = r.mode === 'remove' ? (r.state === 'done' ? 'removed' : 'remove') : (r.state === 'done' ? 'installed' : 'install')
  const deps = r.state === 'done' ? r.upgraded.filter((u) => !items.includes(u.name)).length : 0
  return `${verb} ${what}${r.mode === 'remove' && r.purge ? ' and its settings' : ''}${deps > 0 ? ` and ${deps} other${deps === 1 ? '' : 's'}` : ''}`
}
