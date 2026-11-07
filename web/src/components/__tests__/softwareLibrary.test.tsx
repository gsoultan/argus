import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { SoftwareLibrary } from '~/components/SoftwareLibrary'
import { InstalledSoftware, ScriptReportLines } from '~/components/server/InstalledSoftware'
import type { ScriptFinding, ScriptVersion, SoftwareScript } from '~/lib/softwareApi'
import { theme } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Settings › Software and installing from it: writing a version with the
 * checks running as it is typed, reviewing with a reason for each warning,
 * and running a reviewed script, never on production before it has run
 * somewhere else.
 */

const api = vi.hoisted(() => ({
  scripts: vi.fn(),
  script: vi.fn(),
  scriptRules: vi.fn(),
  createScript: vi.fn(),
  saveScriptVersion: vi.fn(),
  removeScript: vi.fn(),
  checkScript: vi.fn(),
  reviewScript: vi.fn(),
  runScript: vi.fn(),
  installedSoftware: vi.fn(),
  softwareRuns: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))

vi.mock('~/lib/softwareApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/softwareApi')>()),
  ...api,
}))

const warning: ScriptFinding = {
  level: 'warning', rule: 'download-unverified', part: 'install', line: 2, title: 'Runs a download without checking its hash',
  why: 'A changed download would run as root.',
}

const versionOf = (over: Partial<ScriptVersion>): ScriptVersion => ({
  id: 'v1', scriptId: 's1', version: 1, install: 'curl -fsSL https://acme.example/a.deb -o /tmp/a.deb\ndpkg -i /tmp/a.deb\n',
  params: [], runAs: '', timeoutSeconds: 1800, reboot: 'never', installSha: 'b'.repeat(64), findings: [], blocked: false,
  author: 'ada@example.com', createdAt: new Date().toISOString(), reviewedBy: null, reviewedAt: null, selfReviewed: false,
  accepted: [], triedOn: null, triedAt: null, ...over,
})

const scriptOf = (v: ScriptVersion, over: Partial<SoftwareScript> = {}): SoftwareScript => ({
  id: 's1', name: 'Acme agent', platform: 'linux', description: 'The Acme monitoring agent', createdBy: v.author,
  createdAt: v.createdAt, latest: v, ...(v.reviewedBy ? { reviewed: v } : {}), versions: [v], ...over,
})

function renderWith(node: React.ReactNode, me = 'grace@example.com') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(['me'], { email: me, role: 'admin' })
  render(
    <QueryClientProvider client={qc}>
      <MantineProvider theme={theme}>{node}</MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.clearAllMocks())

describe('SoftwareLibrary', () => {
  it('shows where each script stands', async () => {
    api.scripts.mockResolvedValue([
      scriptOf(versionOf({ blocked: true })),
      scriptOf(versionOf({ id: 'v2', reviewedBy: 'grace@example.com', reviewedAt: new Date().toISOString(), triedOn: 'stage-01' }),
        { id: 's2', name: 'Vendor tool' }),
    ])
    renderWith(<SoftwareLibrary canEdit />)
    expect(await screen.findByText('v1 · blocked')).toBeTruthy()
    expect(screen.getByText('nothing reviewed')).toBeTruthy()
    expect(screen.getByText('v1 · reviewed by grace@example.com')).toBeTruthy()
  })

  it('checks a pasted script as it is written and saves it as a draft', async () => {
    api.scripts.mockResolvedValue([])
    api.checkScript.mockResolvedValue({ text: '', findings: [{ ...warning, level: 'blocked', rule: 'sh-rm-root', title: 'Deletes from /' }] })
    api.createScript.mockResolvedValue(scriptOf(versionOf({})))
    renderWith(<SoftwareLibrary canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'New script' }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByLabelText(/^Name/), { target: { value: 'Acme agent' } })
    fireEvent.change(within(dialog).getByLabelText('install script'), { target: { value: 'rm -rf /\n' } })
    await waitFor(() => expect(api.checkScript).toHaveBeenCalledWith({ platform: 'linux', part: 'install', text: 'rm -rf /\n' }),
      { timeout: 2000 })
    expect(await within(dialog).findByText('Deletes from /')).toBeTruthy()
    expect(within(dialog).getByText('1 blocked')).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save version' }))
    await waitFor(() => expect(api.createScript).toHaveBeenCalled())
    const body = api.createScript.mock.calls[0]![0]
    expect(body).toMatchObject({ name: 'Acme agent', platform: 'linux', version: { install: { text: 'rm -rf /\n' }, timeoutMinutes: 30 } })
  })

  it('needs a reason for each warning, and typed words to review your own', async () => {
    const v = versionOf({ author: 'grace@example.com', findings: [warning] })
    api.scripts.mockResolvedValue([scriptOf(v)])
    api.script.mockResolvedValue(scriptOf(v))
    api.reviewScript.mockResolvedValue(scriptOf(v))
    renderWith(<SoftwareLibrary canEdit />)
    fireEvent.click(await screen.findByText('Acme agent'))
    const approve = await screen.findByRole('button', { name: 'Approve v1' }) as HTMLButtonElement
    expect(screen.getByText(/You wrote this version/)).toBeTruthy()
    expect(approve.disabled).toBe(true)
    fireEvent.change(screen.getByPlaceholderText('Why this is acceptable'), { target: { value: 'vendor signs the repo' } })
    fireEvent.change(screen.getByLabelText('Confirm self-review'), { target: { value: 'review my own script' } })
    expect(approve.disabled).toBe(false)
    fireEvent.click(approve)
    await waitFor(() => expect(api.reviewScript).toHaveBeenCalledWith('v1', {
      accepted: [{ key: 'install:download-unverified:2', reason: 'vendor signs the repo' }], confirmSelf: 'review my own script',
    }))
  })
})

