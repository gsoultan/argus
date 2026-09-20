import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { SessionStateBadge } from '~/components/primitives'
import { renderWithProviders as ui } from '~/test/render'
import { api } from '~/lib/api'

/**
 * The console used to read `state === 'active'` as "running now".
 *
 * A gateway killed rather than drained never reports the end, so fifteen
 * sessions in the dev control plane sat `active` for up to nineteen days and
 * every surface presented them as live: the badge, the header count, the
 * Overview tile, and a duration that kept counting up. Nothing reaps them --
 * a gateway carries no identity to attribute orphans to, and with no maximum
 * session duration, age alone proves nothing -- so the fix is for the console
 * to stop claiming to know.
 */
describe('a session nobody is reporting', () => {
  it('is not presented as live', () => {
    ui(
      <SessionStateBadge
        state="active"
        silent
        lastReportedAt={new Date(Date.now() - 40 * 60_000).toISOString()}
      />,
    )
    expect(screen.getByText('unknown')).toBeInTheDocument()
    expect(screen.queryByText('live')).not.toBeInTheDocument()
  })

  it('still reads as live while it is being reported', () => {
    ui(<SessionStateBadge state="active" silent={false} lastReportedAt={new Date().toISOString()} />)
    expect(screen.getByText('live')).toBeInTheDocument()
  })

  // An older control plane sends neither field. Absent evidence of silence,
  // the session is shown as it always was rather than as unknown.
  it('reads as live when the server says nothing about liveness', () => {
    ui(<SessionStateBadge state="active" />)
    expect(screen.getByText('live')).toBeInTheDocument()
  })

  it('is never unknown once it has ended, however long ago it last reported', () => {
    ui(<SessionStateBadge state="closed" silent lastReportedAt={new Date(0).toISOString()} />)
    expect(screen.getByText('closed')).toBeInTheDocument()
    expect(screen.queryByText('unknown')).not.toBeInTheDocument()
  })
})

describe('the fleet counters', () => {
  it('count silent sessions apart from live ones', async () => {
    const stats = await api.stats()
    const sessions = await api.sessions()

    const active = sessions.filter((s) => s.state === 'active')
    const silent = active.filter((s) => s.silent)

    // The fixture has to carry the state or no surface can be checked against
    // it -- and the demo would show a console that cannot express what the
    // real control plane contained.
    expect(silent.length).toBeGreaterThan(0)
    expect(stats.sessionsSilent).toBe(silent.length)
    expect(stats.sessionsActive).toBe(active.length - silent.length)
    // The headline number is the one that was wrong, so assert it is smaller
    // rather than only that the two agree.
    expect(stats.sessionsActive).toBeLessThan(active.length)
  })
})
