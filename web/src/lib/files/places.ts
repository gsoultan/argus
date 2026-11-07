/**
 * The file browser's places: a few folders every Linux server has, the
 * folders this person pinned on this server, and the files they opened
 * recently. Kept in this browser only, per server and account: convenience,
 * not configuration, and nothing the server needs to know.
 */

export interface Place {
  label: string
  path: string
}

export interface RecentFile {
  path: string
  at: number
}

export const STANDARD_PLACES: Place[] = [
  { label: 'Home', path: '~' },
  { label: 'Root', path: '/' },
  { label: 'Logs', path: '/var/log' },
  { label: 'Config', path: '/etc' },
  { label: 'Temporary', path: '/tmp' },
]

const MAX_RECENT = 8
const MAX_PINS = 20

function key(kind: string, host: string, principal: string) {
  return `argus-files-${kind}:${host}:${principal}`
}

function read<T>(k: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(k)
    return raw ? (JSON.parse(raw) as T) : fallback
  } catch {
    return fallback
  }
}

function write(k: string, v: unknown) {
  try {
    localStorage.setItem(k, JSON.stringify(v))
  } catch {
    // Private mode or full: places simply are not remembered.
  }
}

export function pinnedPlaces(host: string, principal: string): Place[] {
  const v = read<Place[]>(key('pins', host, principal), [])
  return Array.isArray(v) ? v.filter((p) => typeof p?.path === 'string').slice(0, MAX_PINS) : []
}

export function setPinned(host: string, principal: string, places: Place[]) {
  write(key('pins', host, principal), places.slice(0, MAX_PINS))
}

export function recentFiles(host: string, principal: string): RecentFile[] {
  const v = read<RecentFile[]>(key('recent', host, principal), [])
  return Array.isArray(v) ? v.filter((r) => typeof r?.path === 'string').slice(0, MAX_RECENT) : []
}

export function rememberFile(host: string, principal: string, path: string): RecentFile[] {
  const next = [{ path, at: Date.now() }, ...recentFiles(host, principal).filter((r) => r.path !== path)]
    .slice(0, MAX_RECENT)
  write(key('recent', host, principal), next)
  return next
}

export function showHiddenPref(): boolean {
  return read<boolean>('argus-files-hidden', false) === true
}

export function setShowHiddenPref(v: boolean) {
  write('argus-files-hidden', v)
}
