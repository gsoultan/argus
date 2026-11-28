/*
 * Sealing, as internal/secretwire does it, in the browser.
 *
 * Format v2, used for every read and write here: HPKE (RFC 9180) with the
 * ML-KEM-768 + X25519 hybrid KEM (X-Wing), HKDF-SHA256 and AES-256-GCM. It
 * stays secret as long as either X25519 or ML-KEM-768 does, so a value
 * recorded in transit today is not opened by a quantum computer later.
 * Layout: version 2 | encapsulated key (1120) | ciphertext.
 *
 * Format v1, kept for a server that has no post-quantum key: an X25519
 * exchange, HKDF-SHA256 over (ephemeral public key || recipient public key)
 * with the info "argus-secret seal v1", then AES-256-GCM.
 * Layout: version 1 | ephemeral public key (32) | nonce (12) | ciphertext.
 *
 * A value read in the console is sealed by argus-secret to a key made here for
 * that one read, and opened here; a value written is sealed here to
 * argus-secret's key. argus-control relays both and can open neither.
 */

import { ml_kem768 } from '@noble/post-quantum/ml-kem.js'
import { sha3_256, shake256 } from '@noble/hashes/sha3.js'

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

/** An X25519 key pair (format v1). */
export interface X25519Key {
  kind: 'x25519'
  publicKey: Uint8Array
  privateKey: CryptoKey
}

/** An ML-KEM-768 + X25519 key pair (format v2). */
export interface PQKey {
  kind: 'pq'
  publicKey: Uint8Array
  secretKey: Uint8Array
}

/** A key pair for one read. The private half never leaves this tab. */
export type OneTimeKey = X25519Key | PQKey

/** A post-quantum key for one read: what the console always uses. */
export async function oneTimeKey(): Promise<PQKey> {
  const secretKey = crypto.getRandomValues(new Uint8Array(32))
  const { publicKey } = await xwingExpand(secretKey)
  return { kind: 'pq', publicKey, secretKey }
}

export async function x25519OneTimeKey(): Promise<X25519Key> {
  const kp = await crypto.subtle.generateKey({ name: 'X25519' }, true, ['deriveBits']) as CryptoKeyPair
  return { kind: 'x25519', publicKey: new Uint8Array(await crypto.subtle.exportKey('raw', kp.publicKey)), privateKey: kp.privateKey }
}

/** Seals plain to recipient: format v2 for a post-quantum key, v1 for a 32-byte X25519 key. */
export async function sealTo(recipient: Uint8Array, plain: Uint8Array, aad: Uint8Array): Promise<Uint8Array> {
  if (recipient.length === PQ_PUBLIC) return sealPQ(recipient, plain, aad)
  if (recipient.length !== 32) throw new Error('not a key this console can seal to')
  const pub = await crypto.subtle.importKey('raw', recipient as Uint8Array<ArrayBuffer>, { name: 'X25519' }, false, [])
  const eph = await x25519OneTimeKey()
  const shared = await crypto.subtle.deriveBits({ name: 'X25519', public: pub }, eph.privateKey, 256)
  const key = await aesKey(shared, eph.publicKey, recipient, 'encrypt')
  const nonce = crypto.getRandomValues(new Uint8Array(12))
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce, additionalData: aad as Uint8Array<ArrayBuffer> }, key, plain as Uint8Array<ArrayBuffer>))
  return concat(new Uint8Array([VERSION]), eph.publicKey, nonce, ct)
}

/** Opens what argus-secret sealed to k. A post-quantum key opens format v2 only. */
export async function openSealed(k: OneTimeKey, sealed: Uint8Array, aad: Uint8Array): Promise<Uint8Array> {
  if (k.kind === 'pq') return openPQ(k, sealed, aad)
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

// Format v2: HPKE base mode, RFC 9180 section 5.1, single message (seq 0).
const VERSION_PQ = 2
const PQ_PUBLIC = 1184 + 32
const PQ_ENC = 1088 + 32
const INFO_PQ = enc.encode('argus-secret seal v2')
// suite_id = "HPKE" || KEM 0x647a (MLKEM768-X25519) || KDF 0x0001 (HKDF-SHA256) || AEAD 0x0002 (AES-256-GCM)
const SUITE = concat(enc.encode('HPKE'), new Uint8Array([0x64, 0x7a, 0x00, 0x01, 0x00, 0x02]))
const V1 = enc.encode('HPKE-v1')

// HKDF-SHA256 by hand on WebCrypto's HMAC: HPKE needs Extract and Expand
// apart, which WebCrypto's HKDF does not offer. An empty salt is HashLen
// zeros (RFC 5869), and no output here is longer than one block.
async function hmac(key: Uint8Array, data: Uint8Array) {
  const k = await crypto.subtle.importKey('raw', (key.length ? key : new Uint8Array(32)) as Uint8Array<ArrayBuffer>,
    { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'])
  return new Uint8Array(await crypto.subtle.sign('HMAC', k, data as Uint8Array<ArrayBuffer>))
}
const labeledExtract = (salt: Uint8Array, label: string, ikm: Uint8Array) =>
  hmac(salt, concat(V1, SUITE, enc.encode(label), ikm))
const labeledExpand = async (prk: Uint8Array, label: string, info: Uint8Array, n: number) =>
  (await hmac(prk, concat(new Uint8Array([n >> 8, n & 0xff]), V1, SUITE, enc.encode(label), info, new Uint8Array([1])))).slice(0, n)

async function schedule(shared: Uint8Array, use: KeyUsage) {
  const empty = new Uint8Array(0)
  const context = concat(new Uint8Array([0]), await labeledExtract(empty, 'psk_id_hash', empty), await labeledExtract(empty, 'info_hash', INFO_PQ))
  const secret = await labeledExtract(shared, 'secret', empty)
  const raw = await labeledExpand(secret, 'key', context, 32)
  const nonce = await labeledExpand(secret, 'base_nonce', context, 12)
  const key = await crypto.subtle.importKey('raw', raw as Uint8Array<ArrayBuffer>, 'AES-GCM', false, [use])
  raw.fill(0)
  secret.fill(0)
  return { key, nonce: nonce as Uint8Array<ArrayBuffer> }
}

async function sealPQ(recipient: Uint8Array, plain: Uint8Array, aad: Uint8Array): Promise<Uint8Array> {
  const { cipherText, sharedSecret } = await xwingEncapsulate(recipient)
  const { key, nonce } = await schedule(sharedSecret, 'encrypt')
  sharedSecret.fill(0)
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce, additionalData: aad as Uint8Array<ArrayBuffer> }, key, plain as Uint8Array<ArrayBuffer>))
  return concat(new Uint8Array([VERSION_PQ]), cipherText, ct)
}

