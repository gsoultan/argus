import { get, send } from '~/lib/live'

/*
 * The firewall on each server, read only: the types the control plane sends
 * and its two calls. Here rather than in live.ts, which every page loads, so
 * only the pages that show firewalls pay for them.
 */

export type FirewallKind = 'ufw' | 'firewalld' | 'nftables' | 'iptables' | 'windows' | ''

export interface FirewallRule {
  direction: 'in' | 'out'
  /** allow, deny, reject, limit, or other for a rule Argus cannot put in words. */
  action: string
  protocol: string
  ports?: string
  service?: string
  sources?: string[]
  name?: string
  argus?: boolean
  argusPort?: number
  profile?: string
  ipv6?: boolean
  raw?: string
}

export interface FirewallProfile {
  name: string
  enabled: boolean
  inUse?: boolean
  defaultIn?: string
  defaultOut?: string
}

export interface HostFirewall {
  kind: FirewallKind
  installed?: string[]
  active: boolean
  defaultIn?: string
  defaultOut?: string
  zones?: string[]
  profiles?: FirewallProfile[]
  rules: FirewallRule[]
  hidden?: number
  docker?: boolean
  manageable: boolean
  readAt: string
  note?: string
  error?: string
}

export interface ServerFirewallRule extends FirewallRule {
  madeBy: 'argus' | 'database' | 'outside'
  database?: { id: string; name: string; engine: string }
}

export interface PortReach {
  everyone: boolean
  public: boolean
  sources?: string[]
  unknown?: boolean
}

export interface FirewallListen {
  port: number
  process?: string
  kind?: string
  localOnly: boolean
  reach: PortReach
}

export interface ServerFirewall {
  reported: boolean
  firewall: HostFirewall
  rules: ServerFirewallRule[]
  listening: FirewallListen[]
  listeningRead: boolean
  /** Servers in Argus, by an address the rules name. */
  names: Record<string, string>
  /** What changing the firewall needs; absent from an older control plane. */
  manage?: FirewallManage
}

export interface FirewallFleetRow {
  assetId: string
  hostname: string
  kind: string
  os: string
  tags: string[]
  reported: boolean
  firewall: FirewallKind
  active: boolean
  manageable: boolean
  readAt?: string
  error?: string
  rules: number
  outside: number
  openToInternet: number[]
  docker?: boolean
}

/* ── Changing it ─────────────────────────────────────────────────────────── */

export type FWSourceKind = 'everyone' | 'private' | 'address' | 'server' | 'tag' | 'policy'

export interface FWSource {
  kind: FWSourceKind
  /** The address, the server's asset id, the tag or the policy's id. */
  value?: string
  /** A server's or policy's name, filled in by the control plane. */
  label?: string
}

/** A line Argus keeps, as a person wrote it. */
export interface FWLine {
  key?: string
  direction: 'in' | 'out'
  action: 'allow' | 'deny'
  protocol: 'tcp' | 'udp' | 'icmp' | 'any'
  ports?: string
  from: FWSource[]
  note?: string
  until?: string
  addedBy?: string
  addedAt?: string
}

export type FWChangeState =
  | 'awaiting_approval' | 'queued' | 'sent' | 'trial' | 'waiting_keep'
  | 'applied' | 'failed' | 'rolled_back' | 'denied' | 'cancelled' | 'interrupted' | 'stopped'

export interface FirewallRan {
  command: string
  ok: boolean
  output?: string
}

export interface FirewallChange {
  id: string
  assetId: string
  hostname: string
  kind: 'lines' | 'take_over' | 'turn_on' | 'put_back' | 'policy' | 'sync'
  summary: string
  reason: string
  preview: string[]
  state: FWChangeState
  rolloutId?: string
  wave: number
  requestedBy: string
  requestedAt: string
  decidedBy?: string
  decidedAt?: string
  keptBy?: string
  keptAt?: string
  sentAt?: string
  endedAt?: string
  updatedAt: string
  message: string
  ran: FirewallRan[]
  keepBefore?: string
  canPutBack: boolean
}

