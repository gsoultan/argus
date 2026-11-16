import { act, renderHook } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ServerSystem } from '~/types/domain'

/*
 * Live usage: one stream per server however many readers, samples written
 * into the System query, the heartbeat's processor warning kept, a reason
 * when live figures cannot be had, and nothing open while the tab is hidden.
 */

vi.mock('~/lib/live', () => ({
  isConfigured: () => true,
  usageLiveURL: (id: string) => `/api/v1/assets/${id}/usage/live`,
}))

class FakeES {
  static all: FakeES[] = []
  static CLOSED = 2
  readyState = 0
  closed = false
  onerror: (() => void) | null = null
  handlers: Record<string, ((e: MessageEvent<string>) => void)[]> = {}
  url: string
  constructor(url: string) { this.url = url; FakeES.all.push(this) }
  addEventListener(t: string, h: (e: MessageEvent<string>) => void) { (this.handlers[t] ??= []).push(h) }
  close() { this.closed = true; this.readyState = 2 }
  emit(t: string, data: unknown) {
    for (const h of this.handlers[t] ?? []) h({ data: JSON.stringify(data) } as MessageEvent<string>)
  }
}

const usage = (cpu: number) => ({
  load: null, cpuBusy: cpu, memTotal: 100, memAvailable: 50, swapTotal: 0, swapFree: 0, disks: [], net: [],
})

describe('useLiveUsage', () => {
  let client: QueryClient
  beforeEach(() => {
    FakeES.all = []
    vi.stubGlobal('EventSource', FakeES)
    client = new QueryClient()
  })
  afterEach(() => vi.unstubAllGlobals())

  const wrapper = ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )

  it('shares one stream and keeps the System query current', async () => {
    const { useLiveUsage, isLiveUsage } = await import('~/lib/liveUsage')
    const before: ServerSystem = {
      reported: true, os: 'Ubuntu', system: null, usage: usage(5), usageAt: '2026-11-16T05:00:00Z',
      warnings: [{ resource: 'cpu', level: 'low', percent: 95, left: 0, total: 0 }],
    }
    client.setQueryData(['asset', 'a1', 'system'], before)
    const one = renderHook(() => useLiveUsage('a1', true), { wrapper })
    const two = renderHook(() => useLiveUsage('a1', true), { wrapper })
    expect(FakeES.all).toHaveLength(1)

    act(() => FakeES.all[0]!.emit('usage', {
      usage: usage(42), usageAt: '2026-11-16T05:00:02Z', everySeconds: 2,
      warnings: [{ resource: 'memory', level: 'low', percent: 8, left: 8, total: 100 }],
    }))
    act(() => FakeES.all[0]!.emit('usage', { usage: usage(43), usageAt: '2026-11-16T05:00:04Z', everySeconds: 2, warnings: [] }))
    expect(one.result.current.state).toBe('live')
    expect(two.result.current.samples.map((s) => s.usage.cpuBusy)).toEqual([42, 43])
    expect(isLiveUsage('a1')).toBe(true)
    const sys = client.getQueryData<ServerSystem>(['asset', 'a1', 'system'])!
    expect(sys.usage?.cpuBusy).toBe(43)
    expect(sys.usageAt).toBe('2026-11-16T05:00:04Z')
    // The minute-long processor warning is the heartbeat's to clear.
    expect(sys.warnings?.map((w) => w.resource)).toEqual(['cpu'])

    one.unmount()
    expect(FakeES.all[0]!.closed).toBe(false)
    two.unmount()
    expect(FakeES.all[0]!.closed).toBe(true)
  })

  it('says why when live figures cannot be had', async () => {
    const { useLiveUsage } = await import('~/lib/liveUsage')
    const h = renderHook(() => useLiveUsage('a2', true), { wrapper })
    act(() => FakeES.all[0]!.emit('end', { reason: 'agent too old' }))
    expect(h.result.current.state).toBe('unavailable')
    expect(h.result.current.reason).toMatch(/update it/)
    expect(FakeES.all[0]!.closed).toBe(true)
    h.unmount()
  })

  it('opens nothing when not enabled, and nothing while the tab is hidden', async () => {
    const { useLiveUsage } = await import('~/lib/liveUsage')
    const off = renderHook(() => useLiveUsage('a3', false), { wrapper })
    expect(FakeES.all).toHaveLength(0)
    off.unmount()

    const h = renderHook(() => useLiveUsage('a4', true), { wrapper })
    const vis = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    act(() => { document.dispatchEvent(new Event('visibilitychange')) })
    expect(FakeES.all[0]!.closed).toBe(true)
    vis.mockReturnValue('visible')
    act(() => { document.dispatchEvent(new Event('visibilitychange')) })
    expect(FakeES.all).toHaveLength(2)
    expect(FakeES.all[1]!.closed).toBe(false)
    h.unmount()
    vis.mockRestore()
  })
})
