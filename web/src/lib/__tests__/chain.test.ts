import { describe, expect, it } from 'vitest'
import { createSHA256 } from 'hash-wasm'
import { canonical, GENESIS, type ChainInput } from '~/workers/chain.worker'

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
    expect(canonical(EVENT)).toBe(
      JSON.stringify([
        EVENT.action, EVENT.actorEmail, EVENT.at, EVENT.detail,
        EVENT.id, EVENT.seq, EVENT.severity, EVENT.target,
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
