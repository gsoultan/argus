import { describe, expect, it } from 'vitest'
import {
  DEFAULT_LEVEL_RULES, attention, editText, hashRef, keyRuleWords, keyspace, levelRules, rulesProblem, sha256Hex,
  treeLevel, userFinding, userNameProblem, networkSummary, firewallCommand, parseSource, inPrefix, listenFor, reachPlan, bindFor, type RedisInfoData, type RedisUser,
} from '~/lib/redisManage'

describe('rulesProblem', () => {
  it('takes Argus’s own rules at every level', () => {
    for (const [l, r] of Object.entries(DEFAULT_LEVEL_RULES)) expect(rulesProblem(r, l as 'read')).toBeNull()
  })
  it('refuses admin reach below DBA, as the agent does', () => {
    expect(rulesProblem(['~*', '+@all'], 'owner')).toMatch(/-@admin/)
    expect(rulesProblem(['~*', '+@read', '+config'], 'write')).toMatch(/DBA/)
    expect(rulesProblem(['~*', '+@all'], 'dba')).toBeNull()
    expect(rulesProblem(['~*', '+@all'])).toBeNull()
  })
  it('refuses what is not a rule', () => {
    expect(rulesProblem([])).not.toBeNull()
    expect(rulesProblem(['on'])).toMatch(/not a rule/)
    expect(rulesProblem(['>secret'])).toMatch(/not a rule/)
    expect(rulesProblem(['%R~cache:*', '&orders:*', '+get'])).toBeNull()
  })
})

describe('levelRules', () => {
  it('uses the database’s own rules, or Argus’s', () => {
    expect(levelRules({ policy: { redisLevels: { read: ['~cart:*', '+get'] } } }, 'read')).toEqual(['~cart:*', '+get'])
    expect(levelRules({ policy: {} }, 'write')).toEqual(DEFAULT_LEVEL_RULES.write)
  })
})

describe('attention', () => {
  const base: RedisInfoData = { info: { role: 'master', used_memory: '100', maxmemory: '1000', maxmemory_policy: 'allkeys-lru', rdb_last_bgsave_status: 'ok' }, slowlog: [], openDefault: false, drift: [] }
  it('is quiet when nothing is wrong', () => { expect(attention(base)).toEqual([]) })
  it('names memory, drift, an open default and slow commands', () => {
    const got = attention({
      ...base,
      info: { ...base.info, used_memory: '960', maxmemory_policy: 'noeviction' },
      openDefault: true, drift: ['maxmemory'],
      slowlog: [{ id: 1, at: 0, micros: 250_000, command: ['KEYS', '*'], client: '10.0.0.4:5000' }],
    })
    expect(got.map((a) => a.tone)).toEqual(['rose', 'rose', 'rose', 'azure'])
    expect(got[0]!.detail).toMatch(/writes are refused/)
    expect(got[3]!.detail).toMatch(/KEYS \* took 250 ms/)
  })
})

describe('helpers', () => {
  it('reads the keyspace', () => {
    expect(keyspace({ db0: 'keys=10,expires=4,avg_ttl=6000', db2: 'keys=1,expires=0,avg_ttl=0', role: 'master' }))
      .toEqual([{ db: 0, keys: 10, expires: 4, avgTtl: 6000 }, { db: 2, keys: 1, expires: 0, avgTtl: 0 }])
  })
  it('groups keys into folders', () => {
    const k = (name: string) => ({ name, type: 'string', ttl: -1 })
    const top = treeLevel([k('cart:1'), k('cart:2'), k('user:1:name'), k('plain')], '')
    expect(top.map((n) => [n.kind, n.label, n.count])).toEqual([['folder', 'cart', 2], ['folder', 'user', 1], ['key', 'plain', 1]])
    expect(treeLevel([k('cart:1'), k('cart:2')], 'cart:').map((n) => n.label)).toEqual(['1', '2'])
  })
  it('hashes a password the way ACL SETUSER # takes it', async () => {
    const h = await sha256Hex('hunter2')
    expect(h).toBe('f52fbd32b2b3b86ff88ef6c490628285f482af15ddcb29541f94bcf526a3f6c7')
    expect(hashRef(h)).toBe('f52fbd32…26a3f6c7')
  })
  it('says what a key rule means', () => {
    expect(keyRuleWords('%R~cache:*')).toBe('read  cache:*')
    expect(keyRuleWords('~cart:*')).toBe('read + write  cart:*')
  })
  it('quotes edits like redis-cli', () => {
    expect(editText([['HSET', 'u:1', 'name', 'Ana Lima'], ['EXPIRE', 'u:1', '60']])).toBe('HSET u:1 name "Ana Lima"\nEXPIRE u:1 60')
  })
  it('keeps Argus’s user names for Argus', () => {
    expect(userNameProblem('argus-ana-read')).toMatch(/Argus/)
    expect(userNameProblem('checkout api')).not.toBeNull()
    expect(userNameProblem('checkout-api')).toBeNull()
  })
  it('flags an open user', () => {
    const u: RedisUser = { name: 'default', on: true, nopass: true, passwords: [], keys: ['~*'], channels: ['&*'], commands: ['+@all'], selectors: [], argus: false, self: false, clients: 2 }
    expect(userFinding(u)?.tone).toBe('rose')
    expect(userFinding({ ...u, argus: true })).toBeNull()
  })
})

