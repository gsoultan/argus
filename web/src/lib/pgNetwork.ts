import {
  firewallLines, listenFor, parseSource, privateNetworks, type PlanStep, type Reach, type ReachView,
} from '~/lib/redisManage'

/*
Which machines can connect to a Postgres, as the agent reports it, and the
plan of what a choice changes: listen_addresses (ALTER SYSTEM, which takes a
restart), Argus's own pg_hba.conf lines, last in the file and always asking
for a password, and the server's firewall. The words match what the agent
refuses, so the console says beforehand what will not run.
*/

export interface PGNetworkData extends ReachView {
  listen: string
  hbaFile: string
  /** What Argus's pg_hba.conf lines ask for: scram-sha-256, or md5. */
  method: string
  ssl: boolean
  /** listen_addresses was changed and waits for a restart. */
  pendingRestart: boolean
  mine?: string[]
  others?: string[]
  trust?: string[]
  openToAll?: boolean
  clients: number
  remote: number
  version?: string
  reach: Reach
  host: string
  /** The systemd service Argus restarts; absent when it found none. */
  service?: string
}

export interface PGNetworkResult {
  network: PGNetworkData
  firewallRan?: string[]
  restarted?: boolean
  restartError?: string
}

/** listen_addresses for a choice, as tunnel.PGListenFor writes it. */
export function pgListenFor(reach: Reach, addresses: string[]): string {
  if (reach === 'any') return '*'
  if (reach === 'local') return 'localhost'
  return ['localhost', ...addresses].join(',')
}

function cidr(src: string): string {
  const p = parseSource(src)
  if ('error' in p) return src
  if (p.source.includes('/')) return p.source
  return p.source.includes(':') ? `${p.source}/128` : `${p.source}/32`
}

/** The pg_hba.conf lines Argus appends, as tunnel.PGHBALines writes them. */
export function pgHBALines(reach: Reach, sources: string[], method: string): string[] {
  const line = (c: string) => `host all all ${c} ${method} # argus`
  if (reach === 'any') return [line('0.0.0.0/0'), line('::/0')]
  if (reach === 'selected' || reach === 'network') return sources.map((s) => line(cidr(s)))
  return []
}

/** Normalize listen_addresses for comparing. */
function listenKey(l: string): string {
  return l.split(',').map((x) => x.trim()).filter(Boolean).sort().join(',')
}

/**
 * What Argus will change on the server for a choice, step by step, and the
 * first thing that stops it, if anything does.
 */
