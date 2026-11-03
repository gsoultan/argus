/**
 * The console's copy of the control plane's password rules
 * (internal/auth/strength.go), so a person sees what is missing as they type.
 * The control plane checks again and has the last word; if the two ever
 * disagree, its refusal is shown as it is.
 */

export interface PasswordCheck {
  /** Short label for the checklist. */
  label: string
  ok: boolean
}

const COMMON = [
  'password', 'letmein', 'welcome', 'admin', 'administrator', 'changeme', 'iloveyou', 'monkey',
  'dragon', 'football', 'baseball', 'sunshine', 'princess', 'trustno', 'superman', 'batman',
  'master', 'shadow', 'michael', 'qwerty', 'starwars', 'whatever', 'argus', 'secret', 'default',
  'login', 'access', 'root', 'toor', 'passwordpassword', 'correcthorsebatterystaple',
]

export const MIN_LENGTH = 12
export const PASSPHRASE_LENGTH = 20

const LEET: Record<string, string> = { '@': 'a', '4': 'a', '0': 'o', '1': 'l', '!': 'l', '3': 'e', $: 's', '5': 's', '7': 't' }

function personalParts(email: string, name: string): string[] {
  const out: string[] = []
  const add = (s: string) => {
    const v = s.trim().toLowerCase()
    if ([...v].length >= 4) out.push(v)
  }
  const [local = '', domain = ''] = email.split('@')
  add(local)
  for (const p of local.split(/[^\p{L}\p{N}]+/u)) add(p)
  const host = domain.split('.')[0]
  if (domain.includes('.') && host) add(host)
  for (const p of name.split(/\s+/)) add(p)
  return out
}

function sequential(s: string): boolean {
  const rows = 'abcdefghijklmnopqrstuvwxyz 0123456789 qwertyuiop asdfghjkl zxcvbnm'
  const chars = [...s]
  for (let i = 0; i + 5 <= chars.length; i++) {
    const w = chars.slice(i, i + 5).join('')
    const rev = [...w].reverse().join('')
    if (rows.includes(w) || rows.includes(rev)) return true
    if (chars.slice(i, i + 5).every((c) => c === chars[i])) return true
  }
  return false
}

function looksCommon(pw: string): boolean {
  const lower = pw.toLowerCase()
  const plain = [...pw].map((c) => LEET[c] ?? (/\p{L}/u.test(c) ? c.toLowerCase() : '')).join('')
  const short = [...pw].length < PASSPHRASE_LENGTH
  const trimmed = lower.replace(/[0-9!@#$%^&*.?]+$/, '')
  return COMMON.some((w) =>
    (w.length >= 6 && short && (plain.includes(w) || lower.includes(w))) || plain === w || trimmed === w)
}

function classes(pw: string): number {
  let n = 0
  if (/\p{Ll}/u.test(pw)) n++
  if (/\p{Lu}/u.test(pw)) n++
  if (/\p{Nd}/u.test(pw)) n++
  if (/[^\p{Ll}\p{Lu}\p{Nd}]/u.test(pw)) n++
  return n
}

/** Every rule, for the checklist under the field. */
export function checkPassword(pw: string, ctx: { email?: string; name?: string } = {}): PasswordCheck[] {
  const len = [...pw].length
  const lower = pw.toLowerCase()
  const personal = personalParts(ctx.email ?? '', ctx.name ?? '').some((p) => lower.includes(p))
  return [
    { label: `At least ${MIN_LENGTH} characters`, ok: len >= MIN_LENGTH },
    {
      label: `Three of: lower case, upper case, digits, symbols (or ${PASSPHRASE_LENGTH}+ characters)`,
      ok: len >= PASSPHRASE_LENGTH || classes(pw) >= 3,
    },
    { label: 'Not your name or email address', ok: pw !== '' && !personal },
    {
      label: 'Not a common password or a run like 12345 or qwerty',
      ok: pw !== '' && !looksCommon(pw) && !sequential(lower) && new Set([...pw]).size >= 6,
    },
  ]
}

export function passwordAcceptable(pw: string, ctx: { email?: string; name?: string } = {}): boolean {
  return pw.trim() === pw && checkPassword(pw, ctx).every((c) => c.ok)
}
