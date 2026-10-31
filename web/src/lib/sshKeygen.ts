/**
 * An SSH key pair made in the browser, so the private half never leaves it:
 * the control plane only ever sees the public key, exactly as if the person
 * had pasted their own.
 *
 * Ed25519 through WebCrypto, written out in OpenSSH's own formats so ssh uses
 * the files as they are.
 */

export interface GeneratedKey {
  /** The authorized_keys line: "ssh-ed25519 AAAA… comment". */
  publicKey: string
  /** An unencrypted "OPENSSH PRIVATE KEY" file. */
  privateKey: string
}

const KEY_TYPE = 'ssh-ed25519'

export function canGenerateSshKey(): boolean {
  return typeof crypto !== 'undefined' && !!crypto.subtle
}

export async function generateSshKey(comment: string): Promise<GeneratedKey> {
  const pair = (await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify'])) as CryptoKeyPair
  const pub = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey))
  // PKCS#8 for Ed25519 is a fixed 16-byte header followed by the 32-byte seed.
  const pkcs8 = new Uint8Array(await crypto.subtle.exportKey('pkcs8', pair.privateKey))
  const seed = pkcs8.slice(pkcs8.length - 32)
  if (pub.length !== 32 || seed.length !== 32) throw new Error('the browser made a key of an unexpected size')

  const pubBlob = concat(sshString(KEY_TYPE), sshString(pub))
  return {
    publicKey: `${KEY_TYPE} ${base64(pubBlob)}${comment ? ` ${comment}` : ''}`,
    privateKey: opensshPrivateKey(pubBlob, pub, seed, comment),
  }
}

function opensshPrivateKey(pubBlob: Uint8Array, pub: Uint8Array, seed: Uint8Array, comment: string): string {
  const check = crypto.getRandomValues(new Uint8Array(4))
  let priv = concat(
    check, check,
    sshString(KEY_TYPE), sshString(pub), sshString(concat(seed, pub)), sshString(comment),
  )
  // Padded with 1, 2, 3, … to the cipher block size, 8 for "none".
  const pad = (8 - (priv.length % 8)) % 8
  priv = concat(priv, Uint8Array.from({ length: pad }, (_, i) => i + 1))

  const body = concat(
    new TextEncoder().encode('openssh-key-v1\0'),
    sshString('none'), sshString('none'), sshString(new Uint8Array()),
    uint32(1), sshString(pubBlob), sshString(priv),
  )
  const lines = base64(body).match(/.{1,70}/g) ?? []
  return ['-----BEGIN OPENSSH PRIVATE KEY-----', ...lines, '-----END OPENSSH PRIVATE KEY-----', ''].join('\n')
}

function uint32(n: number): Uint8Array {
  const b = new Uint8Array(4)
  new DataView(b.buffer).setUint32(0, n)
  return b
}

function sshString(v: string | Uint8Array): Uint8Array {
  const bytes = typeof v === 'string' ? new TextEncoder().encode(v) : v
  return concat(uint32(bytes.length), bytes)
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  return out
}

function base64(b: Uint8Array): string {
  let s = ''
  for (const c of b) s += String.fromCharCode(c)
  return btoa(s)
}
