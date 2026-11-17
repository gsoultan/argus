import { describe, expect, it, vi } from 'vitest'

const get = vi.fn()
vi.mock('~/lib/live', () => ({ get: (...a: unknown[]) => get(...a), send: vi.fn() }))

const { readiness } = await import('~/lib/controlReadiness')

// The settings page reads the report on every section; an answer that is not
// one must fail the query, not the page.
describe('readiness', () => {
  it('refuses an answer that is not a readiness report', async () => {
    for (const bad of [[], {}, null, { outsideCheck: {} }]) {
      get.mockResolvedValueOnce(bad)
      await expect(readiness()).rejects.toThrow(/readiness report/)
    }
  })

  it('passes a report through', async () => {
    const v = { outsideCheck: { configured: false }, servers: [], running: 0,
      backups: { installed: false, timerOn: false, lastOk: false },
      directSsh: { refusing: 0, total: 0 }, confirmations: {}, confirmDays: 90 }
    get.mockResolvedValueOnce(v)
    await expect(readiness()).resolves.toBe(v)
  })
})
