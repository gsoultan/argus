import { describe, expect, it } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { NetworkPanel } from '~/components/DatabaseNetwork'
import type { Admin, AdminOp, Collect } from '~/components/DatabaseAdmin'
import { pgHBALines, pgListenFor, pgReachPlan, type PGNetworkData } from '~/lib/pgNetwork'
import { mysqlReachPlan, type MySQLNetworkData } from '~/lib/mysqlNetwork'
import { mongoReachPlan, type MongoNetworkData } from '~/lib/mongoNetwork'
import { MongoNetwork } from '~/components/mongo/MongoNetwork'
import type { StoreAnswer, StoreRequest, StoreSession } from '~/components/useStoreSession'
import type { Database } from '~/types/domain'

/*
Which machines can connect to a Postgres: the panel sends the agent fixed
operations, and says before the button what will change and what stops it.
*/

const collect: Collect = async () => ({ cols: [], rows: [] })
const base = { collect, levels: {}, self: 'r_hakim', narrow: false, onBack: () => {} }

const net: PGNetworkData = {
  listen: 'localhost', port: 5432, hbaFile: '/etc/postgresql/16/main/pg_hba.conf', method: 'scram-sha-256', ssl: false,
  pendingRestart: false, clients: 4, remote: 0, reach: 'local', host: 'db-01', service: 'postgresql@16-main.service',
  firewall: 'ufw', firewallActive: true,
  addresses: [{ ip: '10.0.0.183', iface: 'eth0', prefix: '10.0.0.0/24', private: true, state: 'closed' }],
}

function fakeAdmin(data: PGNetworkData): { admin: Admin; ops: AdminOp[] } {
  const ops: AdminOp[] = []
  const admin = (async (op: AdminOp) => {
    ops.push(op)
    if (op.kind === 'network') return { data }
    return { data: { network: { ...data, reach: op.reach, listen: 'localhost,10.0.0.183', firewallSources: op.sources }, restarted: true, firewallRan: ['ufw allow from 10.0.0.61 to any port 5432 proto tcp comment argus 5432'] } }
  }) as Admin
  return { admin, ops }
}

