import { describe, expect, it } from 'vitest'
import { generateSshKey } from '~/lib/sshKeygen'

const fromBase64 = (s: string) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0))

type Bytes = Uint8Array<ArrayBuffer>

/** Reads the length-prefixed fields of an SSH wire blob. Indexing past the
 *  last one is a test failure, not a case to type around. */
function fields(b: Uint8Array, from = 0): Bytes[] & Record<0 | 1 | 2 | 3, Bytes> {
  const out: Bytes[] = []
  const view = new DataView(b.buffer, b.byteOffset)
  for (let at = from; at + 4 <= b.length;) {
    const n = view.getUint32(at)
    out.push(b.slice(at + 4, at + 4 + n) as Bytes)
    at += 4 + n
  }
  return out as Bytes[] & Record<0 | 1 | 2 | 3, Bytes>
}

const text = (b: Uint8Array) => new TextDecoder().decode(b)

describe('generateSshKey', () => {
  it('writes a public key line ssh accepts', async () => {
    const k = await generateSshKey('dana@argus')
    const [type, blob, comment] = k.publicKey.split(' ')
    expect(type).toBe('ssh-ed25519')
    expect(comment).toBe('dana@argus')
    const [t, pub] = fields(fromBase64(blob!))
    expect(text(t)).toBe('ssh-ed25519')
    expect(pub).toHaveLength(32)
  })

  it('writes a private key whose seed signs for the public key', async () => {
    const k = await generateSshKey('dana@argus')
    const pem = k.privateKey.trim().split('\n')
    expect(pem[0]).toBe('-----BEGIN OPENSSH PRIVATE KEY-----')
    expect(pem.at(-1)).toBe('-----END OPENSSH PRIVATE KEY-----')
    expect(pem.slice(1, -1).every((l) => l.length <= 70)).toBe(true)

    const body = fromBase64(pem.slice(1, -1).join(''))
    const magic = 'openssh-key-v1\0'
    expect(text(body.slice(0, magic.length))).toBe(magic)
    // cipher, kdf, kdf options, then a key count that is not length-prefixed.
    const head = fields(body, magic.length)
    expect(text(head[0])).toBe('none')
    expect(text(head[1])).toBe('none')
    const afterCount = magic.length + (4 + 4) + (4 + 4) + 4 + 4
    const [pubBlob, priv] = fields(body, afterCount)
    expect(k.publicKey.split(' ')[1]).toBe(btoa(String.fromCharCode(...pubBlob)))

    // Two equal check words, then type, public, seed||public, comment, padding.
    const view = new DataView(priv.buffer, priv.byteOffset)
    expect(view.getUint32(0)).toBe(view.getUint32(4))
    expect(priv.length % 8).toBe(0)
    const [type, pub, both, comment] = fields(priv, 8)
    expect(text(type)).toBe('ssh-ed25519')
    expect(text(comment)).toBe('dana@argus')
    expect(both.slice(32)).toEqual(pub)

    const pkcs8 = new Uint8Array([
      0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20,
      ...both.slice(0, 32),
    ])
    const signer = await crypto.subtle.importKey('pkcs8', pkcs8, { name: 'Ed25519' }, false, ['sign'])
    const verifier = await crypto.subtle.importKey('raw', pub, { name: 'Ed25519' }, false, ['verify'])
    const msg = new TextEncoder().encode('argus')
    const sig = await crypto.subtle.sign('Ed25519', signer, msg)
    expect(await crypto.subtle.verify('Ed25519', verifier, sig, msg)).toBe(true)
  })

  it('makes a different key every time', async () => {
    const [a, b] = await Promise.all([generateSshKey(''), generateSshKey('')])
    expect(a.publicKey).not.toBe(b.publicKey)
    expect(a.publicKey.split(' ')).toHaveLength(2)
  })
})
