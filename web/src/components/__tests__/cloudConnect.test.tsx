import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { MakeItForMe, WatchForRoles } from '~/components/cloud/ConnectRoles'
import { IdentityPicker } from '~/components/cloud/IdentityPicker'
import { AzureSetup } from '~/components/cloud/AzureSetup'
import { GcpSetup } from '~/components/cloud/GcpSetup'
import { RoleSetup } from '~/components/cloud/RoleSetup'
import type { CloudPrepared } from '~/lib/cloud'
import { renderWithProviders } from '~/test/render'

/**
 * Connecting an AWS account: nothing is copied back. The page watches for
 * the roles and connects once they let Argus in, and offers to make them
 * only where argus-control's own identity is.
 */

const api = vi.hoisted(() => ({
  checkCloudAccount: vi.fn(),
  deployCloudAccess: vi.fn(),
}))

vi.mock('~/lib/cloud', async (orig) => ({
  ...(await orig<typeof import('~/lib/cloud')>()),
  ...api,
}))

const prepared: CloudPrepared = {
  provider: 'aws', accountId: '412788931204', externalId: 'argus-0123', principal: 'arn:aws:iam::412788931204:user/argus',
  source: 'the access key', roleName: 'ArgusRead', readRoleArn: 'arn:aws:iam::412788931204:role/ArgusRead',
  template: 'Resources: {}', command: 'aws cloudformation deploy --stack-name argus-access', canDeploy: true,
  identity: 'main', identityName: 'Main',
}

afterEach(() => vi.clearAllMocks())

describe('Connect an AWS account', () => {
  it('connects by itself once the role lets Argus in, and not before', async () => {
    api.checkCloudAccount
      .mockResolvedValueOnce({ ready: false, detail: 'AccessDenied' })
      .mockResolvedValue({ ready: true })
    const onReady = vi.fn()
    renderWithProviders(<WatchForRoles prepared={prepared} manage={false} connecting={false} failed={null} onReady={onReady} />)
    expect(await screen.findByText(/Waiting for the role in 412788931204/)).toBeInTheDocument()
    expect(onReady).not.toHaveBeenCalled()
    await waitFor(() => expect(onReady).toHaveBeenCalledTimes(1), { timeout: 8_000 })
    expect(api.checkCloudAccount).toHaveBeenCalledWith('412788931204', false, 'main', 'aws')
  }, 10_000)

  it('says why a stack failed instead of waiting forever', async () => {
    api.checkCloudAccount.mockResolvedValue({ ready: false, stack: { status: 'ROLLBACK_COMPLETE', reason: 'not authorized to perform iam:CreateRole', done: false, failed: true } })
    const onReady = vi.fn()
    renderWithProviders(<WatchForRoles prepared={prepared} manage={false} connecting={false} failed={null} onReady={onReady} />)
    expect(await screen.findByText(/stopped at ROLLBACK_COMPLETE/)).toBeInTheDocument()
    expect(screen.getByText(/iam:CreateRole/)).toBeInTheDocument()
    expect(onReady).not.toHaveBeenCalled()
  })

  it("says what AWS answers once the stack is made but the role still does not work", async () => {
    api.checkCloudAccount.mockResolvedValue({ ready: false, detail: 'The read role does not let Argus in yet: AccessDenied: not authorized to perform sts:AssumeRole',
      stack: { status: 'CREATE_COMPLETE', done: true, failed: false } })
    renderWithProviders(<WatchForRoles prepared={prepared} manage={false} connecting={false} failed={null} onReady={vi.fn()} />)
    expect(await screen.findByText(/CREATE_COMPLETE, but AWS does not let Argus use the role yet/)).toBeInTheDocument()
    expect(screen.getByText(/not authorized to perform sts:AssumeRole/)).toBeInTheDocument()
  })

  it('shows the policy the identity needs when AWS refuses the identity itself', async () => {
    api.checkCloudAccount.mockResolvedValue({ ready: false, stack: { status: 'CREATE_COMPLETE', done: true, failed: false },
      detail: 'The read role does not let Argus in yet: AccessDenied: User: arn:aws:iam::412788931204:user/argus is not authorized to perform: sts:SetSourceIdentity on resource: arn:aws:iam::412788931204:role/ArgusRead' })
    renderWithProviders(<WatchForRoles prepared={{ ...prepared, canDeploy: false }} manage={false} connecting={false} failed={null} onReady={vi.fn()} />)
    expect(await screen.findByText(/is in another account/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Copy the policy/ })).toBeInTheDocument()
    expect(screen.getByText(/"sts:SetSourceIdentity"/)).toBeInTheDocument()
  })

  it('asks to run the command again when the stack predates the identity policy', async () => {
    api.checkCloudAccount.mockResolvedValue({ ready: false, stack: { status: 'CREATE_COMPLETE', done: true, failed: false },
      detail: 'AccessDenied: User: arn:aws:iam::412788931204:user/argus is not authorized to perform: sts:SetSourceIdentity on resource: arn:aws:iam::412788931204:role/ArgusRead' })
    renderWithProviders(<WatchForRoles prepared={prepared} manage={false} connecting={false} failed={null} onReady={vi.fn()} />)
    expect(await screen.findByText('Run the command once more')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Copy the policy/ })).toBeNull()
  })

  it('shows why Argus could not make the roles itself', async () => {
    api.deployCloudAccess.mockRejectedValue(new Error("argus-control's own identity may not make the roles (AccessDenied)"))
    renderWithProviders(<MakeItForMe prepared={prepared} name="payments-prod" manage />)
    fireEvent.click(screen.getByRole('button', { name: /Make the roles for me/ }))
    expect(await screen.findByText(/may not make the roles/)).toBeInTheDocument()
    expect(api.deployCloudAccess).toHaveBeenCalledWith('412788931204', true, 'payments-prod', 'main')
  })

  it('asks for no ARN: the steps end without anything to copy back', () => {
    renderWithProviders(<RoleSetup prepared={prepared} />)
    expect(screen.getByRole('button', { name: /Copy the command/ })).toBeInTheDocument()
    expect(screen.queryByText(/ReadRoleArn/)).toBeNull()
    fireEvent.click(screen.getByText('Upload a file'))
    expect(screen.getByText(/nothing to copy back/)).toBeInTheDocument()
  })
})

