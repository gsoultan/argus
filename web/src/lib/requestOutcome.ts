import type { AccessRequest } from '~/types/domain'

/**
 * How a request that is no longer waiting ended, in words: the line under a
 * card, and the banner on a review someone opened from a stale alert.
 *
 * `cancelled` is the person who asked taking it back; its decidedByEmail is
 * them. Saying "cancelled by" an address that reads like an approver's would
 * blame the wrong person.
 */
export interface Outcome {
  title: string
  detail?: string
  color: 'teal' | 'rose' | 'slate'
}

export function requestOutcome(r: AccessRequest, me?: string): Outcome | null {
  const by = r.decidedByEmail ?? 'someone'
  const mine = !!me && r.requesterEmail.toLowerCase() === me.toLowerCase()
  const note = r.decisionNote ?? undefined
  switch (r.state) {
    case 'pending':
      return null
    case 'approved':
      return { title: `Approved by ${by}`, detail: note, color: 'teal' }
    case 'denied':
      return { title: `Denied by ${by}`, detail: note, color: 'rose' }
    case 'cancelled':
      return {
        title: mine ? 'You cancelled this request' : `Cancelled by ${r.requesterEmail}, who asked for it`,
        detail: mine ? 'Nobody can approve it now. Ask again if you still need it.' : 'There is nothing left to decide.',
        color: 'slate',
      }
    case 'expired':
      return { title: `Approved by ${by}, and since expired`, detail: note, color: 'slate' }
    case 'revoked':
      return { title: `Approved by ${by}, then revoked`, detail: note, color: 'rose' }
  }
  return { title: r.state, color: 'slate' }
}

/** Whether an error from deciding means the request was settled meanwhile. */
export function settledMeanwhile(err: unknown): boolean {
  const msg = err instanceof Error ? err.message : String(err)
  return /already (approved|denied)|cancelled this request|has since expired|then revoked|not waiting for a decision/.test(msg)
}
