import { describe, expect, it } from 'vitest'
import { createSHA256 } from 'hash-wasm'
import {
  canonical, GENESIS, linkHash, verifyChain,
  type ChainInput, type ChainLink, type Hasher,
} from '~/workers/chain.worker'

const EVENT: ChainInput = {
  seq: 41,
  id: 'ev-41',
  at: '2026-08-28T09:31:02.000Z',
  action: 'session.terminate',
  severity: 'critical',
  actorEmail: 'lin@northwind.id',
  target: 'db-01.northwind.id',
  detail: 'Session opened outside the approved window for INC-4471.',
}

async function webcryptoHex(input: string): Promise<string> {
  const buf = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(input))
  return [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, '0')).join('')
}

describe('audit chain', () => {
  /**
   * The reason this test exists.
   *
   * Verification moved from WebCrypto to a WASM hasher for speed. If the two
   * ever disagree, every chain in the product reads as tampered — so the
   * equivalence is asserted rather than assumed.
   */
  it('hashes identically to WebCrypto', async () => {
    const sha256 = await createSHA256()
    const input = GENESIS + canonical(EVENT)
    expect(sha256.init().update(input).digest('hex')).toBe(await webcryptoHex(input))
  })

  it('reuses one hasher without carrying state between links', async () => {
    const sha256 = await createSHA256()
    const a = sha256.init().update('first').digest('hex')
    sha256.init().update('something else entirely').digest('hex')
    const again = sha256.init().update('first').digest('hex')
    expect(again).toBe(a)
  })

  /** Canonical form is a contract with the Go control plane, not an implementation detail. */
  it('canonicalises fields in a fixed order, independent of object key order', () => {
    const shuffled: ChainInput = {
      target: EVENT.target,
      seq: EVENT.seq,
      detail: EVENT.detail,
      actorEmail: EVENT.actorEmail,
      severity: EVENT.severity,
      at: EVENT.at,
      id: EVENT.id,
      action: EVENT.action,
    }
    expect(canonical(shuffled)).toBe(canonical(EVENT))
    // Six fields, matching internal/control.chainHash. seq and id are
    // assigned by the database, not asserted by the event, and including them
    // is what stopped the console from reproducing any digest the server wrote.
    expect(canonical(EVENT)).toBe(
      JSON.stringify([
        EVENT.action, EVENT.actorEmail, EVENT.at,
        EVENT.detail, EVENT.severity, EVENT.target,
      ]),
    )
  })

  it('changes the digest when any field changes', async () => {
    const sha256 = await createSHA256()
    const base = sha256.init().update(GENESIS + canonical(EVENT)).digest('hex')
    const tampered = sha256
      .init()
      .update(GENESIS + canonical({ ...EVENT, detail: 'Routine maintenance.' }))
      .digest('hex')
    expect(tampered).not.toBe(base)
  })
})

/**
 * The console must produce the digest the Go control plane produced.
 *
 * These values were printed by internal/control.chainHash for the event below.
 * If the canonical form drifts on either side, the console silently stops being
 * able to check a served log -- which is how it came to include seq and id,
 * reproduce nothing the server had written, and report "intact" for a chain it
 * had recomputed from content rather than verified.
 */
describe('canonical form matches the Go control plane', () => {
  const event: ChainInput = {
    seq: 41,
    id: 'ev-41',
    at: '2026-08-28T09:31:02Z',
    action: 'session.terminate',
    severity: 'critical',
    actorEmail: 'lin@northwind.id',
    target: 'db-01.northwind.id',
    detail: 'Session opened outside the approved window for INC-4471.',
  }

  it('reproduces a digest Go produced, from the genesis hash', async () => {
    const sha256 = await createSHA256()
    expect(sha256.init().update(GENESIS + canonical(event)).digest('hex')).toBe(
      '7efcb60158f4bfa57ffa54629ae33fcbb74d86f131f6945f29c37c2bb7cd7fc7',
    )
  })

  it('reproduces a digest Go produced, chained onto a predecessor', async () => {
    const sha256 = await createSHA256()
    expect(sha256.init().update('a1b2c3' + canonical(event)).digest('hex')).toBe(
      '909481853330515a2d25ad4415a0b573cbbdfe22c6857bdc84ff3094cbf796de',
    )
  })

  // seq and id are assigned by the database and are not part of what the event
  // asserts, so they must not reach the digest.
  it('ignores seq and id', () => {
    expect(canonical({ ...event, seq: 9999, id: 'different' })).toBe(canonical(event))
  })

  it('hashes exactly the six fields the control plane hashes', () => {
    expect(JSON.parse(canonical(event))).toEqual([
      event.action, event.actorEmail, event.at,
      event.detail, event.severity, event.target,
    ])
  })
})

/**
 * Verification against a log the control plane signed.
 *
 * This is the property the audit page claims and the one that was not true:
 * the console hashed a different set of fields than the server, so it could
 * never reproduce a served digest. It recomputed the chain from content,
 * compared that to itself, and reported "intact" for a log it had not checked.
 * An edited record passed silently.
 */
describe('verifying a served chain', () => {
  const at = '2026-08-28T09:31:02Z'
  const mk = (seq: number, detail: string): ChainInput => ({
    seq, id: `ev-${seq}`, at, action: 'session.start', severity: 'info',
    actorEmail: 'lin@northwind.id', target: 'db-01', detail,
  })

  /** A chain as the control plane would have written it. */
  async function served(count: number) {
    const sha = (await createSHA256()) as unknown as Hasher
    const links: ChainLink[] = []
    let prev = GENESIS
    for (let i = 1; i <= count; i++) {
      const e = mk(i, `event ${i}`)
      const hash = linkHash(sha, prev, e)
      links.push({ ...e, prevHash: prev, hash })
      prev = hash
    }
    return { sha, links }
  }

  it('accepts a chain the server wrote', async () => {
    const { sha, links } = await served(4)
    expect(verifyChain(sha, links)).toEqual({ ok: true, checked: 4, brokenAt: null })
  })

  // A compromised server rewrites a record and leaves its hash alone.
  it('detects a record edited in place, and names it', async () => {
    const { sha, links } = await served(4)
    links[1]!.detail = 'quietly rewritten'
    const r = verifyChain(sha, links)
    expect(r.ok).toBe(false)
    expect(r.brokenAt).toBe(2)
  })

  // Removing a row leaves every remaining record internally consistent; only
  // the linkage gives it away.
  it('detects a removed record', async () => {
    const { sha, links } = await served(4)
    const withHole = [links[0]!, links[2]!, links[3]!]
    const r = verifyChain(sha, withHole)
    expect(r.ok).toBe(false)
    expect(r.brokenAt).toBe(3)
  })

  it('detects a forged tail appended by someone without the chain', async () => {
    const { sha, links } = await served(3)
    links.push({ ...mk(4, 'forged'), prevHash: 'f'.repeat(64), hash: 'e'.repeat(64) })
    const r = verifyChain(sha, links)
    expect(r.ok).toBe(false)
    expect(r.brokenAt).toBe(4)
  })

  it('is order-independent: a shuffled response verifies the same', async () => {
    const { sha, links } = await served(5)
    const shuffled = [links[3]!, links[0]!, links[4]!, links[1]!, links[2]!]
    expect(verifyChain(sha, shuffled).ok).toBe(true)
  })
})
