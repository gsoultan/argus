/*
NATS user keys made in the browser, so a user's seed -- its secret -- never
leaves it: Argus's agent gets only the public key, signs a user JWT for it,
and the browser puts that JWT and the seed in the .creds file it hands over.

An nkey is an Ed25519 key written as base32 with a prefix byte (U for a user's
public key, SU for a user's seed) and a CRC-16 at the end, as nkeys in Go
writes it. The key pair comes from WebCrypto, which every browser Argus
supports has for Ed25519; a seed is the 32-byte private key it imports from.
*/

const B32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'

export function base32(bytes: Uint8Array): string {
  let out = ''
  let bits = 0
  let value = 0
  for (const b of bytes) {
    value = (value << 8) | b
    bits += 8
    while (bits >= 5) {
      out += B32[(value >>> (bits - 5)) & 31]
      bits -= 5
    }
  }
  if (bits > 0) out += B32[(value << (5 - bits)) & 31]
  return out
}

/** CRC-16/XMODEM, nkeys' checksum. */
export function crc16(bytes: Uint8Array): number {
  let crc = 0
  for (const b of bytes) {
    crc ^= b << 8
    for (let i = 0; i < 8; i++) crc = crc & 0x8000 ? ((crc << 1) ^ 0x1021) & 0xffff : (crc << 1) & 0xffff
  }
  return crc
}

const PREFIX_USER = 20 << 3 // U
const PREFIX_SEED = 18 << 3 // S

function withCRC(raw: Uint8Array): string {
  const c = crc16(raw)
  const out = new Uint8Array(raw.length + 2)
  out.set(raw)
  out[raw.length] = c & 0xff
  out[raw.length + 1] = c >> 8
  return base32(out)
}

/** A user's public key (U…) from its 32 raw bytes. */
export function userPublicKey(pub: Uint8Array): string {
  const raw = new Uint8Array(33)
  raw[0] = PREFIX_USER
  raw.set(pub, 1)
  return withCRC(raw)
}

/** A user's seed (SU…) from its 32 raw private bytes. */
export function userSeed(seed: Uint8Array): string {
  const raw = new Uint8Array(34)
  raw[0] = PREFIX_SEED | (PREFIX_USER >> 5)
  raw[1] = (PREFIX_USER & 31) << 3
  raw.set(seed, 2)
  return withCRC(raw)
}

// PKCS#8 of an Ed25519 private key is this header and the 32 seed bytes.
const PKCS8_ED25519 = [0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20]

function fromB64url(s: string): Uint8Array {
  const b = atob(s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (s.length % 4)) % 4))
  return Uint8Array.from(b, (c) => c.charCodeAt(0))
}

export interface UserKey { publicKey: string; seed: string }

/** The user key of a 32-byte seed. */
export async function userKeyFromSeed(raw: Uint8Array): Promise<UserKey> {
  if (raw.length !== 32) throw new Error('a seed is 32 bytes')
  const pkcs8 = new Uint8Array(48)
  pkcs8.set(PKCS8_ED25519)
  pkcs8.set(raw, 16)
  const key = await crypto.subtle.importKey('pkcs8', pkcs8, { name: 'Ed25519' }, true, ['sign'])
  const jwk = await crypto.subtle.exportKey('jwk', key)
  if (!jwk.x) throw new Error('this browser did not give the public key')
  return { publicKey: userPublicKey(fromB64url(jwk.x)), seed: userSeed(raw) }
}

/** Whether this browser can make NATS user keys. */
export async function canMakeUserKeys(): Promise<boolean> {
  try {
    await userKeyFromSeed(new Uint8Array(32))
    return true
  } catch {
    return false
  }
}

/** A fresh user key from 32 random bytes. */
export async function newUserKey(): Promise<UserKey> {
  return userKeyFromSeed(crypto.getRandomValues(new Uint8Array(32)))
}

/** The .creds file nats-server's clients read, as `nsc` writes it. */
export function credsFile(jwt: string, seed: string): string {
  return `-----BEGIN NATS USER JWT-----
${jwt}
------END NATS USER JWT------

************************* IMPORTANT *************************
NKEY Seed printed below can be used to sign and prove identity.
NKEYs are sensitive and should be treated as secrets.

-----BEGIN USER NKEY SEED-----
${seed}
------END USER NKEY SEED------

*************************************************************
`
}
