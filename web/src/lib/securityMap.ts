import { RISKY_PORTS, portText, type CloudItem, type CloudRule } from './cloud'

/*
The security group map: who can reach in, through which security group, to
which instance. It is built only from the inbound security group rules
Argus last read, so it never claims more than those rules say: network
ACLs, routes and the instances' own firewalls are not in it.
*/

export type MapNodeKind = 'source' | 'group' | 'instance'

export interface MapNode {
  /** Unique within the map: "source:0.0.0.0/0", "group:sg-1", "instance:i-1". */
  key: string
  kind: MapNodeKind
  id: string
  label: string
  meta: string
  /** The internet: 0.0.0.0/0 and ::/0. */
  internet?: boolean
  /** A source or group a risky internet rule passes through. */
  risky?: boolean
  /** For groups and instances: whether Argus made it. */
  madeByArgus?: boolean
}

export interface MapEdge {
  key: string
  from: string
  to: string
  /** The inbound rules that allow it; empty for group → instance. */
  rules: CloudRule[]
  /** The group whose rules these are, for a rule edge. */
  ruleGroup?: string
  label: string
  risky: boolean
}

export interface SecurityMap {
  sources: MapNode[]
  groups: MapNode[]
  instances: MapNode[]
  edges: MapEdge[]
  /** Plain sentences, one per risky path, for the summary. */
  risks: string[]
}

const INTERNET = new Set(['0.0.0.0/0', '::/0'])

export const isGroupRef = (s: string) => /^sg-/.test(s)

/** What a risky internet rule opens, or [] when it opens nothing risky. */
export function riskyOf(rule: CloudRule): string[] {
  if (!rule.inbound || !INTERNET.has(rule.source)) return []
  if (rule.protocol === 'all') return ['every port']
  const out: string[] = []
  for (const [port, name] of Object.entries(RISKY_PORTS)) {
    const p = Number(port)
    if (rule.protocol === 'tcp' && rule.fromPort <= p && p <= rule.toPort) out.push(name)
  }
  return out
}

/** "tcp 443, 80, 22" rather than "tcp 443, tcp 80, tcp 22". */
function ports(rules: CloudRule[]): string {
  const by = new Map<string, string[]>()
  for (const r of rules) {
    const t = portText(r)
    const sp = t.indexOf(' ')
    const proto = /^(tcp|udp) /.test(t) ? t.slice(0, sp) : t
    const rest = proto === t ? '' : t.slice(sp + 1)
    const list = by.get(proto) ?? []
    if (rest && !list.includes(rest)) list.push(rest)
    by.set(proto, list)
  }
  return [...by.entries()].map(([p, l]) => (l.length ? `${p} ${l.join(', ')}` : p)).join('; ')
}

