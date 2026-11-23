/*
The Users tab of a RabbitMQ entry in the console's terms: the shapes the
agent answers with (internal/agent/rabbitusers.go), a user's three
permission patterns read as words, the patterns a new user's choices make,
the queues and exchanges they match now, and the connection details shown
once a user is made. All pure.

A user is the node's and an entry is a vhost: the agent sends what it has
here and names the other vhosts it has permissions on.
*/

export interface RabbitPerms { configure: string; write: string; read: string }
export interface RabbitTopicPerm { exchange: string; write: string; read: string }
export interface RabbitLimits { maxConnections?: number; maxChannels?: number }

export interface RabbitUser {
  name: string
  tags: string[]
  perms: RabbitPerms
  topicPerms: RabbitTopicPerm[]
  limits: RabbitLimits
  elsewhere?: string[]
  connections: number
  hasPassword: boolean
  /** Why it is shown and never changed here: a tag that reaches past one vhost. */
  locked?: string
  /** guest still signs in with guest. */
  defaultPassword?: boolean
}

export interface RabbitUsersData { users: RabbitUser[]; sessions: string[]; notShown: number; vhost: string; amqpPort: number; tlsPort?: number }
export interface RabbitLoginResult { user: string; address: string; ok: boolean; detail?: string }

const NOTHING = new Set(['', '^$'])
const EVERYTHING = new Set(['.*', '^.*$', '^.*', '.+'])

/** A pattern in words, as a list of what it covers. */
export function patternWords(p: string): string {
  if (NOTHING.has(p)) return 'nothing'
  if (EVERYTHING.has(p)) return 'everything'
  const alts = /^\^\(\?:(.*)\)$/.exec(p)?.[1]?.split('|') ?? (p.startsWith('^') && !/[|()]/.test(p) ? [p.slice(1)] : null)
  if (alts && alts.every((a) => /^[A-Za-z0-9_\-:/@\\.]*$/.test(a) && !/[^\\]\./.test(a.replace(/\\\./g, '')))) {
    return alts.map((a) => `starting ${unescape(a)}`).join(' or ')
  }
  return `matching ${p}`
}

function unescape(s: string): string { return s.replace(/\\(.)/g, '$1') }

/** What a user can do on the vhost, in words. */
export function permLines(p: RabbitPerms): { what: string; words: string }[] {
  return [
    { what: 'Publishes to and binds', words: patternWords(p.write) },
    { what: 'Reads from', words: patternWords(p.read) },
    { what: 'Declares and deletes', words: patternWords(p.configure) },
  ].filter((l) => l.words !== 'nothing')
}

/** The choices of a new user, as the form holds them. */
export interface RabbitPlan {
  what: 'publish' | 'consume' | 'both' | 'custom'
  /** Exchanges it publishes to, by prefix; '' is every exchange. */
  exchange: string
  /** Queues it reads, by prefix; '' is every queue. */
  queue: string
  /** It declares its own queues (and binds them) starting with queue. */
  declares: boolean
  custom: RabbitPerms
}

export const escapeRegex = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

/** One pattern from prefixes: nothing, everything, or names starting with any. */
export function prefixPattern(prefixes: (string | null)[]): string {
  const ps = prefixes.filter((p): p is string => p !== null)
  if (ps.length === 0) return '^$'
  if (ps.some((p) => p === '')) return '.*'
  const uniq = [...new Set(ps.map(escapeRegex))]
  return uniq.length === 1 ? `^${uniq[0]}` : `^(?:${uniq.join('|')})`
}

/** The permissions a new user's choices make. */
export function planPerms(p: RabbitPlan): RabbitPerms {
  if (p.what === 'custom') return p.custom
  const pub = p.what === 'publish' || p.what === 'both'
  const con = p.what === 'consume' || p.what === 'both'
  return {
    configure: prefixPattern([con && p.declares ? p.queue : null]),
    write: prefixPattern([pub ? p.exchange : null, con && p.declares ? p.queue : null]),
    read: prefixPattern([con ? p.queue : null, con && p.declares ? p.exchange : null]),
  }
}

/** Why a pattern is not one RabbitMQ will take, or null. Erlang's re is close to JavaScript's for what Argus writes. */
export function patternProblem(p: string): string | null {
  if (p.length > 512) return 'At most 512 characters'
  try { new RegExp(p); return null } catch { return 'Not a regular expression' }
}

/** The names a pattern matches now, from those the Queues and Routing tabs loaded. */
export function matching(p: string, names: string[]): string[] {
  if (NOTHING.has(p)) return []
  try {
    const re = new RegExp(p)
    return names.filter((n) => re.test(n)).sort()
  } catch { return [] }
}

/** Why a new user's name will not do, or null. */
export function rabbitUserNameProblem(name: string, existing: string[]): string | null {
  if (!name) return 'Name it'
  if (!/^[A-Za-z0-9._@-]{1,64}$/.test(name)) return 'Letters, digits, . _ @ and -, up to 64'
  if (name.startsWith('argus-')) return 'argus-… users are Argus’s own, made for each session'
  if (existing.includes(name)) return 'Already a user with access here: open it instead'
  return null
}

/** What needs saying about the users of a vhost. */
export function rabbitUserFindings(users: RabbitUser[]): { user: string; text: string }[] {
  const out: { user: string; text: string }[] = []
  for (const u of users) {
    if (u.defaultPassword) out.push({ user: u.name, text: `${u.name} still signs in with its default password. Give it a new one or delete it.` })
  }
  return out
}

export interface RabbitDetails { host: string; port: number; tls: boolean; vhost: string; user: string; password: string }

/** The amqp:// URL apps use, vhost and credentials encoded. */
export function amqpURL(d: RabbitDetails): string {
  return `${d.tls ? 'amqps' : 'amqp'}://${encodeURIComponent(d.user)}:${encodeURIComponent(d.password)}@${d.host}:${d.port}/${encodeURIComponent(d.vhost)}`
}

/** Ready client settings: the URL, or .env lines. */
export function rabbitSettings(d: RabbitDetails, kind: 'url' | 'env' | 'spring'): string {
  if (kind === 'url') return amqpURL(d)
  if (kind === 'spring') {
    return [
      `spring.rabbitmq.host=${d.host}`, `spring.rabbitmq.port=${d.port}`, `spring.rabbitmq.virtual-host=${d.vhost}`,
      `spring.rabbitmq.username=${d.user}`, `spring.rabbitmq.password=${d.password}`, ...(d.tls ? ['spring.rabbitmq.ssl.enabled=true'] : []),
    ].join('\n')
  }
  return [`AMQP_URL=${amqpURL(d)}`, `RABBITMQ_HOST=${d.host}`, `RABBITMQ_PORT=${d.port}`, `RABBITMQ_VHOST=${d.vhost}`,
    `RABBITMQ_USERNAME=${d.user}`, `RABBITMQ_PASSWORD=${d.password}`].join('\n')
}

export function limitWords(l: RabbitLimits): string {
  const parts = [l.maxConnections ? `at most ${l.maxConnections} connections` : '', l.maxChannels ? `at most ${l.maxChannels} channels` : ''].filter(Boolean)
  return parts.length ? parts.join(', ') : 'No limits'
}
