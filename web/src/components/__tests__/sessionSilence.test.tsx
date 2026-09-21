import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { ArtefactPending, SessionDuration, SessionStateBadge } from '~/components/primitives'
import { renderWithProviders as ui } from '~/test/render'
import { api } from '~/lib/api'
import { EPOCH } from '~/lib/seed'

// Against EPOCH, not Date.now(): with no control plane configured the console
// measures relative times from the fixture's frozen clock, which is what these
// components will be reading.
const at = (minsAgo: number) => new Date(EPOCH - minsAgo * 60_000).toISOString()

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
  const base = {
    startedAt: at(90),
    endedAt: null as string | null,
    state: 'active' as const,
    silent: false,
    lastReportedAt: at(0),
    endInferred: false,
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

/**
 * A sealed recording with no object-storage key is not lost -- the gateway
 * still holds the file and queues the upload for retry -- but it cannot be
 * opened from the console until that lands. 44 sessions in the dev control
 * plane are in that state, and every list showed the fidelity badge, the byte
 * count and a Replay button for all of them.
 */
describe('a recording that has not reached object storage', () => {
  it('is marked on a finished session', () => {
    ui(<ArtefactPending session={{ state: 'closed', recordingKey: null }} />)
    expect(screen.getByLabelText('Recording not in object storage')).toBeInTheDocument()
  })

  it('says nothing when the artefact is there', () => {
    ui(<ArtefactPending session={{ state: 'closed', recordingKey: 'recordings/x.cast' }} />)
    expect(screen.queryByLabelText('Recording not in object storage')).not.toBeInTheDocument()
  })

  // An active session's recording is still being written and has no business
  // being in storage yet, so this would be noise on every live row.
  it('says nothing about a session still running', () => {
    ui(<ArtefactPending session={{ state: 'active', recordingKey: null }} />)
    expect(screen.queryByLabelText('Recording not in object storage')).not.toBeInTheDocument()
  })
})

/**
 * An end the control plane deduced is not a clean logout.
 *
 * A session whose gateway never came back is closed at its last sighting, so
 * its `endedAt` is a lower bound rather than a reported end. Rendered as a
 * plain figure it would say the session ran exactly that long, which is the
 * overstatement the silent case already avoids.
 */
describe('a session Argus closed on its gateway\'s behalf', () => {
  const closed = {
    startedAt: at(200),
    endedAt: at(140),
    state: 'closed' as const,
    silent: false,
    lastReportedAt: at(140),
    endInferred: true,
  }

  it('is not shown as an ordinary close', () => {
    ui(<SessionStateBadge state="closed" endInferred lastReportedAt={at(140)} />)
    expect(screen.getByText('ended (inferred)')).toBeInTheDocument()
  })

  it('leaves a genuinely reported close alone', () => {
    ui(<SessionStateBadge state="closed" endInferred={false} lastReportedAt={at(140)} />)
    expect(screen.getByText('closed')).toBeInTheDocument()
    expect(screen.queryByText('ended (inferred)')).not.toBeInTheDocument()
  })

  it('gives a duration that reads as a floor', () => {
    ui(<SessionDuration session={closed} />)
    expect(screen.getByText('1h 0m+')).toBeInTheDocument()
  })

  it('gives a plain figure when the end was actually reported', () => {
    ui(<SessionDuration session={{ ...closed, endInferred: false }} />)
    expect(screen.getByText('1h 0m')).toBeInTheDocument()
  })
})
