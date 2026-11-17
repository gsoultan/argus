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

export type AddressState = 'signin' | 'open' | 'protected' | 'closed'
export interface RedisAddress { ip: string; iface: string; prefix: string; private: boolean; state: AddressState; detail?: string }
export interface RedisNetworkData {
  port: number; tlsPort?: number; bind: string[]; protectedMode: boolean; defaultOpen: boolean; defaultClients: number
  configFile: string; agentConf: boolean; addresses: RedisAddress[]; firewall: '' | 'ufw' | 'firewalld'
  /** Who can connect now; absent from an older agent. */
  reach?: Reach
  firewallActive?: boolean
  /** The machines Argus's firewall rules let in, and whether one lets everyone in. */
  firewallSources?: string[]
  firewallAllowAll?: boolean
  /** Rules Argus did not add that mention the port; one may already let everyone in. */
  firewallOthers?: string[]
  firewallOpenToAll?: boolean
  firewallError?: string
  /** The server's name, which opening to anywhere is confirmed with. */
  host?: string
  /** Users that are on and need no password. */
  noPassUsers?: string[]
  clients?: number
}
export interface RedisNetworkResult {
  network: RedisNetworkData; rewritten: boolean; rewriteError?: string
  firewallRan?: string[]; firewallError?: string
}
export interface RedisSecureResult { network: RedisNetworkData; savedTo: string }
export interface RedisLogin { address: string; ok: boolean; detail?: string }
export interface RedisLoginResult { user: string; port: number; logins: RedisLogin[] }

export const ADDRESS_STATE: Record<AddressState, { label: string; tone: string }> = {
  signin: { label: 'Reachable, asks for a sign-in', tone: 'teal' },
  open: { label: 'Reachable with no sign-in', tone: 'rose' },
  protected: { label: 'Refused by protected mode', tone: 'amber' },
  closed: { label: 'Not listening', tone: 'slate' },
}

/** What the network can reach, in one line. */
export function networkSummary(n: RedisNetworkData): { tone: Tone | 'teal'; text: string } {
  const lan = n.addresses.filter((a) => a.state !== 'closed')
  if (lan.some((a) => a.state === 'open')) return { tone: 'rose', text: `Anyone who reaches ${lan.filter((a) => a.state === 'open').map((a) => a.ip).join(', ')} can use Redis with no password.` }
  if (lan.some((a) => a.state === 'protected')) return { tone: 'amber', text: 'Redis listens on the network, but protected mode refuses every client there until the default user has a password.' }
  if (lan.length === 0) return { tone: 'azure', text: `Only programs on this server can connect: Redis listens on ${n.bind.join(' ') || 'its defaults'}.` }
  return { tone: 'teal', text: `Clients on the LAN can connect to ${lan.map((a) => `${a.ip}:${n.port}`).join(', ')} with a user and password.` }
}

/** The firewall command that lets a LAN reach the port, for the firewall found. */
export function firewallCommand(fw: RedisNetworkData['firewall'], prefix: string, port: number): string | null {
  if (fw === 'ufw') return `sudo ufw allow from ${prefix} to any port ${port} proto tcp`
  if (fw === 'firewalld') {
    const fam = prefix.includes(':') ? 'ipv6' : 'ipv4'
    return `sudo firewall-cmd --permanent --add-rich-rule='rule family="${fam}" source address="${prefix}" port port="${port}" protocol="tcp" accept' && sudo firewall-cmd --reload`
  }
  return null
}

/** How an application on the LAN checks it can sign in. */
export function connectCommand(ip: string, port: number, user: string): string {
  const host = ip.includes(':') ? `[${ip}]` : ip
  return `redis-cli -h ${ip} -p ${port} --user ${user} --askpass PING   # or redis://${user}:<password>@${host}:${port}/0`
}

/*
Who can connect: the four answers, and the plan of what Argus changes on the
server for one of them. The agent decides and refuses on its own; this only
says beforehand what it will refuse and what it will run, in the same words.
*/

export type Reach = 'local' | 'selected' | 'network' | 'any'

export const REACH: Record<Reach, { label: string; tone: string }> = {
  local: { label: 'This server only', tone: 'amber' },
  selected: { label: 'Selected machines', tone: 'azure' },
  network: { label: 'The private network', tone: 'azure' },
  any: { label: 'Anywhere', tone: 'rose' },
}

function v4(s: string): number | null {
  const p = s.split('.')
  if (p.length !== 4) return null
  let n = 0
  for (const x of p) {
    if (!/^\d{1,3}$/.test(x) || Number(x) > 255) return null
    n = n * 256 + Number(x)
  }
  return n
}

