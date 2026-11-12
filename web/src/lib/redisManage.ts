import type { Database, DBLevel } from '~/types/domain'
import type { RedisKey } from '~/components/useStoreSession'

/*
Managing a Redis from the console: the shapes the agent answers with
(internal/agent/redis_manage.go), Argus's own ACL rules per level
(tunnel.RedisACLRules), what the overview warns about, and the small pieces
the editors share. The agent and the control plane decide; nothing here is a
check, only what the console shows before it asks.
*/

export interface RedisSlow { id: number; at: number; micros: number; command: string[]; client?: string; name?: string }
export interface RedisInfoData { info: Record<string, string>; slowlog: RedisSlow[]; openDefault: boolean; drift: string[] }
export interface RedisCountData {
  count: number; bytes: number; types: Record<string, number>; sample: RedisKey[]; digest: string; more?: boolean
}
export interface RedisBulkResult { changed: number; matched: number; undoId?: string; stopped?: string }
export interface RedisEditResult { ran: number; undoId?: string; noUndo?: string }
export interface RedisUndo { id: string; what: string; at: number; keys: number; bytes: number; expires: number }
export interface RedisSetting {
  name: string; value: string; file?: string; inFile: boolean; default?: string; group: string
  settable: boolean; secret?: boolean; set?: boolean; drift?: boolean
}
export interface RedisConfigData { settings: RedisSetting[]; file: string; fileRead: boolean; aclFile: boolean }
export interface RedisConfigResult { before: Record<string, string>; after: Record<string, string>; rewritten: boolean; rewriteError?: string }
export interface RedisUser {
  name: string; on: boolean; nopass: boolean; passwords: string[]; keys: string[]; channels: string[]
  commands: string[]; selectors: string[]; argus: boolean; self: boolean; clients: number
}
export interface RedisUsersData { users: RedisUser[]; aclFile: boolean }
export interface RedisUserResult { saved: string; saveError?: string }
export interface RedisDryRun { allowed: boolean; reason?: string }
export interface RedisApprovalNeed { text: string; cap?: number }

/** Argus's own rules per level, as the agent gives them: tunnel.RedisLevelRules. */
export const DEFAULT_LEVEL_RULES: Record<DBLevel, string[]> = {
  read: ['~*', '-@all', '+@read', '+@connection', '-@dangerous'],
  write: ['~*', '-@all', '+@read', '+@write', '+@connection', '+@transaction', '-@dangerous'],
  owner: ['~*', '+@all', '-@admin', '-flushall'],
  dba: ['~*', '+@all'],
}

/** The rules a level's user gets on db: its own, or Argus's. */
export function levelRules(db: Pick<Database, 'policy'>, level: DBLevel): string[] {
  return db.policy?.redisLevels?.[level] ?? DEFAULT_LEVEL_RULES[level]
}

export type RuleKind = 'key' | 'channel' | 'command'

export function ruleKind(r: string): RuleKind {
  if (r.startsWith('~') || r.startsWith('%') || r === 'allkeys') return 'key'
  if (r.startsWith('&') || r === 'allchannels') return 'channel'
  return 'command'
}

/** A key rule in words: "read + write  cart:*", "read  *". */
export function keyRuleWords(r: string): string {
  if (r === 'allkeys' || r === '~*') return 'read + write  *'
  const m = /^%(RW|R|W)~(.*)$/.exec(r)
  if (m) return `${m[1] === 'R' ? 'read' : m[1] === 'W' ? 'write' : 'read + write'}  ${m[2]}`
  return `read + write  ${r.replace(/^~/, '')}`
}

const ADMIN_ONLY = ['+@admin', 'allcommands', '+acl', '+config', '+shutdown', '+debug', '+module', '+replicaof',
  '+slaveof', '+failover', '+flushall', '+save', '+bgsave', '+bgrewriteaof']

/**
 * Why rules would be refused, or null: the console's copy of
 * tunnel.ValidRedisRules, so a mistake shows as it is typed. level is the
 * Argus level they are for, or undefined for a server user.
 */
export function rulesProblem(rules: string[], level?: DBLevel): string | null {
  if (rules.length === 0) return 'at least one rule'
  if (rules.length > 64) return 'at most 64 rules'
  let all = false
  let minusAdmin = false
  for (const r of rules) {
    if (!r || r.length > 200 || /[\s()]/.test(r)) return `a rule is one word: "${r}"`
    const ok = ['allkeys', 'allchannels', 'allcommands', 'nocommands'].includes(r)
      || /^(~|%R~|%W~|%RW~)./.test(r) || /^&./.test(r)
      || /^[+-](@[a-z]+|[a-z][a-z0-9_-]*(\|[a-z0-9_-]+)?)$/.test(r.toLowerCase())
    if (!ok) return `not a rule Argus writes: ${r}`
    const lr = r.toLowerCase()
    if (lr === '+@all' || lr === 'allcommands') all = true
    if (lr === '-@admin') minusAdmin = true
    if (level && level !== 'dba' && lr !== '+@all' && ADMIN_ONLY.includes(lr)) {
      return `${r} is DBA's; below DBA no rule may reach the server's configuration or users`
    }
  }
  if (level && level !== 'dba' && all && !minusAdmin) return 'below DBA, +@all needs -@admin with it'
  return null
}

