/*
The Users tab of a Kafka entry in the console's terms: the shapes the agent
answers with (internal/agent/kafkausers.go), ACLs read as words, the ACLs a
new user's choices make, and the client settings shown once a user is made.
All pure, so it is tested without a cluster.

An entry is a scope (orders.) and a user is cluster-wide: the agent sends the
ACLs inside the scope and, apart, those outside it, which are counted here and
never changed.
*/

export interface KafkaACL {
  resource: 'topic' | 'group' | 'transactional_id' | string
  pattern: 'LITERAL' | 'PREFIXED' | string
  name: string
  operation: string
  permission: 'ALLOW' | 'DENY' | string
  host: string
}

export interface KafkaQuotas { produceBytes?: number; consumeBytes?: number; requestPct?: number }

export interface KafkaUser {
  name: string
  /** SCRAM mechanisms; none is a principal that signs in another way, as a certificate's CN=… does. */
  mechanisms?: string[]
  acls: KafkaACL[]
  outside?: KafkaACL[]
  quotas: KafkaQuotas
  /** The user the agent signs in as: shown, never changed. */
  self?: boolean
}

export interface KafkaUsersData { users: KafkaUser[]; sessions: string[]; notShown: number; scope: string }
export interface KafkaUserResult { user: string; added?: number; removed?: number }
export interface KafkaLoginResult { user: string; address: string; ok: boolean; detail?: string; topics?: string[]; elsewhere?: number }

/** One Activity entry about a user of this entry. */
export interface BrokerUserEvent { user: string; action: 'broker.user.created' | 'broker.user.changed' | 'broker.user.deleted'; by: string; at: string; text: string }

export type KafkaCap = 'Produce' | 'Consume' | 'Alter topics' | 'Deny'

const ALTER_OPS = new Set(['CREATE', 'DELETE', 'ALTER', 'ALTER_CONFIGS'])

/** A topic ACL's topics in words: an exact name, "orders.*" for a prefix, "every topic" for *. */
export function topicTarget(a: KafkaACL): string {
  if (a.name === '*' && a.pattern === 'LITERAL') return 'every topic'
  return a.pattern === 'PREFIXED' ? `${a.name}*` : a.name
}

/** What a user can do, in words: the capabilities and the topics each covers. */
export function userCaps(acls: KafkaACL[]): { cap: KafkaCap; topics: string[] }[] {
  const by = new Map<KafkaCap, Set<string>>()
  const add = (c: KafkaCap, t: string) => { if (!by.has(c)) by.set(c, new Set()); by.get(c)!.add(t) }
  for (const a of acls) {
    if (a.resource !== 'topic') continue
    const t = topicTarget(a)
    if (a.permission === 'DENY') { add('Deny', t); continue }
    if (a.operation === 'READ') add('Consume', t)
    else if (a.operation === 'WRITE') add('Produce', t)
    else if (ALTER_OPS.has(a.operation)) add('Alter topics', t)
  }
  const order: KafkaCap[] = ['Produce', 'Consume', 'Alter topics', 'Deny']
  return order.filter((c) => by.has(c)).map((cap) => ({ cap, topics: [...by.get(cap)!].sort() }))
}

/** The consumer groups a user may use, as names or prefixes. */
export function userGroups(acls: KafkaACL[]): string[] {
  return [...new Set(acls.filter((a) => a.resource === 'group' && a.permission === 'ALLOW' && a.operation === 'READ')
    .map((a) => (a.name === '*' ? 'any group' : a.pattern === 'PREFIXED' ? `${a.name}…` : a.name)))].sort()
}

/** How a user signs in, in words. */
export function signsInWith(u: KafkaUser): string {
  if (u.mechanisms?.length) return u.mechanisms.join(', ')
  return u.name.includes('=') ? 'Certificate' : 'No password set'
}

/** An ACL as one line. */
export function aclLine(a: KafkaACL): string {
  return `${a.permission === 'DENY' ? 'Deny' : 'Allow'} ${a.operation} on ${a.resource.replace('_', ' ')} ${a.pattern === 'PREFIXED' ? 'starting' : ''} ${a.name}${a.host !== '*' ? ` from ${a.host}` : ''}`.replace(/\s+/g, ' ').trim()
}

export const aclKey = (a: KafkaACL) => [a.resource, a.pattern, a.name, a.operation, a.permission, a.host].join('\u0000')

/** What needs saying about the users of an entry, worst first. */
export function kafkaUserFindings(users: KafkaUser[], scope: string): { user: string; text: string }[] {
  const out: { user: string; text: string }[] = []
  for (const u of users) {
    const everything = [...u.acls, ...(u.outside ?? [])].some((a) => a.resource === 'topic' && a.permission === 'ALLOW' && a.name === '*' && a.pattern === 'LITERAL' && a.operation === 'READ')
    if (everything && scope !== '*') {
      out.push({ user: u.name, text: `${u.name} can read every topic on the cluster (an ACL on *), wider than this entry. Change it from a * entry.` })
    }
  }
  return out
}