describe('Which identity a new account connects as', () => {
  const bases = [
    { provider: 'aws' as const, identity: 'main', name: 'Main', default: true, principal: 'arn:aws:iam::1:user/a', source: 'a key' },
    { provider: 'aws' as const, identity: 'gov', name: 'GovCloud', default: false, error: 'no EC2 IMDS role found' },
    { provider: 'aws' as const, identity: 'pay', name: 'Payments', default: false, principal: 'arn:aws:iam::1:user/p', source: 'a key' },
  ]
  it('offers every identity, marks the default, and refuses one that does not answer', () => {
    const onChange = vi.fn()
    renderWithProviders(<IdentityPicker bases={bases} value="main" onChange={onChange} />)
    expect(screen.getByText('Default')).toBeInTheDocument()
    expect(screen.getByText('no EC2 IMDS role found')).toBeInTheDocument()
    const cards = screen.getAllByRole('radio')
    expect(cards[1]).toBeDisabled()
    fireEvent.click(screen.getByText('Payments'))
    expect(onChange).toHaveBeenCalledWith('pay')
  })
})

describe('Connect an AWS account when a stack is stuck', () => {
  it('says how to fix a stuck stack and keeps watching', async () => {
    api.checkCloudAccount.mockResolvedValue({ ready: false, stack: { status: 'ROLLBACK_FAILED', reason: 'not authorized to perform: iam:GetRole', done: false, failed: true } })
    renderWithProviders(<WatchForRoles prepared={prepared} manage={false} connecting={false} failed={null} onReady={vi.fn()} />)
    expect(await screen.findByText(/stopped at ROLLBACK_FAILED/)).toBeInTheDocument()
    expect(screen.getByText(/deletes this stack first/)).toBeInTheDocument()
  })

  it('offers the narrow policy when Argus may not make the roles', async () => {
    api.deployCloudAccess.mockRejectedValue(new Error('arn:aws:iam::412788931204:user/argus may not iam:GetRole, so Argus made nothing.'))
    renderWithProviders(<MakeItForMe prepared={{ ...prepared, deployRights: '{"Version":"2012-10-17"}' }} name="payments-prod" manage />)
    fireEvent.click(screen.getByRole('button', { name: /Make the roles for me/ }))
    expect(await screen.findByText('Argus made nothing')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Copy the policy/ })).toBeInTheDocument()
  })
})

