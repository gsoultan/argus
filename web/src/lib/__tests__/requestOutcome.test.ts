import { describe, expect, it } from 'vitest'
import { requestOutcome, settledMeanwhile } from '~/lib/requestOutcome'
import type { AccessRequest } from '~/types/domain'

const req = (over: Partial<AccessRequest>): AccessRequest => ({
  id: 'r1', requesterId: 'u1', requesterEmail: 'ana@example.com', assetIds: [], assetHostnames: ['db-01'],
  principal: 'ubuntu', justification: 'the disk on db-01 is filling up again', durationMinutes: 60,
  state: 'pending', createdAt: '', decidedAt: null, decidedByEmail: null, decisionNote: null,
  expiresAt: null, breakGlass: false, ...over,
})

describe('How a request ended', () => {
  it('says nothing while it waits', () => {
    expect(requestOutcome(req({}))).toBeNull()
  })
  it('names the asker, not an approver, for a cancellation', () => {
    const r = req({ state: 'cancelled', decidedByEmail: 'ana@example.com' })
    expect(requestOutcome(r, 'bo@example.com')?.title).toBe('Cancelled by ana@example.com, who asked for it')
    expect(requestOutcome(r, 'ANA@example.com')?.title).toBe('You cancelled this request')
  })
  it('names who decided', () => {
    expect(requestOutcome(req({ state: 'denied', decidedByEmail: 'bo@example.com', decisionNote: 'not now' }))).toEqual(
      { title: 'Denied by bo@example.com', detail: 'not now', color: 'rose' })
  })
  it('tells a refusal for a settled request from a real failure', () => {
    expect(settledMeanwhile(new Error('this request was already denied by bo@example.com'))).toBe(true)
    expect(settledMeanwhile(new Error('ana@example.com cancelled this request; there is nothing left to decide'))).toBe(true)
    expect(settledMeanwhile(new Error('internal error'))).toBe(false)
  })
})