/** The choices of a new user, as the form holds them. */
export interface UserPlan {
  what: 'produce' | 'consume' | 'both' | 'custom'
  match: 'prefix' | 'exact'
  topic: string
  groupPrefix: string
  transactions: boolean
  custom: KafkaACL[]
}

const allow = (resource: string, pattern: string, name: string, operation: string): KafkaACL =>
  ({ resource, pattern, name, operation, permission: 'ALLOW', host: '*' })

/** The ACLs a new user's choices make. */
export function planACLs(p: UserPlan, user: string): KafkaACL[] {
  if (p.what === 'custom') return p.custom
  const pattern = p.match === 'prefix' ? 'PREFIXED' : 'LITERAL'
  const out: KafkaACL[] = []
  if (p.what === 'produce' || p.what === 'both') out.push(allow('topic', pattern, p.topic, 'WRITE'))
  if (p.what === 'consume' || p.what === 'both') out.push(allow('topic', pattern, p.topic, 'READ'))
  out.push(allow('topic', pattern, p.topic, 'DESCRIBE'))
  if ((p.what === 'consume' || p.what === 'both') && p.groupPrefix) out.push(allow('group', 'PREFIXED', p.groupPrefix, 'READ'))
  if (p.transactions && p.what !== 'consume') {
    out.push(allow('transactional_id', 'PREFIXED', user, 'WRITE'), allow('transactional_id', 'PREFIXED', user, 'DESCRIBE'))
  }
  return out
}

/** Why a topic choice is not inside the entry's scope, or null. */
export function topicProblem(scope: string, match: 'prefix' | 'exact', topic: string): string | null {
  if (!topic) return 'Name the topics'
  if (!/^[A-Za-z0-9._-]{1,249}$/.test(topic)) return 'Letters, digits, . _ and -'
  if (scope !== '*' && !topic.startsWith(scope)) return `Inside this entry: starts with ${scope}`
  if (match === 'exact' && scope !== '*' && topic === scope) return `Name a topic, not just ${scope}`
  return null
}

/** The topics a choice covers now, from those the Topics tab has. */
export function coveredTopics(match: 'prefix' | 'exact', topic: string, topics: string[]): string[] {
  if (!topic) return []
  return topics.filter((t) => (match === 'prefix' ? t.startsWith(topic) : t === topic)).sort()
}

/** Why a new user's name will not do, or null. */
export function kafkaUserNameProblem(name: string, existing: string[]): string | null {
  if (!name) return 'Name it'
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(name)) return 'Letters, digits, . _ and -, up to 64'
  if (name.startsWith('argus-')) return 'argus-… users are Argus’s own, made for each session'
  if (existing.includes(name)) return 'Already a user on this cluster: open it and give it access here instead'
  return null
}

export const MB = 1024 * 1024

/** A quota in MB/s for a field, '' for none. */
export function mbps(bytes?: number): number | '' {
  return bytes && bytes > 0 ? Math.round((bytes / MB) * 100) / 100 : ''
}

export function quotaWords(q: KafkaQuotas): string {
  const parts = [
    q.produceBytes ? `produce ${mbps(q.produceBytes)} MB/s` : '',
    q.consumeBytes ? `consume ${mbps(q.consumeBytes)} MB/s` : '',
    q.requestPct ? `${q.requestPct}% request time` : '',
  ].filter(Boolean)
  return parts.length ? parts.join(' · ') : 'No limits'
}

/** Where apps reach the broker, from the Network tab's answer. */
export interface KafkaNetworkLite { listeners?: string; advertised?: string; protocol?: string; port?: number; firewallActive?: boolean; firewallAllowAll?: boolean; firewallSources?: string[] }

const SECURITY = new Set(['PLAINTEXT', 'SSL', 'SASL_PLAINTEXT', 'SASL_SSL'])

/**
 * The address apps on other machines use: the first advertised listener
 * that is not the loopback, with its security protocol when its name is
 * one. loopbackOnly says apps elsewhere cannot use it yet.
 */
export function appBootstrap(n: KafkaNetworkLite | null, fallback: string): { address: string; protocol: string; loopbackOnly: boolean } {
  const entries = (n?.advertised || n?.listeners || '').split(',').map((s) => s.trim()).filter(Boolean)
  for (const e of entries) {
    const m = /^([A-Za-z0-9_]+):\/\/(.*)$/.exec(e)
    if (!m) continue
    const host = m[2]!.replace(/:\d+$/, '').replace(/^\[|\]$/g, '')
    if (host === '' || host === 'localhost' || host === '127.0.0.1' || host === '::1' || host === '0.0.0.0') continue
    const name = m[1]!.toUpperCase()
    return { address: m[2]!, protocol: SECURITY.has(name) ? name : `the ${name} listener's protocol`, loopbackOnly: false }
  }
  const first = entries[0] ? /^([A-Za-z0-9_]+):\/\/(.*)$/.exec(entries[0]) : null
  const name = first?.[1]?.toUpperCase() ?? 'SASL_PLAINTEXT'
  return { address: first?.[2] || fallback, protocol: SECURITY.has(name) ? name : 'SASL_PLAINTEXT', loopbackOnly: true }
}