describe('Connect a Google Cloud project', () => {
  const gcp: CloudPrepared = {
    provider: 'gcp', accountId: 'payments-prod-4182', externalId: 'argus-0123', principal: 'argus-control@ops-home.iam.gserviceaccount.com',
    source: 'the VM', roleName: 'argus-read', readRoleArn: 'argus-read@payments-prod-4182.iam.gserviceaccount.com',
    template: '', command: 'gcloud iam service-accounts create argus-read', canDeploy: false, identity: 'main', identityName: 'Main',
  }

  it('watches for argus-read in the project and connects by itself', async () => {
    api.checkCloudAccount.mockResolvedValueOnce({ ready: false, detail: 'denied' }).mockResolvedValue({ ready: true })
    const onReady = vi.fn()
    renderWithProviders(<WatchForRoles prepared={gcp} manage={false} connecting={false} failed={null} onReady={onReady} />)
    expect(await screen.findByText('Waiting for argus-read in payments-prod-4182')).toBeInTheDocument()
    expect(screen.queryByText(/denied/)).not.toBeInTheDocument()
    await waitFor(() => expect(onReady).toHaveBeenCalledTimes(1), { timeout: 8_000 })
    expect(api.checkCloudAccount).toHaveBeenCalledWith('payments-prod-4182', false, 'main', 'gcp')
  }, 10_000)

  it('gives one command and Cloud Shell on the project, with the console steps one click away', () => {
    renderWithProviders(<GcpSetup prepared={gcp} />)
    expect(screen.getByRole('button', { name: /Copy the command/ })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Open Cloud Shell/ })).toHaveAttribute('href', expect.stringContaining('project=payments-prod-4182'))
    fireEvent.click(screen.getByText(/Rather click through the console/))
    expect(screen.getByText(/argus-external-id=argus-0123/)).toBeInTheDocument()
    expect(screen.getByText(gcp.principal)).toBeInTheDocument()
  })
})

describe('Connect an Azure subscription', () => {
  const azure: CloudPrepared = {
    provider: 'azure', accountId: '6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90', externalId: 'argus-0123',
    principal: '3f8a1c52-7b0e-4d19-a6c4-91d2e0b57f36', source: 'the app registration', roleName: 'Reader',
    readRoleArn: '/subscriptions/6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90/providers/Microsoft.Authorization/roleDefinitions/acdd72a7-3385-48ef-bd42-f606fba81ae7',
    template: '', command: 'az rest --method put', canDeploy: false, identity: 'main', identityName: 'Main',
  }

  it('watches for the Reader role in the subscription and connects by itself', async () => {
    api.checkCloudAccount.mockResolvedValueOnce({ ready: false, detail: 'no Reader role' }).mockResolvedValue({ ready: true })
    const onReady = vi.fn()
    renderWithProviders(<WatchForRoles prepared={azure} manage={false} connecting={false} failed={null} onReady={onReady} />)
    expect(await screen.findByText(`Waiting for the Reader role in ${azure.accountId}`)).toBeInTheDocument()
    // A refusal before the command can have run is only the expected "not yet".
    expect(screen.queryByText(/no Reader role/)).not.toBeInTheDocument()
    await waitFor(() => expect(onReady).toHaveBeenCalledTimes(1), { timeout: 8_000 })
    expect(api.checkCloudAccount).toHaveBeenCalledWith(azure.accountId, false, 'main', 'azure')
  }, 10_000)

  it('gives one command and Cloud Shell, with the portal steps one click away', () => {
    renderWithProviders(<AzureSetup prepared={azure} />)
    expect(screen.getByRole('button', { name: /Copy the command/ })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Open Cloud Shell/ })).toHaveAttribute('href', expect.stringContaining('shell.azure.com'))
    fireEvent.click(screen.getByText(/Rather click through the portal/))
    expect(screen.getByText(/argus-external-id=argus-0123/)).toBeInTheDocument()
    expect(screen.getByText(azure.principal)).toBeInTheDocument()
    expect(screen.queryByText(/cannot be made in the portal/)).not.toBeInTheDocument()
  })

  it('says the role for changes is made by the command, not in the portal', async () => {
    const both = { ...azure, manageRoleName: 'Argus manage', manageRoleArn: azure.readRoleArn.replace(/[^/]+$/, '5a1d0c3e-7b2f-4e6a-9c8d-1f2e3d4c5b6a') }
    renderWithProviders(<AzureSetup prepared={both} />)
    fireEvent.click(screen.getByText(/Rather click through the portal/))
    expect(screen.getByText(/cannot be made in the portal/)).toBeInTheDocument()
    renderWithProviders(<WatchForRoles prepared={both} manage connecting={false} failed={null} onReady={vi.fn()} />)
    expect(await screen.findByText(`Waiting for the roles in ${azure.accountId}`)).toBeInTheDocument()
  })
})
