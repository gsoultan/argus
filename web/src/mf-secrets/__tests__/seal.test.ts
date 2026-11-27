import { describe, expect, it } from 'vitest'
import { b64urlDecode, oneTimeKey, openSealed, readAAD, sealTo, stdB64Decode, stdB64Encode, writeAAD } from '../seal'

// Sealed by internal/secretwire.SealTo in Go, to the key below, as
// argus-secret answers a read of prod/db version 3, field password.
const GO = {
  d: '2XJUCWKqvFzIHGFBQ_TXDy3KzdJCzHOQPlbwXnK6mww',
  x: 'tzHQK92AUkD2zNtMvwEG31ZhLUUxpf_BKgE7_opeGD0',
  sealed: 'Aa3iqDhF0rk+yP6y/oSTatNe6HdhJWygScOfSGMZKL1ljeeYG5g+AR9ugDWeaBb9h9A47ZtlT8BCF4wbyscRuos6OKU=',
}

async function goKey() {
  const privateKey = await crypto.subtle.importKey('jwk', { kty: 'OKP', crv: 'X25519', d: GO.d, x: GO.x },
    { name: 'X25519' }, false, ['deriveBits'])
  return { privateKey, publicKey: b64urlDecode(GO.x) }
}

describe('sealing, byte for byte with secretwire', () => {
  it('opens what argus-secret sealed', async () => {
    const plain = await openSealed(await goKey(), stdB64Decode(GO.sealed), readAAD('prod/db', 3, 'password'))
    expect(new TextDecoder().decode(plain)).toBe('hunter2')
  })

  it('refuses it as another secret, version or field', async () => {
    const k = await goKey()
    for (const aad of [readAAD('prod/other', 3, 'password'), readAAD('prod/db', 2, 'password'), readAAD('prod/db', 3, '')]) {
      await expect(openSealed(k, stdB64Decode(GO.sealed), aad)).rejects.toThrow(/cannot be opened/)
    }
  })

  it('refuses it with another key, or changed', async () => {
    await expect(openSealed(await oneTimeKey(), stdB64Decode(GO.sealed), readAAD('prod/db', 3, 'password'))).rejects.toThrow()
    const bent = stdB64Decode(GO.sealed)
    bent[bent.length - 1]! ^= 1
    await expect(openSealed(await goKey(), bent, readAAD('prod/db', 3, 'password'))).rejects.toThrow()
  })

  it('seals in the same layout, for the recipient only', async () => {
    const k = await oneTimeKey()
    const sealed = await sealTo(k.publicKey, new TextEncoder().encode('new value'), writeAAD('prod/db'))
    expect(sealed[0]).toBe(1)
    expect(sealed.length).toBe(1 + 32 + 12 + 'new value'.length + 16)
    expect(new TextDecoder().decode(await openSealed(k, sealed, writeAAD('prod/db')))).toBe('new value')
    await expect(openSealed(k, sealed, writeAAD('prod/other'))).rejects.toThrow()
    expect(stdB64Decode(stdB64Encode(sealed))).toEqual(sealed)
  })
})