export function pgReachPlan(n: PGNetworkData, reach: Reach, sources: string[], restart: boolean): { steps: PlanStep[]; blocked: string | null } {
  const steps: PlanStep[] = []
  const host = n.host || 'this server'
  let blocked: string | null = null
  const block = (why: string) => { blocked ??= why }
  const sent = reach === 'network' ? privateNetworks(n) : reach === 'selected' ? sources : []
  const listen = pgListenFor(reach, listenFor(n, reach, sent))

  if (reach !== 'local' && n.firewallError) block(`the firewall could not be read: ${n.firewallError}`)
  if (reach === 'selected' && sources.length === 0) block('add at least one machine')
  if (reach === 'network' && sent.length === 0) block(`${host} has no private address to listen on`)

  const listenChanges = listenKey(listen) !== listenKey(n.listen)
  steps.push(listenChanges
    ? { state: 'change', title: reach === 'local' ? `Listen on ${host} only` : reach === 'any' ? 'Listen on every address' : `Listen on ${listen.split(',').slice(1).join(', ')}`,
      detail: 'Set with ALTER SYSTEM, in postgresql.auto.conf. Postgres listens anew once it restarts.',
      diff: { from: `listen_addresses = '${n.listen}'`, to: `listen_addresses = '${listen}'` } }
    : { state: 'ok', title: `Postgres already listens on ${listen === '*' ? 'every address' : listen}` })

  const lines = pgHBALines(reach, sent, n.method)
  const mine = n.mine ?? []
  if (reach === 'any' && (n.trust?.length ?? 0) > 0) {
    block('pg_hba.conf lets machines in with no password')
    steps.push({ state: 'blocked', title: 'A pg_hba.conf line asks for no password', detail: `Change it on the server first: ${n.trust!.join('; ')}` })
  }
  if (lines.join('\n') === mine.join('\n')) {
    steps.push({ state: 'ok', title: lines.length ? 'pg_hba.conf already lets them in' : 'pg_hba.conf has no Argus lines' })
  } else {
    steps.push({ state: 'change', title: lines.length ? `pg_hba.conf: let ${reach === 'any' ? 'every address' : sent.join(', ')} sign in with a password` : 'pg_hba.conf: remove Argus’s lines',
      detail: `${n.hbaFile}. Only lines ending “# argus” are Argus’s; the server’s own come first and still decide. Checked by Postgres before it is reloaded.`,
      lines: [...mine.map((l) => `- ${l}`), ...lines.map((l) => `+ ${l}`)] })
  }
  if (reach !== 'local') {
    steps.push({ state: 'ok', title: `A password is always asked (${n.method})`, detail: n.ssl ? 'TLS is on.' : 'TLS is off: passwords use SCRAM, but data crosses the network unencrypted.' })
  }

  const fw = n.firewall
  const fwLines = firewallLines(n, reach, sent)
  if (!fw || !n.firewallActive) {
    if (reach === 'selected') {
      block('no firewall is on, so Postgres cannot be limited to selected machines')
      steps.push({ state: 'blocked', title: 'No firewall is on', detail: `Argus changes ufw or firewalld and never turns one on, since that can cut off SSH. Choose the private network, or turn a firewall on on ${host} first.` })
    } else {
      steps.push({ state: 'skip', title: 'No firewall is on', detail: reach === 'local' ? undefined : 'Nothing on this server limits who reaches the port; pg_hba.conf and the password do.' })
    }
  } else if (reach === 'selected' && n.firewallOpenToAll) {
    block('a firewall rule Argus did not add already lets everyone in')
    steps.push({ state: 'blocked', title: 'A rule already lets everyone in', detail: `Remove it on the server first: ${(n.firewallOthers ?? []).join('; ')}` })
  } else if (fwLines.length) {
    steps.push({ state: 'change', title: `Firewall on ${host} (${fw})`, lines: fwLines,
      detail: reach === 'local' ? 'Only the rules Argus added are removed.' : reach === 'any' ? 'Lets everyone reach the port.' : 'Lets these in and refuses the port to everyone else.' })
  } else {
    steps.push({ state: 'ok', title: `Firewall on ${host} (${fw}): nothing to change` })
  }

  if (listenChanges || n.pendingRestart) {
    if (!restart) {
      steps.push({ state: 'warn', title: 'Not restarted', detail: 'Postgres keeps listening as it does now until it is restarted on the server.' })
    } else if (!n.service) {
      steps.push({ state: 'warn', title: 'Restart Postgres on the server', detail: 'Argus found no systemd service for it, so it cannot restart it from here.' })
    } else {
      steps.push({ state: 'change', title: `Restart ${n.service}`,
        detail: `Ends every open connection, ${n.clients} now, this console’s included; applications reconnect.` })
    }
  } else {
    steps.push({ state: 'ok', title: 'No restart', detail: 'pg_hba.conf is reloaded; open connections stay.' })
  }
  return { steps, blocked }
}

/** Who can connect now, in one line. */
export function pgNowText(n: PGNetworkData): { tone: string; text: string } {
  const host = n.host || 'this server'
  const src = n.firewallSources ?? []
  const wait = n.pendingRestart ? ' A change waits for Postgres to restart.' : ''
  switch (n.reach) {
    case 'local': return { tone: 'amber', text: `Only programs on ${host} can connect. Postgres listens on ${n.listen || 'localhost'}, so other machines get “connection refused”.${wait}` }
    case 'selected': return { tone: 'teal', text: `${src.length} selected machine${src.length === 1 ? '' : 's'} can connect: ${src.join(', ')}.${wait}` }
    case 'network': return { tone: 'azure', text: `Machines on the networks ${host} is on can connect with a password.${wait}` }
    default: return { tone: 'rose', text: `Postgres listens on every address of ${host}${n.firewallActive ? '' : ', and no firewall here narrows it'}.${wait}` }
  }
}
