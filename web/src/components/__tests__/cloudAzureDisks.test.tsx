import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { NewAddress, NewDisk, StorageActions } from '~/components/cloud/StorageActions'
import {
  AZURE_DISK_NAME, cloudAccountsQuery, cloudChangesQuery, cloudItemsQuery, roleLacks, type CloudAccount, type CloudItem,
} from '~/lib/cloud'
import { meQuery } from '~/lib/queries'
import { theme } from '~/theme'

/**
 * Disks in an Azure subscription: a disk lives in a resource group, may
 * have no zone, grows in size only, and has no final snapshot of its own.
 */

const account = {
  id: 'ca3', provider: 'azure', accountId: '6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90', name: 'billing-azure', access: 'manage',
  environment: 'development', regions: ['eastus'], manageMissing: [], manageCheckedAt: null,
  actions: ['start', 'stop', 'reboot', 'resize', 'tags', 'volume-create', 'volume-grow', 'volume-attach', 'volume-detach', 'volume-delete',
    'snapshot-create', 'snapshot-delete', 'address-allocate', 'address-associate', 'address-disassociate', 'address-release'],
  tagKinds: ['instance', 'volume', 'snapshot'],
} as unknown as CloudAccount

const item = (resource: Record<string, unknown>): CloudItem => ({
  accountId: 'ca3', accountName: 'billing-azure', provider: 'azure', environment: 'development',
  resource: { region: 'eastus', details: { resourceGroup: 'Web-RG' }, ...resource },
}) as unknown as CloudItem

const vm = item({ kind: 'instance', id: 'vm-1', name: 'web-1', zone: 'eastus-2', state: 'running' })
const osDisk = item({ kind: 'volume', id: 'd-1', name: 'web-1_OsDisk', zone: 'eastus-2', state: 'in-use', size: '30 GiB',
  details: { resourceGroup: 'Web-RG', type: 'Premium_LRS', platform: 'linux' }, links: [{ kind: 'instance', id: 'vm-1', name: 'web-1' }] })
const leftOver = item({ kind: 'volume', id: 'd-3', name: 'left-over', state: 'available', size: '200 GiB',
  details: { resourceGroup: 'Web-RG', type: 'Standard_LRS' } })

const lbAddress = item({ kind: 'address', id: 'ip-4', name: 'lb-ip', state: 'attached', publicIp: '20.6.6.6' })

function show(node: React.ReactNode, acct: CloudAccount = account) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } })
  client.setQueryData(meQuery().queryKey, { role: 'owner', email: 'dev@northwind.id' } as never)
  client.setQueryData(cloudAccountsQuery().queryKey, { accounts: [acct], bases: [] })
  client.setQueryData(cloudChangesQuery({ pending: true }).queryKey, [])
  client.setQueryData(cloudItemsQuery('instance').queryKey, [vm])
  client.setQueryData(cloudItemsQuery('volume').queryKey, [osDisk, leftOver])
  client.setQueryData(cloudItemsQuery('subnet').queryKey, [])
  client.setQueryData(cloudItemsQuery('image').queryKey, [])
  client.setQueryData(cloudItemsQuery('address').queryKey, [lbAddress])
  return render(
    <MantineProvider theme={theme} defaultColorScheme="light" forceColorScheme="light">
      <QueryClientProvider client={client}>{node}</QueryClientProvider>
    </MantineProvider>,
  )
}

describe('Disks in an Azure subscription', () => {
  it('grows a disk in size only, and says the disk a VM starts from needs the VM stopped', () => {
    show(<StorageActions it={osDisk} />)
    expect(screen.queryByText('Grow or change')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Grow' }))
    expect(screen.getByText(/Azure grows that one only while the VM is stopped/)).toBeInTheDocument()
    expect(screen.getByLabelText('Size, GiB')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Grow disk' })).toBeDisabled()
  })

  it('takes a snapshot under a name Azure accepts, next to the disk', () => {
    show(<StorageActions it={leftOver} />)
    fireEvent.click(screen.getByRole('button', { name: 'Snapshot' }))
    expect(screen.getByText(/Azure copies the disk as it is now/)).toBeInTheDocument()
    expect(screen.getByText(/in the disk's resource group, Web-RG,/)).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'a/b' } })
    expect(screen.getByText(/starting with a letter or digit, up to 80/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Take snapshot' })).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'left-over-before-upgrade' } })
    expect(screen.getByRole('button', { name: 'Take snapshot' })).toBeEnabled()
  })

  it('says where a disk without a zone can go', () => {
    show(<StorageActions it={leftOver} />)
    fireEvent.click(screen.getByRole('button', { name: 'Attach' }))
    expect(screen.getByText(/A disk without a zone attaches only to a VM without one. Azure picks the LUN/)).toBeInTheDocument()
    expect(screen.getByPlaceholderText('No instances without a zone here')).toBeInTheDocument()
  })

  it('asks a new disk for its resource group and a name, and makes the zone optional', () => {
    show(<NewDisk onClose={() => {}} />)
    expect(screen.getByText('Resource group')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('No zone')).toBeInTheDocument()
    expect(screen.getByText(/Encrypted with a key Azure manages/)).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'logs-1' } })
    // Named, but in no resource group yet.
    expect(screen.getByRole('button', { name: 'Make disk' })).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'bad name' } })
    expect(screen.getByText(/Letters, digits, \. _ and -/)).toBeInTheDocument()
  })

  it('holds a change back on a manage role made before it, by Azure\'s own action names', () => {
    const older = { ...account, manageMissing: ['Microsoft.Compute/disks/delete', 'Microsoft.Compute/snapshots/write'] } as CloudAccount
    expect(roleLacks(older, 'volume-delete')).toBe(true)
    expect(roleLacks(older, 'snapshot-create')).toBe(true)
    expect(roleLacks(older, 'volume-grow')).toBe(false)
    expect(roleLacks(account, 'volume-delete')).toBe(false)
  })

  it('makes a public address in a resource group, under a name', () => {
    show(<NewAddress onClose={() => {}} />)
    expect(screen.getByText('New public address')).toBeInTheDocument()
    expect(screen.getByText('Resource group')).toBeInTheDocument()
    expect(screen.getByText(/Azure bills a Standard static address/)).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'api-ip' } })
    expect(screen.getByRole('button', { name: 'Make address' })).toBeDisabled()
  })

  it('does not offer to take an address off a load balancer', () => {
    show(<StorageActions it={lbAddress} />)
    expect(screen.queryByRole('button', { name: 'Take it off' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Attach to an instance' })).toBeNull()
  })

  it('knows what Azure takes as a disk name', () => {
    for (const ok of ['logs-1', 'web-1_OsDisk_1', 'A', '9lives', 'ends_']) expect(AZURE_DISK_NAME.test(ok)).toBe(true)
    for (const bad of ['', '-lead', 'ends-', 'ends.', 'a b', 'a/b', 'a'.repeat(81)]) expect(AZURE_DISK_NAME.test(bad)).toBe(false)
  })
})