/** One source as the agent writes it, or why it is not one. */
export function parseSource(raw: string): { source: string } | { error: string } {
  const s = raw.trim()
  const [addr, bitsText, extra] = s.split('/')
  if (!addr || extra !== undefined) return { error: `${s || 'That'} is not an address or a network like 10.0.4.0/24` }
  const n4 = v4(addr)
  const is6 = n4 === null && /^[0-9a-fA-F:]+$/.test(addr) && addr.includes(':')
  if (n4 === null && !is6) return { error: `${s} is not an address or a network like 10.0.4.0/24` }
  const max = n4 !== null ? 32 : 128
  let bits = max
  if (bitsText !== undefined) {
    if (!/^\d{1,3}$/.test(bitsText) || Number(bitsText) > max) return { error: `${s} is not an address or a network like 10.0.4.0/24` }
    bits = Number(bitsText)
  }
  if (bits === 0) return { error: `${s} is every address; choose Anywhere instead` }
  if ((n4 !== null && addr.startsWith('127.')) || addr === '::1') return { error: `${s} is this server itself, which can always connect` }
  if (n4 !== null && bits < 32) {
    const mask = bits === 0 ? 0 : (0xffffffff << (32 - bits)) >>> 0
    const net = (n4 & mask) >>> 0
    return { source: `${[24, 16, 8, 0].map((sh) => (net >>> sh) & 255).join('.')}/${bits}` }
  }
  return { source: bits === max ? addr : `${addr}/${bits}` }
}

/** Whether an IPv4 address or network lies in a prefix; false for IPv6. */
export function inPrefix(source: string, prefix: string): boolean {
  const [a, b] = source.split('/')
  const [pa, pb] = prefix.split('/')
  const n = v4(a ?? ''), p = v4(pa ?? '')
  if (n === null || p === null) return false
  const bits = Number(pb ?? 32), sbits = Number(b ?? 32)
  if (sbits < bits) return false
  const mask = bits === 0 ? 0 : (0xffffffff << (32 - bits)) >>> 0
  return ((n & mask) >>> 0) === ((p & mask) >>> 0)
}

/**
 * The addresses of this server Redis listens on for a choice: for selected
 * machines, the private addresses on the same network as one of them (all
 * private ones when none is, as for a machine reached through a router); for
 * the private network, every private address.
 */
/** What every engine's "which machines can connect" view shares. */
export interface ReachView {
  port: number
  addresses: RedisAddress[]
  firewall: '' | 'ufw' | 'firewalld'
  firewallActive?: boolean
  firewallSources?: string[]
  firewallAllowAll?: boolean
  firewallOthers?: string[]
  firewallOpenToAll?: boolean
  firewallError?: string
  host?: string
}

export function listenFor(n: ReachView, reach: Reach, sources: string[]): string[] {
  const priv = n.addresses.filter((a) => a.private && !a.ip.includes(':'))
  if (reach === 'network') return priv.map((a) => a.ip)
  if (reach !== 'selected') return []
  const near = priv.filter((a) => sources.some((s) => inPrefix(s, a.prefix)))
  return (near.length ? near : priv).map((a) => a.ip)
}

/** The networks "the private network" lets in. */
export function privateNetworks(n: ReachView): string[] {
  return [...new Set(n.addresses.filter((a) => a.private && !a.ip.includes(':')).map((a) => a.prefix))]
}

/** The bind a choice writes, as tunnel.RedisBindFor does. */
export function bindFor(reach: Reach, listen: string[]): string {
  if (reach === 'any') return '* -::*'
  return ['127.0.0.1', '-::1', ...(reach === 'local' ? [] : listen.map((a) => `-${a}`))].join(' ')
}

/** The firewall commands a choice runs, for the firewall the agent found. */
export function firewallLines(n: ReachView, reach: Reach, sources: string[]): string[] {
  const p = n.port
  const mine = (n.firewallSources?.length ?? 0) + (n.firewallAllowAll ? 1 : 0)
  const out: string[] = []
  if (n.firewall === 'ufw') {
    if (mine) out.push(`remove Argus's earlier rules for ${p} (comment "argus ${p}")`)
    if (reach === 'selected' || reach === 'network') {
      for (const s of sources) out.push(`ufw allow from ${s} to any port ${p} proto tcp comment 'argus ${p}'`)
      out.push(`ufw deny proto tcp to any port ${p} comment 'argus ${p}'`)
    }
    if (reach === 'any') out.push(`ufw allow proto tcp to any port ${p} comment 'argus ${p}'`)
  } else if (n.firewall === 'firewalld') {
    if (mine) out.push(`remove the rules Argus added for ${p}`)
    if (reach === 'selected' || reach === 'network') {
      for (const s of sources) {
        out.push(`firewall-cmd --add-rich-rule='rule family="${s.includes(':') ? 'ipv6' : 'ipv4'}" source address="${s}" port port="${p}" protocol="tcp" accept'`)
      }
    }
    if (reach === 'any' && !n.firewallOpenToAll) out.push(`firewall-cmd --add-port=${p}/tcp`)
  }
  return out
}