/** A number from INFO, or undefined. */
export function infoNum(info: Record<string, string> | undefined, key: string): number | undefined {
  const v = info?.[key]
  if (v === undefined || v === '') return undefined
  const n = Number(v)
  return Number.isFinite(n) ? n : undefined
}

export function bytesText(n: number | undefined): string {
  if (n === undefined) return '—'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${u[i]}`
}

export function ttlText(s: number): string {
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  if (s < 86400) return `${Math.round(s / 3600)}h`
  return `${Math.round(s / 86400)}d`
}

/** Keyspace lines of INFO: db0:keys=1,expires=0,avg_ttl=0. */
export function keyspace(info: Record<string, string> | undefined): { db: number; keys: number; expires: number; avgTtl: number }[] {
  const out: { db: number; keys: number; expires: number; avgTtl: number }[] = []
  for (const [k, v] of Object.entries(info ?? {})) {
    const m = /^db(\d+)$/.exec(k)
    if (!m) continue
    const f = Object.fromEntries(v.split(',').map((p) => p.split('=') as [string, string]))
    out.push({ db: Number(m[1]), keys: Number(f.keys ?? 0), expires: Number(f.expires ?? 0), avgTtl: Number(f.avg_ttl ?? 0) })
  }
  return out.sort((a, b) => a.db - b.db)
}

export type Tone = 'rose' | 'amber' | 'azure'
export interface Attention { tone: Tone; title: string; detail: string; go?: 'config' | 'users' | 'slowlog' }

/**
 * What the overview warns about, from data it already has: memory near
 * maxmemory, settings a restart would undo, the default user open, a failed
 * save, the slow log. Rules, not guesses: each says what it read.
 */
export function attention(d: RedisInfoData | null, slowOverMs = 100): Attention[] {
  if (!d) return []
  const out: Attention[] = []
  const used = infoNum(d.info, 'used_memory')
  const max = infoNum(d.info, 'maxmemory')
  const policy = d.info.maxmemory_policy
  if (used !== undefined && max) {
    const pct = used / max
    if (pct >= 0.8) {
      out.push({ tone: pct >= 0.95 ? 'rose' : 'amber', go: 'config',
        title: `Memory is at ${Math.round(pct * 100)}% of maxmemory`,
        detail: `${bytesText(used)} of ${bytesText(max)}. At the limit, ${policy === 'noeviction'
          ? 'writes are refused (noeviction)' : `${policy} evicts keys`}.` })
    }
  } else if (used !== undefined && max === 0 && d.info.role === 'master') {
    out.push({ tone: 'azure', go: 'config', title: 'No maxmemory is set',
      detail: `Redis can grow until the server runs out of memory (${bytesText(used)} now).` })
  }
  if (d.drift.length > 0) {
    out.push({ tone: 'rose', go: 'config',
      title: `${d.drift.length === 1 ? '1 setting differs' : `${d.drift.length} settings differ`} from the config file`,
      detail: `${d.drift.slice(0, 3).join(', ')}${d.drift.length > 3 ? '…' : ''}: a restart would undo ${d.drift.length === 1 ? 'it' : 'them'}.` })
  }
  if (d.openDefault) {
    out.push({ tone: 'rose', go: 'users', title: 'User default has no password',
      detail: 'Anyone who reaches this Redis signs in as default with every permission it has.' })
  }
  if (d.info.rdb_last_bgsave_status && d.info.rdb_last_bgsave_status !== 'ok') {
    out.push({ tone: 'rose', go: 'config', title: 'The last save failed', detail: 'RDB bgsave reported an error; check the server log and free disk.' })
  }
  if (d.info.aof_last_write_status && d.info.aof_last_write_status !== 'ok') {
    out.push({ tone: 'rose', go: 'config', title: 'AOF writes are failing', detail: 'Redis could not write its append-only file.' })
  }
  const slow = d.slowlog.filter((s) => s.micros >= slowOverMs * 1000)
  if (slow.length > 0) {
    const worst = slow.reduce((a, b) => (b.micros > a.micros ? b : a))
    out.push({ tone: 'azure', go: 'slowlog', title: `${slow.length} slow command${slow.length === 1 ? '' : 's'} in the slow log`,
      detail: `${worst.command.slice(0, 3).join(' ')} took ${Math.round(worst.micros / 1000)} ms${worst.client ? `, from ${worst.client}` : ''}.` })
  }
  return out
}

/** What is wrong with a server user, in words; null when nothing is. */
export function userFinding(u: RedisUser): { tone: Tone; text: string } | null {
  if (u.argus) return null
  if (u.on && u.nopass) return { tone: 'rose', text: 'No password: anyone who reaches Redis signs in as it' }
  const all = u.commands.includes('+@all') && !u.commands.includes('-@admin') && !u.commands.includes('-@dangerous')
  if (u.on && all) return { tone: 'amber', text: 'Can run everything, CONFIG, FLUSHALL and ACL included' }
  if (!u.on && u.clients === 0) return { tone: 'azure', text: 'Off, and nothing is signed in as it' }
  if (u.on && u.passwords.length === 0 && !u.nopass) return { tone: 'azure', text: 'On, but has no password, so nobody can sign in' }
  return null
}

export const SETTING_GROUPS: { id: string; label: string }[] = [
  { id: 'memory', label: 'Memory & eviction' },
  { id: 'persistence', label: 'Persistence' },
  { id: 'replication', label: 'Replication' },
  { id: 'clients', label: 'Clients & network' },
  { id: 'slowlog', label: 'Slow log & latency' },
  { id: 'security', label: 'Security' },
  { id: 'advanced', label: 'Advanced' },
  { id: 'locked', label: 'Locked' },
]

/** A warning for a value before it is applied, or null. */
export function settingWarning(name: string, value: string): string | null {
  const v = value.trim().toLowerCase()
  if (name === 'appendfsync' && v === 'always') return 'Every write waits for the disk; expect writes to be several times slower.'
  if (name === 'maxmemory-policy' && v === 'noeviction') return 'At maxmemory, writes are refused instead of evicting keys.'
  if (name === 'save' && v === '') return 'No RDB snapshots: with AOF off too, a restart loses everything.'
  if (name === 'appendonly' && v === 'no') return 'Turning AOF off leaves only RDB snapshots: writes since the last one are lost on a crash.'
  if (name === 'protected-mode' && v === 'no') return 'Redis will answer clients from other machines even without a password set.'
  if (name === 'maxmemory' && (v === '0' || v === '')) return 'No limit: Redis can grow until the server runs out of memory.'
  return null
}

/** SHA-256 of a password, in hex, as ACL SETUSER #… takes it. */
export async function sha256Hex(text: string): Promise<string> {
  const sum = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text))
  return Array.from(new Uint8Array(sum), (b) => b.toString(16).padStart(2, '0')).join('')
}

/** The short form the agent shows a password's hash in. */
export function hashRef(hex: string): string {
  const h = hex.toLowerCase()
  return h.length < 16 ? h : `${h.slice(0, 8)}…${h.slice(-8)}`
}

/** A Redis user name Argus may make or change: not one of its own. */
export function userNameProblem(name: string): string | null {
  if (!/^[A-Za-z0-9_.-]{1,64}$/.test(name)) return 'Letters, digits, ".", "-" and "_", up to 64'
  if (name.startsWith('argus-')) return 'Names starting with argus- are Argus’s own session users'
  return null
}

/** Templates for a new user's rules. */
export const USER_TEMPLATES: { id: string; label: string; rules: string[] }[] = [
  { id: 'read', label: 'Read only', rules: ['~*', '+@read', '+@connection', '-@dangerous'] },
  { id: 'write', label: 'Read & write', rules: ['~*', '+@read', '+@write', '+@connection', '+@transaction', '-@dangerous'] },
  { id: 'app', label: 'Everything but admin', rules: ['~*', '&*', '+@all', '-@admin', '-@dangerous'] },
]

/** One folder or key of the key tree. */
export interface TreeNode { kind: 'folder' | 'key'; path: string; label: string; count: number; key?: RedisKey }

/**
 * The listed keys grouped on a separator, one level at a time: under prefix,
 * the folders (with how many listed keys they hold) and the keys themselves.
 */
export function treeLevel(keys: RedisKey[], prefix: string, sep = ':'): TreeNode[] {
  const folders = new Map<string, number>()
  const leaves: TreeNode[] = []
  for (const k of keys) {
    if (!k.name.startsWith(prefix)) continue
    const rest = k.name.slice(prefix.length)
    const i = rest.indexOf(sep)
    if (i > 0 && i < rest.length - 1) {
      const f = rest.slice(0, i)
      folders.set(f, (folders.get(f) ?? 0) + 1)
    } else {
      leaves.push({ kind: 'key', path: k.name, label: rest || k.name, count: 1, key: k })
    }
  }
  const fs: TreeNode[] = [...folders.entries()].sort(([a], [b]) => a.localeCompare(b))
    .map(([f, n]) => ({ kind: 'folder', path: prefix + f + sep, label: f, count: n }))
  return [...fs, ...leaves.sort((a, b) => a.label.localeCompare(b.label))]
}

/** A Redis argument as redis-cli would read it back. */
export function redisQuote(s: string): string {
  return /^[^\s"'\\]+$/.test(s) && s !== '' ? s : `"${s.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\n/g, '\\n')}"`
}

/** An edit's commands as one text, as the pending bar shows them. */
export function editText(edits: string[][]): string {
  return edits.map((e) => e.map(redisQuote).join(' ')).join('\n')
}

/** Seconds for an amount of a unit. */
export function unitSeconds(n: number, unit: 'seconds' | 'minutes' | 'hours' | 'days'): number {
  return Math.round(n * { seconds: 1, minutes: 60, hours: 3600, days: 86400 }[unit])
}
