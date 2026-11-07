import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FileOpError, FilesClient } from '~/lib/files/client'

/**
 * A transfer that is moving can be stopped: the console ends the connection,
 * which the agent takes as the transfer cut short, and opens a fresh one on a
 * new ticket. What was cancelled fails as cancelled, not as an error.
 */

class FakeSocket {
  static OPEN = 1
  static all: FakeSocket[] = []
  readyState = 1
  bufferedAmount = 0
  binaryType = ''
  sent: unknown[] = []
  closed = false
  url: string
  onmessage: ((ev: { data: unknown }) => void) | null = null
  onerror: (() => void) | null = null
  onclose: (() => void) | null = null
  constructor(url: string) {
    this.url = url
    FakeSocket.all.push(this)
  }
  send(d: unknown) { this.sent.push(d) }
  close() {
    this.closed = true
    this.readyState = 3
  }
  say(m: object) { this.onmessage?.({ data: JSON.stringify(m) }) }
}

const flush = () => new Promise((r) => setTimeout(r, 0))

describe('cancelling a transfer that is moving', () => {
  beforeEach(() => {
    FakeSocket.all = []
    vi.stubGlobal('WebSocket', FakeSocket)
  })
  afterEach(() => vi.unstubAllGlobals())

  it('ends the connection, fails the download as cancelled, and comes back on a new ticket', async () => {
    let tickets = 0
    const states: string[] = []
    const c = new FilesClient({
      endpoint: '/api/v1/files', target: 'web-01', principal: 'gita',
      getTicket: async () => `t${++tickets}`,
      onState: (s) => states.push(s),
    })
    await c.open()
    const first = FakeSocket.all[0]!
    first.say({ type: 'ready' })
    expect(c.state).toBe('live')

    const dl = c.download('/var/log/big.log')
    await flush()
    first.onmessage?.({ data: new ArrayBuffer(8) })

    const back = c.cancel()
    const err = await dl.catch((e: unknown) => e)
    expect(err).toBeInstanceOf(FileOpError)
    expect((err as FileOpError).cancelled).toBe(true)
    expect(first.closed).toBe(true)

    await flush()
    const second = FakeSocket.all[1]!
    expect(second.url).toContain('ticket=t2')
    second.say({ type: 'ready' })
    await back
    expect(c.state).toBe('live')
    expect(states).not.toContain('closed')
    expect(states).not.toContain('error')

    // The old connection closing late changes nothing.
    first.onclose?.()
    expect(c.state).toBe('live')
    c.close()
  })

  it('stops an upload before it says it is finished', async () => {
    const c = new FilesClient({
      endpoint: '/api/v1/files', target: 'web-01', principal: 'gita',
      getTicket: async () => 'tk', onState: () => {},
    })
    await c.open()
    const ws = FakeSocket.all[0]!
    ws.say({ type: 'ready' })
    const up = c.upload('/tmp/a.bin', new Blob([new Uint8Array(10)]), false)
    await flush()
    ws.say({ type: 'upload-ready' })
    void c.cancel()
    const err = await up.catch((e: unknown) => e)
    expect((err as FileOpError).cancelled).toBe(true)
    expect(ws.sent.some((d) => typeof d === 'string' && d.includes('upload-end'))).toBe(false)
    c.close()
  })
})