describe('installing from a script', () => {
  const reviewed = versionOf({
    reviewedBy: 'grace@example.com', reviewedAt: new Date().toISOString(),
    params: [{ name: 'VERSION', label: 'Version', kind: 'text', required: true }, { name: 'TOKEN', label: 'Enrolment token', kind: 'secret' }],
  })
  const open = async (asset: Asset) => {
    api.installedSoftware.mockResolvedValue({ state: 'ok', manager: 'apt', total: 0, manual: 0, installed: [], found: [] })
    api.softwareRuns.mockResolvedValue({ runs: [], needsApproval: false })
    api.scripts.mockResolvedValue([scriptOf(reviewed), scriptOf(versionOf({ id: 'v9' }), { id: 's9', name: 'Draft only' })])
    renderWith(<InstalledSoftware asset={asset} canEdit apps={[]} active />)
    fireEvent.click(await screen.findByRole('button', { name: /Install…/ }))
    const drawer = await screen.findByRole('dialog')
    fireEvent.click(within(drawer).getByText('Script'))
    fireEvent.click(await within(drawer).findByText('Acme agent'))
    expect(within(drawer).queryByText('Draft only')).toBeNull()
    return drawer
  }

  it('runs a reviewed version with its parameters', async () => {
    api.runScript.mockResolvedValue({})
    const drawer = await open({ id: 'a1', hostname: 'stage-01', protocol: 'ssh', tags: ['staging'] } as Asset)
    const go = within(drawer).getByRole('button', { name: 'Run install' }) as HTMLButtonElement
    expect(go.disabled).toBe(true)
    fireEvent.change(within(drawer).getByLabelText(/^Version/), { target: { value: '2.1.0' } })
    fireEvent.change(within(drawer).getByLabelText(/^Enrolment token/), { target: { value: 's3cret' } })
    fireEvent.click(go)
    await waitFor(() => expect(api.runScript).toHaveBeenCalledWith('a1',
      { versionId: 'v1', action: 'install', params: { VERSION: '2.1.0', TOKEN: 's3cret' } }))
  })

  it('will not run an untried version on production', async () => {
    const drawer = await open({ id: 'a2', hostname: 'db-01', protocol: 'ssh', tags: ['Production'] } as Asset)
    expect(within(drawer).getByText(/db-01 is production/)).toBeTruthy()
    fireEvent.change(within(drawer).getByLabelText(/^Version/), { target: { value: '2.1.0' } })
    expect((within(drawer).getByRole('button', { name: 'Run install' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('says what a script changed', () => {
    renderWith(<ScriptReportLines report={{ exit: 0, services_added: ['acme-agent.service'], ports_opened: [9100],
      folders_added: ['/opt/acme'], checked: true, installed: true, check_version: '2.1.0' }} />)
    expect(screen.getByText('New services: acme-agent.service')).toBeTruthy()
    expect(screen.getByText('Now listening on 9100')).toBeTruthy()
    expect(screen.getByText('Check: installed 2.1.0')).toBeTruthy()
  })
})
