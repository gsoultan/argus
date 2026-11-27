/*
 * Sealing, as internal/secretwire does it, in the browser: an X25519
 * exchange, HKDF-SHA256 over (ephemeral public key || recipient public key)
 * with the info "argus-secret seal v1", then AES-256-GCM.
 *
 * Layout: version 1 | ephemeral public key (32) | nonce (12) | ciphertext.
 *
 * A value read in the console is sealed by argus-secret to a key made here for
 * that one read, and opened here; a value written is sealed here to
 * argus-secret's key. argus-control relays both and can open neither.
 */

const VERSION = 1
const INFO = new TextEncoder().encode('argus-secret seal v1')
const enc = new TextEncoder()

/** The AAD a read answer is sealed with: secretwire.ReadAAD. */
export const readAAD = (name: string, version: number, field: string) =>
  enc.encode(`argus-secret read v1\x00${name}\x00${version}\x00${field}`)

/** The AAD a written value is sealed with: secretwire.WriteAAD. */
export const writeAAD = (name: string) => enc.encode(`argus-secret write v1\x00${name}`)

export const b64urlEncode = (b: Uint8Array) =>
  stdB64Encode(b).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')

/** Reads base64, either alphabet, with or without padding. */
export function b64urlDecode(s: string): Uint8Array<ArrayBuffer> {
  const t = s.replace(/=+$/, '').replace(/-/g, '+').replace(/_/g, '/')
  const raw = atob(t + '='.repeat((4 - (t.length % 4)) % 4))
  const out = new Uint8Array(raw.length)
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i)
  return out
}

/** Go's []byte travels in JSON as standard base64 with padding. */
export function stdB64Encode(b: Uint8Array): string {
  let s = ''
  for (const x of b) s += String.fromCharCode(x)
  return btoa(s)
}

export const stdB64Decode = (s: string) => b64urlDecode(s)

const concat = (...parts: Uint8Array[]) => {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let o = 0
  for (const p of parts) { out.set(p, o); o += p.length }
  return out
}

async function aesKey(shared: ArrayBuffer, ephPub: Uint8Array, recipientPub: Uint8Array, use: KeyUsage) {
  const ikm = await crypto.subtle.importKey('raw', shared, 'HKDF', false, ['deriveKey'])
  return crypto.subtle.deriveKey(
    { name: 'HKDF', hash: 'SHA-256', salt: concat(ephPub, recipientPub), info: INFO },
    ikm, { name: 'AES-GCM', length: 256 }, false, [use],
  )
}

/** A key pair for one read. The private half never leaves this tab. */
export interface OneTimeKey {
  publicKey: Uint8Array
  privateKey: CryptoKey
}

export async function oneTimeKey(): Promise<OneTimeKey> {
  const kp = await crypto.subtle.generateKey({ name: 'X25519' }, true, ['deriveBits']) as CryptoKeyPair
  return { publicKey: new Uint8Array(await crypto.subtle.exportKey('raw', kp.publicKey)), privateKey: kp.privateKey }
}

/** Seals plain to recipient (32 raw bytes). */
export async function sealTo(recipient: Uint8Array, plain: Uint8Array, aad: Uint8Array): Promise<Uint8Array> {
  const pub = await crypto.subtle.importKey('raw', recipient as Uint8Array<ArrayBuffer>, { name: 'X25519' }, false, [])
  const eph = await oneTimeKey()
  const shared = await crypto.subtle.deriveBits({ name: 'X25519', public: pub }, eph.privateKey, 256)
  const key = await aesKey(shared, eph.publicKey, recipient, 'encrypt')
  const nonce = crypto.getRandomValues(new Uint8Array(12))
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce, additionalData: aad as Uint8Array<ArrayBuffer> }, key, plain as Uint8Array<ArrayBuffer>))
  return concat(new Uint8Array([VERSION]), eph.publicKey, nonce, ct)
}

/** Opens what argus-secret sealed to k. */
export async function openSealed(k: OneTimeKey, sealed: Uint8Array, aad: Uint8Array): Promise<Uint8Array> {
  if (sealed.length < 1 + 32 + 12 + 16 || sealed[0] !== VERSION) throw new Error('the answer cannot be opened with this key')
  const ephPub = sealed.slice(1, 33)
  const pub = await crypto.subtle.importKey('raw', ephPub, { name: 'X25519' }, false, [])
  const shared = await crypto.subtle.deriveBits({ name: 'X25519', public: pub }, k.privateKey, 256)
  const key = await aesKey(shared, ephPub, k.publicKey, 'decrypt')
  try {
    return new Uint8Array(await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: sealed.slice(33, 45), additionalData: aad as Uint8Array<ArrayBuffer> }, key, sealed.slice(45)))
  } catch {
    throw new Error('the answer cannot be opened with this key')
  }
}
