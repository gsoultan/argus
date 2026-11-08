import { BASE, TOKEN, apiFetch, get, readReply, send, type PackageRun } from '~/lib/live'

/*
 * Installing and removing software on a Linux server. Here rather than in
 * live.ts, which every page loads, so only the Apps tab pays for it.
 *
 * The installed list, a search and a preview are questions put to the
 * server's agent while the person waits; the install or removal itself is a
 * package run, like an update, and shows its progress the same way.
 */

export interface InstalledPackage {
  /** On Windows an id (msi-…, app-…); label is the program's name. */
  name: string
  label?: string
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
  label?: string
  from?: string
  to?: string
  asked?: boolean
}

export interface SoftwareItem {
  name: string
  version?: string
  /** What to call it, on Windows where name is an id. */
  label?: string
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
const APP_VERBS: Record<string, [string, string]> = {
  app_install: ['install', 'installed'],
  app_uninstall: ['uninstall', 'uninstalled'],
  app_disable: ['disable', 'disabled'],
  app_restore: ['restore', 'restored'],
}

export function softwareRunText(r: PackageRun): string {
  const app = APP_VERBS[r.mode]
  if (app) {
    const item = r.items?.[0]
    const what = item ? `${item.name}${r.mode === 'app_install' && item.version ? ` ${item.version}` : ''}` : 'an app'
    return `${r.state === 'done' ? app[1] : app[0]} ${what}`
  }
  if (r.mode === 'file_install') {
    const what = r.softwareFileName ?? r.items?.[0]?.label ?? 'a library file'
    return `${r.state === 'done' ? 'installed' : 'install'} ${what}`
  }
  if (r.mode === 'script') {
    return `${r.state === 'done' ? 'ran' : 'run'} the ${r.scriptAction ?? 'install'} script of ${r.scriptName ?? 'a script'}`
  }
  const items = (r.items ?? []).map((i) => i.label || i.name)
  const what = items.length <= 3 ? items.join(', ') : `${items.length} packages`
  const verb = r.mode === 'remove' ? (r.state === 'done' ? 'removed' : 'remove') : (r.state === 'done' ? 'installed' : 'install')
  const deps = r.state === 'done' ? r.upgraded.filter((u) => !items.includes(u.name)).length : 0
  return `${verb} ${what}${r.mode === 'remove' && r.purge ? ' and its settings' : ''}${deps > 0 ? ` and ${deps} other${deps === 1 ? '' : 's'}` : ''}`
}

/* ── Install scripts ─────────────────────────────────────────────────────── */

export type ScriptPlatform = 'linux' | 'windows'

export interface ScriptParam {
  name: string
  label: string
  kind: 'text' | 'choice' | 'secret'
  choices?: string[]
  required?: boolean
  default?: string
}

export interface ScriptFinding {
  rule: string
  level: 'blocked' | 'warning' | 'note'
  title: string
  why?: string
  part: 'install' | 'uninstall' | 'check'
  line?: number
  excerpt?: string
}

/** The key a reviewer accepts a warning by, as the control plane makes it. */
export const findingKey = (f: ScriptFinding) => `${f.part}:${f.rule}:${f.line ?? 0}`

export interface ScriptVersion {
  id: string
  scriptId: string
  version: number
  install?: string
  uninstall?: string
  check?: string
  params: ScriptParam[]
  runAs: string
  timeoutSeconds: number
  reboot: 'never' | 'if_asked'
  installSha: string
  uninstallSha?: string
  checkSha?: string
  findings: ScriptFinding[]
  blocked: boolean
  note?: string
  author: string
  createdAt: string
  reviewedBy: string | null
  reviewedAt: string | null
  selfReviewed: boolean
  accepted: { key: string; reason: string }[]
  triedOn: string | null
  triedAt: string | null
}

export interface SoftwareScript {
  id: string
  name: string
  platform: ScriptPlatform
  description: string
  createdBy: string
  createdAt: string
  latest?: ScriptVersion
  reviewed?: ScriptVersion
  versions?: ScriptVersion[]
}

export interface ScriptRule {
  id: string
  platform: string
  level: ScriptFinding['level']
  group: string
  title: string
  why: string
  pattern: string
}

/** One part as sent: pasted text, or a file read in the browser. */
export interface ScriptPartInput {
  text?: string
  fileName?: string
  fileBase64?: string
}

export interface ScriptVersionInput {
  install: ScriptPartInput
  uninstall: ScriptPartInput
  check: ScriptPartInput
  params: ScriptParam[]
  runAs: string
  timeoutMinutes: number
  reboot: 'never' | 'if_asked'
  note?: string
}

export async function scripts(): Promise<SoftwareScript[]> {
  return get('/api/v1/software/scripts')
}

export async function script(id: string): Promise<SoftwareScript> {
  return get(`/api/v1/software/scripts/${encodeURIComponent(id)}`)
}

export async function scriptRules(platform: ScriptPlatform): Promise<ScriptRule[]> {
  return get(`/api/v1/software/script-rules?platform=${platform}`)
}

export async function createScript(body: {
  name: string; platform: ScriptPlatform; description: string; version: ScriptVersionInput
}): Promise<SoftwareScript> {
  return send('POST', '/api/v1/software/scripts', body)
}

export async function saveScriptVersion(id: string, body: ScriptVersionInput): Promise<SoftwareScript> {
  return send('POST', `/api/v1/software/scripts/${encodeURIComponent(id)}/versions`, body)
}

export async function removeScript(id: string): Promise<void> {
  return send('DELETE', `/api/v1/software/scripts/${encodeURIComponent(id)}`)
}

export async function checkScript(body: { platform: ScriptPlatform; part: string } & ScriptPartInput): Promise<{
  text: string; findings: ScriptFinding[]
}> {
  return send('POST', '/api/v1/software/script-check', body)
}

export async function reviewScript(versionId: string, body: {
  accepted: { key: string; reason: string }[]; confirmSelf?: string
}): Promise<SoftwareScript> {
  return send('POST', `/api/v1/software/script-versions/${encodeURIComponent(versionId)}/review`, body)
}

export async function runScript(assetId: string, body: {
  versionId: string; action: 'install' | 'uninstall'; params: Record<string, string>
}): Promise<PackageRun> {
  return send('POST', `${base(assetId)}/script-runs`, body)
}

/**
 * A file sent as multipart/form-data, with the same contract as send(). The
 * browser sets the boundary, so no Content-Type is given here. Kept out of
 * live.ts, which every route loads, because only the library uploads.
 */
async function upload<T>(path: string, form: FormData): Promise<T> {
  const res = await apiFetch(`${BASE}${path}`, {
    method: 'POST',
    credentials: 'include',
    headers: TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {},
    body: form,
  })
  return readReply<T>(res)
}

/** A file as base64, for an upload the control plane checks is a script. */
export async function fileBase64(file: File): Promise<string> {
  const buf = new Uint8Array(await file.arrayBuffer())
  let s = ''
  for (let i = 0; i < buf.length; i += 0x8000) s += String.fromCharCode(...buf.subarray(i, i + 0x8000))
  return btoa(s)
}

/* ── Library files and apps on Android devices ───────────────────────────── */

export interface SoftwareFile {
  id: string
  platform: 'android' | 'windows'
  kind: 'apk' | 'msi' | 'exe'
  name: string
  fileName: string
  sha256: string
  size: number
  /** What an APK says about itself. */
  app?: {
    package: string
    versionCode: number
    versionName: string
    label: string
    minSdk: number
    /** SHA-256 of the signing certificate. */
    signer: string
    scheme: 'v1' | 'v2' | 'v3'
  }
  note: string
  createdBy: string
  createdAt: string
  /** What an MSI or EXE runs with, and who signed it. */
  windows?: {
    args: string[]
    /** SHA-256 of the signing certificate; absent when unsigned or an MSI. */
    signer?: string
    subject?: string
  }
  /** Said once, when the file is added. */
  warnings?: string[]
}

export function sizeOf(bytes: number): string {
  if (bytes >= 1 << 30) return `${(bytes / (1 << 30)).toFixed(1)} GB`
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MB`
  return `${Math.max(1, Math.round(bytes / 1024))} KB`
}

/** The signer as people compare it: the first and last few hex digits. */
export const signerShort = (s: string) => `${s.slice(0, 8)}…${s.slice(-4)}`

export async function softwareFiles(platform?: 'android' | 'windows'): Promise<SoftwareFile[]> {
  return get(`/api/v1/software/files${platform ? `?platform=${platform}` : ''}`)
}

export async function uploadSoftwareFile(file: File, note: string, opts: { name?: string; args?: string } = {}): Promise<SoftwareFile> {
  const form = new FormData()
  form.append('file', file, file.name)
  const q = new URLSearchParams({ note })
  if (opts.name) q.set('name', opts.name)
  if (opts.args) q.set('args', opts.args)
  return upload(`/api/v1/software/files?${q.toString()}`, form)
}

/** Installs a library MSI or EXE on a Windows server. */
export async function runSoftwareFile(assetId: string, fileId: string): Promise<PackageRun> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/software/file-runs`, { fileId })
}

export async function removeSoftwareFile(id: string): Promise<void> {
  await send('DELETE', `/api/v1/software/files/${encodeURIComponent(id)}`)
}

/** A library item put on many servers, one at a time. */
export interface BatchResult {
  runs: PackageRun[]
  skipped: { assetId: string; hostname: string; reason: string }[]
}

export async function startSoftwareBatch(body: {
  kind: 'script' | 'file'; versionId?: string; action?: 'install' | 'uninstall'; params?: Record<string, string>
  fileId?: string; assetIds: string[]
}): Promise<BatchResult> {
  return send('POST', '/api/v1/software/batches', body)
}

export type AppAction = 'install' | 'uninstall' | 'disable' | 'restore'

export async function runAndroidApp(assetId: string, body: { action: AppAction; fileId?: string; package?: string }): Promise<PackageRun> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/android-apps/runs`, body)
}
