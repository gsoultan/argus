import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { FirewallActions, InstanceFirewalls } from '~/components/cloud/NetworkActions'
import {
  azureRemovalProblem, azureRuleName, cloudAccountsQuery, cloudChangesQuery, cloudItemsQuery, roleLacks, validSource,
  type CloudAccount, type CloudItem, type CloudRule,
} from '~/lib/cloud'
import { meQuery } from '~/lib/queries'
import { theme } from '~/theme'

/**
 * Network security groups in an Azure subscription: one Azure rule can let
 * several sources reach several ports, which Argus lists as a rule for
 * each pair, so not every one of them can be taken away on its own; and a
 * VM is behind the group on its interface and the one on its subnet.
 */

const tcp = (port: number, source: string, description: string): CloudRule =>
  ({ inbound: true, protocol: 'tcp', fromPort: port, toPort: port, source, description })

const rules = [
  tcp(80, '10.1.0.0/16', 'web'), tcp(443, '10.1.0.0/16', 'web'), tcp(80, '10.2.0.0/16', 'web'), tcp(443, '10.2.0.0/16', 'web'),
  tcp(22, 'tag:VirtualNetwork', 'admin: the bastion'),
]

const account = { id: 'ca3', name: 'billing-azure', provider: 'azure', environment: 'development', access: 'manage', regions: ['eastus'],
  actions: ['firewall-rules', 'firewall-delete', 'instance-firewalls'], tagKinds: ['instance'] } as unknown as CloudAccount

const item = (kind: string, id: string, name: string, more: object = {}): CloudItem => ({
  accountId: 'ca3', accountName: 'billing-azure', provider: 'azure', environment: 'development',
  resource: { kind, id, name, region: 'eastus', state: 'enabled', details: { resourceGroup: 'Web-RG' }, ...more },
} as unknown as CloudItem)

const app = item('firewall', 'g-app', 'app-nsg', { rules, details: { resourceGroup: 'Web-RG', interfaces: '0', denyRules: '2' } })
const onSubnet = item('firewall', 'g-subnet', 'subnet-nsg', { details: { resourceGroup: 'Web-RG', interfaces: '1' } })
const onNic = item('firewall', 'g-web', 'web-nsg', { details: { resourceGroup: 'Web-RG', interfaces: '1' } })
const subnet = item('subnet', 'web-rg/main/web', 'web', { links: [{ kind: 'firewall', id: 'g-subnet' }] })
const vm = item('instance', 'vm-1', 'web-1', { state: 'running',
  links: [{ kind: 'subnet', id: 'web-rg/main/web' }, { kind: 'firewall', id: 'g-subnet' }, { kind: 'firewall', id: 'g-web' }] })

function show(ui: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  client.setQueryData(meQuery().queryKey, { role: 'owner', email: 'dev@northwind.id' } as never)
  client.setQueryData(cloudAccountsQuery().queryKey, { accounts: [account] } as never)
  client.setQueryData(cloudChangesQuery({ pending: true }).queryKey, [] as never)
  client.setQueryData(cloudItemsQuery('firewall').queryKey, [app, onSubnet, onNic])
  client.setQueryData(cloudItemsQuery('subnet').queryKey, [subnet])
  client.setQueryData(cloudItemsQuery('instance').queryKey, [vm])
  client.setQueryData(cloudItemsQuery('load-balancer').queryKey, [])
  return render(
    <MantineProvider theme={theme} defaultColorScheme="light" forceColorScheme="light">
      <QueryClientProvider client={client}>{ui}</QueryClientProvider>
    </MantineProvider>,
  )
}

describe('Network security groups in an Azure subscription', () => {
  it('knows which rules can be taken away on their own', () => {
    expect(azureRuleName(rules[4]!)).toBe('admin')
    expect(azureRemovalProblem(rules, new Set([0]))).toMatch(/Azure rule web lets several sources reach several ports together/)
    for (const ok of [[4], [0, 2], [0, 1], [0, 1, 2, 3], []]) expect(azureRemovalProblem(rules, new Set(ok))).toBeUndefined()
  })

  it('takes a range, an address or one of two service tags as the source of a new rule', () => {
    for (const ok of ['10.0.0.0/16', '203.0.113.7/32', 'tag:VirtualNetwork', 'tag:azureloadbalancer', '::/0']) expect(validSource(ok, false, true)).toBe(true)
    for (const bad of ['sg-0123456789abcdef0', 'tag:Storage', 'tag:web', 'office']) expect(validSource(bad, false, true)).toBe(false)
  })

  it('says what a group is on and that its deny rules are not Argus\'s to change', () => {
    show(<FirewallActions it={app} />)
    expect(screen.getByText(/It is on no network interface or subnet\. It also has 2 deny rules, which Argus never changes/)).toBeInTheDocument()
    expect(screen.getByText('It is in front of no VM now.')).toBeInTheDocument()
  })

  it('holds back a removal Azure cannot say, and says how it adds a rule', () => {
    show(<FirewallActions it={app} />)
    fireEvent.click(screen.getByRole('button', { name: 'Edit rules' }))
    expect(screen.getByText(/^outbound rules$/i)).toBeInTheDocument()
    fireEvent.click(screen.getAllByRole('button', { name: 'Take this rule away' })[0]!)
    expect(screen.getByText(/Azure rule web lets several sources reach several ports together/)).toBeInTheDocument()
    expect(screen.getByText(/Argus never puts one\s+ahead of a deny rule/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save 1 change' })).toBeDisabled()
    fireEvent.click(screen.getAllByRole('button', { name: 'Take this rule away' })[1]!)
    expect(screen.queryByText(/lets several sources reach several ports together/)).toBeNull()
  })

  it('offers one group for a VM\'s interface and leaves its subnet\'s alone', () => {
    show(<InstanceFirewalls it={vm} env="development" onClose={() => {}} />)
    expect(screen.getByText('Network security group of web-1')).toBeInTheDocument()
    expect(screen.getByText(/Also in front of it, from its subnet: subnet-nsg/)).toBeInTheDocument()
    expect(screen.getByText(/One, from eastus, or none when its subnet has a group/)).toBeInTheDocument()
    expect(screen.queryByText(/AWS|Google/)).toBeNull()
  })

  it('is held back while the manage role cannot write a group', () => {
    const old = { provider: 'azure', manageMissing: ['Microsoft.Network/networkSecurityGroups/write'] } as unknown as CloudAccount
    expect(roleLacks(old, 'firewall-rules')).toBe(true)
    expect(roleLacks(old, 'firewall-create')).toBe(true)
    expect(roleLacks(old, 'instance-firewalls')).toBe(false)
  })
})
