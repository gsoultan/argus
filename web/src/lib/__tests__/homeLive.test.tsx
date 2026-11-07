import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { StreamStatus } from '~/lib/alerts'
import type { HomeFrame, HomeSnapshot } from '~/types/domain'

/**
 * Home's live store: the whole dashboard once, then only what changed, in
 * order; a gap asks for all of it again, and a hidden tab lets Home go.
 */

const hub = vi.hoisted(() => ({
  frame: null as ((f: HomeFrame) => void) | null,
  status: null as ((s: StreamStatus) => void) | null,
  want: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({ ...(await orig<typeof import('~/lib/live')>()), isConfigured: () => true }))
vi.mock('~/lib/alertStream', () => ({
  subscribeHome: (f: (x: HomeFrame) => void, s: (x: StreamStatus) => void) => {
    hub.frame = f
    hub.status = s
    return { want: hub.want, stop: () => {} }
  },
}))

const { api } = await import('~/lib/api')
const { retainHome, useHome, useHomeStatus, resetHomeForTests } = await import('~/lib/homeLive')

function Probe() {
  const live = useHome((s) => s.live)
  const servers = useHome((s) => s.servers)
  const { status } = useHomeStatus()
  return <div data-testid="probe">{status}:{live?.count ?? '-'}:{servers?.total ?? '-'}</div>
}

const whole = { live: { count: 1, root: 0, database: 0, silent: 0 }, servers: { total: 3, answering: 3, quiet: 0, down: 0 } } as Partial<HomeSnapshot>

async function frame(f: HomeFrame) {
  await act(async () => {
    hub.frame?.(f)
    await new Promise((r) => setTimeout(r, 30))
  })
}

beforeEach(() => {
  resetHomeForTests()
  hub.want.mockClear()
  vi.spyOn(api, 'home').mockResolvedValue(whole as HomeSnapshot)
})
afterEach(() => {
  vi.restoreAllMocks()
  resetHomeForTests()
})

describe('Home over the stream', () => {
  it('applies the whole dashboard, then each change in order, and asks again after a gap', async () => {
    let release = () => {}
    act(() => { release = retainHome() })
    render(<Probe />)
    await vi.waitFor(() => expect(hub.want).toHaveBeenCalledWith(true))

    await frame({ v: 4, full: true, set: whole })
    expect(screen.getByTestId('probe')).toHaveTextContent('live:1:3')

    await frame({ v: 5, set: { live: { count: 6, root: 1, database: 0, silent: 0 } } })
    // Only the field that changed; the rest is as it was.
    expect(screen.getByTestId('probe')).toHaveTextContent('live:6:3')

    // v7 after v5: one was missed. It is not applied, and the store asks for
    // all of it again.
    hub.want.mockClear()
    await frame({ v: 7, set: { live: { count: 99, root: 0, database: 0, silent: 0 } } })
    expect(screen.getByTestId('probe')).toHaveTextContent('live:6:3')
    expect(hub.want).toHaveBeenCalledWith(true, true)
    act(() => release())
  })

  it('reads every 15 s on its own when the stream is gone, and lets Home go when hidden', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      let release = () => {}
      act(() => { release = retainHome() })
      render(<Probe />)
      await vi.waitFor(() => expect(hub.want).toHaveBeenCalledWith(true))
      const reads = vi.mocked(api.home).mock.calls.length
      act(() => hub.status?.('ended'))
      await act(async () => { await vi.advanceTimersByTimeAsync(15_100) })
      expect(screen.getByTestId('probe')).toHaveTextContent(/^polling/)
      expect(vi.mocked(api.home).mock.calls.length).toBe(reads + 1)

      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' })
      act(() => { document.dispatchEvent(new Event('visibilitychange')) })
      await act(async () => { await vi.advanceTimersByTimeAsync(10_100) })
      expect(hub.want).toHaveBeenLastCalledWith(false)
      expect(screen.getByTestId('probe')).toHaveTextContent(/^paused/)
      // Hidden, it reads nothing.
      const hiddenReads = vi.mocked(api.home).mock.calls.length
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000) })
      expect(vi.mocked(api.home).mock.calls.length).toBe(hiddenReads)
      act(() => release())
    } finally {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' })
      vi.useRealTimers()
    }
  })
})
