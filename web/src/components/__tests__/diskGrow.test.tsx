import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  DisksTab, fsPercent, growable, percentAfter, plannedCommands, roomSources, sortedFilesystems, stuck,
} from '~/components/server/DisksTab'
import { DiskGrowSettings } from '~/components/DiskGrowSettings'
import type { DiskFS, DiskGrowPolicy, DiskRun, DisksView } from '~/lib/diskApi'
import { theme } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Growing disks on a server's page and in Settings: each filesystem's stack
 * and answer, what a grow runs, approvals, and who may press.
 */

const live = vi.hoisted(() => ({
  assetDisks: vi.fn(),
  scanDisks: vi.fn(),
  startDiskGrow: vi.fn(),
  decideDiskRun: vi.fn(),
  diskRun: vi.fn(),
  diskPolicy: vi.fn(),
  saveDiskPolicy: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))

vi.mock('~/lib/diskApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/diskApi')>()),
  ...live,
}))

const asset = { id: 'a1', hostname: 'db-02.impack.local', protocol: 'ssh' } as Asset
const GB = 1024 ** 3

const root: DiskFS = {
  key: 'uuid:root', mount: '/', fs: 'ext4', total: 99 * GB, used: 92 * GB, avail: 7 * GB, answer: 'grow', room: 100 * GB, lvm: true,
  layers: [
    { kind: 'disk', name: 'sda', detail: 'SCSI', size: 200 * GB, room: 100 * GB },
    { kind: 'partition', name: 'sda3', size: 99 * GB, room: 100 * GB },
    { kind: 'pv', name: 'sda3', size: 99 * GB },
    { kind: 'vg', name: 'ubuntu-vg', size: 99 * GB, used: 99 * GB },
    { kind: 'lv', name: 'ubuntu-lv', size: 99 * GB },
    { kind: 'fs', name: 'ext4', size: 99 * GB, used: 92 * GB },
  ],
  path: {
    disk: 'sda', disk_size: 200 * GB, part: 'sda3', part_num: 3, part_size: 99 * GB, part_room: 100 * GB, table: 'gpt',
    pv: '/dev/sda3', vg: 'ubuntu-vg', lv: '/dev/ubuntu-vg/ubuntu-lv', fs_device: 'dm-0', fs_type: 'ext4', fs_size: 99 * GB, mount: '/',
  },
}
const boot: DiskFS = {
  key: 'uuid:boot', mount: '/boot', fs: 'ext4', total: GB, used: 0.3 * GB, avail: 0.7 * GB, answer: 'full',
  why: 'sda3 comes right after it.', layers: [{ kind: 'fs', name: 'ext4', size: GB, used: 0.3 * GB }],
}
const efi: DiskFS = {
  key: 'uuid:efi', mount: '/boot/efi', fs: 'vfat', total: GB, used: 0.95 * GB, avail: 0.05 * GB, answer: 'blocked',
  why: 'vfat cannot grow while mounted.', layers: [],
}

const view = (over: Partial<DisksView> = {}): DisksView => ({
  hostname: 'db-02.impack.local', enrolled: true, reported: true, supported: true, reason: '', scanning: false,
  scanAsked: false, rule: { position: 0, tag: '', needsApproval: false, autoPercent: 0 }, runs: [],
  report: {
    scanned_at: new Date().toISOString(), os: 'linux', supported: true, hypervisor: 'VMware',
    filesystems: [boot, root], disks: [{ name: 'sda', size: 200 * GB, bus: 'scsi' }],
  },
  ...over,
})

const runOf = (over: Partial<DiskRun>): DiskRun => ({
  id: 'r1', hostname: 'db-02', assetHostname: 'db-02.impack.local', trigger: 'manual', state: 'queued', key: 'uuid:root',
  mount: '/', path: root.path!, growBytes: 0, plannedBytes: 100 * GB, summary: 'grow / (ext4) by 100 GB', reason: '',
  requestedBy: 'ada@example.com', requestedAt: new Date().toISOString(), decidedBy: null, decidedAt: null, sentAt: null,
  startedAt: null, finishedAt: null, updatedAt: new Date().toISOString(), step: '', message: '', steps: [], tableBefore: '',
  sizeBefore: 99 * GB, sizeAfter: 0, ...over,
})

function renderWith(node: React.ReactNode, email = 'grace@example.com') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(['me'], { email, role: 'admin' })
  render(
    <QueryClientProvider client={qc}>
      <MantineProvider theme={theme}>{node}</MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.clearAllMocks())

describe('helpers', () => {
  it('counts as df does, fullest first', () => {
    expect(fsPercent(root)).toBe(93)
    expect(percentAfter(root, 100 * GB)).toBe(47)
    expect(sortedFilesystems([boot, root, efi]).map((f) => f.mount)).toEqual(['/boot/efi', '/', '/boot'])
  })
  it('adds up the room and says when a filesystem is stuck', () => {
    expect(growable(view())).toBe(100 * GB)
    expect(stuck(view())).toBe(false)
    const v = view()
    v.report!.filesystems = [efi]
    expect(stuck(v)).toBe(true)
  })
  it('names where the space comes from', () => {
    expect(roomSources(root.path!)).toEqual(['100 GB unallocated after sda3'])
  })
  it('shows the commands the agent runs, in its order', () => {
    expect(plannedCommands('linux', root.path!, 0).filter((l) => !l.startsWith('#'))).toEqual([
      'sfdisk --dump /dev/sda', 'growpart /dev/sda 3', 'lvm pvresize /dev/sda3',
      'lvm lvextend -l +100%FREE /dev/ubuntu-vg/ubuntu-lv', 'resize2fs /dev/ubuntu-vg/ubuntu-lv',
    ])
    const vg = { fs_type: 'xfs', fs_size: GB, mount: '/data', vg: 'data', vg_free: 40 * GB, lv: '/dev/data/pg', pv: '/dev/sdb' }
    expect(plannedCommands('linux', vg, 10 * GB).filter((l) => !l.startsWith('#'))).toEqual([
      `lvm lvextend -L +${10 * GB}b /dev/data/pg`, 'xfs_growfs /data',
    ])
    expect(plannedCommands('windows', { fs_type: 'NTFS', fs_size: GB, mount: 'C:', disk_number: 0, partition_number: 3 }, 0)
      .some((l) => l.startsWith('Resize-Partition -DiskNumber 0 -PartitionNumber 3'))).toBe(true)
  })
})