/** The map of one account and region. */
export function buildSecurityMap(groups: CloudItem[], instances: CloudItem[], accountId: string, region: string): SecurityMap {
  const here = (it: CloudItem) => it.accountId === accountId && it.resource.region === region
  const gs = groups.filter(here)
  const byId = new Map(gs.map((g) => [g.resource.id, g]))
  const sources = new Map<string, MapNode>()
  const edges = new Map<string, MapEdge>()
  const risks: string[] = []
  const riskyGroups = new Set<string>()

  for (const g of gs) {
    const to = `group:${g.resource.id}`
    for (const rule of g.resource.rules ?? []) {
      if (!rule.inbound) continue
      let from: string
      if (byId.has(rule.source) || isGroupRef(rule.source)) {
        if (!byId.has(rule.source)) continue // a group in another account or region
        from = `group:${rule.source}`
      } else {
        const internet = INTERNET.has(rule.source)
        const sk = internet ? 'internet' : rule.source
        from = `source:${sk}`
        if (!sources.has(from)) {
          sources.set(from, { key: from, kind: 'source', id: sk, internet,
            label: internet ? 'Internet' : rule.description || rule.source,
            meta: internet ? '0.0.0.0/0, ::/0' : rule.source })
        }
      }
      const key = `${from}>${to}`
      const e = edges.get(key) ?? { key, from, to, rules: [], ruleGroup: g.resource.id, label: '', risky: false }
      e.rules.push(rule)
      const bad = riskyOf(rule)
      if (bad.length) {
        e.risky = true
        riskyGroups.add(g.resource.id)
        sources.get(from)!.risky = true
      }
      edges.set(key, e)
    }
  }
  for (const e of edges.values()) {
    const from = e.from.startsWith('group:') ? byId.get(e.from.slice(6))?.resource.name : undefined
    e.label = ports(e.rules) + (from ? ` from ${from}` : '')
  }

  const insts: MapNode[] = []
  const users = new Map<string, string[]>()
  for (const it of instances.filter(here)) {
    const r = it.resource
    const mine = (r.links ?? []).filter((l) => l.kind === 'firewall' && byId.has(l.id))
    if (mine.length === 0) continue
    const key = `instance:${r.id}`
    insts.push({ key, kind: 'instance', id: r.id, label: r.name,
      meta: [r.addresses?.[0], r.state, it.server ? 'Argus server' : ''].filter(Boolean).join(' · '),
      madeByArgus: Boolean(it.madeByArgus) })
    for (const l of mine) {
      const ek = `group:${l.id}>${key}`
      edges.set(ek, { key: ek, from: `group:${l.id}`, to: key, rules: [], label: '', risky: riskyGroups.has(l.id) })
      users.set(l.id, [...(users.get(l.id) ?? []), r.name])
    }
  }

  const groupNodes: MapNode[] = gs.map((g) => {
    const inbound = (g.resource.rules ?? []).filter((x) => x.inbound).length
    return { key: `group:${g.resource.id}`, kind: 'group', id: g.resource.id, label: g.resource.name,
      meta: `${inbound} in · ${(g.resource.rules?.length ?? 0) - inbound} out`, risky: riskyGroups.has(g.resource.id),
      madeByArgus: Boolean(g.madeByArgus) }
  })

  for (const e of edges.values()) {
    if (!e.risky || !e.from.startsWith('source:')) continue
    const g = byId.get(e.to.slice(6))!
    const what = [...new Set(e.rules.flatMap(riskyOf))].join(', ')
    const who = users.get(g.resource.id)
    risks.push(`${what} from anywhere on the internet reaches ${who?.length ? who.join(', ') : 'nothing yet'}, through ${g.resource.name}.`)
  }

  // Order: groups by how far in they are (reached from outside first, then
  // the groups those let in, and so on; unreachable last), instances under
  // the first group they use, sources with the internet first.
  const depth = new Map<string, number>()
  let frontier = [...edges.values()].filter((e) => e.from.startsWith('source:')).map((e) => e.to)
  for (let d = 0; frontier.length && d < 50; d++) {
    const next: string[] = []
    for (const k of frontier) {
      if (depth.has(k)) continue
      depth.set(k, d)
      for (const e of edges.values()) if (e.from === k && e.to.startsWith('group:')) next.push(e.to)
    }
    frontier = next
  }
  const dep = (n: MapNode) => depth.get(n.key) ?? 99
  groupNodes.sort((a, b) => dep(a) - dep(b) || a.label.localeCompare(b.label))
  const rank = new Map(groupNodes.map((g, i) => [g.id, i]))
  const first = (n: MapNode) => Math.min(...[...edges.values()].filter((e) => e.to === n.key).map((e) => rank.get(e.from.slice(6)) ?? 99))
  insts.sort((a, b) => first(a) - first(b) || a.label.localeCompare(b.label))
  const srcs = [...sources.values()].sort((a, b) => Number(Boolean(b.internet)) - Number(Boolean(a.internet)) || a.label.localeCompare(b.label))
  return { sources: srcs, groups: groupNodes, instances: insts, edges: [...edges.values()], risks }
}

/** Who can reach an instance in, by source, for the phone list. */
export function reachOf(map: SecurityMap, instanceKey: string): { from: string; ports: string; risky: boolean }[] {
  const out: { from: string; ports: string; risky: boolean }[] = []
  const name = (k: string) => [...map.sources, ...map.groups].find((n) => n.key === k)?.label ?? k
  for (const ge of map.edges.filter((e) => e.to === instanceKey)) {
    for (const e of map.edges.filter((x) => x.to === ge.from && x.rules.length)) {
      out.push({ from: name(e.from), ports: ports(e.rules), risky: e.risky })
    }
  }
  return out
}

/** The keys an edge or node touches, so the rest can fade. */
export function pathOf(map: SecurityMap, key: string): Set<string> {
  const keep = new Set<string>([key])
  const edge = map.edges.find((e) => e.key === key)
  if (edge) {
    keep.add(edge.from).add(edge.to)
    return keep
  }
  // A node: everything that reaches it and everything it reaches, one hop
  // through groups on either side.
  const walk = (k: string, dir: 'in' | 'out', depth: number) => {
    if (depth > 3) return
    for (const e of map.edges) {
      const next = dir === 'in' ? (e.to === k ? e.from : null) : (e.from === k ? e.to : null)
      if (next && !keep.has(e.key)) {
        keep.add(e.key).add(next)
        walk(next, dir, depth + 1)
      }
    }
  }
  walk(key, 'in', 0)
  walk(key, 'out', 0)
  return keep
}
