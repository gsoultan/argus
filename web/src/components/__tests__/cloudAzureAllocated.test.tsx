import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { InstanceActions } from '~/components/cloud/InstanceActions'
import { cloudAccountsQuery, cloudChangesQuery, type CloudAccount, type CloudItem } from '~/lib/cloud'
import { meQuery } from '~/lib/queries'
import { theme } from '~/theme'

vi.mock('@tanstack/react-router', async (orig) => ({ ...(await orig<object>()), useNavigate: () => () => {} }))

/**
 * A VM shut down from inside it is stopped as far as anyone signed in can
 * tell, but Azure keeps it allocated and bills it in full. Argus says so
 * and offers the stop that deallocates it; a deallocated VM is offered
 * only a start.
 */

const account = { id: 'ca3', name: 'billing-azure', provider: 'azure', environment: 'development', access: 'manage', regions: ['eastus'],
  actions: ['start', 'stop', 'reboot'], tagKinds: ['instance'] } as unknown as CloudAccount

const vm = (details: Record<string, string>): CloudItem => ({
  accountId: 'ca3', accountName: 'billing-azure', provider: 'azure', environment: 'development',
  resource: { kind: 'instance', id: 'vm-1', name: 'web-1', region: 'eastus', state: 'stopped', size: 'Standard_D2s_v5', details },
} as unknown as CloudItem)

function show(it: CloudItem, acct = account) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  client.setQueryData(meQuery().queryKey, { role: 'owner', email: 'dev@northwind.id' } as never)
  client.setQueryData(cloudAccountsQuery().queryKey, { accounts: [acct] } as never)
  client.setQueryData(cloudChangesQuery({ pending: true }).queryKey, [] as never)
  return render(
    <MantineProvider theme={theme} defaultColorScheme="light" forceColorScheme="light">
      <QueryClientProvider client={client}><InstanceActions it={it} /></QueryClientProvider>
    </MantineProvider>,
  )
}

describe('An Azure VM that is stopped but still allocated', () => {
  it('says Azure still bills it and offers to deallocate it', () => {
    show(vm({ allocated: 'yes' }))
    expect(screen.getByText(/shut down from inside the VM, so Azure keeps it allocated and still bills it in full/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Start' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Deallocate' }))
    expect(screen.getByText(/Azure shuts the VM down and deallocates it/)).toBeInTheDocument()
  })

  it('offers a deallocated VM only a start', () => {
    show(vm({}))
    expect(screen.getByRole('button', { name: 'Start' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Deallocate' })).toBeNull()
    expect(screen.queryByText(/still bills it in full/)).toBeNull()
  })

  it('does not treat another cloud\'s stopped instance so', () => {
    show(vm({ allocated: 'yes' }), { ...account, provider: 'aws' } as CloudAccount)
    expect(screen.queryByRole('button', { name: 'Deallocate' })).toBeNull()
  })
})
