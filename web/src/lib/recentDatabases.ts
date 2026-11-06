/*
The databases this browser opened last, newest first, by id. Kept in
localStorage and nowhere else: it is a convenience for the person at this
browser, never a record of who opened what (the session recordings are that),
and the Databases page says "in this browser" when it shows it.

Only ids are kept. A database the person can no longer see simply drops out,
because the page shows an id only when the control plane listed it.
*/

const KEY = 'argus-db-recent'
const MAX = 6

export interface RecentOpen { id: string; at: number }

export function loadRecentDatabases(): RecentOpen[] {
  try {
    const raw = localStorage.getItem(KEY)
    if (!raw) return []
    const v = JSON.parse(raw) as unknown
    if (!Array.isArray(v)) return []
    return v
      .filter((x): x is RecentOpen => typeof x === 'object' && x !== null && typeof (x as RecentOpen).id === 'string' && typeof (x as RecentOpen).at === 'number')
      .slice(0, MAX)
  } catch {
    return []
  }
}

export function noteDatabaseOpened(id: string, at = Date.now()): void {
  try {
    const next = [{ id, at }, ...loadRecentDatabases().filter((r) => r.id !== id)].slice(0, MAX)
    localStorage.setItem(KEY, JSON.stringify(next))
  } catch {
    // Private windows and blocked storage: the list is a convenience, so nothing breaks without it.
  }
}
