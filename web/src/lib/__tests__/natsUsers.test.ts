import { describe, expect, it } from 'vitest'
import { expiry, presetSpec, revokeCommand, ruleLines, userFindings, userProblems, validSource, type NatsAppUser } from '~/lib/natsUsers'

describe('nats app users', () => {
  it('builds each kind of app from its subjects', () => {
    expect(presetSpec('publisher', ['orders.>'])).toEqual({ pubAllow: ['orders.>'], subAllow: [], days: 90 })
    expect(presetSpec('service', ['svc.price'], 30)).toEqual({ pubAllow: [], subAllow: ['svc.price'], replies: true, days: 30 })
    expect(presetSpec('jetstream', ['orders.>'])).toMatchObject({ jetstream: 'use', requests: true })
  })
  it('says what is wrong as the agent would', () => {
    expect(userProblems('orders-api', presetSpec('publisher', ['orders.>']))).toEqual([])
    expect(userProblems('argus-ana-owner', presetSpec('publisher', ['orders.>']))[0]).toContain('Argus')
    expect(userProblems('a', { pubAllow: ['$SYS.>'], subAllow: [], days: 1 })).toEqual(['The system account’s subjects are never given to an app.'])
    expect(userProblems('a', { pubAllow: [], subAllow: [], days: 1 })).toEqual(['The user may do nothing yet: give it a subject to publish or subscribe to.'])
    expect(userProblems('a', { pubAllow: ['x'], subAllow: [], days: 400 })).toHaveLength(1)
    expect(userProblems('a', { pubAllow: ['x', 'x'], subAllow: [], days: 1 })).toHaveLength(1)
  })
  it('takes addresses and ranges, not a range that does not start where it says', () => {
    expect(['10.0.0.0/8', '192.0.2.7', '192.0.2.7/32', '2001:db8::/32', '::1'].every(validSource)).toBe(true)
    expect(['10.0.0.1/8', '300.1.1.1', '10.0.0.0/33', 'host.example', ''].some(validSource)).toBe(false)
  })
  it('says the rules and the expiry in words', () => {
    expect(ruleLines({ pubAllow: ['orders.>'], pubDeny: ['orders.admin.>'], subAllow: [], replies: true, days: 1 })).toEqual([
      { what: 'Publishes to', words: 'orders.>, except orders.admin.>' },
      { what: 'Subscribes to', words: 'nothing' },
      { what: 'Replies', words: 'answers requests, once each, within a minute' },
    ])
    const now = Date.parse('2026-11-24T00:00:00Z')
    expect(expiry('2027-02-22T00:00:00Z', now)).toEqual({ text: 'Expires in 90 days', color: 'slate' })
    expect(expiry('2026-12-01T00:00:00Z', now)).toEqual({ text: 'Expires in 7 days', color: 'amber' })
    expect(expiry('2026-11-21T00:00:00Z', now)).toEqual({ text: 'Expired 3 days ago', color: 'rose' })
  })
  it('lists the users to renew, and the revoke for the operator', () => {
    const now = Date.parse('2026-11-24T00:00:00Z')
    const u = (name: string, expires: string, connections: number, inArgus = true): NatsAppUser =>
      ({ name, publicKey: `U${name}`, expires, connections, inArgus, spec: { pubAllow: ['a'], subAllow: [], days: 90 } })
    const f = userFindings([u('soon', '2026-11-30T00:00:00Z', 1), u('gone', '2026-11-01T00:00:00Z', 0), u('late', '2026-11-20T00:00:00Z', 2),
      u('fine', '2027-06-01T00:00:00Z', 1), u('nsc', '', 3, false)], now)
    expect(f.map((x) => x.key)).toEqual(['Usoon', 'Ulate'])
    expect(f[1]!.text).toContain('with 2 connections now')
    expect(revokeCommand('ORDERS', 'UABC')).toBe('nsc revocations add-user --account ORDERS --user-public-key UABC\nnsc push --account ORDERS')
  })
})
