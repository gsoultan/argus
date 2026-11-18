/*
What Postgres said beside a statement that did not fail.

A GRANT the session's role may not give is not an error to Postgres: it ends
with the tag GRANT and a WARNING, SQLSTATE 01007, "no privileges were granted
for ...". The console used to show that as a plain success while nothing
changed. The agent now passes notices on (PGAnswer.notices), and this is how
the console reads them.
*/

export interface PGNotice {
  severity: string
  code?: string
  message: string
  detail?: string
  hint?: string
}

/** SQLSTATE 01007, warning_privilege_not_granted. */
export const NO_GRANT = '01007'

/** A WARNING, as against a NOTICE or INFO that is only information. */
export function isWarning(n: PGNotice): boolean {
  return n.severity === 'WARNING' || (n.code?.startsWith('01') ?? false)
}

export function warningsIn(notices: PGNotice[] | undefined): PGNotice[] {
  return (notices ?? []).filter(isWarning)
}

/** Whether a GRANT ran and gave less than it named, often nothing. */
export function grantedLess(notices: PGNotice[] | undefined): boolean {
  return (notices ?? []).some((n) => n.code === NO_GRANT)
}

/** The heading over a statement's notices, worded for what they mean. */
export function noticeHeading(notices: PGNotice[] | undefined): string | null {
  const all = notices ?? []
  if (all.length === 0) return null
  if (all.every((n) => n.code === NO_GRANT && /^no privileges were granted/i.test(n.message))) {
    return 'Postgres ran this, but granted nothing'
  }
  if (grantedLess(all)) return 'Postgres ran this, but did not grant everything it names'
  const w = warningsIn(all).length
  if (w > 0) return `Postgres ran this, with ${w === 1 ? 'a warning' : `${w} warnings`}`
  return `Postgres ran this, and said`
}

/**
 * The next step for a GRANT that granted less than it named: why it happens
 * and what to do, since Postgres's own words name only the object.
 */
export const NO_GRANT_NOTE =
  'Only an object\'s owner, or a role given the privilege WITH GRANT OPTION, can grant it. ' +
  'The role this session acts as owns none of these objects, so the grantee has nothing new. ' +
  'Run the GRANT as the objects\' owner (often the server\'s postgres account), or have it change their owner first.'

/** A few words for a summary line: "2 warnings", "nothing granted". */
export function noticeShort(notices: PGNotice[] | undefined): string | null {
  const all = notices ?? []
  if (all.length === 0) return null
  if (grantedLess(all)) return noticeHeading(all)?.endsWith('nothing') ? 'nothing granted' : 'not all granted'
  const w = warningsIn(all).length
  if (w > 0) return w === 1 ? '1 warning' : `${w} warnings`
  return all.length === 1 ? '1 notice' : `${all.length} notices`
}