export interface FWHeld {
  id: string
  name: string
  version: number
  latest: number
  defaultOut: string
  lines: FWLine[]
  wanted: boolean
}

export interface FirewallManage {
  canChange: boolean
  ready: boolean
  reason?: string
  install?: 'ufw' | 'firewalld'
  own: FWLine[]
  policies: FWHeld[]
  defaultOut: string
  always: string[]
  changes: FirewallChange[]
  open?: FirewallChange
  managers: string[]
  remotePort: number
}

/** A change asked for on the Firewall tab. */
export interface FirewallAsk {
  add?: FWLine[]
  remove?: string[]
  takeOver?: string[]
  turnOn?: boolean
  install?: string
  putBack?: string
  reason?: string
  confirm?: string
}

export interface FirewallPlan {
  kind: string
  summary: string
  preview: string[]
  lines: { key: string; direction: string; action: string; protocol: string; ports?: string; sources?: string[] }[]
  needsApproval: boolean
  needsConfirm?: string
  warnings: string[]
  keep: boolean
}

export interface FWTarget {
  id: string
  assetId?: string
  hostname?: string
  tag?: string
}

export interface FWRollout {
  id: string
  policyId?: string
  policyName: string
  version: number
  state: 'awaiting_approval' | 'running' | 'done' | 'stopped' | 'denied' | 'cancelled'
  summary: string
  requestedBy: string
  requestedAt: string
  decidedBy?: string
  endedAt?: string
  message: string
  servers?: FirewallChange[]
  total: number
  done: number
  failed: number
}

export interface FirewallPolicy {
  id: string
  name: string
  description: string
  version: number
  lines: FWLine[]
  defaultOut: 'allow' | 'deny'
  starter: boolean
  createdBy: string
  updatedBy: string
  updatedAt: string
  targets: FWTarget[]
  servers: number
  current: number
  rollout?: FWRollout
}

export interface FirewallPolicyInput {
  name: string
  description?: string
  lines: FWLine[]
  defaultOut: 'allow' | 'deny'
  note?: string
  starter?: boolean
}

const asset = (id: string) => `/api/v1/assets/${encodeURIComponent(id)}/firewall`

export async function planFirewall(assetId: string, ask: FirewallAsk): Promise<FirewallPlan> {
  return send('POST', `${asset(assetId)}/plan`, ask)
}

export async function askFirewallChange(assetId: string, ask: FirewallAsk): Promise<FirewallChange> {
  return send('POST', `${asset(assetId)}/changes`, ask)
}

export async function setFirewallManagers(assetId: string, emails: string[]): Promise<{ managers: string[] }> {
  return send('PUT', `${asset(assetId)}/managers`, { emails })
}

export type FWChangeAction = 'approve' | 'deny' | 'cancel' | 'keep'

/** confirmSelf is the phrase for approving your own, when nobody else can. */
export async function actOnFirewallChange(id: string, action: FWChangeAction, confirmSelf?: string): Promise<FirewallChange> {
  return send('POST', `/api/v1/firewall-changes/${encodeURIComponent(id)}/${action}`, confirmSelf ? { confirmSelf } : undefined)
}

export async function firewallPolicies(): Promise<{ policies: FirewallPolicy[] }> {
  return get('/api/v1/firewall-policies')
}

export async function firewallStarters(): Promise<{ starters: FirewallPolicyInput[] }> {
  return get('/api/v1/firewall-policies/starters')
}

export async function firewallPolicy(id: string): Promise<FirewallPolicy> {
  return get(`/api/v1/firewall-policies/${encodeURIComponent(id)}`)
}

export async function createFirewallPolicy(p: FirewallPolicyInput): Promise<FirewallPolicy> {
  return send('POST', '/api/v1/firewall-policies', p)
}

