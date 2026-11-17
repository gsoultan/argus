import { firewallLines, listenFor, privateNetworks, type PlanStep, type Reach, type ReachView } from '~/lib/redisManage'

/*
Which machines can connect to a MySQL or MariaDB, as the agent reports it,
and the plan of what a choice changes: bind_address, kept for the next start
(SET PERSIST_ONLY on MySQL, an option file of Argus's own on MariaDB), the
server's firewall, and a restart, since mysqld reads bind_address only when
it starts. Who may sign in from where is each user's host part, which Argus
shows and leaves alone.
*/

export interface MySQLNetworkData extends ReachView {
  bind: string
  /** bind_address at the next start. */
  next: string
  pendingRestart: boolean
  skipNetworking?: boolean
  version: string
  mariadb: boolean
  /** bind_address takes a list (MySQL 8.0.13, MariaDB 10.11). */
  listOK: boolean
  ssl: boolean
  remoteUsers?: string[]
  noPassRemote?: string[]
  keptIn?: string
  clients: number
  remote: number
  reach: Reach
  host: string
  service?: string
}

export interface MySQLNetworkResult {
  network: MySQLNetworkData
  firewallRan?: string[]
  restarted?: boolean
  restartError?: string
}

/** bind_address for a choice, as tunnel.MySQLBindFor writes it. */
export function mysqlBindFor(reach: Reach, addresses: string[]): string {
  if (reach === 'any') return '*'
  if (reach === 'local') return '127.0.0.1'
  return ['127.0.0.1', ...addresses].join(',')
}

const key = (b: string) => b.split(',').map((x) => x.trim()).join(',')

export function mysqlReachPlan(n: MySQLNetworkData, reach: Reach, sources: string[], restart: boolean): { steps: PlanStep[]; blocked: string | null } {
  const steps: PlanStep[] = []
  const host = n.host || 'this server'
  const name = n.mariadb ? 'MariaDB' : 'MySQL'
  let blocked: string | null = null
  const block = (why: string) => { blocked ??= why }
  const sent = reach === 'network' ? privateNetworks(n) : reach === 'selected' ? sources : []
  const bind = mysqlBindFor(reach, listenFor(n, reach, sent))

  if (reach !== 'local' && n.firewallError) block(`the firewall could not be read: ${n.firewallError}`)
  if (reach !== 'local' && n.skipNetworking) block(`${name} runs with skip-networking; turn that off on the server first`)
  if ((reach === 'selected' || reach === 'network') && !n.listOK) block(`${name} ${n.version} listens on one address or on every one, so it cannot keep the loopback and add another`)
  if (reach === 'selected' && sources.length === 0) block('add at least one machine')
  if (reach === 'network' && sent.length === 0) block(`${host} has no private address to listen on`)

  const changes = key(bind) !== key(n.next)
  steps.push(changes
    ? { state: 'change', title: reach === 'local' ? `Listen on ${host} only` : reach === 'any' ? 'Listen on every address' : `Listen on ${bind.split(',').slice(1).join(', ')}`,
      detail: `Kept for the next start in ${n.keptIn ?? 'the server’s options'}.`, diff: { from: `bind_address = ${n.next}`, to: `bind_address = ${bind}` } }
    : { state: 'ok', title: `${name} already listens on ${bind === '*' ? 'every address' : bind}${n.pendingRestart ? ' from its next start' : ''}` })

  if (reach !== 'local') {
    const users = n.remoteUsers ?? []
    if (reach === 'any' && (n.noPassRemote?.length ?? 0) > 0) {
      block(`${n.noPassRemote!.join(', ')} can sign in with no password`)
      steps.push({ state: 'blocked', title: 'A user needs no password', detail: `${n.noPassRemote!.join(', ')} may sign in from another machine with none. Give it a password first.` })
    } else if (users.length === 0) {
      steps.push({ state: 'warn', title: 'No user may sign in from another machine yet',
        detail: 'Users are made for a host, like \'app\'@\'10.0.0.%\'. Give an application user one; Argus does not change users’ hosts.' })
    } else {
      steps.push({ state: 'ok', title: `${users.length} user${users.length === 1 ? '' : 's'} may sign in from another machine`, detail: users.join(', ') })
    }
    if (reach === 'any' && !n.ssl) steps.push({ state: 'warn', title: 'TLS is off', detail: 'Passwords and data cross the network unencrypted.' })
  }

  const fw = n.firewall
  const lines = firewallLines(n, reach, sent)
  if (!fw || !n.firewallActive) {
    if (reach === 'selected') {
      block(`no firewall is on, so ${name} cannot be limited to selected machines`)
      steps.push({ state: 'blocked', title: 'No firewall is on', detail: `Argus changes ufw or firewalld and never turns one on, since that can cut off SSH. Choose the private network, or turn a firewall on on ${host} first.` })
    } else {
      steps.push({ state: 'skip', title: 'No firewall is on', detail: reach === 'local' ? undefined : 'Nothing on this server limits who reaches the port; users’ hosts and passwords do.' })
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
    if (!restart) steps.push({ state: 'warn', title: 'Not restarted', detail: `${name} keeps listening as it does now until it is restarted on the server.` })
    else if (!n.service) steps.push({ state: 'warn', title: `Restart ${name} on the server`, detail: 'Argus found no systemd service for it, so it cannot restart it from here.' })
    else steps.push({ state: 'change', title: `Restart ${n.service}`, detail: `${name} reads bind_address only when it starts. Ends every open connection, ${n.clients} now, this console’s included.` })
  } else {
    steps.push({ state: 'ok', title: 'No restart' })
  }
  return { steps, blocked }
}

export function mysqlNowText(n: MySQLNetworkData): { tone: string; text: string } {
  const host = n.host || 'this server'
  const name = n.mariadb ? 'MariaDB' : 'MySQL'
  const src = n.firewallSources ?? []
  const wait = n.pendingRestart ? ` From its next start it listens on ${n.next}.` : ''
  switch (n.reach) {
    case 'local': return { tone: 'amber', text: `Only programs on ${host} can connect. ${name} listens on ${n.skipNetworking ? 'no address (skip-networking)' : n.bind}, so other machines get “connection refused”.${wait}` }
    case 'selected': return { tone: 'teal', text: `${src.length} selected machine${src.length === 1 ? '' : 's'} can connect: ${src.join(', ')}.${wait}` }
    case 'network': return { tone: 'azure', text: `Machines on the networks ${host} is on can connect, as a user made for their host.${wait}` }
    default: return { tone: 'rose', text: `${name} listens on every address of ${host}${n.firewallActive ? '' : ', and no firewall here narrows it'}.${wait}` }
  }
}