export interface PlanStep {
  state: 'ok' | 'change' | 'skip' | 'blocked' | 'warn'
  title: string
  detail?: string
  diff?: { from: string; to: string }
  lines?: string[]
}

/**
 * What Argus will change on the server for a choice, step by step, and the
 * first thing that stops it, if anything does.
 */
export function reachPlan(n: RedisNetworkData, reach: Reach, sources: string[], version?: string): { steps: PlanStep[]; blocked: string | null } {
  const steps: PlanStep[] = []
  const host = n.host || 'this server'
  const listen = listenFor(n, reach, sources)
  const bind = bindFor(reach, listen)
  const now = n.bind.join(' ')
  let blocked: string | null = null
  const block = (why: string) => { blocked ??= why }

  if (reach !== 'local' && n.firewallError) block(`the firewall could not be read: ${n.firewallError}`)
  const major = Number((version ?? '').split('.')[0])
  if (major > 0 && major < 7) block(`Redis ${version} cannot change bind while it runs, and Argus does not restart Redis yet; set bind in its config file and restart it on the server`)
  if (reach === 'selected' && sources.length === 0) block('add at least one machine')
  if (reach === 'network' && privateNetworks(n).length === 0) block(`${host} has no private address to listen on`)

  steps.push(now === bind
    ? { state: 'ok', title: `Redis already listens on ${reach === 'any' ? 'every address' : bind}` }
    : { state: 'change', title: reach === 'local' ? `Listen on ${host} only` : reach === 'any' ? 'Listen on every address' : `Listen on ${listen.join(', ')}`,
      detail: 'Changed while Redis runs (CONFIG SET bind), then saved to its config file.', diff: { from: `bind ${now}`, to: `bind ${bind}` } })
  steps.push({ state: 'ok', title: 'Protected mode stays on', detail: 'Argus never turns it off.' })

  if (reach !== 'local') {
    if (n.defaultOpen) {
      block('give the default user a password first')
      steps.push({ state: 'blocked', title: 'The default user has no password', detail: 'Anyone who reaches Redis would be admin. Give it a password first.' })
    } else if (reach === 'any' && (n.noPassUsers?.length ?? 0) > 0) {
      const u = n.noPassUsers!.join(', ')
      block(`${u} ${n.noPassUsers!.length === 1 ? 'has' : 'have'} no password`)
      steps.push({ state: 'blocked', title: 'Every user needs a password', detail: `${u} can sign in with none. Give ${n.noPassUsers!.length === 1 ? 'it' : 'each'} a password or turn ${n.noPassUsers!.length === 1 ? 'it' : 'them'} off first.` })
    } else {
      steps.push({ state: 'ok', title: 'A password is required', detail: reach === 'any' ? 'The default user and every other user that is on have one.' : 'The default user has one.' })
    }
  }

  const fw = n.firewall
  const lines = firewallLines(n, reach, reach === 'network' ? privateNetworks(n) : sources)
  if (!fw || !n.firewallActive) {
    if (reach === 'selected') {
      block('no firewall is on, so Redis cannot be limited to selected machines')
      steps.push({ state: 'blocked', title: 'No firewall is on', detail: `Argus changes ufw or firewalld and never turns one on, since that can cut off SSH. Choose the private network, or turn a firewall on on ${host} first.` })
    } else {
      steps.push({ state: 'skip', title: 'No firewall is on', detail: reach === 'local' ? undefined : 'Nothing on this server limits who reaches the port; only Redis\'s own sign-in does.' })
    }
  } else if (reach === 'selected' && n.firewallOpenToAll) {
    block('a firewall rule Argus did not add already lets everyone in')
    steps.push({ state: 'blocked', title: 'A rule already lets everyone in', detail: `Remove it on the server first: ${(n.firewallOthers ?? []).join('; ')}` })
  } else if (lines.length) {
    steps.push({ state: 'change', title: `Firewall on ${host} (${fw})`, lines,
      detail: reach === 'local' ? 'Only the rules Argus added are removed.' : reach === 'any' ? 'Lets everyone reach the port.' : 'Lets these in and refuses the port to everyone else.' })
  } else {
    steps.push({ state: 'ok', title: `Firewall on ${host} (${fw}): nothing to change` })
  }
  steps.push({ state: 'ok', title: 'No restart', detail: `Redis changes bind while it runs; ${n.clients ?? 'the'} connected client${n.clients === 1 ? '' : 's'} stay connected.` })
  return { steps, blocked }
}