async function openPQ(k: PQKey, sealed: Uint8Array, aad: Uint8Array): Promise<Uint8Array> {
  if (sealed.length < 1 + PQ_ENC + 16 || sealed[0] !== VERSION_PQ) throw new Error('the answer cannot be opened with this key')
  try {
    const shared = await xwingDecapsulate(sealed.slice(1, 1 + PQ_ENC), k.secretKey)
    const { key, nonce } = await schedule(shared, 'decrypt')
    shared.fill(0)
    return new Uint8Array(await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: nonce, additionalData: aad as Uint8Array<ArrayBuffer> }, key, sealed.slice(1 + PQ_ENC)))
  } catch {
    throw new Error('the answer cannot be opened with this key')
  }
}

/*
 * X-Wing (draft-connolly-cfrg-xwing-kem), the KEM Go's crypto/hpke calls
 * MLKEM768-X25519: ML-KEM-768 from @noble/post-quantum, X25519 from the
 * browser's own WebCrypto, combined with SHA3-256. The secret key is a 32-byte
 * seed, the form Go's hpke PrivateKey.Bytes gives.
 */
const XWING_LABEL = new Uint8Array([0x5c, 0x2e, 0x2f, 0x2f, 0x5e, 0x5c]) // \.//^\
// PKCS #8 for an X25519 private key: what WebCrypto imports a raw scalar from.
const X25519_PKCS8 = new Uint8Array([0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, 0x04, 0x22, 0x04, 0x20])

async function x25519Private(scalar: Uint8Array) {
  const key = await crypto.subtle.importKey('pkcs8', concat(X25519_PKCS8, scalar) as Uint8Array<ArrayBuffer>, { name: 'X25519' }, true, ['deriveBits'])
  const jwk = await crypto.subtle.exportKey('jwk', key)
  return { key, publicKey: b64urlDecode(jwk.x!) }
}

async function x25519(key: CryptoKey, pub: Uint8Array) {
  const p = await crypto.subtle.importKey('raw', pub as Uint8Array<ArrayBuffer>, { name: 'X25519' }, false, [])
  return new Uint8Array(await crypto.subtle.deriveBits({ name: 'X25519', public: p }, key, 256))
}

async function xwingExpand(seed: Uint8Array) {
  const expanded = shake256(seed, { dkLen: 96 })
  const m = ml_kem768.keygen(expanded.slice(0, 64))
  const x = await x25519Private(expanded.slice(64, 96))
  expanded.fill(0)
  return { publicKey: concat(m.publicKey, x.publicKey), mlkemSecret: m.secretKey, x25519: x.key, x25519Public: x.publicKey }
}

const combine = (ssM: Uint8Array, ssX: Uint8Array, ctX: Uint8Array, pkX: Uint8Array) =>
  sha3_256(concat(ssM, ssX, ctX, pkX, XWING_LABEL))

async function xwingEncapsulate(pk: Uint8Array) {
  const pkM = pk.slice(0, 1184), pkX = pk.slice(1184)
  const eph = await x25519Private(crypto.getRandomValues(new Uint8Array(32)))
  const ssX = await x25519(eph.key, pkX)
  const { cipherText: ctM, sharedSecret: ssM } = ml_kem768.encapsulate(pkM)
  const sharedSecret = combine(ssM, ssX, eph.publicKey, pkX)
  ssM.fill(0)
  ssX.fill(0)
  return { cipherText: concat(ctM, eph.publicKey), sharedSecret }
}

async function xwingDecapsulate(ct: Uint8Array, seed: Uint8Array) {
  const k = await xwingExpand(seed)
  const ctM = ct.slice(0, 1088), ctX = ct.slice(1088)
  const ssM = ml_kem768.decapsulate(ctM, k.mlkemSecret)
  const ssX = await x25519(k.x25519, ctX)
  k.mlkemSecret.fill(0)
  const shared = combine(ssM, ssX, ctX, k.x25519Public)
  ssM.fill(0)
  ssX.fill(0)
  return shared
}
