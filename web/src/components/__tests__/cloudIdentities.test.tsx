import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ControlSection, useControlSettings } from '~/components/ControlSettings'
import type { AWSIdentity, ControlSettings } from '~/lib/live'
import { renderWithProviders } from '~/test/render'

/**
 * Settings › Cloud identities: who argus-control is to AWS. The secret is
 * never shown, a test says what AWS answered, and a test that would move the
 * trust off the connected accounts says so before anyone saves.
 */

const live = vi.hoisted(() => ({
  controlSettings: vi.fn(),
  testCloudIdentity: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  ...live,
  isConfigured: () => true,
}))

vi.mock('~/lib/cloud', async (orig) => ({
  ...(await orig<typeof import('~/lib/cloud')>()),
  cloudAccountsQuery: () => ({
    queryKey: ['cloud-accounts-test'],
    queryFn: async () => ({
      accounts: [{ accountId: '111122223333' }, { accountId: '444455556666' }],
      bases: [{ provider: 'aws', principal: 'arn:aws:iam::999988887777:user/old', source: 'a shared credentials file' }],
    }),
  }),
}))

function settings(aws?: Partial<AWSIdentity>): ControlSettings {
  return {
    configFile: '/var/lib/argus-control/control.yaml',
    ...(aws ? {
      cloud: {
        aws: {
          source: 'default', accessKeyId: '', secretAccessKeySet: false, profile: '',
          credentialsFile: '', homeRegion: '', ...aws,
        },
      },
    } : {}),
  } as unknown as ControlSettings
}

function Page() {
  const ctl = useControlSettings(true)
  return <ControlSection id="cloud" ctl={ctl} />
}

afterEach(() => vi.clearAllMocks())

describe('Cloud identities', () => {
  it('outside the default organization, says who sets it and offers nothing', async () => {
    live.controlSettings.mockResolvedValue(settings())
    renderWithProviders(<Page />)
    expect(await screen.findByText(/set from the default organization/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Test' })).toBeNull()
  })

  it('shows what the connected accounts trust now', async () => {
    live.controlSettings.mockResolvedValue(settings({}))
    renderWithProviders(<Page />)
    expect(await screen.findByText('arn:aws:iam::999988887777:user/old')).toBeInTheDocument()
    expect(screen.getByText(/2 connected accounts trust it/)).toBeInTheDocument()
  })

  it('an access key needs its secret before it can be tested', async () => {
    live.controlSettings.mockResolvedValue(settings({}))
    renderWithProviders(<Page />)
    fireEvent.click(await screen.findByLabelText('An access key'))
    fireEvent.change(screen.getByLabelText(/Access key id/), { target: { value: 'AKIAIOSFODNN7EXAMPLE' } })
    expect(screen.getByText(/Still needed: secret access key/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Test' })).toBeDisabled()
  })

  it('a configured secret is never shown, and a test that moves the trust warns', async () => {
    live.controlSettings.mockResolvedValue(settings({ source: 'key', accessKeyId: 'AKIAIOSFODNN7EXAMPLE', secretAccessKeySet: true }))
    live.testCloudIdentity.mockResolvedValue({
      provider: 'aws', arn: 'arn:aws:iam::999988887777:user/argus-control',
      principal: 'arn:aws:iam::999988887777:user/argus-control', source: 'the access key in Settings › Cloud identities',
    })
    renderWithProviders(<Page />)
    const secret = await screen.findByLabelText(/Secret access key/)
    expect(secret).toHaveValue('')
    expect(secret).toHaveAttribute('placeholder', 'Set. Type to replace it.')
    await screen.findByText('arn:aws:iam::999988887777:user/old')
    fireEvent.click(screen.getByRole('button', { name: 'Test' }))
    await waitFor(() => expect(live.testCloudIdentity).toHaveBeenCalledWith('aws', expect.objectContaining({
      aws: expect.objectContaining({ source: 'key', accessKeyId: 'AKIAIOSFODNN7EXAMPLE' }),
    })))
    expect(await screen.findByText('The connected accounts do not trust this identity yet')).toBeInTheDocument()
    expect(screen.getByText(/update each account's argus-access stack/)).toBeInTheDocument()
  })
})
