import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { RecordingScopeCard, scopeConfirmation } from '~/components/RecordingScope'
import { assets } from '~/lib/seed'
import { renderWithProviders as ui } from '~/test/render'
import type { Asset } from '~/types/domain'

const base: Asset = assets[0]!

/*
 * A control that appears set must be set. The saved value and the value the
 * agent is enforcing are separate facts, and only the agent's check-in turns
 * the first into the second.
 */
describe('scopeConfirmation', () => {
  it('never calls a setting applied when the agent has not said so', () => {
    const c = scopeConfirmation({ ...base, recordingScope: 'argus_only', agentRecordingScope: null })
    expect(c.applied).toBe(false)
    expect(c.text).toMatch(/not confirmed/i)
  })

  it('says the agent is still on the old setting when they differ', () => {
    const c = scopeConfirmation({
      ...base, recordingScope: 'argus_only', agentRecordingScope: 'every_login',
    })
    expect(c.applied).toBe(false)
    expect(c.text).toContain('direct logins')
  })

  it('confirms only when the agent reports the saved value', () => {
    const c = scopeConfirmation({
      ...base, recordingScope: 'argus_only', agentRecordingScope: 'argus_only',
    })
    expect(c.applied).toBe(true)
  })

  it('reads an older control plane with no setting as every login', () => {
    const c = scopeConfirmation({
      ...base, recordingScope: undefined, agentRecordingScope: 'every_login',
    })
    expect(c.applied).toBe(true)
  })
})

describe('RecordingScopeCard', () => {
  it('cannot be changed by someone who is not an administrator, and says so', () => {
    ui(<RecordingScopeCard asset={{ ...base, recordingScope: 'every_login' }} canEdit={false} />)
    for (const radio of screen.getAllByRole('radio')) expect(radio).toBeDisabled()
    expect(screen.getByText(/only an administrator or owner/i)).toBeInTheDocument()
  })

  it('states what Argus-only gives up', () => {
    ui(<RecordingScopeCard asset={{ ...base, recordingScope: 'argus_only' }} canEdit />)
    expect(screen.getByText(/no replay/i)).toBeInTheDocument()
    expect(screen.getByRole('radio', { name: /through Argus only/ })).toBeChecked()
  })
})
