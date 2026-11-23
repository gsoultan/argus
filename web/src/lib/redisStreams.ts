/*
A Redis stream's consumer groups in the key editor. Reading them is the
agent's stream_groups; every change is an edit's command line (XGROUP, XACK,
XCLAIM), staged, recorded and with an undo copy like any other edit.
*/

export interface StreamGroup {
  name: string
  consumers: number
  pending: number
  lastDelivered: string
  /** Entries after the last delivered one; absent where Redis does not know (before 7). */
  behind?: number
  entriesRead?: number
}

export interface StreamConsumer { name: string; pending: number; idleMs: number; inactiveMs?: number }
export interface StreamPending { id: string; consumer: string; idleMs: number; deliveries: number }

export interface StreamGroups {
  groups: StreamGroup[]
  group?: string
  consumers?: StreamConsumer[]
  pending?: StreamPending[]
  pendingMore?: boolean
}

/** Where a new group starts, or where a group is moved to. */
export type GroupFrom = 'start' | 'now' | 'id'

const ID_RE = /^\d+(-\d+)?$/

/** Whether s is a stream entry ID: ms or ms-seq. */
export const validEntryId = (s: string) => ID_RE.test(s)

/** The ID XGROUP takes for a place. */
export function placeId(from: GroupFrom, id: string): string | null {
  if (from === 'start') return '0'
  if (from === 'now') return '$'
  return validEntryId(id.trim()) ? id.trim() : null
}

export const newGroupCmd = (key: string, group: string, id: string, mkstream = false) =>
  ['XGROUP', 'CREATE', key, group, id, ...(mkstream ? ['MKSTREAM'] : [])]
export const moveGroupCmd = (key: string, group: string, id: string) => ['XGROUP', 'SETID', key, group, id]
export const deleteGroupCmd = (key: string, group: string) => ['XGROUP', 'DESTROY', key, group]
export const removeConsumerCmd = (key: string, group: string, consumer: string) => ['XGROUP', 'DELCONSUMER', key, group, consumer]
export const ackCmd = (key: string, group: string, ids: string[]) => ['XACK', key, group, ...ids]
export const claimCmd = (key: string, group: string, to: string, minIdleMs: number, ids: string[]) =>
  ['XCLAIM', key, group, to, String(Math.max(0, Math.floor(minIdleMs))), ...ids]

/** The pending entries of a consumer idle for at least minIdleMs, to claim. */
export function claimable(pending: StreamPending[], from: string, minIdleMs: number): string[] {
  return pending.filter((p) => p.consumer === from && p.idleMs >= minIdleMs).map((p) => p.id)
}

/** "6 min", "3 s", "2 h": how long, short. */
export function idleWords(ms: number): string {
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s} s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h`
  return `${Math.floor(h / 24)} days`
}

/** A group name Redis takes and the editor can stage: no spaces or quotes. */
export function groupNameProblem(name: string, groups: StreamGroup[]): string | null {
  if (!name) return 'Give it a name.'
  if (!/^[^\s"'\\]{1,200}$/.test(name)) return 'A name has no spaces or quotes, up to 200 characters.'
  if (groups.some((g) => g.name === name)) return `There is a group named ${name} already.`
  return null
}

/** Stuck work: the consumer holding the most pending entries idle for over five minutes. */
export function stuckWork(consumers: StreamConsumer[]): StreamConsumer | null {
  const stuck = consumers.filter((c) => c.pending > 0 && c.idleMs >= 5 * 60_000).sort((a, b) => b.pending - a.pending)
  return stuck[0] ?? null
}

/** How many entries the groups are behind, all told, where known. */
export function totalBehind(groups: StreamGroup[]): number {
  return groups.reduce((n, g) => n + (g.behind ?? 0), 0)
}
