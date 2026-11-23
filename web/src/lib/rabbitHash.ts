/*
RabbitMQ's password_hash made in the browser: rabbit_password_hashing_sha256
is 4 random salt bytes, then SHA-256 of the salt and the password (UTF-8),
the whole base64. The management API takes exactly that, so the password
never leaves the browser except for the sign-in check right after.
*/

import { toB64 } from '~/lib/scram'

/** The hash RabbitMQ stores for a password; salt is 4 random bytes unless given. */
export async function rabbitPasswordHash(password: string, salt?: Uint8Array): Promise<string> {
  const s = salt ?? crypto.getRandomValues(new Uint8Array(4))
  if (s.length !== 4) throw new Error('RabbitMQ salts with 4 bytes')
  const pw = new TextEncoder().encode(password)
  const buf = new Uint8Array(4 + pw.length)
  buf.set(s, 0)
  buf.set(pw, 4)
  const sum = new Uint8Array(await crypto.subtle.digest('SHA-256', buf))
  const out = new Uint8Array(36)
  out.set(s, 0)
  out.set(sum, 4)
  return toB64(out)
}
