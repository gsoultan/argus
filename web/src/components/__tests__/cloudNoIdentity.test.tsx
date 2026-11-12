import { screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { CloudOverview } from '~/components/cloud/CloudOverview'
import { renderWithProviders } from '~/test/render'

/**
 * Cloud with no accounts and no AWS identity. The identity is set from the
 * default organization only, so elsewhere the page names who sets it and
 * offers no button to a setting the admin cannot reach.
 */

// The page links to Settings and Connect; no router here.
vi.mock('~/components/links', () => ({
  ButtonLink: ({ children }: { children: React.ReactNode }) => <a href="/settings">{children}</a>,
}))

const bases = [{ provider: 'aws' as const, identity: 'main', name: 'Main', default: true, error: 'no EC2 IMDS role found' }]

describe('Cloud, before argus-control has an AWS identity', () => {
  it('in the default organization, sends an admin to the setting', async () => {
    renderWithProviders(<CloudOverview accounts={[]} bases={bases} admin platform onTab={() => {}} />)
    expect(await screen.findByText(/access key under Settings › Cloud identities/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Set the AWS identity' })).toBeInTheDocument()
  })

  it('in another organization, says the platform owner sets it and offers no button', async () => {
    renderWithProviders(<CloudOverview accounts={[]} bases={bases} admin platform={false} onTab={() => {}} />)
    expect(await screen.findByText(/Ask the platform's owner: it is set from the default organization/)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Set the AWS identity' })).not.toBeInTheDocument()
  })
})
