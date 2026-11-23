/*
SCRAM credentials made in the browser. Kafka stores a user's password as a
salt and a salted password (PBKDF2 of the password with the salt, RFC 5802),
and its AlterUserSCRAMCredentials request takes exactly those. So the browser
makes them with WebCrypto and sends only them: the control plane and the
agent never have the password, the same promise the Redis Users tab makes.
*/

export type ScramMechanism = 'SCRAM-SHA-512' | 'SCRAM-SHA-256'

/** What Kafka stores for a password, each part base64. */
export interface ScramCredential { salt: string; saltedPassword: string; iterations: number }

/** Kafka takes 4096 to 16384 iterations; 8192 is what Argus's own session users get. */
export const SCRAM_ITERATIONS = 8192

function b64(bytes: ArrayBuffer | Uint8Array): string {
  const u = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes)
  let s = ''
  for (const b of u) s += String.fromCharCode(b)
  return btoa(s)
}

export function fromB64(s: string): Uint8Array<ArrayBuffer> {
  const bin = atob(s)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

/** PBKDF2 of the password with the salt, as SCRAM's Hi(): the salted password. */
export async function saltedPassword(password: string, salt: Uint8Array<ArrayBuffer>, iterations: number, mechanism: ScramMechanism): Promise<Uint8Array<ArrayBuffer>> {
  const hash = mechanism === 'SCRAM-SHA-256' ? 'SHA-256' : 'SHA-512'
  const bits = mechanism === 'SCRAM-SHA-256' ? 256 : 512
  const key = await crypto.subtle.importKey('raw', new TextEncoder().encode(password), 'PBKDF2', false, ['deriveBits'])
  return new Uint8Array(await crypto.subtle.deriveBits({ name: 'PBKDF2', hash, salt, iterations }, key, bits))
}

/** A new credential for a password: a random 24-byte salt and its salted password. */
export async function scramCredential(password: string, mechanism: ScramMechanism, iterations = SCRAM_ITERATIONS): Promise<ScramCredential> {
  const salt = new Uint8Array(24)
  crypto.getRandomValues(salt)
  return { salt: b64(salt), saltedPassword: b64(await saltedPassword(password, salt, iterations, mechanism)), iterations }
}

export { b64 as toB64 }
