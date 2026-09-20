import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { SessionDuration, SessionStateBadge } from '~/components/primitives'
import { renderWithProviders as ui } from '~/test/render'
import { api } from '~/lib/api'
import { EPOCH } from '~/lib/seed'

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

/**
 * `duration(startedAt, endedAt)` counts to now when the end time is null, which
 * is right for a session in progress and wrong for every other reason that
 * field can be empty. Two of those are in the dev control plane: 15 sessions
 * whose gateway went away, and 26 `terminated` ones stored before the end time
 * was written outside the seal branch. Every one of them rendered as still
 * running -- the oldest at 310 hours -- on the Overview and in two tables.
 */
describe('how long a session ran', () => {
  // Against EPOCH, not Date.now(): with no control plane configured the
  // console measures relative times from the fixture's frozen clock, which is
  // what these components will be reading.
  const at = (minsAgo: number) => new Date(EPOCH - minsAgo * 60_000).toISOString()
  const base = {
    startedAt: at(90),
    endedAt: null as string | null,
    state: 'active' as const,
    silent: false,
    lastReportedAt: at(0),
  }

  it('counts to now only while the session is actually running', () => {
    ui(<SessionDuration session={base} />)
    expect(screen.getByText('1h 30m')).toBeInTheDocument()
  })

  it('stops at the last report when nothing is speaking for the session', () => {
    ui(
      <SessionDuration
        session={{
          ...base,
          silent: true,
          lastReportedAt: at(30),
        }}
      />,
    )
    // An hour of the ninety minutes is unaccounted for, so it is not claimed.
    expect(screen.getByText('1h 0m+')).toBeInTheDocument()
  })

  it('gives no figure at all for a session that ended at an unknown time', () => {
    ui(<SessionDuration session={{ ...base, state: 'terminated', endedAt: null }} />)
    expect(screen.getByText('—')).toBeInTheDocument()
    // The bug this replaces: a terminated session rendered as 310 hours of
    // uptime because the clock ran to now.
    expect(screen.queryByText(/\dh/)).not.toBeInTheDocument()
  })

  it('uses the recorded end time when there is one', () => {
    ui(
      <SessionDuration
        session={{
          ...base,
          state: 'closed',
          endedAt: at(78),
        }}
      />,
    )
    expect(screen.getByText('12m 0s')).toBeInTheDocument()
  })
})
