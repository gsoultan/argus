import { describe, expect, it } from 'vitest'
import {
  appBootstrap, clientSettings, coveredTopics, kafkaUserFindings, kafkaUserNameProblem, madeBy, planACLs, quotaWords,
  signsInWith, topicProblem, userCaps, userGroups, type KafkaACL, type UserPlan,
} from '~/lib/kafkaUsers'
import { fromB64, saltedPassword, scramCredential, toB64 } from '~/lib/scram'

const acl = (resource: string, pattern: string, name: string, operation: string, permission = 'ALLOW'): KafkaACL =>
  ({ resource, pattern, name, operation, permission, host: '*' })

describe('SCRAM credentials', () => {
  // RFC 7677's example: the salted password that gives its client proof.
  it('salts the RFC 7677 password as Kafka does', async () => {
    const salt = fromB64('W22ZaJ0SNY7soEsUEjb6gQ==')
    expect(toB64(await saltedPassword('pencil', salt, 4096, 'SCRAM-SHA-256'))).toBe('xKSVEDI6tPlSysH6mUQZOeeOp01r6B3fcJbodRPcYV0=')
    expect(toB64(await saltedPassword('pencil', salt, 4096, 'SCRAM-SHA-512')))
      .toBe('8W7+G+Z/HQlQLr1e2SYv3f+6Wjd6tPC2h+XtW6D1Boa4pK4WZHbairO5UdL6kji2OZj0VGG8M6RkgUlJzsljHQ==')
  })
  it('makes a new salt each time and never carries the password', async () => {
    const a = await scramCredential('a-long-password-1', 'SCRAM-SHA-512')
    const b = await scramCredential('a-long-password-1', 'SCRAM-SHA-512')
    expect(a.salt).not.toBe(b.salt)
    expect(fromB64(a.saltedPassword)).toHaveLength(64)
    expect(a.iterations).toBe(8192)
    expect(JSON.stringify(a)).not.toContain('a-long-password-1')
  })
})

describe('Kafka users in words', () => {
  it('reads ACLs as what a user can do', () => {
    const acls = [acl('topic', 'PREFIXED', 'orders.', 'WRITE'), acl('topic', 'LITERAL', 'orders.eu', 'READ'),
      acl('topic', 'LITERAL', 'orders.x', 'READ', 'DENY'), acl('group', 'PREFIXED', 'billing', 'READ')]
    expect(userCaps(acls)).toEqual([
      { cap: 'Produce', topics: ['orders.*'] }, { cap: 'Consume', topics: ['orders.eu'] }, { cap: 'Deny', topics: ['orders.x'] },
    ])
    expect(userGroups(acls)).toEqual(['billing…'])
    expect(signsInWith({ name: 'CN=app', acls: [], quotas: {} })).toBe('Certificate')
    expect(signsInWith({ name: 'a', mechanisms: ['SCRAM-SHA-512'], acls: [], quotas: {} })).toBe('SCRAM-SHA-512')
  })
  it('says when a user reads every topic, wider than the entry', () => {
    const users = [{ name: 'spy', acls: [], outside: [acl('topic', 'LITERAL', '*', 'READ')], quotas: {} }]
    expect(kafkaUserFindings(users, 'orders.')).toHaveLength(1)
    expect(kafkaUserFindings(users, '*')).toHaveLength(0)
  })
})

