import { act, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveTerminal, type TerminalState } from '~/components/LiveTerminal'
import { renderWithProviders } from '~/test/render'

/**
 * A dropped connection is not the end of a terminal session: the console goes
 * back to the same shell, by its session id, on a fresh ticket. Only the
 * server saying the session is over, or someone else's now, stops it trying;
 * and closing the terminal on purpose tells the server to hang up.
 */

vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80
    rows = 24
    loadAddon() {}
    open() {}
    write() {}
    writeln() {}
    clear() {}
    reset() {}
    dispose() {}
    onData() { return { dispose() {} } }
  },
}))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit() {} } }))
vi.mock('@xterm/xterm/css/xterm.css', () => ({}))

class FakeSocket {
  static all: FakeSocket[] = []
  static OPEN = 1
  readyState = 1
  binaryType = ''
  sent: { type: string }[] = []
  onopen: (() => void) | null = null
  onmessage: ((ev: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  onclose: (() => void) | null = null
  url: URL
  constructor(url: string) { this.url = new URL(url); FakeSocket.all.push(this) }
  send(frame: string) { this.sent.push(JSON.parse(frame) as { type: string }) }
  close() { this.readyState = 3 }
  push(m: unknown) { act(() => { this.onmessage?.({ data: JSON.stringify(m) }) }) }
  drop() { act(() => { this.readyState = 3; this.onclose?.() }) }
}

class FakeObserver { observe() {} disconnect() {} }

function render() {
  const states: TerminalState[] = []
  const end: { current: (() => void) | null } = { current: null }
  let n = 0
  const view = renderWithProviders(
    <LiveTerminal endpoint="/api/v1/terminal" target="web-01" principal="ana"
      getTicket={async () => `ticket-${++n}`}
      endRef={end}
      onStateChange={(s) => states.push(s)} />,
  )
  return { states, view, end }
}

describe('LiveTerminal reconnect', () => {
  beforeEach(() => {
    vi.stubGlobal('WebSocket', FakeSocket)
    vi.stubGlobal('ResizeObserver', FakeObserver)
  })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.all = []; sessionStorage.clear() })

  it('goes back to the same session after the connection drops', async () => {
    const { states } = render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    const first = FakeSocket.all[0]!
    expect(first.url.searchParams.get('resume')).toBeNull()
    first.push({ type: 'ready', session: 'sess-1' })
    expect(states.at(-1)).toBe('live')

    first.drop()
    expect(states.at(-1)).toBe('reconnecting')
    await waitFor(() => expect(FakeSocket.all).toHaveLength(2), { timeout: 3000 })
    const second = FakeSocket.all[1]!
    expect(second.url.searchParams.get('resume')).toBe('sess-1')
    expect(second.url.searchParams.get('ticket')).toBe('ticket-2')
    second.push({ type: 'ready', session: 'sess-1', resumed: true })
    expect(states.at(-1)).toBe('live')
  })

  it('stops when the server says the session is gone', async () => {
    const { states } = render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    FakeSocket.all[0]!.push({ type: 'ready', session: 'sess-1' })
    FakeSocket.all[0]!.drop()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(2), { timeout: 3000 })
    const second = FakeSocket.all[1]!
    second.push({ type: 'gone', data: 'the session has ended on the server' })
    second.drop()
    expect(states.at(-1)).toBe('lost')
    await new Promise((r) => setTimeout(r, 800))
    expect(FakeSocket.all).toHaveLength(2)
  })

  it('does not fight another window for the session', async () => {
    const { states } = render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    const first = FakeSocket.all[0]!
    first.push({ type: 'ready', session: 'sess-1' })
    first.push({ type: 'moved', data: 'this session was opened in another window' })
    first.drop()
    expect(states.at(-1)).toBe('lost')
    await new Promise((r) => setTimeout(r, 800))
    expect(FakeSocket.all).toHaveLength(1)
  })

  it('hangs up only when ended on purpose, not when the terminal goes away', async () => {
    const first = render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    FakeSocket.all[0]!.push({ type: 'ready', session: 'sess-1' })
    // Another page, a reload: the shell is left running for this tab.
    first.view.unmount()
    expect(FakeSocket.all[0]!.sent.map((m) => m.type)).not.toContain('hangup')

    const second = render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(2))
    const sock = FakeSocket.all[1]!
    expect(sock.url.searchParams.get('resume')).toBe('sess-1')
    sock.push({ type: 'ready', session: 'sess-1', resumed: true })
    // Disconnect: hung up, and not picked up again.
    act(() => second.end.current?.())
    expect(sock.sent.map((m) => m.type)).toContain('hangup')
    second.view.unmount()
    render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(3))
    expect(FakeSocket.all[2]!.url.searchParams.get('resume')).toBeNull()
  })

  it('asks before the tab is closed while a session runs', async () => {
    render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    FakeSocket.all[0]!.push({ type: 'ready', session: 'sess-1' })
    const ev = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(ev)
    expect(ev.defaultPrevented).toBe(true)
  })

  it('opens a new session when the one this tab left has ended', async () => {
    sessionStorage.setItem('argus.terminal:ana@web-01', 'old-sess')
    const { states } = render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    expect(FakeSocket.all[0]!.url.searchParams.get('resume')).toBe('old-sess')
    FakeSocket.all[0]!.push({ type: 'gone', data: 'no such session' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(2))
    expect(FakeSocket.all[1]!.url.searchParams.get('resume')).toBeNull()
    FakeSocket.all[1]!.push({ type: 'ready', session: 'new-sess' })
    expect(states.at(-1)).toBe('live')
  })

  it('keeps trying while the server is still waiting for the agent', async () => {
    const { states } = render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    FakeSocket.all[0]!.push({ type: 'ready', session: 'sess-1' })
    FakeSocket.all[0]!.drop()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(2), { timeout: 3000 })
    // argus-control restarted and the agent is not back yet.
    FakeSocket.all[1]!.push({ type: 'retry', data: 'waiting for the agent on web-01 to reconnect' })
    FakeSocket.all[1]!.drop()
    expect(states.at(-1)).toBe('reconnecting')
    await waitFor(() => expect(FakeSocket.all).toHaveLength(3), { timeout: 4000 })
    expect(FakeSocket.all[2]!.url.searchParams.get('resume')).toBe('sess-1')
    FakeSocket.all[2]!.push({ type: 'ready', session: 'sess-1', resumed: true })
    expect(states.at(-1)).toBe('live')
  })

  it('shows the agent link being restored while the page stays connected', async () => {
    const { states } = render()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    const sock = FakeSocket.all[0]!
    sock.push({ type: 'ready', session: 'sess-1' })
    sock.push({ type: 'notice', kind: 'agent-reconnecting', data: 'lost the connection to the agent' })
    expect(states.at(-1)).toBe('reconnecting')
    sock.push({ type: 'notice', kind: 'agent-back', data: 'reconnected' })
    expect(states.at(-1)).toBe('live')
  })
})
