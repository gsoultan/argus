import type { ChangeApproval, Database, DBLabel } from '~/types/domain'

/*
Approval of changes on a database, as the console shows it.

Whether a change waits is the database's own switch (policy.writeApproval),
which starts at its label's default: on for production, off for staging and
development. The control plane decides (statementGate); this is the same rule
for what the console says before anyone runs anything.

The impact is what the dry run counted when the person asked: per table, and
the first rows each statement changes. It is stored with the ask, so the
approver sees what the asker saw, and the run stops if it would change more.
*/

/**
 * Whether a statement that changes db waits for someone else's approval. For
 * a Redis: whether its bulk, configuration and user changes wait.
 */
export function writesNeedApproval(db: Pick<Database, 'engine' | 'label' | 'policy'>): boolean {
  if (db.engine !== 'postgres' && db.engine !== 'mysql' && db.engine !== 'redis') return false
  return db.policy?.writeApproval ?? db.label === 'production'
}

/** Whether the switch is where the label puts it. */
export function approvalIsDefault(db: Pick<Database, 'policy'>): boolean {
  return db.policy?.writeApproval === undefined
}

export const LABEL_NAME: Record<DBLabel, string> = { production: 'Production', staging: 'Staging', development: 'Development' }

/** Who can decide, in words, for the prompt and the asker's wait. */
export function deciderWords(dbName: string, a: ChangeApproval | null, me?: string): string {
  if (!a || a.approvers !== 'editors') return 'an admin or an approver'
  const others = a.editors.filter((e) => e.toLowerCase() !== me?.toLowerCase())
  if (others.length === 0) return `an admin or an approver (no other editor of ${dbName} can yet)`
  return `an admin, an approver or another editor of ${dbName}`
}

/** A person's short name: what comes before the @. */
export function shortName(email: string): string {
  const at = email.indexOf('@')
  return at > 0 ? email.slice(0, at) : email
}

/** Two letters for an avatar. */
export function initials(email: string): string {
  return shortName(email).slice(0, 2).toUpperCase()
}

/* ── Impact ──────────────────────────────────────────────────────────────── */

export interface ImpactTable { name: string; inserted?: number; updated?: number; deleted?: number }
export interface ImpactPreview {
  /** insert, update or delete. */
  change: string
  columns: string[]
  rows: (string | null)[][]
}
/** What an ask's summary says it changes. */
export interface ChangeImpact {
  /** Every row the statements change, cascades included. */
  rows?: number
  tables?: ImpactTable[]
  /** The first rows, statement by statement, at most PREVIEW_ROWS in all. */
  preview?: ImpactPreview[]
  /** Set when the counts come from a dry run, not a guess. */
  checked?: boolean
}

export const PREVIEW_ROWS = 20
/** More rows than this is shown as a big change. */
export const BIG_CHANGE = 1000

/** The check answer's parts the impact reads. */
export interface CheckLike {
  rows: number
  tables?: ImpactTable[]
  preview?: ImpactPreview | null
}

/** Reads an ask's summary, whatever shape an older console stored. */
export function impactOf(summary: Record<string, unknown> | undefined | null): ChangeImpact {
  if (!summary || typeof summary !== 'object') return {}
  const out: ChangeImpact = {}
  if (typeof summary.rows === 'number') out.rows = summary.rows
  if (Array.isArray(summary.tables)) {
    out.tables = (summary.tables as unknown[]).filter((t): t is ImpactTable =>
      !!t && typeof t === 'object' && typeof (t as ImpactTable).name === 'string')
  }
  if (Array.isArray(summary.preview)) {
    out.preview = (summary.preview as unknown[]).filter((p): p is ImpactPreview =>
      !!p && typeof p === 'object' && Array.isArray((p as ImpactPreview).columns) && Array.isArray((p as ImpactPreview).rows))
  }
  if (summary.checked === true) out.checked = true
  return out
}

/** Adds up the dry runs of an ask's statements into one impact. */
export function combineChecks(checks: (CheckLike | null)[]): ChangeImpact {
  const tables = new Map<string, ImpactTable>()
  const preview: ImpactPreview[] = []
  let rows = 0
  let left = PREVIEW_ROWS
  let any = false
  for (const c of checks) {
    if (!c) continue
    any = true
    const touched = c.tables ?? []
    for (const t of touched) {
      const cur = tables.get(t.name) ?? { name: t.name }
      cur.inserted = (cur.inserted ?? 0) + (t.inserted ?? 0)
      cur.updated = (cur.updated ?? 0) + (t.updated ?? 0)
      cur.deleted = (cur.deleted ?? 0) + (t.deleted ?? 0)
      tables.set(t.name, cur)
    }
    rows += touched.length
      ? touched.reduce((n, t) => n + (t.inserted ?? 0) + (t.updated ?? 0) + (t.deleted ?? 0), 0)
      : c.rows
    if (c.preview && c.preview.rows.length && left > 0) {
      const kept = c.preview.rows.slice(0, left)
      left -= kept.length
      preview.push({ change: c.preview.change, columns: c.preview.columns, rows: kept })
    }
  }
  if (!any) return {}
  const list = [...tables.values()].map((t) => ({
    name: t.name,
    ...(t.inserted ? { inserted: t.inserted } : {}),
    ...(t.updated ? { updated: t.updated } : {}),
    ...(t.deleted ? { deleted: t.deleted } : {}),
  }))
  return { rows, tables: list, preview, checked: true }
}

const plural = (n: number, one: string, many = one + 's') => `${n.toLocaleString()} ${n === 1 ? one : many}`

/** "18 rows · 2 tables", or "" when nothing was counted. */
export function impactWords(i: ChangeImpact): string {
  if (i.rows === undefined) return ''
  const t = i.tables?.length ?? 0
  return t > 1 ? `${plural(i.rows, 'row')} · ${plural(t, 'table')}` : plural(i.rows, 'row')
}

/** A table's name without the public schema. */
export function tableName(name: string): string {
  return name.startsWith('public.') ? name.slice(7) : name
}

/** The impact as stat tiles: updated, deleted and inserted, each with its tables. */
export function impactTiles(i: ChangeImpact): { n: number; what: 'updated' | 'deleted' | 'inserted'; tables: string[] }[] {
  const out: { n: number; what: 'updated' | 'deleted' | 'inserted'; tables: string[] }[] = []
  for (const what of ['updated', 'deleted', 'inserted'] as const) {
    const ts = (i.tables ?? []).filter((t) => (t[what] ?? 0) > 0)
    if (ts.length) out.push({ n: ts.reduce((n, t) => n + (t[what] ?? 0), 0), what, tables: ts.map((t) => tableName(t.name)) })
  }
  return out
}
