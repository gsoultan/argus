import { describe, expect, it } from 'vitest'
import { exposures, firewallEnv, firewallUsers, normalSource, ruleExposure, sameRule, validSource, type CloudItem, type CloudRule } from '~/lib/cloud'

const rule = (r: Partial<CloudRule>): CloudRule => ({ inbound: true, protocol: 'tcp', fromPort: 22, toPort: 22, source: '0.0.0.0/0', ...r })

const item = (id: string, kind: CloudItem['resource']['kind'], extra: Partial<CloudItem['resource']> = {}, environment: CloudItem['environment'] = 'development'): CloudItem => ({
  accountId: 'a', accountName: 'acct', environment, firstSeen: '', seenAt: '',
  resource: { kind, id, name: id, region: 'r', ...extra },
})

describe('exposure', () => {
  it('names risky services open to the internet, and leaves web ports alone', () => {
    expect(ruleExposure(rule({}))).toBe('SSH (22)')
    expect(ruleExposure(rule({ source: '::/0', fromPort: 3389, toPort: 3389 }))).toBe('Remote Desktop (3389)')
    expect(ruleExposure(rule({ protocol: 'all', fromPort: 0, toPort: 65535 }))).toBe('every port')
    expect(ruleExposure(rule({ fromPort: 443, toPort: 443 }))).toBe('')
    expect(ruleExposure(rule({ fromPort: 8080, toPort: 8080 }))).toBe('tcp 8080')
    expect(ruleExposure(rule({ source: '10.0.0.0/8' }))).toBe('')
    expect(ruleExposure(rule({ inbound: false }))).toBe('')
    expect(exposures([rule({}), rule({ fromPort: 20, toPort: 25 })])).toEqual(['SSH (22)', 'FTP (21), SSH (22), Telnet (23)'])
  })
})

describe('sources', () => {
  it('reads an address as its own range and knows groups', () => {
    expect(normalSource(' 203.0.113.9 ')).toBe('203.0.113.9/32')
    expect(normalSource('2001:db8::1')).toBe('2001:db8::1/128')
    expect(normalSource('sg-0123456789abcdef0')).toBe('sg-0123456789abcdef0')
    expect(validSource('10.0.0.0/16')).toBe(true)
    expect(validSource('sg-0123456789abcdef0')).toBe(true)
    expect(validSource('anywhere')).toBe(false)
    expect(sameRule(rule({ description: 'a' }), rule({ description: 'b' }))).toBe(true)
  })
})

describe('a group\'s environment', () => {
  it('is the strictest of what uses it', () => {
    const g = item('sg-1', 'firewall', { tags: { Environment: 'staging' } })
    const prod = item('i-1', 'instance', { state: 'running', links: [{ kind: 'firewall', id: 'sg-1' }] }, 'production')
    const gone = item('i-2', 'instance', { state: 'terminated', links: [{ kind: 'firewall', id: 'sg-1' }] }, 'production')
    const users = firewallUsers(g, [prod, gone])
    expect(users.map((u) => u.resource.id)).toEqual(['i-1'])
    expect(firewallEnv(g, users)).toBe('production')
    expect(firewallEnv(g, [])).toBe('staging')
  })
})
