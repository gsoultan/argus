/**
 * Paths on the server, as the file browser handles them: POSIX, with `~` for
 * the account's home until the agent answers with the absolute path.
 */

export function joinPath(dir: string, name: string): string {
  return dir.endsWith('/') ? dir + name : `${dir}/${name}`
}

export function parentPath(p: string): string {
  if (p === '/' || p === '~') return p
  const trimmed = p.replace(/\/+$/, '')
  const i = trimmed.lastIndexOf('/')
  if (i < 0) return '~'
  return i === 0 ? '/' : trimmed.slice(0, i)
}

export function baseName(p: string): string {
  const trimmed = p.replace(/\/+$/, '')
  return trimmed.slice(trimmed.lastIndexOf('/') + 1) || trimmed
}

/** Each folder from the root down, for breadcrumbs: label and full path. */
export function crumbs(p: string): { label: string; path: string }[] {
  if (p.startsWith('~')) {
    const rest = p.slice(1).split('/').filter(Boolean)
    return [{ label: '~', path: '~' }, ...rest.map((s, i) => ({ label: s, path: '~/' + rest.slice(0, i + 1).join('/') }))]
  }
  const parts = p.split('/').filter(Boolean)
  return [{ label: '/', path: '/' }, ...parts.map((s, i) => ({ label: s, path: '/' + parts.slice(0, i + 1).join('/') }))]
}

/**
 * A name that is not already taken in the folder, for "Keep both":
 * report.csv, report (1).csv, report (2).csv… A leading dot is part of the
 * name, not an extension, so .env becomes .env (1).
 */
export function freeName(name: string, taken: Iterable<string>): string {
  const used = new Set(taken)
  if (!used.has(name)) return name
  const dot = name.lastIndexOf('.')
  const hasExt = dot > 0
  const stem = hasExt ? name.slice(0, dot) : name
  const ext = hasExt ? name.slice(dot) : ''
  for (let i = 1; i < 10_000; i++) {
    const candidate = `${stem} (${i})${ext}`
    if (!used.has(candidate)) return candidate
  }
  return `${stem} (${Date.now()})${ext}`
}

/** A name a person may give a file or folder: one path segment. */
export function validName(name: string): string | null {
  const n = name.trim()
  if (!n) return 'Give it a name.'
  if (n === '.' || n === '..') return 'That name means a folder already.'
  if (n.includes('/')) return 'A name cannot contain a slash.'
  if (n.length > 255) return 'Names are at most 255 characters.'
  return null
}

export function formatSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}

/** Whether a file is worth previewing as text, from its name. */
export function looksTextual(name: string): boolean {
  const lower = name.toLowerCase()
  if (/\.(gz|tgz|zip|xz|bz2|zst|7z|rar|tar|jar|war|png|jpe?g|gif|webp|ico|pdf|so|o|a|bin|iso|img|qcow2|deb|rpm|db|sqlite|mp[34]|mov|woff2?|ttf|class|pyc)$/.test(lower)) {
    return false
  }
  return true
}
