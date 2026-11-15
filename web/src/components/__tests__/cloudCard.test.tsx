import { screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
vi.mock('~/components/links', () => ({
  AnchorLink: ({ children }: { children: React.ReactNode }) => <a href="/cloud">{children}</a>,
}))

import { CloudCardBody, cloudHeaderLabel } from '~/components/server/CloudCard'
import type { AssetCloud, CloudItem } from '~/lib/cloud'
import { renderWithProviders } from '~/test/render'

/**
 * The Cloud card on a server's Overview: the instance it runs as, and how
 * Argus knows. A match by address is never shown as something the agent
 * said, and an instance in an account Argus does not read is named, not
 * hidden.
 */

const item = (by: 'agent' | 'address' | 'name'): CloudItem => ({
  accountId: 'acc-1', accountName: 'payments-prod', provider: 'aws', providerAccount: '412788931204',
  environment: 'production', firstSeen: '2026-11-01T00:00:00Z', seenAt: '2026-11-15T07:00:00Z',
  resource: { kind: 'instance', id: 'i-0a41c27b9e3f1d27d', name: 'web-01', region: 'ap-southeast-3', zone: 'ap-southeast-3a',
    state: 'running', size: 'm6i.large', addresses: ['10.20.1.14'], publicIp: '43.218.7.21' },
  server: { id: 'a1', hostname: 'web-01', by },
})

describe('CloudCardBody', () => {
  it('shows the instance and says the agent reported it', () => {
    renderWithProviders(<CloudCardBody data={{ reported: null, instance: item('agent') }} />)
    expect(screen.getByText('i-0a41c27b9e3f1d27d')).toBeInTheDocument()
    expect(screen.getByText('m6i.large')).toBeInTheDocument()
    expect(screen.getByText('reported by the agent')).toBeInTheDocument()
    expect(screen.queryByText(/Matched by/)).not.toBeInTheDocument()
  })

  it('labels an address match as a match', () => {
    renderWithProviders(<CloudCardBody data={{ reported: null, instance: item('address') }} />)
    expect(screen.getByText('matched by address')).toBeInTheDocument()
    expect(screen.getByText(/Matched by address, not reported by the agent/)).toBeInTheDocument()
  })

  it('names an instance in an account Argus does not read', () => {
    const data: AssetCloud = { instance: null,
      reported: { provider: 'aws', account: '999999999999', region: 'us-east-1', zone: 'us-east-1b', id: 'i-0123456789abcdef0', type: 't3.small' } }
    renderWithProviders(<CloudCardBody data={data} />)
    expect(screen.getByTestId('cloud-card-unread')).toHaveTextContent('999999999999')
    expect(screen.getByText(/Argus does not read that account/)).toBeInTheDocument()
  })
})

describe('cloudHeaderLabel', () => {
  it('says the cloud, account and type, or nothing', () => {
    expect(cloudHeaderLabel({ reported: null, instance: item('agent') })).toBe('AWS · payments-prod · m6i.large')
    expect(cloudHeaderLabel({ reported: null, instance: null })).toBeNull()
    expect(cloudHeaderLabel(undefined)).toBeNull()
  })
})