export async function saveFirewallPolicy(id: string, p: FirewallPolicyInput): Promise<FirewallPolicy> {
  return send('PUT', `/api/v1/firewall-policies/${encodeURIComponent(id)}`, p)
}

export async function deleteFirewallPolicy(id: string): Promise<void> {
  return send('DELETE', `/api/v1/firewall-policies/${encodeURIComponent(id)}`)
}

export async function addFirewallTarget(id: string, t: { assetId?: string; tag?: string }): Promise<{ rollout?: FWRollout }> {
  return send('POST', `/api/v1/firewall-policies/${encodeURIComponent(id)}/targets`, t)
}

export async function removeFirewallTarget(id: string, target: string): Promise<{ rollout?: FWRollout }> {
  return send('DELETE', `/api/v1/firewall-policies/${encodeURIComponent(id)}/targets/${encodeURIComponent(target)}`)
}

export type FWRolloutAction = 'approve' | 'deny' | 'stop'

export async function actOnFirewallRollout(id: string, action: FWRolloutAction, confirmSelf?: string): Promise<FWRollout> {
  return send('POST', `/api/v1/firewall-rollouts/${encodeURIComponent(id)}/${action}`, confirmSelf ? { confirmSelf } : undefined)
}

export async function assetFirewall(assetId: string): Promise<ServerFirewall> {
  return get(`/api/v1/assets/${encodeURIComponent(assetId)}/firewall`)
}

export async function firewallFleet(): Promise<{ servers: FirewallFleetRow[] }> {
  return get('/api/v1/firewalls')
}

/* ── Words ───────────────────────────────────────────────────────────────── */

const FIREWALL_NAME: Record<string, string> = {
  ufw: 'ufw',
  firewalld: 'firewalld',
  nftables: 'nftables',
  iptables: 'iptables',
  windows: 'Windows Defender Firewall',
}

/** The firewall's name as people know it. */
export function firewallName(kind: string): string {
  return FIREWALL_NAME[kind] ?? kind
}

/** Common ports by what people call them. */
const PORT_NAME: Record<number, string> = {
  20: 'FTP', 21: 'FTP', 22: 'SSH', 25: 'Mail', 53: 'DNS', 80: 'Web', 110: 'Mail', 123: 'Time', 143: 'Mail',
  443: 'Web', 445: 'File sharing', 465: 'Mail', 587: 'Mail', 631: 'Printing', 993: 'Mail', 995: 'Mail',
  1433: 'SQL Server', 1521: 'Oracle', 2049: 'NFS', 3000: 'Grafana', 3306: 'MySQL', 3389: 'Remote Desktop',
  5432: 'Postgres', 5672: 'RabbitMQ', 5985: 'Windows remote management', 5986: 'Windows remote management',
  6379: 'Redis', 6432: 'PgBouncer', 8080: 'Web', 8443: 'Web', 9090: 'Prometheus', 9092: 'Kafka',
  9093: 'Kafka', 9100: 'Metrics', 11211: 'Memcached', 15672: 'RabbitMQ admin', 27017: 'MongoDB', 51820: 'WireGuard',
}

const SERVICE_NAME: Record<string, string> = {
  ssh: 'SSH', http: 'Web', https: 'Web', 'dhcpv6-client': 'DHCP', cockpit: 'Cockpit', mysql: 'MySQL',
  postgresql: 'Postgres', redis: 'Redis', dns: 'DNS', 'samba': 'File sharing', 'rdp': 'Remote Desktop',
  'prometheus-node-exporter': 'Metrics', OpenSSH: 'SSH',
}

/** What a rule lets through, in words: "Web", "SSH", "Port 8081", "Every port". */
export function ruleWhat(r: Pick<FirewallRule, 'ports' | 'service' | 'protocol'>): string {
  if (r.service) return SERVICE_NAME[r.service] ?? r.service
  if (!r.ports) return r.protocol === 'icmp' ? 'Ping' : 'Every port'
  const names = new Set(
    r.ports.split(',').map((p) => {
      const n = Number(p.trim())
      return Number.isInteger(n) ? PORT_NAME[n] : undefined
    }),
  )
  if (names.size === 1) {
    const [only] = names
    if (only) return only
  }
  return r.ports.includes(',') || r.ports.includes('-') ? 'Ports' : 'Port'
}