describe('network', () => {
  const base = {
    port: 6379, bind: ['127.0.0.1', '-::1'], protectedMode: true, defaultOpen: false, defaultClients: 0,
    configFile: '/etc/redis/redis.conf', agentConf: true, firewall: '' as const,
    addresses: [{ ip: '192.168.1.20', iface: 'eth0', prefix: '192.168.1.0/24', private: true, state: 'closed' as const }],
  }
  it('says who can connect', () => {
    expect(networkSummary(base).text).toMatch(/Only programs on this server/)
    expect(networkSummary({ ...base, addresses: [{ ...base.addresses[0]!, state: 'protected' }] }).tone).toBe('amber')
    expect(networkSummary({ ...base, addresses: [{ ...base.addresses[0]!, state: 'open' }] }).tone).toBe('rose')
    expect(networkSummary({ ...base, addresses: [{ ...base.addresses[0]!, state: 'signin' }] }).text).toMatch(/192\.168\.1\.20:6379/)
  })
  it('gives the firewall command for the firewall found', () => {
    expect(firewallCommand('ufw', '192.168.1.0/24', 6379)).toBe('sudo ufw allow from 192.168.1.0/24 to any port 6379 proto tcp')
    expect(firewallCommand('firewalld', '10.0.0.0/8', 6380)).toMatch(/source address="10.0.0.0\/8" port port="6380"/)
    expect(firewallCommand('', '10.0.0.0/8', 6380)).toBeNull()
  })
})

describe('who can connect', () => {
  const n = {
    port: 6379, bind: ['127.0.0.1', '-::1'], protectedMode: true, defaultOpen: false, defaultClients: 0,
    configFile: '/etc/redis/redis.conf', agentConf: true, firewall: 'ufw' as const, firewallActive: true, host: 'cache-01',
    addresses: [
      { ip: '10.0.0.183', iface: 'eth0', prefix: '10.0.0.0/24', private: true, state: 'closed' as const },
      { ip: '172.17.0.1', iface: 'docker0', prefix: '172.17.0.0/16', private: true, state: 'closed' as const },
      { ip: '203.0.113.9', iface: 'eth1', prefix: '203.0.113.0/24', private: false, state: 'closed' as const },
    ],
  }
  it('reads a source as the agent writes it', () => {
    expect(parseSource(' 10.0.0.61 ')).toEqual({ source: '10.0.0.61' })
    expect(parseSource('10.0.4.9/24')).toEqual({ source: '10.0.4.0/24' })
    expect(parseSource('fd00::1')).toEqual({ source: 'fd00::1' })
    expect(parseSource('0.0.0.0/0')).toHaveProperty('error')
    expect(parseSource('127.0.0.1')).toHaveProperty('error')
    expect(parseSource('10.0.0.5; reboot')).toHaveProperty('error')
    expect(parseSource('10.0.0.256')).toHaveProperty('error')
  })
  it('listens only on the address next to the machines let in', () => {
    expect(inPrefix('10.0.0.61', '10.0.0.0/24')).toBe(true)
    expect(inPrefix('10.0.1.61', '10.0.0.0/24')).toBe(false)
    expect(listenFor(n, 'selected', ['10.0.0.61'])).toEqual(['10.0.0.183'])
    expect(listenFor(n, 'selected', ['192.168.5.5'])).toEqual(['10.0.0.183', '172.17.0.1'])
    expect(listenFor(n, 'network', [])).toEqual(['10.0.0.183', '172.17.0.1'])
    expect(bindFor('any', [])).toBe('* -::*')
    expect(bindFor('selected', ['10.0.0.183'])).toBe('127.0.0.1 -::1 -10.0.0.183')
  })
  it('plans bind and the firewall, and says what stops it', () => {
    const p = reachPlan(n, 'selected', ['10.0.0.61'], '7.2.4')
    expect(p.blocked).toBeNull()
    expect(p.steps[0]).toMatchObject({ state: 'change', diff: { from: 'bind 127.0.0.1 -::1', to: 'bind 127.0.0.1 -::1 -10.0.0.183' } })
    expect(p.steps.flatMap((s) => s.lines ?? [])).toEqual([
      "ufw allow from 10.0.0.61 to any port 6379 proto tcp comment 'argus 6379'",
      "ufw deny proto tcp to any port 6379 comment 'argus 6379'",
    ])
    expect(reachPlan(n, 'selected', ['10.0.0.61'], '6.2.0').blocked).toMatch(/cannot change bind while it runs/)
    expect(reachPlan({ ...n, firewallActive: false }, 'selected', ['10.0.0.61']).blocked).toMatch(/no firewall is on/)
    expect(reachPlan({ ...n, firewallOpenToAll: true }, 'selected', ['10.0.0.61']).blocked).toMatch(/already lets everyone in/)
    expect(reachPlan({ ...n, defaultOpen: true }, 'network', []).blocked).toMatch(/default user a password/)
    expect(reachPlan({ ...n, noPassUsers: ['legacy'] }, 'any', []).blocked).toMatch(/legacy has no password/)
    expect(reachPlan({ ...n, noPassUsers: ['legacy'] }, 'network', []).blocked).toBeNull()
    expect(reachPlan(n, 'selected', []).blocked).toMatch(/add at least one machine/)
    expect(reachPlan(n, 'local', []).steps[0]).toMatchObject({ state: 'ok' })
  })
})
