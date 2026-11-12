import { describe, expect, it } from 'vitest'
import {
  DEFAULT_LEVEL_RULES, attention, editText, hashRef, keyRuleWords, keyspace, levelRules, rulesProblem, sha256Hex,
  treeLevel, userFinding, userNameProblem, type RedisInfoData, type RedisUser,
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
