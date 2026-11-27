import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { renderWithProviders as ui } from '~/test/render'
import { MongoPerformance } from '~/components/mongo/MongoPerformance'
import type { StoreRequest, StoreSession } from '~/components/useStoreSession'
import captured from '~/lib/mongoPerformance.fixture.json'

/*
MongoDB's Performance panel asks the agent one read-only operation, perf,
explains what it found, and puts a fix on the command line instead of
running it.
*/

function fakeSession(fail?: string): { s: StoreSession; asked: StoreRequest[] } {
  const asked: StoreRequest[] = []
  const s: StoreSession = {
    status: 'ready', fatal: null, ready: null, cancel: () => {}, lastId: () => 0,
    ask: async (req: StoreRequest) => {
      asked.push(req)
      return fail ? { type: 'error', error: fail } : { type: 'result', data: captured }
    },
  } as unknown as StoreSession
  return { s, asked }
}

describe('MongoDB Performance', () => {
  it('reads perf only, explains, and puts a fix on the command line', async () => {
    const { s, asked } = fakeSession()
    const use = vi.fn()
    const close = vi.fn()
    ui(<MongoPerformance s={s} opened onClose={close} narrow={false} onUse={use} />)
    expect(await screen.findByText(/A heavy query on orders/)).toBeInTheDocument()
    expect(screen.getByText('orders.status_1 is covered by status_1_total_1')).toBeInTheDocument()
    expect(screen.getByText('Slowest operations')).toBeInTheDocument()
    expect(asked).toEqual([{ op: 'perf' }])

    fireEvent.click(screen.getAllByText('Put on the command line')[0]!)
    expect(use).toHaveBeenCalledWith('db.orders.createIndex({ customer: 1 })')
    expect(close).toHaveBeenCalled()
    expect(asked).toHaveLength(1)
  })

  it('says what MongoDB refused', async () => {
    const { s } = fakeSession('not authorized on admin to execute command { serverStatus: 1 }')
    ui(<MongoPerformance s={s} opened onClose={() => {}} narrow={false} onUse={() => {}} />)
    expect(await screen.findByText(/not authorized on admin/)).toBeInTheDocument()
  })
})