describe('Postgres network panel', () => {
  it('is labelled, not hidden, below DBA and asks the agent for nothing', () => {
    const { admin, ops } = fakeAdmin(net)
    ui(<NetworkPanel {...base} admin={admin} level="write" />)
    expect(screen.getByText('Needs DBA access')).toBeInTheDocument()
    expect(ops).toHaveLength(0)
  })

  it('lets selected machines in through listen_addresses, pg_hba.conf and the firewall, with a restart', async () => {
    const { admin, ops } = fakeAdmin(net)
    ui(<NetworkPanel {...base} admin={admin} level="dba" />)
    await screen.findByText(/Only programs on db-01 can connect/)
    expect(ops[0]).toEqual({ kind: 'network' })
    fireEvent.click(screen.getByRole('radio', { name: /Selected machines/ }))
    fireEvent.change(screen.getByLabelText('Add an address or range'), { target: { value: '10.0.0.61' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(screen.getByText(/listen_addresses = 'localhost,10\.0\.0\.183'/)).toBeInTheDocument()
    expect(screen.getByText(/\+ host all all 10\.0\.0\.61\/32 scram-sha-256 # argus/)).toBeInTheDocument()
    expect(screen.getByText('Restart postgresql@16-main.service')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }))
    await screen.findByText('Now: selected machines. Postgres was restarted and listens anew.')
    expect(ops.at(-1)).toEqual({ kind: 'network.set', reach: 'selected', addresses: ['10.0.0.183'], sources: ['10.0.0.61'], restart: true, confirm: undefined })
  })

  it('opens to anywhere only with the name typed, and never past a no-password line', async () => {
    const { admin } = fakeAdmin({ ...net, trust: ['host all all 10.0.0.0/8 trust'] })
    ui(<NetworkPanel {...base} admin={admin} level="dba" />)
    await screen.findByText(/Only programs on db-01/)
    fireEvent.click(screen.getByRole('radio', { name: /Anywhere/ }))
    expect(await screen.findByText(/Not yet: pg_hba.conf lets machines in with no password/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Ask for approval' })).toBeDisabled()
  })

  it('asks for approval to open to anywhere once the name is typed', async () => {
    const { admin } = fakeAdmin(net)
    ui(<NetworkPanel {...base} admin={admin} level="dba" />)
    await screen.findByText(/Only programs on db-01/)
    fireEvent.click(screen.getByRole('radio', { name: /Anywhere/ }))
    const go = await screen.findByRole('button', { name: 'Ask for approval' })
    expect(go).toBeDisabled()
    fireEvent.change(screen.getByLabelText("Type the server's name"), { target: { value: 'DB-01' } })
    await waitFor(() => expect(go).toBeEnabled())
  })
})

describe('Postgres network plan', () => {
  it('writes what the agent writes', () => {
    expect(pgListenFor('selected', ['10.0.0.183'])).toBe('localhost,10.0.0.183')
    expect(pgListenFor('any', [])).toBe('*')
    expect(pgHBALines('selected', ['10.0.0.61', '10.0.4.0/24', 'fd00::1'], 'md5')).toEqual([
      'host all all 10.0.0.61/32 md5 # argus', 'host all all 10.0.4.0/24 md5 # argus', 'host all all fd00::1/128 md5 # argus'])
    expect(pgHBALines('any', [], 'scram-sha-256')).toHaveLength(2)
  })
  it('says what stops a change', () => {
    expect(pgReachPlan({ ...net, firewallActive: false }, 'selected', ['10.0.0.61'], true).blocked).toMatch(/no firewall is on/)
    expect(pgReachPlan({ ...net, firewallOpenToAll: true }, 'selected', ['10.0.0.61'], true).blocked).toMatch(/already lets everyone in/)
    expect(pgReachPlan(net, 'selected', [], true).blocked).toMatch(/add at least one machine/)
    expect(pgReachPlan({ ...net, addresses: [] }, 'network', [], true).blocked).toMatch(/no private address/)
    expect(pgReachPlan(net, 'network', [], true).blocked).toBeNull()
    const noRestart = pgReachPlan(net, 'network', [], false)
    expect(noRestart.steps.at(-1)).toMatchObject({ state: 'warn', title: 'Not restarted' })
    const noService = pgReachPlan({ ...net, service: undefined }, 'network', [], true)
    expect(noService.steps.at(-1)).toMatchObject({ state: 'warn', title: 'Restart Postgres on the server' })
  })
})

describe('MySQL network panel', () => {
  const my: MySQLNetworkData = {
    bind: '127.0.0.1', next: '127.0.0.1', pendingRestart: false, port: 3306, version: '10.11.14-MariaDB', mariadb: true,
    listOK: true, ssl: false, remoteUsers: ["'app'@'10.0.0.%'"], keptIn: '/etc/mysql/mariadb.conf.d/zz-argus-network.cnf',
    clients: 3, remote: 0, reach: 'local', host: 'db-02', service: 'mariadb.service', firewall: 'ufw', firewallActive: true,
    addresses: [{ ip: '10.0.0.184', iface: 'eth0', prefix: '10.0.0.0/24', private: true, state: 'closed' }],
  }
  it('keeps bind_address for the next start and restarts', async () => {
    const ops: AdminOp[] = []
    const admin = (async (op: AdminOp) => {
      ops.push(op)
      return op.kind === 'network' ? { data: my } : { data: { network: { ...my, reach: 'selected', firewallSources: op.sources }, restarted: true } }
    }) as Admin
    ui(<NetworkPanel {...base} admin={admin} level="dba" engine="mysql" />)
    await screen.findByText(/Only programs on db-02 can connect. MariaDB listens on 127.0.0.1/)
    fireEvent.click(screen.getByRole('radio', { name: /Selected machines/ }))
    fireEvent.change(screen.getByLabelText('Add an address or range'), { target: { value: '10.0.0.61' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(screen.getByText(/bind_address = 127\.0\.0\.1,10\.0\.0\.184/)).toBeInTheDocument()
    expect(screen.getByText("'app'@'10.0.0.%'")).toBeInTheDocument()
    expect(screen.getByText('Restart mariadb.service')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }))
    await screen.findByText('Now: selected machines. MariaDB was restarted and listens anew.')
    expect(ops.at(-1)).toMatchObject({ kind: 'network.set', reach: 'selected', addresses: ['10.0.0.184'], sources: ['10.0.0.61'], restart: true })
  })
  it('says what stops it', () => {
    expect(mysqlReachPlan({ ...my, listOK: false, version: '10.6.18-MariaDB' }, 'selected', ['10.0.0.61'], true).blocked).toMatch(/one address or on every one/)
    expect(mysqlReachPlan({ ...my, noPassRemote: ["'legacy'@'%'"] }, 'any', [], true).blocked).toMatch(/legacy/)
    expect(mysqlReachPlan({ ...my, skipNetworking: true }, 'network', [], true).blocked).toMatch(/skip-networking/)
    expect(mysqlReachPlan({ ...my, remoteUsers: [] }, 'network', [], true).steps.some((s) => s.title === 'No user may sign in from another machine yet')).toBe(true)
  })
})

describe('MongoDB network panel', () => {
  const mg: MongoNetworkData = {
    bind: '127.0.0.1', next: '127.0.0.1', pendingRestart: false, port: 27017, version: '7.0.14', configFile: '/etc/mongod.conf',
    tls: false, clients: 5, remote: 0, reach: 'local', host: 'db-03', service: 'mongod.service', firewall: 'ufw', firewallActive: true,
    addresses: [{ ip: '10.0.0.185', iface: 'eth0', prefix: '10.0.0.0/24', private: true, state: 'closed' }],
  }
  it('asks the MongoDB session, and changes bindIp for the next start with a restart', async () => {
    const asked: StoreRequest[] = []
    const s: StoreSession = {
      status: 'ready', fatal: null, cancel: () => {}, lastId: () => 1,
      ready: { id: 0, type: 'ready', level: 'dba', ops: ['run', 'scan', 'value', 'network', 'network_set'] },
      ask: async (req) => {
        asked.push(req)
        const a: StoreAnswer = req.op === 'network'
          ? { id: 1, type: 'result', data: mg }
          : { id: 2, type: 'result', data: { network: { ...mg, bind: '127.0.0.1,10.0.0.185', next: '127.0.0.1,10.0.0.185', reach: 'selected', firewallSources: req.sources }, restarted: true } }
        return a
      },
    }
    const db = { id: 'd1', engine: 'mongodb', dbname: 'app', hostname: 'db-03', label: 'staging', assetId: 'a1' } as unknown as Database
    ui(<MongoNetwork s={s} database={db} opened onClose={() => {}} narrow={false} />)
    await screen.findByText(/Only programs on db-03 can connect. MongoDB listens on 127.0.0.1/)
    expect(asked[0]).toEqual({ op: 'network' })
    fireEvent.click(screen.getByRole('radio', { name: /Selected machines/ }))
    fireEvent.change(screen.getByLabelText('Add an address or range'), { target: { value: '10.0.0.61' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(screen.getByText(/bindIp: 127\.0\.0\.1,10\.0\.0\.185/)).toBeInTheDocument()
    expect(screen.getByText('Restart mongod.service')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }))
    await screen.findByText('Now: selected machines. MongoDB was restarted and listens anew.')
    expect(asked.at(-1)).toEqual({ op: 'network_set', reach: 'selected', addresses: ['10.0.0.185'], sources: ['10.0.0.61'], restart: true })
  })
  it('says what stops it', () => {
    expect(mongoReachPlan({ ...mg, cmdLine: true }, 'network', [], true).blocked).toMatch(/--bind_ip/)
    expect(mongoReachPlan({ ...mg, configFile: undefined }, 'network', [], true).blocked).toMatch(/no configuration file/)
    expect(mongoReachPlan({ ...mg, firewallActive: false }, 'selected', ['10.0.0.61'], true).blocked).toMatch(/no firewall is on/)
    const any = mongoReachPlan(mg, 'any', [], true)
    expect(any.blocked).toBeNull()
    expect(any.steps[0]).toMatchObject({ state: 'change', diff: { from: 'bindIp: 127.0.0.1', to: 'bindIpAll: true' } })
    expect(any.steps.some((x) => x.title === 'TLS is off')).toBe(true)
  })
})
