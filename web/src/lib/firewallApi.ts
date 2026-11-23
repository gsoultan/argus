import { get } from '~/lib/live'

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
