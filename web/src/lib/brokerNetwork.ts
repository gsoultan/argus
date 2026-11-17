import { firewallLines, listenFor, privateNetworks, type PlanStep, type Reach, type ReachView } from '~/lib/redisManage'

/*
Which machines can connect to a message broker, as the agent reports it,
and the plan of what a choice changes.

RabbitMQ: the AMQP listener's listeners.tcp lines in rabbitmq.conf, read
when the node starts, the server's firewall, and a restart. Opening past
this server is refused while the guest user still signs in with its default
password from anywhere.

Kafka: the firewall alone, for the entry's listener port. Argus leaves the
broker's listeners as server.properties says, since they are tied to
advertised.listeners and the cluster's other brokers.
*/

export interface RabbitNetworkData extends ReachView {
  bind: string
  next: string
  pendingRestart: boolean
  version: string
  configFile?: string
  advanced?: boolean
  tls: boolean
  guestRemote?: boolean
  clients: number
  remote: number
  reach: Reach
  host: string
  service?: string
}

export interface KafkaNetworkData extends ReachView {
  listeners: string
  advertised?: string
  bind: string
  protocol?: string
  brokerId: number
  brokers: number
  tls: boolean
  reach: Reach
  host: string
}

/** The addresses RabbitMQ's AMQP listener binds for a choice, as tunnel.RabbitBindFor writes them. */
export function rabbitBindFor(reach: Reach, addresses: string[]): string {
  if (reach === 'any') return '*'
  if (reach === 'local') return '127.0.0.1'
  return ['127.0.0.1', ...addresses].join(',')
}

const key = (b: string) => b.split(',').map((x) => x.trim()).filter(Boolean).sort().join(',')
const said = (b: string) => b === '*' ? 'listeners.tcp.default' : b ? b.split(',').map((a, i) => `listeners.tcp.${i + 1} = ${a}`).join('; ') : 'listeners.tcp = none'

type Plan = { steps: PlanStep[]; blocked: string | null }

function firewallSteps(n: ReachView & { host: string }, reach: Reach, sent: string[], name: string, block: (w: string) => void, steps: PlanStep[], noFirewall: string) {
  const host = n.host || 'this server'
  const lines = firewallLines(n, reach, sent)
  if (!n.firewall || !n.firewallActive) {
    if (reach === 'selected') {
      block(`no firewall is on, so ${name} cannot be limited to selected machines`)
      steps.push({ state: 'blocked', title: 'No firewall is on', detail: `Argus changes ufw or firewalld and never turns one on, since that can cut off SSH. Turn a firewall on on ${host} first.` })
    } else {
      steps.push({ state: 'skip', title: 'No firewall is on', detail: reach === 'local' ? undefined : noFirewall })
    }
  } else if (reach === 'selected' && n.firewallOpenToAll) {
    block('a firewall rule Argus did not add already lets everyone in')
    steps.push({ state: 'blocked', title: 'A rule already lets everyone in', detail: `Remove it on the server first: ${(n.firewallOthers ?? []).join('; ')}` })
  } else if (lines.length) {
    steps.push({ state: 'change', title: `Firewall on ${host} (${n.firewall})`, lines,
      detail: reach === 'local' ? 'Only the rules Argus added are removed.' : reach === 'any' ? 'Lets everyone reach the port.' : 'Lets these in and refuses the port to everyone else.' })
  } else {
    steps.push({ state: 'ok', title: `Firewall on ${host} (${n.firewall}): nothing to change` })
  }
}

