import { describe, expect, it } from 'vitest'
import type { CloudItem, CloudResource } from '~/lib/cloud'
import { buildSecurityMap, pathOf, reachOf } from '~/lib/securityMap'

const at = (r: Partial<CloudResource> & { id: string; kind: CloudResource['kind'] }, made = false): CloudItem => ({
  accountId: 'a1', accountName: 'payments-prod', environment: 'production', firstSeen: '', seenAt: '',
  resource: { name: r.id, region: 'ap-southeast-3', ...r } as CloudResource, madeByArgus: made,
})

const tcp = (port: number, source: string) => ({ inbound: true, protocol: 'tcp', fromPort: port, toPort: port, source })

const groups = [
  at({ kind: 'firewall', id: 'sg-web', name: 'sg-web-public', rules: [tcp(443, '0.0.0.0/0'), tcp(22, '0.0.0.0/0'), tcp(22, '203.0.113.0/24'),
    { inbound: false, protocol: 'all', fromPort: 0, toPort: 65535, source: '0.0.0.0/0' }] }),
  at({ kind: 'firewall', id: 'sg-api', name: 'sg-pay-api', rules: [tcp(8080, 'sg-web')] }),
  at({ kind: 'firewall', id: 'sg-db', name: 'sg-db', rules: [tcp(5432, 'sg-api')] }),
  at({ kind: 'firewall', id: 'sg-agent', name: 'sg-argus-agent', rules: [] }, true),
  at({ kind: 'firewall', id: 'sg-elsewhere', name: 'other', region: 'us-east-1', rules: [tcp(22, '0.0.0.0/0')] } as CloudResource & { id: string; kind: 'firewall' }),
]
const fw = (id: string) => ({ kind: 'firewall' as const, id })
const instances = [
  at({ kind: 'instance', id: 'i-web1', name: 'web-01', state: 'running', links: [fw('sg-web')] }),
  at({ kind: 'instance', id: 'i-api1', name: 'pay-api-01', state: 'running', links: [fw('sg-api')] }),
  at({ kind: 'instance', id: 'i-db1', name: 'pay-db-01', state: 'running', links: [fw('sg-db')] }),
  at({ kind: 'instance', id: 'i-runner', name: 'ops-runner', state: 'running', links: [fw('sg-agent')] }, true),
]

describe('security group map', () => {
  const map = buildSecurityMap(groups, instances, 'a1', 'ap-southeast-3')

  it('keeps to one account and region, and draws only inbound rules', () => {
    expect(map.groups.map((g) => g.id).sort()).toEqual(['sg-agent', 'sg-api', 'sg-db', 'sg-web'])
    expect(map.sources.map((s) => s.label)).toEqual(['Internet', '203.0.113.0/24'])
    expect(map.edges.some((e) => e.rules.some((r) => !r.inbound))).toBe(false)
  })

  it('merges the rules between the same two ends, and flags what is risky', () => {
    const internet = map.edges.find((e) => e.from === 'source:internet' && e.to === 'group:sg-web')!
    expect(internet.rules).toHaveLength(2)
    expect(internet.risky).toBe(true)
    expect(internet.label).toBe('tcp 443, 22')
    expect(map.edges.find((e) => e.from === 'source:203.0.113.0/24')!.risky).toBe(false)
    expect(map.risks).toEqual(['SSH from anywhere on the internet reaches web-01, through sg-web-public.'])
  })

  it('draws a rule naming a group from that group, the way AWS evaluates it', () => {
    const e = map.edges.find((x) => x.to === 'group:sg-api')!
    expect(e.from).toBe('group:sg-web')
    expect(e.label).toBe('tcp 8080 from sg-web-public')
  })

  it('orders groups from the outside in', () => {
    expect(map.groups.map((g) => g.label)).toEqual(['sg-web-public', 'sg-pay-api', 'sg-db', 'sg-argus-agent'])
    expect(map.instances.map((i) => i.label)).toEqual(['web-01', 'pay-api-01', 'pay-db-01', 'ops-runner'])
  })

  it('says what Argus made', () => {
    expect(map.groups.find((g) => g.id === 'sg-agent')!.madeByArgus).toBe(true)
    expect(map.groups.find((g) => g.id === 'sg-web')!.madeByArgus).toBe(false)
  })

  it('lists who can reach each instance in, for the phone', () => {
    expect(reachOf(map, 'instance:i-api1')).toEqual([{ from: 'sg-web-public', ports: 'tcp 8080', risky: false }])
    expect(reachOf(map, 'instance:i-runner')).toEqual([])
  })

  it('fades everything off a selected path', () => {
    const p = pathOf(map, 'group:sg-web>group:sg-api')
    expect([...p].sort()).toEqual(['group:sg-api', 'group:sg-web', 'group:sg-web>group:sg-api'])
    const node = pathOf(map, 'instance:i-api1')
    expect(node.has('group:sg-web')).toBe(true)
    expect(node.has('instance:i-db1')).toBe(false)
  })
})
