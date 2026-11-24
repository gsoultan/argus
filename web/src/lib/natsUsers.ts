import { validSubject } from '~/lib/nats'

/*
The users a NATS account's apps sign in with, as the Users tab shows and
makes them. The agent signs a user JWT for a public key the browser made
(lib/nkeys.ts) and keeps a list of what it signed; these are the browser's
half of the rules in internal/tunnel/natsusers.go, so a form says what is
wrong before anything is sent.

A signed user cannot be taken back from Argus: it expires, and the operator
revokes it earlier with nsc (revokeCommand).
*/

export type NatsJetStream = '' | 'use' | 'manage'

export interface NatsAppUserSpec {
  pubAllow: string[]
  pubDeny?: string[]
  subAllow: string[]
  subDeny?: string[]
  requests?: boolean
  replies?: boolean
  jetstream?: NatsJetStream
  days: number
  sources?: string[]
}

export interface NatsAppUser {
  name: string
  publicKey: string
  issuedAt?: string
  expires?: string
  by?: string
  spec: NatsAppUserSpec
  inArgus: boolean
  connections: number
}

export interface NatsUsersData {
  users: NatsAppUser[]
  account: string
  accountId: string
  connsError?: string
}

export interface NatsIssued { user: NatsAppUser; jwt: string }

export const MAX_DAYS = 365
export const DEFAULT_DAYS = 90
export const DAY_CHOICES = [7, 30, 90, 180, 365]

export type NatsPreset = 'publisher' | 'subscriber' | 'service' | 'jetstream' | 'custom'

/** What each kind of app usually needs; the subjects come from the form. */
export const PRESETS: { value: NatsPreset; label: string; hint: string }[] = [
  { value: 'publisher', label: 'Publishes', hint: 'Sends to its subjects' },
  { value: 'subscriber', label: 'Subscribes', hint: 'Listens on its subjects' },
  { value: 'service', label: 'Service', hint: 'Answers requests on its subjects' },
  { value: 'jetstream', label: 'Uses streams', hint: 'Publishes to and consumes streams' },
  { value: 'custom', label: 'Custom', hint: 'Every rule by hand' },
]

/** The rules a preset starts from, on the subjects the person gave. */
export function presetSpec(p: NatsPreset, subjects: string[], days = DEFAULT_DAYS): NatsAppUserSpec {
  switch (p) {
    case 'publisher': return { pubAllow: subjects, subAllow: [], days }
    case 'subscriber': return { pubAllow: [], subAllow: subjects, days }
    case 'service': return { pubAllow: [], subAllow: subjects, replies: true, days }
    case 'jetstream': return { pubAllow: subjects, subAllow: [], jetstream: 'use', requests: true, days }
    default: return { pubAllow: [], subAllow: [], days }
  }
}

const NAME_RE = /^[A-Za-z0-9._-]{1,64}$/

export function userNameProblem(name: string): string | null {
  if (!name) return 'Name the user, as the app is known.'
  if (name.startsWith('argus-')) return 'Names starting argus- are Argus’s own, made for each session.'
  if (!NAME_RE.test(name)) return 'A name is 1 to 64 letters, digits, . _ and -.'
  return null
}

/** Whether s is an IP address or a range like 10.0.0.0/8, as the agent takes it. */
export function validSource(s: string): boolean {
  const [addr, bits, more] = s.split('/')
  if (more !== undefined || !addr) return false
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(addr)
  if (v4) {
    const parts = v4.slice(1).map(Number)
    if (parts.some((n) => n > 255)) return false
    if (bits === undefined) return true
    if (!/^\d{1,2}$/.test(bits) || Number(bits) > 32) return false
    // The agent wants the range's first address: 10.0.0.0/8, not 10.0.0.1/8.
    const n = ((parts[0]! << 24) | (parts[1]! << 16) | (parts[2]! << 8) | parts[3]!) >>> 0
    const mask = Number(bits) === 0 ? 0 : (0xffffffff << (32 - Number(bits))) >>> 0
    return (n & mask) >>> 0 === n
  }
  if (!/^[0-9a-fA-F:]+$/.test(addr) || !addr.includes(':')) return false
  return bits === undefined || (/^\d{1,3}$/.test(bits) && Number(bits) <= 128)
}