export interface ClientDetails { bootstrap: string; protocol: string; mechanism: string; user: string; password: string; group?: string }

/** Ready client settings, in three dialects. */
export function clientSettings(d: ClientDetails, kind: 'java' | 'librdkafka' | 'env'): string {
  const q = (s: string) => s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')
  if (kind === 'java') {
    return [
      `bootstrap.servers=${d.bootstrap}`,
      `security.protocol=${d.protocol}`,
      `sasl.mechanism=${d.mechanism}`,
      `sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username="${q(d.user)}" password="${q(d.password)}";`,
      ...(d.group ? [`group.id=${d.group}`] : []),
    ].join('\n')
  }
  if (kind === 'librdkafka') {
    return [
      `bootstrap.servers=${d.bootstrap}`,
      `security.protocol=${d.protocol.toLowerCase()}`,
      `sasl.mechanisms=${d.mechanism}`,
      `sasl.username=${d.user}`,
      `sasl.password=${d.password}`,
      ...(d.group ? [`group.id=${d.group}`] : []),
    ].join('\n')
  }
  return [
    `KAFKA_BOOTSTRAP_SERVERS=${d.bootstrap}`,
    `KAFKA_SECURITY_PROTOCOL=${d.protocol}`,
    `KAFKA_SASL_MECHANISM=${d.mechanism}`,
    `KAFKA_USERNAME=${d.user}`,
    `KAFKA_PASSWORD=${d.password}`,
    ...(d.group ? [`KAFKA_GROUP_ID=${d.group}`] : []),
  ].join('\n')
}

/** Who made each user in Argus, from Activity: the newest creation of each. */
export function madeBy(events: BrokerUserEvent[]): Map<string, BrokerUserEvent> {
  const out = new Map<string, BrokerUserEvent>()
  for (const e of events) {
    if (e.action === 'broker.user.created' && !out.has(e.user)) out.set(e.user, e)
  }
  return out
}

/** The operations an ACL may hold, by resource, as the agent allows them. */
export const ACL_OPERATIONS: Record<string, string[]> = {
  topic: ['READ', 'WRITE', 'DESCRIBE', 'DESCRIBE_CONFIGS', 'CREATE', 'DELETE', 'ALTER', 'ALTER_CONFIGS'],
  group: ['READ', 'DESCRIBE', 'DELETE'],
  transactional_id: ['WRITE', 'DESCRIBE'],
}

/** Whether a topic ACL covers topic. */
export function aclCovers(a: KafkaACL, topic: string): boolean {
  if (a.resource !== 'topic') return false
  if (a.pattern === 'PREFIXED') return topic.startsWith(a.name)
  return a.name === topic || a.name === '*'
}

/** How a topic ACL names its topics, as the Access tab shows why. */
export function aclBecause(a: KafkaACL): string {
  if (a.pattern === 'LITERAL' && a.name === '*') return 'LITERAL * (the whole cluster)'
  return `${a.pattern} ${a.name}`
}

/** An app user that reaches a topic: what it can do there, and the ACLs that let it. */
export interface TopicReach { user: string; mechanisms: string[]; caps: KafkaCap[]; because: string[] }

/** The app users whose ACLs reach topic, Argus's own session users left out. */
export function topicAccess(data: KafkaUsersData, topic: string): TopicReach[] {
  const out: TopicReach[] = []
  for (const u of data.users) {
    if (u.self || data.sessions.includes(u.name)) continue
    const hits = [...u.acls, ...(u.outside ?? [])].filter((a) => aclCovers(a, topic))
    if (hits.length === 0) continue
    const caps = userCaps(hits).map((c) => c.cap)
    if (caps.length === 0) continue
    out.push({ user: u.name, mechanisms: u.mechanisms ?? [], caps, because: [...new Set(hits.map(aclBecause))] })
  }
  return out.sort((a, b) => a.user.localeCompare(b.user))
}

/** What an Argus level lets a person do on a Kafka entry's topics. */
export function levelCaps(level: string): KafkaCap[] {
  switch (level) {
    case 'read': return ['Consume']
    case 'write': return ['Produce', 'Consume']
    case 'owner': case 'dba': return ['Produce', 'Consume', 'Alter topics']
  }
  return []
}
