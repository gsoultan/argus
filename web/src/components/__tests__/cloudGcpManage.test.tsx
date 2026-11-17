import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { TagsChange } from '~/components/cloud/ChangeModal'
import { GcpSetup } from '~/components/cloud/GcpSetup'
import { GCP_NAME, allows, allowsTags, gcpDevicePath, labelProblem, type CloudItem, type CloudPrepared } from '~/lib/cloud'
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

  it('labels only the kinds the account lists', () => {
    const gcp = { actions: ['tags'], tagKinds: ['instance', 'volume'] }
    expect(allowsTags(gcp, 'volume')).toBe(true)
    expect(allowsTags(gcp, 'snapshot')).toBe(false)
    expect(allowsTags({ actions: ['start'] }, 'instance')).toBe(false)
    expect(allowsTags({}, 'firewall')).toBe(true)
  })

  it('knows what Google takes as a disk or snapshot name, and where the disk shows up', () => {
    for (const ok of ['logs', 'etl-data-1', 'a']) expect(GCP_NAME.test(ok)).toBe(true)
    for (const bad of ['Logs', '1logs', 'logs-', 'etl data', 'a'.repeat(64)]) expect(GCP_NAME.test(bad)).toBe(false)
    expect(gcpDevicePath('logs')).toBe('/dev/disk/by-id/google-logs')
  })

  it('knows what Google takes as a label', () => {
    expect(labelProblem('team', 'payments')).toBeUndefined()
    expect(labelProblem('argusschedule', 'office-hours')).toBeUndefined()
    expect(labelProblem('Team', 'payments')).toMatch(/Lowercase/)
    expect(labelProblem('goog-x', 'y')).toMatch(/Google keeps/)
    expect(labelProblem('team', 'Payments')).toMatch(/Lowercase/)
  })

  it('says a label must be lowercase as it is typed, and will not save it', () => {
    const it: CloudItem = {
      accountId: 'a1', accountName: 'web-gcp', provider: 'gcp', environment: 'development',
      resource: { kind: 'instance', id: '101', name: 'web-1', region: 'us-central1', state: 'running', tags: { team: 'payments' } },
    } as unknown as CloudItem
    renderWithProviders(<TagsChange it={it} env="development" gcp onClose={() => {}} />)
    expect(screen.getByText('Labels on web-1')).toBeInTheDocument()
    fireEvent.change(screen.getAllByLabelText('Label key')[0]!, { target: { value: 'Team' } })
    expect(screen.getByText(/starting with a letter/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Save/ })).toBeDisabled()
    fireEvent.change(screen.getAllByLabelText('Label key')[0]!, { target: { value: 'owner' } })
    expect(screen.getByRole('button', { name: 'Save 2 changes' })).toBeEnabled()
  })
})
