import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { RedisWorkspace } from '~/components/RedisWorkspace'
import { renderWithProviders } from '~/test/render'
import type { Database } from '~/types/domain'

/**
 * The Redis management paths that change data: the key editor sends only
 * the commands it staged, with an undo copy asked for; a new server user's
 * password leaves the browser only as its SHA-256.
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
  async reply(op: string, ans: Record<string, unknown>) {
    await waitFor(() => expect(this.sent.some((r) => r.op === op && !this.done.has(r.id as number))).toBe(true))
    const r = this.sent.find((x) => x.op === op && !this.done.has(x.id as number))!
    this.done.add(r.id as number)
    this.answer({ id: r.id, ...ans })
    return r
  }
  done = new Set<number>()
}

const db: Database = {
  id: 'r1', assetId: 'a1', hostname: 'cache-01', engine: 'redis', host: '127.0.0.1', port: 6379,
  poolerHost: '', poolerPort: 0, dbname: '0', adminUser: '', label: 'staging', policy: {},
  createdBy: '', createdAt: '2026-11-02T00:00:00Z', tunnelConnected: true,
}

const ALL = ['run', 'scan', 'value', 'info', 'edit', 'count', 'bulk', 'undo_list', 'undo', 'config_get', 'config_set', 'save', 'acl_list', 'acl_setuser', 'acl_deluser', 'acl_dryrun', 'acl_login', 'network', 'network_set', 'default_secure']

async function open() {
  renderWithProviders(
    <RedisWorkspace database={db} access={{ level: 'dba' }} endpoint="/api/v1/postgres"
      getTicket={async () => ({ ticket: 't', role: 'ana', level: 'dba' })} />,
  )
  await waitFor(() => expect(FakeSocket.last).not.toBeNull())
  const ws = FakeSocket.last!
  ws.push({ type: 'ready' })
  ws.answer({ id: 0, type: 'ready', engine: 'redis', level: 'dba', user: 'argus-ana-dba', ops: ALL })
  await ws.reply('scan', { type: 'result', keys: [{ name: 'user:1', type: 'hash', ttl: -1 }], cursor: '0', total: 1 })
  return ws
}

describe('Redis management', () => {
  beforeEach(() => { vi.stubGlobal('WebSocket', FakeSocket) })
  afterEach(() => { vi.unstubAllGlobals(); FakeSocket.last = null })

  it('stages a hash field change and applies it with an undo copy', async () => {
    const ws = await open()
    fireEvent.click(await screen.findByText('user:1'))
    await ws.reply('value', { type: 'result', keyType: 'hash', ttl: -1, total: 1, value: { t: 'a', a: [{ t: 'b', v: 'name' }, { t: 'b', v: 'Ana' }] } })
    fireEvent.click(await screen.findByLabelText('Edit name'))
    const input = screen.getByLabelText('New value')
    fireEvent.change(input, { target: { value: 'Ana Lima' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(await screen.findByText('HSET user:1 name "Ana Lima"')).toBeInTheDocument()
    expect(ws.sent.some((r) => r.op === 'edit')).toBe(false)
    fireEvent.click(screen.getByRole('button', { name: 'Apply 1' }))
    const edit = await ws.reply('edit', { type: 'result', data: { ran: 1, undoId: 'a'.repeat(32) } })
    expect(edit).toMatchObject({ key: 'user:1', edits: [['HSET', 'user:1', 'name', 'Ana Lima']], undo: true })
  })

  it('sends a new user’s password only as its SHA-256', async () => {
    const ws = await open()
    fireEvent.click(screen.getByRole('tab', { name: 'Users & ACL' }))
    await ws.reply('acl_list', { type: 'result', data: { aclFile: true, users: [
      { name: 'default', on: true, nopass: true, passwords: [], keys: ['~*'], channels: ['&*'], commands: ['+@all'], selectors: [], argus: false, self: false, clients: 0 },
    ] } })
    expect(await screen.findByText(/anyone who reaches Redis signs in as it/i)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'New user' }))
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'checkout-api' } })
    fireEvent.click(screen.getByText('Type one'))
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'hunter2hunter2' } })
    fireEvent.click(screen.getByRole('button', { name: 'Make user' }))
    const req = await ws.reply('acl_setuser', { type: 'result', data: { saved: 'ACL SAVE' } })
    expect(req).toMatchObject({ user: 'checkout-api', create: true, state: 'on' })
    expect(req.addHashes).toEqual([expect.stringMatching(/^[0-9a-f]{64}$/)])
    expect(JSON.stringify(req)).not.toContain('hunter2hunter2')
    expect(req.rules).toEqual(['~*', '+@read', '+@connection', '-@dangerous'])

    // Made with a password typed here: it is checked for real, on every address.
    const check = await ws.reply('acl_login', { type: 'result', data: { user: 'checkout-api', port: 6379, logins: [
      { address: '127.0.0.1:6379', ok: true },
      { address: '192.168.1.20:6379', ok: false, detail: 'nothing answers here: Redis does not listen on this address' },
    ] } })
    expect(check).toMatchObject({ user: 'checkout-api', password: 'hunter2hunter2' })
    expect(await screen.findByText('checkout-api signs in on this server only')).toBeInTheDocument()
  })

  it('opens Redis to the LAN only on purpose, once the default user has a password', async () => {
    const ws = await open()
    fireEvent.click(screen.getByRole('tab', { name: 'Users & ACL' }))
    await ws.reply('acl_list', { type: 'result', data: { aclFile: true, users: [] } })
    const net = {
      port: 6379, bind: ['127.0.0.1', '-::1'], protectedMode: true, defaultOpen: true, defaultClients: 0,
      configFile: '/etc/redis/redis.conf', agentConf: true, firewall: 'ufw',
      addresses: [{ ip: '192.168.1.20', iface: 'eth0', prefix: '192.168.1.0/24', private: true, state: 'closed' }],
    }
    await ws.reply('network', { type: 'result', data: net })
    expect(await screen.findByText('The default user has no password')).toBeInTheDocument()
    fireEvent.click(screen.getByLabelText('Listen on 192.168.1.20'))
    expect(screen.getByRole('button', { name: 'Open to the LAN…' })).toBeDisabled()

    fireEvent.click(screen.getByRole('button', { name: 'Give the default user a password' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Set a password' }))
    await ws.reply('default_secure', { type: 'result', data: { savedTo: '/etc/redis/redis.conf', network: { ...net, defaultOpen: false } } })
    fireEvent.click(await screen.findByLabelText('Listen on 192.168.1.20'))
    fireEvent.click(screen.getByRole('button', { name: 'Open to the LAN…' }))
    const go = await screen.findByRole('button', { name: 'Open to the LAN' })
    expect(go).toBeDisabled()
    expect(ws.sent.some((r) => r.op === 'network_set')).toBe(false)
    fireEvent.click(screen.getByLabelText('I understand every machine on these networks can reach Redis'))
    fireEvent.click(go)
    const set = await ws.reply('network_set', { type: 'result', data: { rewritten: true, network: {
      ...net, defaultOpen: false, bind: ['127.0.0.1', '-::1', '-192.168.1.20'],
      addresses: [{ ...net.addresses[0], state: 'signin' }] } } })
    expect(set).toMatchObject({ addresses: ['192.168.1.20'] })
    expect(await screen.findByText('sudo ufw allow from 192.168.1.0/24 to any port 6379 proto tcp')).toBeInTheDocument()
  })
})
