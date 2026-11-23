import { describe, expect, it } from 'vitest'
import { rabbitPasswordHash } from '~/lib/rabbitHash'
import { amqpURL, matching, patternWords, permLines, planPerms, prefixPattern, rabbitSettings, rabbitUserNameProblem, type RabbitPlan } from '~/lib/rabbitUsers'
import { fromB64 } from '~/lib/scram'

describe('RabbitMQ password_hash', () => {
  // RabbitMQ's documented example: salt 908DC60A and the password test12.
  it('hashes as rabbit_password_hashing_sha256 does', async () => {
    expect(await rabbitPasswordHash('test12', new Uint8Array([0x90, 0x8d, 0xc6, 0x0a]))).toBe('kI3GCqW5JLMJa4iX1lo7X4D6XbYqlLgxIs30+P6tENUV2POR')
  })
  it('salts each time with 4 new bytes', async () => {
    const a = await rabbitPasswordHash('a-long-password'), b = await rabbitPasswordHash('a-long-password')
    expect(a).not.toBe(b)
    expect(fromB64(a)).toHaveLength(36)
  })
})

describe('RabbitMQ users in words', () => {
  const plan: RabbitPlan = { what: 'both', exchange: 'invoices.', queue: 'invoices.', declares: true, custom: { configure: '', write: '', read: '' } }
  it('turns the choices into the three patterns', () => {
    expect(planPerms(plan)).toEqual({ configure: '^invoices\\.', write: '^invoices\\.', read: '^invoices\\.' })
    expect(planPerms({ ...plan, what: 'publish' })).toEqual({ configure: '^$', write: '^invoices\\.', read: '^$' })
    expect(planPerms({ ...plan, what: 'consume', exchange: 'events', declares: true })).toEqual({ configure: '^invoices\\.', write: '^invoices\\.', read: '^(?:invoices\\.|events)' })
    expect(prefixPattern(['', 'x'])).toBe('.*')
  })
  it('reads patterns back as words', () => {
    expect(patternWords('^$')).toBe('nothing')
    expect(patternWords('.*')).toBe('everything')
    expect(patternWords('^invoices\\.')).toBe('starting invoices.')
    expect(patternWords('^(?:invoices\\.|events)')).toBe('starting invoices. or starting events')
    expect(patternWords('^a.*b$')).toBe('matching ^a.*b$')
    expect(permLines({ configure: '^$', write: '^x', read: '.*' }).map((l) => l.what)).toEqual(['Publishes to and binds', 'Reads from'])
  })
  it('says what a pattern matches now and checks names', () => {
    expect(matching('^invoices\\.', ['invoices.eu', 'orders', 'invoices.us'])).toEqual(['invoices.eu', 'invoices.us'])
    expect(matching('^$', ['a'])).toEqual([])
    expect(rabbitUserNameProblem('argus-x', [])).toMatch(/Argus/)
    expect(rabbitUserNameProblem('invoices', [])).toBeNull()
  })
  it('writes the URL with the vhost encoded', () => {
    const d = { host: 'mq-01', port: 5672, tls: false, vhost: '/orders', user: 'inv', password: 'p@ss/w' }
    expect(amqpURL(d)).toBe('amqp://inv:p%40ss%2Fw@mq-01:5672/%2Forders')
    expect(rabbitSettings(d, 'spring')).toContain('spring.rabbitmq.virtual-host=/orders')
  })
})
