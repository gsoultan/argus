import { describe, expect, it } from 'vitest'
import { base32, crc16, credsFile, newUserKey, userKeyFromSeed } from '~/lib/nkeys'

/*
The keys must be the ones nkeys in Go makes from the same seed, or the agent
would refuse the public key and NATS the .creds file. The vector below is
nkeys.FromRawSeed(PrefixByteUser, 0x00..0x1f) in Go.
*/
describe('nkeys', () => {
  it('writes base32 and CRC-16/XMODEM as nkeys does', () => {
    expect(base32(new TextEncoder().encode('foobar'))).toBe('MZXW6YTBOI')
    expect(crc16(new TextEncoder().encode('123456789'))).toBe(0x31c3)
  })
  it('makes the same user key as Go from the same seed', async () => {
    const k = await userKeyFromSeed(Uint8Array.from({ length: 32 }, (_, i) => i))
    expect(k.seed).toBe('SUAAAAICAMCAKBQHBAEQUCYMBUHA6EARCIJRIFIWC4MBSGQ3DQOR4H776Y')
    expect(k.publicKey).toBe('UAB2CB576PHBBPQ5ODORRZ2LYCMWPZGWGCN2KDK7DXOIMZASKUY3RLKK')
  })
  it('makes a fresh key each time', async () => {
    const [a, b] = await Promise.all([newUserKey(), newUserKey()])
    expect(a.publicKey).toMatch(/^U[A-Z2-7]{55}$/)
    expect(a.seed).toMatch(/^SU[A-Z2-7]{56}$/)
    expect(a.publicKey).not.toBe(b.publicKey)
  })
  it('writes the .creds file as nsc does', () => {
    const f = credsFile('eyJ.x.y', 'SUABC')
    expect(f.startsWith('-----BEGIN NATS USER JWT-----\neyJ.x.y\n------END NATS USER JWT------\n\n')).toBe(true)
    expect(f).toContain('\n-----BEGIN USER NKEY SEED-----\nSUABC\n------END USER NKEY SEED------\n')
  })
})
