import { firewallLines, listenFor, privateNetworks, type PlanStep, type Reach, type ReachView } from '~/lib/redisManage'

/*
Which machines can connect to a MongoDB, as the agent reports it, and the
plan of what a choice changes: net.bindIp in mongod's configuration file,
read only when mongod starts, the server's firewall, and a restart. Argus
adds only a MongoDB whose access control is on, so every machine that
reaches it still signs in as a user.
*/

export interface MongoNetworkData extends ReachView {
  /** net.bindIp as mongod runs now; "*" is bindIpAll. */
  bind: string
  /** What the configuration file says for the next start. */
  next: string
  pendingRestart: boolean
  version: string
  configFile?: string
  /** mongod was started with --bind_ip, which the file does not override. */
  cmdLine?: boolean
  ipv6?: boolean
  tls: boolean
  clients: number
  remote: number
  reach: Reach
  host: string
  service?: string
}

export interface MongoNetworkResult {
  network: MongoNetworkData
  firewallRan?: string[]
  restarted?: boolean
  restartError?: string
}

/** net.bindIp for a choice, as tunnel.MongoBindFor writes it. */
export function mongoBindFor(reach: Reach, addresses: string[]): string {
  if (reach === 'any') return '*'
  if (reach === 'local') return '127.0.0.1'
  return ['127.0.0.1', ...addresses].join(',')
}

const key = (b: string) => b.split(',').map((x) => x.trim()).join(',')
const said = (b: string) => b === '*' ? 'bindIpAll: true' : `bindIp: ${b}`

export function mongoReachPlan(n: MongoNetworkData, reach: Reach, sources: string[], restart: boolean): { steps: PlanStep[]; blocked: string | null } {
  const steps: PlanStep[] = []
  const host = n.host || 'this server'
  let blocked: string | null = null
  const block = (why: string) => { blocked ??= why }
  const sent = reach === 'network' ? privateNetworks(n) : reach === 'selected' ? sources : []
  const addrs = listenFor(n, reach, sent)
  const bind = mongoBindFor(reach, addrs)

  if (reach !== 'local' && n.firewallError) block(`the firewall could not be read: ${n.firewallError}`)
  if (n.cmdLine) block('mongod was started with --bind_ip on its command line; change it in the service on the server')
  else if (!n.configFile) block('mongod runs with no configuration file; start it with --config on the server first')
  if (!n.ipv6 && addrs.some((a) => a.includes(':'))) block('an IPv6 address needs net.ipv6 on')
  if (reach === 'selected' && sources.length === 0) block('add at least one machine')
  if (reach === 'network' && sent.length === 0) block(`${host} has no private address to listen on`)

  const changes = key(bind) !== key(n.next)
  steps.push(changes
    ? { state: 'change', title: reach === 'local' ? `Listen on ${host} only` : reach === 'any' ? 'Listen on every address' : `Listen on ${addrs.join(', ')}`,
      detail: `In ${n.configFile ?? 'mongod’s configuration file'}, for the next start. Only this line of net: changes.`, diff: { from: said(n.next), to: said(bind) } }
    : { state: 'ok', title: `MongoDB already listens on ${bind === '*' ? 'every address' : bind}${n.pendingRestart ? ' from its next start' : ''}` })

  if (reach !== 'local') {
    steps.push({ state: 'ok', title: 'Access control is on', detail: 'Every machine still signs in as a MongoDB user with its password.' })
    if (!n.tls) steps.push({ state: 'warn', title: 'TLS is off', detail: 'Passwords use SCRAM, but data crosses the network unencrypted.' })
  }

  const fw = n.firewall
  const lines = firewallLines(n, reach, sent)
  if (!fw || !n.firewallActive) {
    if (reach === 'selected') {
      block('no firewall is on, so MongoDB cannot be limited to selected machines')
      steps.push({ state: 'blocked', title: 'No firewall is on', detail: `Argus changes ufw or firewalld and never turns one on, since that can cut off SSH. Choose the private network, or turn a firewall on on ${host} first.` })
    } else {
      steps.push({ state: 'skip', title: 'No firewall is on', detail: reach === 'local' ? undefined : 'Nothing on this server limits who reaches the port; users and passwords do.' })
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

  if (changes || n.pendingRestart) {
    if (!restart) steps.push({ state: 'warn', title: 'Not restarted', detail: 'MongoDB keeps listening as it does now until it is restarted on the server.' })
    else if (!n.service) steps.push({ state: 'warn', title: 'Restart MongoDB on the server', detail: 'Argus found no systemd service for it, so it cannot restart it from here.' })
    else steps.push({ state: 'change', title: `Restart ${n.service}`, detail: `mongod reads bindIp only when it starts. Ends every open connection, ${n.clients} now, this console’s included.` })
  } else {
    steps.push({ state: 'ok', title: 'No restart' })
  }
  return { steps, blocked }
}

export function mongoNowText(n: MongoNetworkData): { tone: string; text: string } {
  const host = n.host || 'this server'
  const src = n.firewallSources ?? []
  const wait = n.pendingRestart ? ` From its next start it listens on ${n.next === '*' ? 'every address' : n.next}.` : ''
  switch (n.reach) {
    case 'local': return { tone: 'amber', text: `Only programs on ${host} can connect. MongoDB listens on ${n.bind}, so other machines get “connection refused”.${wait}` }
    case 'selected': return { tone: 'teal', text: `${src.length} selected machine${src.length === 1 ? '' : 's'} can connect: ${src.join(', ')}.${wait}` }
    case 'network': return { tone: 'azure', text: `Machines on the networks ${host} is on can connect, as a MongoDB user.${wait}` }
    default: return { tone: 'rose', text: `MongoDB listens on every address of ${host}${n.firewallActive ? '' : ', and no firewall here narrows it'}.${wait}` }
  }
}
