import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { CostsPanel } from '~/components/cloud/Costs'
import type { CloudAccount } from '~/lib/cloud'
import { cloudCostsQuery, cloudIdleQuery, type CloudCostRead } from '~/lib/cloudCosts'
import { meQuery } from '~/lib/queries'
import { theme } from '~/theme'

/**
 * An Azure subscription's costs sit beside AWS's: read from Azure Cost
 * Management, which does not charge, in US dollars as Azure converts them.
 */

const account = (id: string, provider: string) =>
  ({ id, name: `billing-${provider}`, provider, accountId: id, environment: 'development', access: 'read', regions: [] }) as unknown as CloudAccount

const read = (accountId: string, name: string, forecast: number | null): CloudCostRead => ({
  accountId, accountName: name, cloudAccountId: accountId, environment: 'development', readAt: '2026-11-10T09:00:00Z',
  costs: { month: '2026-11', currency: 'USD', monthToDate: 48.25, lastMonth: 160.5, lastMonthToDate: 100, forecast,
    forecastRest: forecast === null ? undefined : 20.5, days: [], services: [{ service: 'Virtual Machines', thisMonth: 42, lastMonth: 140 }] },
} as unknown as CloudCostRead)

function show(accounts: CloudAccount[], reads: CloudCostRead[]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  client.setQueryData(meQuery().queryKey, { role: 'owner', email: 'dev@northwind.id' } as never)
  client.setQueryData(cloudCostsQuery().queryKey, { accounts: reads, budgets: [], intervalHours: 8, askEveryMinutes: 15 } as never)
  client.setQueryData(cloudIdleQuery().queryKey, { items: [], monthly: 0 })
  return render(
    <MantineProvider theme={theme} defaultColorScheme="light" forceColorScheme="light">
      <QueryClientProvider client={client}><CostsPanel accounts={accounts} /></QueryClientProvider>
    </MantineProvider>,
  )
}

describe('Costs of an Azure subscription', () => {
  it('says the forecast is Azure\'s and that a read costs nothing', () => {
    show([account('ca3', 'azure')], [read('ca3', 'billing-azure', 55.75)])
    expect(screen.getByText(/\$20\.50 Azure predicts for/)).toBeInTheDocument()
    expect(screen.getByText(/An Azure subscription is read from Azure Cost Management, which does not charge, in US dollars as Azure converts them/)).toBeInTheDocument()
    expect(screen.getByText('Virtual Machines')).toBeInTheDocument()
  })

  it('names no one provider when AWS and Azure are read together', () => {
    show([account('ca1', 'aws'), account('ca3', 'azure')], [read('ca1', 'billing-aws', 80), read('ca3', 'billing-azure', 55.75)])
    expect(screen.getByText(/the provider predicts for/)).toBeInTheDocument()
  })

  it('keeps AWS\'s words where only AWS is connected', () => {
    show([account('ca1', 'aws')], [read('ca1', 'billing-aws', null)])
    expect(screen.getByText('AWS gives one after a few weeks of data')).toBeInTheDocument()
    expect(screen.queryByText(/Azure/)).toBeNull()
  })
})
