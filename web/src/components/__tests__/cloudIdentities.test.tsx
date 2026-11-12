import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ControlSection, useControlSettings } from '~/components/ControlSettings'
import type { AWSIdentity, ControlSettings } from '~/lib/live'
import { renderWithProviders } from '~/test/render'

/**
 * Settings › Cloud identities: a list of who argus-control is to AWS. The
 * secret is never shown, an identity accounts still use cannot be removed,
 * and outside the default organization the list is read only.
 */

const live = vi.hoisted(() => ({
  controlSettings: vi.fn(),
  testCloudIdentity: vi.fn(),
  cloudIdentityStatus: vi.fn(),
  organizations: vi.fn(),
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
      accounts: [{ accountId: '111122223333', identity: 'main' }],
      bases: [
        { provider: 'aws', identity: 'main', name: 'Main', default: true, principal: 'arn:aws:iam::999988887777:user/old', source: 'a shared credentials file' },
        { provider: 'aws', identity: 'gov', name: 'GovCloud', default: false, principal: 'arn:aws-us-gov:iam::1:user/a', source: 'a key' },
      ],
    }),
  }),
}))

const main: AWSIdentity = {
  id: 'main', name: 'Main', organizations: [], source: 'file', accessKeyId: '', secretAccessKeySet: false,
  profile: 'argus', credentialsFile: '', homeRegion: '',
}
const keyed: AWSIdentity = {
  id: 'second', name: 'Second', organizations: ['acme'], source: 'key', accessKeyId: 'AKIAIOSFODNN7EXAMPLE',
  secretAccessKeySet: true, profile: '', credentialsFile: '', homeRegion: '',
}

function settings(identities?: AWSIdentity[], def = 'main'): ControlSettings {
  return {
    configFile: '/var/lib/argus-control/control.yaml',
    ...(identities ? { cloud: { aws: { default: def, identities } } } : {}),
  } as unknown as ControlSettings
}

function Page() {
  const ctl = useControlSettings(true)
  return <ControlSection id="cloud" ctl={ctl} />
}

function platform(identities: AWSIdentity[]) {
  live.controlSettings.mockResolvedValue(settings(identities))
  live.organizations.mockResolvedValue([{ id: 'default', name: 'Northwind' }, { id: 'acme', name: 'Acme Logistics' }])
  live.cloudIdentityStatus.mockResolvedValue([
    { identity: 'main', name: 'Main', principal: 'arn:aws:iam::999988887777:user/old',
      use: [{ organization: 'default', name: 'Northwind', accounts: 2 }, { organization: 'acme', name: 'Acme Logistics', accounts: 1 }] },
    { identity: 'second', name: 'Second', principal: 'arn:aws:iam::999988887777:user/second', use: [] },
  ])
}

/** The table row an identity is listed on. */
async function rowOf(name: string): Promise<HTMLElement> {
  await screen.findAllByText(name)
  const row = screen.getAllByText(name).map((e) => e.closest('tbody tr')).find(Boolean)
  if (!row) throw new Error(`no row for ${name}`)
  return row as HTMLElement
}

afterEach(() => vi.clearAllMocks())

describe('Cloud identities', () => {
  it('outside the default organization, lists the ones it may use and changes nothing', async () => {
    live.controlSettings.mockResolvedValue(settings())
    renderWithProviders(<Page />)
    expect(await screen.findByText('GovCloud')).toBeInTheDocument()
    expect(screen.getByText('1 of your accounts')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Add identity' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull()
    expect(live.cloudIdentityStatus).not.toHaveBeenCalled()
  })

  it('lists every identity with its default, who may use it, and who does', async () => {
    platform([main, keyed])
    renderWithProviders(<Page />)
    const row = await rowOf('Main')
    expect(within(row).getByText('Default')).toBeInTheDocument()
    expect(within(row).getByText('All organizations')).toBeInTheDocument()
    expect(await within(row).findByText('3 accounts · 2 organizations')).toBeInTheDocument()
    const second = await rowOf('Second')
    expect(within(second).getByText('Acme Logistics')).toBeInTheDocument()
    expect(within(second).getByText('Not used yet')).toBeInTheDocument()
    // The key id is cut, the secret never appears.
    expect(within(second).getByText(/Access key AKIA…MPLE/)).toBeInTheDocument()
  })

  it('an identity accounts use cannot be removed, and says where they are', async () => {
    platform([main, keyed])
    renderWithProviders(<Page />)
    const row = await rowOf('Main')
    await within(row).findByText('3 accounts · 2 organizations')
    fireEvent.click(within(row).getByRole('button', { name: 'Remove' }))
    expect(await screen.findByText('Main is still used by 3 accounts')).toBeInTheDocument()
    expect(screen.getByText('Acme Logistics', { selector: 'td' })).toBeInTheDocument()
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).queryByRole('button', { name: 'Remove' })).toBeNull()
  })

  it('an unused identity is removed into the draft, not saved', async () => {
    platform([main, keyed])
    renderWithProviders(<Page />)
    const row = await rowOf('Second')
    await within(row).findByText('Not used yet')
    fireEvent.click(within(row).getByRole('button', { name: 'Remove' }))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove' }))
    await waitFor(() => expect(screen.queryAllByText('Second').filter((e) => e.closest('tbody tr'))).toHaveLength(0))
    expect(await rowOf('Main')).toBeInTheDocument()
  })

  it('a new access key needs its secret before it can be tested, and a test is sent as the identity', async () => {
    platform([main])
    live.testCloudIdentity.mockResolvedValue({
      provider: 'aws', arn: 'arn:aws:iam::999988887777:user/new', principal: 'arn:aws:iam::999988887777:user/new', source: 'the access key',
    })
    renderWithProviders(<Page />)
    fireEvent.click(await screen.findByRole('button', { name: 'Add identity' }))
    const drawer = await screen.findByRole('dialog')
    fireEvent.change(within(drawer).getByLabelText(/^Name/), { target: { value: 'Payments' } })
    fireEvent.click(within(drawer).getByLabelText('An access key'))
    fireEvent.change(within(drawer).getByLabelText(/Access key id/), { target: { value: 'AKIAIOSFODNN7EXAMPLE' } })
    expect(within(drawer).getByText(/Still needed: secret access key/)).toBeInTheDocument()
    expect(within(drawer).getByRole('button', { name: 'Test' })).toBeDisabled()
    fireEvent.change(within(drawer).getByLabelText(/Secret access key/), { target: { value: 'wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY' } })
    fireEvent.click(within(drawer).getByRole('button', { name: 'Test' }))
    await waitFor(() => expect(live.testCloudIdentity).toHaveBeenCalledWith('aws', expect.objectContaining({
      name: 'Payments', source: 'key', accessKeyId: 'AKIAIOSFODNN7EXAMPLE',
    })))
    expect(await within(drawer).findByText('AWS knows this identity')).toBeInTheDocument()
  })

  it('a second identity cannot also come from the host', async () => {
    platform([{ ...main, source: 'default', profile: '' }])
    renderWithProviders(<Page />)
    fireEvent.click(await screen.findByRole('button', { name: 'Add identity' }))
    const drawer = await screen.findByRole('dialog')
    expect(within(drawer).getByLabelText('From where argus-control runs')).toBeDisabled()
  })
})
