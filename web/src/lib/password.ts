/**
 * A first password, generated in the browser.
 *
 * In a file of its own so the pages that offer one -- setup, install, people,
 * organizations -- share it without pulling the setup page into their chunk,
 * or each other into the first screen's.
 */

/** No 0/O, 1/l/I: a generated password gets read off a screen and typed. */
const PASSWORD_ALPHABET = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'

/**
 * Four groups of six from a 57-letter alphabet: about 140 bits, far past what
 * the argon2 hash behind it needs, and grouped so it can be checked by eye.
 * Rejection sampling, so no letter is likelier than another.
 */
export function generatePassword(): string {
  const out: string[] = []
  const limit = 256 - (256 % PASSWORD_ALPHABET.length)
  const buf = new Uint8Array(64)
  while (out.length < 24) {
    crypto.getRandomValues(buf)
    for (const b of buf) {
      if (b < limit && out.length < 24) out.push(PASSWORD_ALPHABET[b % PASSWORD_ALPHABET.length]!)
    }
  }
  return [0, 6, 12, 18].map((i) => out.slice(i, i + 6).join('')).join('-')
}