describe('a new Kafka user', () => {
  const plan: UserPlan = { what: 'both', match: 'prefix', topic: 'orders.inv', groupPrefix: 'invoices-app', transactions: true, custom: [] }
  it('turns the choices into ACLs', () => {
    expect(planACLs(plan, 'invoices-app')).toEqual([
      acl('topic', 'PREFIXED', 'orders.inv', 'WRITE'), acl('topic', 'PREFIXED', 'orders.inv', 'READ'),
      acl('topic', 'PREFIXED', 'orders.inv', 'DESCRIBE'), acl('group', 'PREFIXED', 'invoices-app', 'READ'),
      acl('transactional_id', 'PREFIXED', 'invoices-app', 'WRITE'), acl('transactional_id', 'PREFIXED', 'invoices-app', 'DESCRIBE'),
    ])
    expect(planACLs({ ...plan, what: 'produce', transactions: false }, 'x').map((a) => a.operation)).toEqual(['WRITE', 'DESCRIBE'])
  })
  it('keeps topics and names inside what the entry allows', () => {
    expect(topicProblem('orders.', 'prefix', 'payments.')).toMatch(/starts with orders\./)
    expect(topicProblem('orders.', 'exact', 'orders.')).toMatch(/Name a topic/)
    expect(topicProblem('*', 'prefix', 'payments.')).toBeNull()
    expect(coveredTopics('prefix', 'orders.e', ['orders.us', 'orders.eu', 'orders.eu2'])).toEqual(['orders.eu', 'orders.eu2'])
    expect(kafkaUserNameProblem('argus-ana', [])).toMatch(/Argus/)
    expect(kafkaUserNameProblem('app', ['app'])).toMatch(/Already/)
    expect(kafkaUserNameProblem('invoices-app', [])).toBeNull()
  })
  it('gives apps the advertised address, not the loopback', () => {
    expect(appBootstrap({ advertised: 'SASL_PLAINTEXT://localhost:9092,SASL_SSL://kafka.example.com:9093' }, 'x:1'))
      .toEqual({ address: 'kafka.example.com:9093', protocol: 'SASL_SSL', loopbackOnly: false })
    expect(appBootstrap({ advertised: 'SASL_PLAINTEXT://127.0.0.1:9092' }, 'x:1').loopbackOnly).toBe(true)
    expect(appBootstrap(null, 'stream-01:9092').address).toBe('stream-01:9092')
  })
  it('writes client settings with the password quoted', () => {
    const d = { bootstrap: 'k:9093', protocol: 'SASL_SSL', mechanism: 'SCRAM-SHA-512', user: 'app', password: 'p"w', group: 'app' }
    expect(clientSettings(d, 'java')).toContain('password="p\\"w";')
    expect(clientSettings(d, 'librdkafka')).toContain('security.protocol=sasl_ssl')
    expect(clientSettings(d, 'env')).toContain('KAFKA_GROUP_ID=app')
    expect(quotaWords({ produceBytes: 5 * 1024 * 1024 })).toBe('produce 5 MB/s')
  })
  it('finds who made each user, newest first', () => {
    const m = madeBy([
      { user: 'a', action: 'broker.user.changed', by: 'x', at: '2026-11-03T00:00:00Z', text: '' },
      { user: 'a', action: 'broker.user.created', by: 'ana@example.com', at: '2026-11-02T00:00:00Z', text: '' },
    ])
    expect(m.get('a')?.by).toBe('ana@example.com')
  })
})

describe('who reaches a topic', () => {
  const acl = (name: string, pattern: string, operation: string, permission = 'ALLOW') => ({ resource: 'topic', pattern, name, operation, permission, host: '*' })
  it('matches literal, prefixed and whole-cluster ACLs, without Argus’s own users', async () => {
    const { topicAccess, levelCaps } = await import('~/lib/kafkaUsers')
    const reach = topicAccess({ scope: 'orders.', notShown: 0, sessions: ['argus-x'], users: [
      { name: 'a', quotas: {}, acls: [acl('orders.paid', 'LITERAL', 'READ')] },
      { name: 'b', quotas: {}, acls: [acl('orders.', 'PREFIXED', 'WRITE')] },
      { name: 'c', quotas: {}, acls: [], outside: [acl('*', 'LITERAL', 'READ')] },
      { name: 'd', quotas: {}, acls: [acl('orders.eu', 'LITERAL', 'READ')] },
      { name: 'argus-x', quotas: {}, acls: [acl('*', 'LITERAL', 'ALL')] },
    ] }, 'orders.paid')
    expect(reach.map((r) => [r.user, r.caps.join(','), r.because.join(',')])).toEqual([
      ['a', 'Consume', 'LITERAL orders.paid'],
      ['b', 'Produce', 'PREFIXED orders.'],
      ['c', 'Consume', 'LITERAL * (the whole cluster)'],
    ])
    expect(levelCaps('write')).toEqual(['Produce', 'Consume'])
  })
})
