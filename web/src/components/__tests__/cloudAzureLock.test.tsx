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
 * Azure protects a VM from deletion with a resource lock. Argus puts one
 * on and takes its own off, and says so in Azure's words, not in AWS's
 * "termination protection".
 */

const account = { id: 'ca3', name: 'billing-azure', provider: 'azure', environment: 'development', access: 'manage', regions: ['eastus'],
  actions: ['start', 'stop', 'reboot', 'protect', 'unprotect'], tagKinds: ['instance'] } as unknown as CloudAccount

const vm: CloudItem = {
  accountId: 'ca3', accountName: 'billing-azure', provider: 'azure', environment: 'development',
  resource: { kind: 'instance', id: 'vm-1', name: 'web-1', region: 'eastus', state: 'running', size: 'Standard_D2s_v5', details: {} },
} as unknown as CloudItem

function show(acct = account) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  client.setQueryData(meQuery().queryKey, { role: 'owner', email: 'dev@northwind.id' } as never)
  client.setQueryData(cloudAccountsQuery().queryKey, { accounts: [acct] } as never)
  client.setQueryData(cloudChangesQuery({ pending: true }).queryKey, [] as never)
  return render(
    <MantineProvider theme={theme} defaultColorScheme="light" forceColorScheme="light">
      <QueryClientProvider client={client}><InstanceActions it={vm} /></QueryClientProvider>
    </MantineProvider>,
  )
}

describe('The delete lock on an Azure VM', () => {
  it('is put on from the VM\'s menu, in Azure\'s words', async () => {
    show()
    fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    expect(await screen.findByText('Turn off the delete lock')).toBeInTheDocument()
    expect(screen.queryByText(/termination protection/)).toBeNull()
    fireEvent.click(screen.getByText('Turn on the delete lock'))
    expect(await screen.findByText('Put a delete lock on web-1?')).toBeInTheDocument()
    expect(screen.getByText(/It can still be stopped, restarted, resized and tagged/)).toBeInTheDocument()
  })

  it('comes off as Argus\'s own, leaving anybody else\'s', async () => {
    show()
    fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    fireEvent.click(await screen.findByText('Turn off the delete lock'))
    expect(await screen.findByText('Take the delete lock off web-1?')).toBeInTheDocument()
    expect(screen.getByText(/A lock somebody put on in the Azure portal stays/)).toBeInTheDocument()
  })

  it('is not offered where the subscription\'s role cannot set one', () => {
    show({ ...account, actions: ['start', 'stop', 'reboot'] } as unknown as CloudAccount)
    expect(screen.queryByRole('button', { name: 'More actions' })).toBeNull()
  })
})
