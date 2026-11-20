import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { RemoveChange } from '~/components/cloud/InstanceActions'
import { cloudRemovePlanQuery, roleLacks, type CloudAccount, type CloudItem, type CloudRemovePlanView } from '~/lib/cloud'
import { meQuery } from '~/lib/queries'
import { theme } from '~/theme'

/**
 * Removing a VM in an Azure subscription: Azure deletes with a VM only
 * what was marked to go with it, so the dialog says what stays, and a
 * resource lock comes off as its own change.
 */

const vm = {
  accountId: 'ca3', accountName: 'billing-azure', provider: 'azure', environment: 'development',
  resource: { kind: 'instance', id: 'vm-1', name: 'web-1', region: 'eastus', state: 'running', details: { resourceGroup: 'Web-RG' } },
} as unknown as CloudItem

const plan: CloudRemovePlanView = {
  plan: {
    id: 'vm-1', name: 'web-1', state: 'running', size: 'Standard_B2s', protected: false, elasticIp: '20.1.2.3',
    stays: ['network interface web-1-nic'],
    volumes: [
      { id: 'd-1', device: 'os', sizeGb: 30, deleted: true, description: 'web-1_OsDisk (boot)' },
      { id: 'd-2', device: 'lun 0', sizeGb: 128, deleted: false, description: 'web-1-data' },
    ],
  },
  hash: 'h1', environment: 'development', needsApproval: false,
}

function show(v: CloudRemovePlanView) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  client.setQueryData(meQuery().queryKey, { role: 'owner', email: 'dev@northwind.id' } as never)
  client.setQueryData(cloudRemovePlanQuery(vm).queryKey, v)
  return render(
    <MantineProvider theme={theme} defaultColorScheme="light" forceColorScheme="light">
      <QueryClientProvider client={client}><RemoveChange it={vm} gcp={false} azure onClose={() => {}} /></QueryClientProvider>
    </MantineProvider>,
  )
}

describe('Removing a VM in an Azure subscription', () => {
  it('says what Azure deletes with it and what stays behind', () => {
    show(plan)
    expect(screen.getByText(/Azure deletes the VM/)).toBeInTheDocument()
    expect(screen.getByText('web-1_OsDisk (boot)')).toBeInTheDocument()
    expect(screen.getByText('Kept, still billed')).toBeInTheDocument()
    expect(screen.getByText(/Public address 20\.1\.2\.3 is taken off it and stays in the subscription, still billed/)).toBeInTheDocument()
    expect(screen.getByText(/Also left behind, at no charge: network interface web-1-nic/)).toBeInTheDocument()
    expect(screen.getByText(/Argus waits for each snapshot to finish/)).toBeInTheDocument()
    expect(screen.queryByText(/Elastic IP|AWS/)).toBeNull()
    expect(screen.getByRole('button', { name: 'Remove VM' })).toBeDisabled()
  })

  it('says how a lock comes off', () => {
    show({ ...plan, plan: { ...plan.plan, protected: true }, refusal: 'it has a delete lock in Azure; turn its protection off first, which is its own change, or take off in the Azure portal a lock Argus did not put on' })
    expect(screen.getByText(/turn its protection off first/)).toBeInTheDocument()
    expect(screen.queryByLabelText(/to confirm/)).toBeNull()
  })

  it('is held back while the manage role cannot delete a VM', () => {
    const old = { provider: 'azure', manageMissing: ['Microsoft.Compute/virtualMachines/delete'] } as unknown as CloudAccount
    expect(roleLacks(old, 'remove')).toBe(true)
    expect(roleLacks({ ...old, manageMissing: [] } as CloudAccount, 'remove')).toBe(false)
  })
})
