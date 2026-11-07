import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { DeviceReplayTimeline, describe as describeEvent } from '~/components/DeviceReplayTimeline'
import { renderWithProviders } from '~/test/render'
import type { ReplayActivity } from '~/types/domain'

const activity: ReplayActivity[] = [
  { ms: 0, kind: 'app', package: 'com.android.chrome', title: 'Chrome' },
  { ms: 4000, kind: 'secure', hidden: true },
  { ms: 9000, kind: 'typed', password: true },
  { ms: 12000, kind: 'lock' },
]

describe('the device replay timeline', () => {
  it('shows each event in plain words and never the text of a password', () => {
    const onSeek = vi.fn()
    renderWithProviders(<DeviceReplayTimeline activity={activity} cursorMs={0} onSeek={onSeek} />)

    expect(screen.getByText(/Chrome.*came to the front/i)).toBeInTheDocument()
    expect(screen.getByText(/Secure content hidden/i)).toBeInTheDocument()
    expect(screen.getByText('Typed into a password field')).toBeInTheDocument()
    expect(screen.getByText('Screen locked')).toBeInTheDocument()
  })

  it('seeks when an event is clicked', async () => {
    const onSeek = vi.fn()
    renderWithProviders(<DeviceReplayTimeline activity={activity} cursorMs={0} onSeek={onSeek} />)
    fireEvent.click(screen.getByText('Screen locked'))
    expect(onSeek).toHaveBeenCalledWith(12000)
  })

  it('renders no timeline when there is no activity', () => {
    renderWithProviders(<DeviceReplayTimeline activity={[]} cursorMs={0} onSeek={() => {}} />)
    expect(screen.queryByText('Activity')).not.toBeInTheDocument()
  })
})

describe('describe()', () => {
  it('names a typed password without carrying its text', () => {
    expect(describeEvent({ ms: 0, kind: 'typed', password: true, text: 'should-not-appear' }))
      .toBe('Typed into a password field')
  })
  it('quotes ordinary typed text', () => {
    expect(describeEvent({ ms: 0, kind: 'typed', text: 'hello' })).toContain('hello')
  })
})
