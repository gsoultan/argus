import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { RedisWorkspace } from '~/components/RedisWorkspace'
import { formatRedis } from '~/components/useStoreSession'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'

/**
 * The Redis workspace's one guarded path: a command the agent will only run
 * confirmed asks for the server's name, sends nothing until it is typed
 * exactly, and then sends the same command carrying it. The agent is the one
 * that refuses without it; this pins that the console asks rather than
 * quietly dropping the command or confirming on the person's behalf.
 */

class FakeSocket {
  static last: FakeSocket | null = null
  static OPEN = 1
  readyState = 1
  sent: Record<string, unknown>[] = []
  onmessage: ((ev: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  url: string
  constructor(url: string) { this.url = url; FakeSocket.last = this }
  send(frame: string) { this.sent.push((JSON.parse(frame) as { req: Record<string, unknown> }).req) }
  close() {}
  push(m: unknown) { act(() => { this.onmessage?.({ data: JSON.stringify(m) }) }) }
  answer(ans: Record<string, unknown>) { this.push({ type: 'ans', ans }) }
}

const db: Database = {
  id: 'r1', assetId: 'a1', hostname: 'cache-01', engine: 'redis', host: '127.0.0.1', port: 6379,
  poolerHost: '', poolerPort: 0, dbname: '0', adminUser: '', label: 'production', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

describe('RedisWorkspace', () => {
  beforeEach(() => { vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('asks for the server name before a confirmed command, and sends it only once typed', async () => {
    renderWithProviders(
      <RedisWorkspace database={db} access={{ level: 'dba' }} endpoint="/api/v1/postgres"
        getTicket={async () => ({ ticket: 't', role: 'ana', level: 'dba' })} />,
    )
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    expect(ws.url).toContain('principal=ana%40dba')
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'redis', level: 'dba', user: 'argus-ana-dba', ops: ['run', 'scan', 'value'] })
    // The key browser's first scan.
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'scan')).toBe(true))
    const scan = ws.sent.find((r) => r.op === 'scan')!
    ws.answer({ id: scan.id, type: 'result', keys: [], cursor: '0', total: 0 })

    const input = screen.getByLabelText('Redis command')
    fireEvent.change(input, { target: { value: 'FLUSHALL' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'run')).toBe(true))
    const first = ws.sent.find((r) => r.op === 'run')!
    expect(first.confirm).toBeUndefined()
    ws.answer({ id: first.id, type: 'error', needsConfirm: true, error: 'FLUSHALL needs you to type the server’s name' })

    const name = await screen.findByLabelText('Server name')
    const go = screen.getByRole('button', { name: 'Do it' })
    fireEvent.change(name, { target: { value: 'cache-0' } })
    expect(go).toBeDisabled()
    expect(ws.sent.filter((r) => r.op === 'run')).toHaveLength(1)

    fireEvent.change(name, { target: { value: 'cache-01' } })
    expect(go).not.toBeDisabled()
    fireEvent.click(go)
    await waitFor(() => expect(ws.sent.filter((r) => r.op === 'run')).toHaveLength(2))
    const second = ws.sent.filter((r) => r.op === 'run')[1]!
    expect(second).toMatchObject({ command: 'FLUSHALL', confirm: 'cache-01' })
    ws.answer({ id: second.id, type: 'result', value: { t: 's', v: 'OK' } })
    expect(await screen.findByText('OK')).toBeInTheDocument()
  })
})

describe('RedisWorkspace command line autocomplete', () => {
  beforeEach(() => { vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('completes a key the browser listed, and Enter takes the suggestion before it runs anything', async () => {
    renderWithProviders(
      <RedisWorkspace database={db} access={{ level: 'read' }} endpoint="/api/v1/postgres"
        getTicket={async () => ({ ticket: 't', role: 'ana', level: 'read' })} />,
    )
    await waitFor(() => expect(FakeSocket.last).not.toBeNull())
    const ws = FakeSocket.last!
    ws.push({ type: 'ready' })
    ws.answer({ id: 0, type: 'ready', engine: 'redis', level: 'read', user: 'argus-ana-read', ops: ['run', 'scan', 'value'] })
    await waitFor(() => expect(ws.sent.some((r) => r.op === 'scan')).toBe(true))
    const scan = ws.sent.find((r) => r.op === 'scan')!
    ws.answer({ id: scan.id, type: 'result', keys: [{ name: 'session:42', type: 'hash' }], cursor: '0', total: 1 })
    await screen.findByText('session:42')

    const input = screen.getByLabelText('Redis command') as HTMLInputElement
    input.focus()
    fireEvent.change(input, { target: { value: 'HGETALL sess' } })
    input.selectionStart = input.selectionEnd = input.value.length
    await screen.findByRole('listbox')
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(input.value).toBe('HGETALL session:42')
    expect(ws.sent.some((r) => r.op === 'run')).toBe(false)

    fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => expect(ws.sent.find((r) => r.op === 'run')).toMatchObject({ command: 'HGETALL session:42' }))
  })
})

describe('formatRedis', () => {
  it('prints replies the way redis-cli does', () => {
    expect(formatRedis({ t: 'i', i: 3 })).toBe('(integer) 3')
    expect(formatRedis({ t: 'n' })).toBe('(nil)')
    expect(formatRedis({ t: 'e', v: 'NOPERM no' })).toBe('(error) NOPERM no')
    expect(formatRedis({ t: 'a', a: [{ t: 'b', v: 'a' }, { t: 'b', v: 'b' }] })).toBe('1) "a"\n2) "b"')
    expect(formatRedis({ t: 'a', a: [{ t: 'b', v: 'a' }], cut: true, n: 5 })).toContain('4 more not shown')
  })
})
