import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { MakeItForMe, WatchForRoles } from '~/components/cloud/ConnectRoles'
import { IdentityPicker } from '~/components/cloud/IdentityPicker'
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
    expect(api.checkCloudAccount).toHaveBeenCalledWith('412788931204', false, 'main')
  }, 10_000)

  it('says why a stack failed instead of waiting forever', async () => {
    api.checkCloudAccount.mockResolvedValue({ ready: false, stack: { status: 'ROLLBACK_COMPLETE', reason: 'not authorized to perform iam:CreateRole', done: false, failed: true } })
    const onReady = vi.fn()
    renderWithProviders(<WatchForRoles prepared={prepared} manage={false} connecting={false} failed={null} onReady={onReady} />)
    expect(await screen.findByText(/stopped at ROLLBACK_COMPLETE/)).toBeInTheDocument()
    expect(screen.getByText(/iam:CreateRole/)).toBeInTheDocument()
    expect(onReady).not.toHaveBeenCalled()
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
    expect(screen.getByRole('button', { name: /Copy the policy it needs/ })).toBeInTheDocument()
  })
})