export function rabbitReachPlan(n: RabbitNetworkData, reach: Reach, sources: string[], restart: boolean): Plan {
  const steps: PlanStep[] = []
  const host = n.host || 'this server'
  let blocked: string | null = null
  const block = (why: string) => { blocked ??= why }
  const sent = reach === 'network' ? privateNetworks(n) : reach === 'selected' ? sources : []
  const addrs = listenFor(n, reach, sent)
  const bind = rabbitBindFor(reach, addrs)

  if (reach !== 'local' && n.firewallError) block(`the firewall could not be read: ${n.firewallError}`)
  if (n.advanced) block('advanced.config sets tcp_listeners, which overrides rabbitmq.conf; change it on the server')
  else if (!n.configFile) block('Argus found no rabbitmq.conf to keep the listener in')
  if (reach !== 'local' && n.guestRemote) block('the guest user signs in with its default password from anywhere; change its password or delete it first')
  if (addrs.some((a) => a.includes(':'))) block('choose an IPv4 address')
  if (reach === 'selected' && sources.length === 0) block('add at least one machine')
  if (reach === 'network' && sent.length === 0) block(`${host} has no private address to listen on`)

  const changes = key(bind) !== key(n.next)
  steps.push(changes
    ? { state: 'change', title: reach === 'local' ? `Listen on ${host} only` : reach === 'any' ? 'Listen on every address' : `Listen on ${addrs.join(', ')}`,
      detail: `In ${n.configFile ?? 'rabbitmq.conf'}, for the next start. Argus’s listeners.tcp lines replace the file’s; the management port is left as it is.`,
      diff: { from: said(n.next), to: said(bind) } }
    : { state: 'ok', title: `RabbitMQ already listens on ${bind === '*' ? 'every address' : bind}${n.pendingRestart ? ' from its next start' : ''}` })
  if (reach !== 'local') {
    if (n.guestRemote) steps.push({ state: 'blocked', title: 'guest signs in with its default password', detail: 'It is not kept to this server (loopback_users). Change its password or delete it first.' })
    else steps.push({ state: 'ok', title: 'Every machine signs in as a RabbitMQ user', detail: 'guest stays kept to this server, or has its own password.' })
    if (!n.tls) steps.push({ state: 'warn', title: 'TLS is off', detail: 'Passwords and messages cross the network unencrypted.' })
  }
  firewallSteps(n, reach, sent, 'RabbitMQ', block, steps, 'Nothing on this server limits who reaches the port; users and passwords do.')
  if (changes || n.pendingRestart) {
    if (!restart) steps.push({ state: 'warn', title: 'Not restarted', detail: 'RabbitMQ keeps listening as it does now until it is restarted on the server.' })
    else if (!n.service) steps.push({ state: 'warn', title: 'Restart RabbitMQ on the server', detail: 'Argus found no systemd service for it, so it cannot restart it from here.' })
    else steps.push({ state: 'change', title: `Restart ${n.service}`, detail: `The node reads its listeners only when it starts. Ends every open connection, ${n.clients} now.` })
  } else {
    steps.push({ state: 'ok', title: 'No restart' })
  }
  return { steps, blocked }
}

export function kafkaReachPlan(n: KafkaNetworkData, reach: Reach, sources: string[]): Plan {
  const steps: PlanStep[] = []
  let blocked: string | null = null
  const block = (why: string) => { blocked ??= why }
  const sent = reach === 'network' ? privateNetworks(n) : reach === 'selected' ? sources : []
  const local = n.bind === '127.0.0.1' || n.bind === '::1'

  if (reach !== 'local' && n.firewallError) block(`the firewall could not be read: ${n.firewallError}`)
  if (reach !== 'local' && local) block(`Kafka’s listener binds ${n.bind} only; change listeners and advertised.listeners in server.properties on the server first`)
  if (reach === 'local' && !local) block('Kafka’s listener binds past this server, and Argus does not change Kafka’s listeners')
  if (reach === 'selected' && sources.length === 0) block('add at least one machine')
  if (reach === 'network' && sent.length === 0) block(`${n.host || 'this server'} has no private address`)

  steps.push({ state: 'ok', title: `Listeners stay as they are`, detail: `${n.listeners}${n.advertised ? ` · advertised ${n.advertised}` : ''}. Argus does not change Kafka’s listeners: they tie to advertised.listeners and the other brokers.` })
  if (reach !== 'local' && !n.tls) steps.push({ state: 'warn', title: 'TLS is off', detail: `${n.protocol ?? 'The listener'}: SASL passwords and messages cross the network unencrypted.` })
  firewallSteps(n, reach, sent, 'Kafka', block, steps, 'Nothing on this server limits who reaches the port; SASL users and ACLs do.')
  steps.push({ state: 'ok', title: 'No restart' })
  return { steps, blocked }
}

function nowFor(name: string, n: ReachView & { reach: Reach; host: string; bind: string }, wait: string): { tone: string; text: string } {
  const host = n.host || 'this server'
  const src = n.firewallSources ?? []
  switch (n.reach) {
    case 'local': return { tone: 'amber', text: `Only programs on ${host} can connect. ${name} listens on ${n.bind}, so other machines get “connection refused”.${wait}` }
    case 'selected': return { tone: 'teal', text: `${src.length} selected machine${src.length === 1 ? '' : 's'} can connect: ${src.join(', ')}.${wait}` }
    case 'network': return { tone: 'azure', text: `Machines on the networks ${host} is on can connect, as a ${name} user.${wait}` }
    default: return { tone: 'rose', text: `${name} listens on every address of ${host}${n.firewallActive ? '' : ', and no firewall here narrows it'}.${wait}` }
  }
}

export function rabbitNowText(n: RabbitNetworkData) {
  return nowFor('RabbitMQ', n, n.pendingRestart ? ` From its next start it listens on ${n.next === '*' ? 'every address' : n.next}.` : '')
}

export function kafkaNowText(n: KafkaNetworkData) {
  return nowFor('Kafka', n, '')
}
