/*
The query workspace's tabs, kept in this browser per database.

What is kept is each tab's name and editor text, and nothing a statement
returned: results can hold data the person should not leave lying in a
browser's storage, so they live only as long as the page. Storage can be
missing (a private window) or full; every read and write here survives that.
*/

export interface QueryTab {
  key: string
  name: string
  /** The person named it; otherwise it follows the first comment line. */
  named: boolean
  sql: string
  maxRows: number
  /** The saved query this tab was opened from or saved as. */
  savedId?: string
  /** The text as last saved, so the tab can say it changed. */
  savedSql?: string
}

export const MAX_TABS = 10
const ROW_LIMITS = [100, 1000, 10000, 100000]

function storageKey(dbId: string): string {
  return `argus.workspace.tabs.${dbId}`
}

let seq = 0
export function newKey(): string {
  seq++
  return `t${Date.now().toString(36)}${seq}`
}

/** "Query N" for the lowest N no open tab uses. */
export function nextName(tabs: QueryTab[]): string {
  const used = new Set(tabs.map((t) => t.name))
  for (let i = 1; ; i++) {
    if (!used.has(`Query ${i}`)) return `Query ${i}`
  }
}

export function blankTab(tabs: QueryTab[], sql = '', name?: string): QueryTab {
  return { key: newKey(), name: name ?? nextName(tabs), named: Boolean(name), sql, maxRows: 1000 }
}

/** A tab's name from its first comment line, when it has one. */
export function nameFromSQL(sql: string): string | null {
  const m = /^\s*--\s*(.+?)\s*$/m.exec(sql.split('\n').find((l) => l.trim() !== '') ?? '')
  if (!m) return null
  const n = m[1]!.trim()
  return n ? (n.length > 40 ? `${n.slice(0, 39)}…` : n) : null
}

export function loadTabs(dbId: string): { tabs: QueryTab[]; active: string } {
  try {
    const raw = localStorage.getItem(storageKey(dbId))
    if (raw) {
      const v = JSON.parse(raw) as { tabs?: unknown; active?: unknown }
      const tabs = (Array.isArray(v.tabs) ? v.tabs : []).filter(isTab).slice(0, MAX_TABS).map((t) => ({
        ...t, key: newKey(), maxRows: ROW_LIMITS.includes(t.maxRows) ? t.maxRows : 1000,
      }))
      if (tabs.length) {
        const i = typeof v.active === 'number' && v.active >= 0 && v.active < tabs.length ? v.active : 0
        return { tabs, active: tabs[i]!.key }
      }
    }
  } catch {
    // unreadable or blocked storage: start fresh
  }
  const first = blankTab([])
  return { tabs: [first], active: first.key }
}

export function saveTabs(dbId: string, tabs: QueryTab[], active: string): void {
  try {
    localStorage.setItem(storageKey(dbId), JSON.stringify({
      tabs: tabs.map(({ name, named, sql, maxRows, savedId, savedSql }) => ({ name, named, sql, maxRows, savedId, savedSql })),
      active: tabs.findIndex((t) => t.key === active),
    }))
  } catch {
    // full or blocked: the tabs still work for this page
  }
}

function isTab(v: unknown): v is QueryTab {
  if (!v || typeof v !== 'object') return false
  const t = v as Record<string, unknown>
  return typeof t.name === 'string' && typeof t.sql === 'string' && t.sql.length <= 200_000 &&
    typeof t.maxRows === 'number'
}

/**
 * Whether the person's own statements have left a transaction open on the
 * session, as far as this console sent them. Every statement of the session
 * goes through this page, so the answer only drifts if a statement's text
 * hides its effect (a function that commits); EXPLAIN ANALYZE uses it to
 * refuse rather than roll back someone's own work.
 */
export function txAfter(open: boolean, stmt: string, ok: boolean): boolean {
  const s = stmt.replace(/^(\s*--[^\n]*\n|\s*\/\*[\s\S]*?\*\/)*/g, '').trim().toLowerCase()
  if (/^(begin|start\s+transaction)\b/.test(s)) return ok || open
  if (/^(commit|end|abort)\b/.test(s)) return false
  if (/^rollback\b/.test(s) && !/^rollback\s+(work\s+|transaction\s+)?to\b/.test(s) && !/^rollback\s+prepared\b/.test(s)) return false
  return open
}

/** Whether a statement could change data, so ANALYZE must be rolled back. */
export function mayWrite(stmt: string): boolean {
  return /\b(insert|update|delete|merge|truncate|create|alter|drop|grant|revoke|call|do|copy|refresh|lock|nextval|setval)\b/i
    .test(stmt.replace(/'(?:[^']|'')*'/g, "''"))
}

const HEIGHT_KEY = 'argus.workspace.editorHeight'

export function loadEditorHeight(fallback: number): number {
  try {
    const n = Number(localStorage.getItem(HEIGHT_KEY))
    return Number.isFinite(n) && n >= 80 && n <= 2000 ? n : fallback
  } catch {
    return fallback
  }
}

export function saveEditorHeight(h: number | null): void {
  try {
    if (h === null) localStorage.removeItem(HEIGHT_KEY)
    else localStorage.setItem(HEIGHT_KEY, String(Math.round(h)))
  } catch {
    // blocked storage: the height lasts for this page
  }
}