/** The ports and protocol beside the name: "80, 443", "6000-6010/udp". */
export function rulePorts(r: Pick<FirewallRule, 'ports' | 'protocol'>): string {
  if (!r.ports) return r.protocol === 'any' || r.protocol === 'tcp' || r.protocol === 'icmp' ? '' : r.protocol
  const p = r.ports.split(',').map((s) => s.trim()).join(', ')
  return r.protocol === 'udp' ? `${p}/udp` : p
}

/** One source in words: a server in Argus by name, else the address. */
export function sourceName(s: string, names: Record<string, string>): string {
  return names[s] ?? s
}

/** A read server whose firewall is off, or that has none, lets everything in. */
export function firewallOff(r: FirewallFleetRow): boolean {
  return r.reported && !r.error && !r.active
}

/** The listening ports anyone on the internet can reach, in words. */
export function openWords(ports: number[]): string {
  return ports.map((p) => (PORT_NAME[p] ? `${PORT_NAME[p]} (${p})` : String(p))).join(', ')
}

/** A change still going: waiting, sent, on trial or waiting for Keep. */
export function changeOpen(s: FWChangeState): boolean {
  return s === 'awaiting_approval' || s === 'queued' || s === 'sent' || s === 'trial' || s === 'waiting_keep'
}

const CHANGE_STATE: Record<FWChangeState, string> = {
  awaiting_approval: 'Waiting for approval',
  queued: 'Waiting for the server',
  sent: 'Being made',
  trial: 'On trial',
  waiting_keep: 'Waiting for Keep',
  applied: 'Applied',
  failed: 'Failed',
  rolled_back: 'Undone',
  denied: 'Denied',
  cancelled: 'Taken back',
  interrupted: 'Interrupted',
  stopped: 'Stopped',
}

export function changeStateWords(s: FWChangeState): string {
  return CHANGE_STATE[s] ?? s
}

export function changeStateColor(s: FWChangeState): string {
  if (s === 'applied') return 'teal'
  if (s === 'failed' || s === 'rolled_back' || s === 'interrupted') return 'rose'
  if (s === 'awaiting_approval' || s === 'waiting_keep') return 'amber'
  if (changeOpen(s)) return 'azure'
  return 'slate'
}

/** Who a line is for, in words. */
export function sourceWords(s: FWSource): string {
  switch (s.kind) {
    case 'everyone': return 'Everyone'
    case 'private': return 'Private networks'
    case 'tag': return `Servers tagged ${s.value ?? ''}`
    case 'policy': return `Servers with ${s.label || 'a policy'}`
    case 'server': return s.label || 'A server'
    default: return s.value ?? ''
  }
}

/** What a line lets through, in words: "Web 80, 443", "Ping", "Everything". */
export function lineWhat(l: Pick<FWLine, 'protocol' | 'ports'>): string {
  if (l.protocol === 'icmp') return 'Ping'
  if (l.protocol === 'any') return 'Everything'
  const name = ruleWhat({ ports: l.ports, protocol: l.protocol })
  return `${name} ${rulePorts({ ports: l.ports, protocol: l.protocol })}`.trim()
}

/** A line as a sentence: "Let in Web 443 from Private networks". */
export function lineWords(l: FWLine): string {
  const verb = l.direction === 'out'
    ? (l.action === 'deny' ? 'Block going out' : 'Let out')
    : (l.action === 'deny' ? 'Keep out' : 'Let in')
  const prep = l.direction === 'out' ? 'to' : 'from'
  return `${verb} ${lineWhat(l)} ${prep} ${l.from.map(sourceWords).join(', ')}`
}