/** What is wrong with a new user, in the order a person fixes it. */
export function userProblems(name: string, spec: NatsAppUserSpec): string[] {
  const out: string[] = []
  const n = userNameProblem(name)
  if (n) out.push(n)
  const lists: [string, string[] | undefined][] = [['publish to', spec.pubAllow], ['keep from publishing to', spec.pubDeny],
    ['subscribe to', spec.subAllow], ['keep from subscribing to', spec.subDeny]]
  for (const [what, l] of lists) {
    for (const s of l ?? []) {
      if (!validSubject(s)) out.push(`${s} is not a subject or a subject filter.`)
      else if (s === '$SYS' || s.startsWith('$SYS.')) out.push('The system account’s subjects are never given to an app.')
    }
    if ((l?.length ?? 0) > 64) out.push(`At most 64 subjects to ${what}.`)
    if (new Set(l).size !== (l?.length ?? 0)) out.push(`A subject to ${what} is named twice.`)
  }
  if (!(spec.days >= 1 && spec.days <= MAX_DAYS)) out.push(`A user is valid 1 to ${MAX_DAYS} days.`)
  for (const s of spec.sources ?? []) if (!validSource(s)) out.push(`${s} is not an IP address or a range like 10.0.0.0/8.`)
  if (spec.pubAllow.length === 0 && spec.subAllow.length === 0 && !spec.requests && !spec.replies && !spec.jetstream) {
    out.push('The user may do nothing yet: give it a subject to publish or subscribe to.')
  }
  return out
}

function list(l: string[] | undefined): string {
  return l && l.length > 0 ? l.join(', ') : 'nothing'
}

/** What the user may do, a line each, in words. */
export function ruleLines(spec: NatsAppUserSpec): { what: string; words: string }[] {
  const out = [
    { what: 'Publishes to', words: list(spec.pubAllow) + (spec.pubDeny?.length ? `, except ${spec.pubDeny.join(', ')}` : '') },
    { what: 'Subscribes to', words: list(spec.subAllow) + (spec.subDeny?.length ? `, except ${spec.subDeny.join(', ')}` : '') },
  ]
  if (spec.requests) out.push({ what: 'Requests', words: 'makes requests and reads their replies' })
  if (spec.replies) out.push({ what: 'Replies', words: 'answers requests, once each, within a minute' })
  if (spec.jetstream === 'use') out.push({ what: 'JetStream', words: 'reads streams, makes and pulls from consumers, acks' })
  if (spec.jetstream === 'manage') out.push({ what: 'JetStream', words: 'everything, making and deleting streams too' })
  if (spec.sources?.length) out.push({ what: 'From', words: spec.sources.join(', ') })
  return out
}

/** How long a user has left, and how urgent that is. */
export function expiry(expires: string | undefined, now = Date.now()): { text: string; color: 'slate' | 'amber' | 'rose' } {
  if (!expires) return { text: 'Not known', color: 'slate' }
  const ms = new Date(expires).getTime() - now
  const days = Math.round(Math.abs(ms) / 86_400_000)
  if (ms <= 0) return { text: days === 0 ? 'Expired today' : `Expired ${days} day${days === 1 ? '' : 's'} ago`, color: 'rose' }
  const text = days === 0 ? 'Expires today' : `Expires in ${days} day${days === 1 ? '' : 's'}`
  return { text, color: days <= 14 ? 'amber' : 'slate' }
}

/** How the operator revokes a user before it expires. */
export function revokeCommand(account: string, key: string): string {
  return `nsc revocations add-user --account ${account} --user-public-key ${key}\nnsc push --account ${account}`
}

/** The first and last characters of a key, as it is shown in a list. */
export function shortKey(k: string): string {
  return k.length > 14 ? `${k.slice(0, 6)}…${k.slice(-6)}` : k
}

/** What needs looking at: users expiring soon or expired but still listed. */
export function userFindings(users: NatsAppUser[], now = Date.now()): { key: string; text: string }[] {
  const out: { key: string; text: string }[] = []
  for (const u of users) {
    if (!u.inArgus) continue
    const e = expiry(u.expires, now)
    if (e.color === 'rose' && u.connections === 0) continue
    if (e.color !== 'slate') {
      out.push({ key: u.publicKey, text: `${u.name}: ${e.text.toLowerCase()}${u.connections ? `, with ${u.connections} connection${u.connections === 1 ? '' : 's'} now` : ''}. Sign a new one and give its .creds file to the app.` })
    }
  }
  return out
}

/** The file name a user's .creds file is saved under. */
export function credsName(account: string, name: string): string {
  return `${account}-${name}.creds`
}

/** How an app uses its .creds file, with the server's address. */
export function connectSnippets(url: string, file: string): { label: string; code: string }[] {
  return [
    { label: 'nats CLI', code: `nats --server ${url} --creds ${file} pub orders.test "hello"` },
    { label: 'Go', code: `nc, err := nats.Connect("${url}", nats.UserCredentials("${file}"))` },
    { label: 'Node.js', code: `const nc = await connect({ servers: "${url}", authenticator: credsAuthenticator(readFileSync("${file}")) })` },
    { label: 'Python', code: `nc = await nats.connect("${url}", user_credentials="${file}")` },
  ]
}
