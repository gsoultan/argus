import { render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, expect, it, vi } from 'vitest'
import { FirewallTab, reachWords } from '~/components/server/FirewallTab'
import { firewallOff, openWords, ruleWhat, type FirewallFleetRow, type ServerFirewall } from '~/lib/firewallApi'
import { assets } from '~/lib/seed'
import { theme } from '~/theme'

/**
 * A server's Firewall tab says, in plain words, whether its firewall is on,
 * what each rule lets in and who made it, and who can reach each port that
 * listens. Assets › Firewalls says the same for the fleet in one line each.
 */

const report: ServerFirewall = {
  reported: true,
  listeningRead: true,
  firewall: {
    kind: 'ufw', installed: ['ufw'], active: true, defaultIn: 'deny', defaultOut: 'allow',
    manageable: true, readAt: '2026-11-23T05:00:00Z',
    rules: [],
  },
  rules: [
    { direction: 'in', action: 'allow', protocol: 'tcp', ports: '22', sources: ['10.8.0.0/24'], madeBy: 'outside' },
    { direction: 'in', action: 'allow', protocol: 'tcp', ports: '80,443', madeBy: 'outside' },
    { direction: 'in', action: 'allow', protocol: 'tcp', ports: '6379', sources: ['10.20.0.21'], argus: true, argusPort: 6379, madeBy: 'argus' },
  ],
  listening: [
    { port: 443, process: 'nginx', kind: '', localOnly: false, reach: { everyone: true, public: true } },
    { port: 5432, process: 'postgres', kind: 'postgres', localOnly: true, reach: { everyone: false, public: false } },
  ],
  names: { '10.20.0.21': 'app-01' },
}

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))

vi.mock('~/lib/firewallApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/firewallApi')>()),
  assetFirewall: async () => report,
}))

function renderTab() {
  const asset = assets.find((a) => a.protocol !== 'rdp')!
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MantineProvider theme={theme}>
        <FirewallTab asset={asset} />
      </MantineProvider>
    </QueryClientProvider>,
  )
}

describe('Firewall tab', () => {
  it('says the firewall is on, read only, and who made each rule', async () => {
    renderTab()
    expect(await screen.findByText(/Firewall is on/)).toBeTruthy()
    expect(screen.getByText('Read only for now')).toBeTruthy()
    expect(screen.getAllByText('By hand')).toHaveLength(2)
    expect(screen.getByText('app-01')).toBeTruthy()
  })

  it('names a rule by what it lets in', () => {
    expect(ruleWhat(report.rules[0]!)).toMatch(/SSH/)
  })

  it('tells who can reach a port, in words', () => {
    expect(reachWords({ localOnly: true, reach: { everyone: false, public: false } }, {}).text).toBe('Only this server')
    expect(reachWords({ localOnly: false, reach: { everyone: true, public: true } }, {}).text).toBe('Everyone, the internet included')
    expect(reachWords({ localOnly: false, reach: { everyone: false, public: false, sources: [] } }, {}).text).toBe('Nobody from outside')
    expect(reachWords({ localOnly: false, reach: { everyone: false, public: false, sources: ['10.20.0.21'] } }, { '10.20.0.21': 'app-01' }).text).toBe('app-01')
    expect(reachWords({ localOnly: false, reach: { everyone: false, public: true, sources: ['203.0.113.5'] } }, {}).color).toBe('amber')
  })
})

describe('Assets › Firewalls', () => {
  const row = (over: Partial<FirewallFleetRow>): FirewallFleetRow => ({
    assetId: 'a', hostname: 'web-01', kind: 'server', os: 'Ubuntu', tags: [], reported: true,
    firewall: 'ufw', active: true, manageable: true, rules: 3, outside: 2, openToInternet: [], ...over,
  })

  it('counts a firewall that is off or missing, not one never read', () => {
    expect(firewallOff(row({ active: false }))).toBe(true)
    expect(firewallOff(row({ active: false, firewall: '' as never }))).toBe(true)
    expect(firewallOff(row({ active: false, reported: false }))).toBe(false)
    expect(firewallOff(row({ active: false, error: 'not root' }))).toBe(false)
    expect(firewallOff(row({}))).toBe(false)
  })

  it('names the ports open to the internet', () => {
    expect(openWords([443, 6379, 9999])).toBe('Web (443), Redis (6379), 9999')
  })
})
