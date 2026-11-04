/*
Short, readable summaries of JSON values: a list of objects reads as the
names of its items, an object as its keys and short values.
*/

export type J = null | boolean | number | string | J[] | { [k: string]: J }

const LABEL_KEYS = ['name', 'title', 'label', 'key', 'code', 'id', 'type', 'desc', 'description']

/** What names an object in a list: its first label-like key with a short value. */
export function itemLabel(v: J): string | null {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return null
  for (const k of LABEL_KEYS) {
    const x = v[k]
    if ((typeof x === 'string' || typeof x === 'number') && String(x).length > 0) return String(x)
  }
  return null
}

/** A one-line summary of a value, for a collapsed node or a table cell. */
export function summary(v: J, max = 80): string {
  if (v === null) return 'null'
  if (Array.isArray(v)) {
    const names = v.map(itemLabel).filter((x): x is string => x !== null)
    if (names.length) return `${v.length} items: ${names.slice(0, 3).join(', ')}${v.length > 3 ? ', …' : ''}`
    const s = JSON.stringify(v)
    return s.length > max ? `[${v.length} items]` : s
  }
  if (typeof v === 'object') {
    const keys = Object.keys(v)
    const s = keys.map((k) => `${k}: ${short(v[k]!)}`).join(' · ')
    return s.length > max ? `${s.slice(0, max - 1)}…` : s
  }
  return typeof v === 'string' ? v : String(v)
}

const short = (v: J): string => (Array.isArray(v) ? `[${v.length}]` : v && typeof v === 'object' ? `{${Object.keys(v).length}}` : typeof v === 'string' ? (v.length > 30 ? `${v.slice(0, 29)}…` : v) : String(v))
