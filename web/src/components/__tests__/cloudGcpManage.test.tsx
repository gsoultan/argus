import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { GcpSetup } from '~/components/cloud/GcpSetup'
import { allows, type CloudPrepared } from '~/lib/cloud'
import { renderWithProviders } from '~/test/render'

/**
 * Changes in a Google Cloud project: argus-manage is made by the same
 * command, and the console offers only the changes Argus has built for
 * Google so far.
 */

const prepared: CloudPrepared = {
  provider: 'gcp', accountId: 'payments-prod-4182', externalId: 'argus-0123', principal: 'argus-control@ops-home.iam.gserviceaccount.com',
  source: 'the VM', roleName: 'argus-read', readRoleArn: 'argus-read@payments-prod-4182.iam.gserviceaccount.com',
  manageRoleName: 'argus-manage', manageRoleArn: 'argus-manage@payments-prod-4182.iam.gserviceaccount.com',
  template: '', command: 'P=payments-prod-4182 M=argus-manage@payments-prod-4182.iam.gserviceaccount.com', canDeploy: false,
  identity: 'main', identityName: 'Main',
}

describe('Changes in a Google Cloud project', () => {
  it('only offers what the account lists, and everything when it lists nothing', () => {
    const gcp = { actions: ['start', 'stop', 'reboot', 'volume-grow'] }
    expect(allows(gcp, 'volume-grow')).toBe(true)
    expect(allows(gcp, 'volume-delete')).toBe(false)
    expect(allows(gcp, 'launch')).toBe(false)
    expect(allows({}, 'launch')).toBe(true)
  })

  it('shows how to make argus-manage by hand, with its own description', () => {
    renderWithProviders(<GcpSetup prepared={prepared} />)
    expect(screen.getByText(/argus-read and argus-manage work/)).toBeInTheDocument()
    fireEvent.click(screen.getByText(/Rather click through the console/))
    expect(screen.getByText(/Argus makes the Compute Engine changes people ask for in Argus through this account. argus-external-id=argus-0123/)).toBeInTheDocument()
    expect(screen.getByText('Compute Instance Admin (v1)')).toBeInTheDocument()
  })

  it('says nothing of argus-manage when the project stays read only', () => {
    renderWithProviders(<GcpSetup prepared={{ ...prepared, manageRoleName: undefined, manageRoleArn: undefined, command: 'P=payments-prod-4182' }} />)
    expect(screen.getByText(/Argus connects as soon as argus-read works/)).toBeInTheDocument()
    fireEvent.click(screen.getByText(/Rather click through the console/))
    expect(screen.queryByText(/argus-manage/)).toBeNull()
  })
})