describe('DisksTab', () => {
  it('draws each filesystem with its answer', async () => {
    live.assetDisks.mockResolvedValue(view())
    renderWith(<DisksTab asset={asset} canEdit />)
    expect(await screen.findByText('Can grow now by 100 GB')).toBeTruthy()
    expect(screen.getByText("Can't grow here")).toBeTruthy()
    expect(screen.getByText('sda3 comes right after it.')).toBeTruthy()
    expect(screen.getByText('93% used')).toBeTruthy()
    expect(screen.getByText(/VMware/)).toBeTruthy()
  })

  it('shows what runs before it grows, and sends only the key', async () => {
    live.assetDisks.mockResolvedValue(view())
    live.startDiskGrow.mockResolvedValue(runOf({}))
    renderWith(<DisksTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Grow by 100 GB…' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/Grow \/ on db-02/)).toBeTruthy()
    expect(within(dialog).getByTestId('grow-commands').textContent).toContain('growpart /dev/sda 3')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Grow by 100 GB' }))
    await waitFor(() => expect(live.startDiskGrow).toHaveBeenCalledWith('a1', { key: 'uuid:root', bytes: 0 }))
  })

  it('asks for approval where the rule says so', async () => {
    live.assetDisks.mockResolvedValue(view({ rule: { position: 0, tag: 'production', needsApproval: true, autoPercent: 0 } }))
    live.startDiskGrow.mockResolvedValue(runOf({ state: 'awaiting_approval' }))
    renderWith(<DisksTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Grow by 100 GB…' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/second admin/)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Ask for approval' }))
    await waitFor(() => expect(live.startDiskGrow).toHaveBeenCalled())
  })

  it('asks the disks for their size when looking for more space', async () => {
    live.assetDisks.mockResolvedValue(view())
    live.scanDisks.mockResolvedValue({ scanId: 's1' })
    renderWith(<DisksTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Look for more space' }))
    await waitFor(() => expect(live.scanDisks).toHaveBeenCalledWith('a1', true))
  })

  it('offers nothing to grow to someone who may not', async () => {
    live.assetDisks.mockResolvedValue(view())
    renderWith(<DisksTab asset={asset} canEdit={false} />)
    expect(await screen.findByText('Can grow now by 100 GB')).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Grow/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Scan now' })).toBeNull()
  })

  it('says why disks cannot be grown', async () => {
    live.assetDisks.mockResolvedValue(view({ supported: false, reason: 'the agent does not run as root', report: null }))
    renderWith(<DisksTab asset={asset} canEdit />)
    expect(await screen.findByText(/cannot be grown on this server from the console: the agent does not run as root/)).toBeTruthy()
  })

  it('shows a grow going step by step, and lets another admin approve one', async () => {
    live.assetDisks.mockResolvedValue(view({ runs: [runOf({ state: 'awaiting_approval' })] }))
    live.decideDiskRun.mockResolvedValue(runOf({ state: 'queued' }))
    renderWith(<DisksTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(live.decideDiskRun).toHaveBeenCalledWith('r1', 'approve'))
  })

  it('does not let the one who asked approve', async () => {
    live.assetDisks.mockResolvedValue(view({ runs: [runOf({ state: 'awaiting_approval' })] }))
    renderWith(<DisksTab asset={asset} canEdit />, 'ada@example.com')
    expect(await screen.findByRole('button', { name: 'Take back' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull()
  })

  it('shows each step of a grow as it runs', async () => {
    live.assetDisks.mockResolvedValue(view({
      runs: [runOf({
        state: 'running', step: 'Grow sda3',
        steps: [{ title: 'Check the layout', state: 'done' }, { title: 'Grow sda3', command: 'growpart /dev/sda 3', state: 'running' }],
      })],
    }))
    renderWith(<DisksTab asset={asset} canEdit />)
    expect(await screen.findByText('growpart /dev/sda 3')).toBeTruthy()
    expect(screen.getByText('Check the layout')).toBeTruthy()
  })
})

describe('DiskGrowSettings', () => {
  const policy: DiskGrowPolicy = {
    rules: [
      { id: 'p', position: 0, tag: 'production', needsApproval: true, autoPercent: 0 },
      { id: 'd', position: 1, tag: '', needsApproval: false, autoPercent: 0 },
    ],
  }
  it('saves growing on its own for every other server', async () => {
    live.diskPolicy.mockResolvedValue(policy)
    live.saveDiskPolicy.mockResolvedValue(policy)
    renderWith(<DiskGrowSettings canEdit />)
    const toggles = await screen.findAllByLabelText(/Grow on its own/)
    fireEvent.click(toggles[0]!)
    fireEvent.click(await screen.findByRole('button', { name: 'Save' }))
    await waitFor(() => expect(live.saveDiskPolicy).toHaveBeenCalledWith({
      rules: [
        { position: 0, tag: 'production', needsApproval: true, autoPercent: 0, id: undefined },
        { position: 1, tag: '', needsApproval: false, autoPercent: 90, id: undefined },
      ],
    }))
  })
})
