import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Code, Group, Loader, Modal, NumberInput, Progress, ScrollArea, Skeleton, Stack,
  Table, Text, ThemeIcon, useMatches,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconCheck, IconDatabase, IconHourglass, IconMinus, IconRefresh, IconSearch, IconX,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import { bytes } from '~/components/server/SystemCard'
import { diskRunFinished, type DiskFS, type DiskLayer, type DiskRun, type DisksView, type GrowPath } from '~/lib/diskApi'
import { assetDisksQuery, diskRunQuery, useDecideDiskRun, useScanDisks, useStartDiskGrow } from '~/lib/diskQueries'
import { downloadText } from '~/lib/download'
import { isConfigured } from '~/lib/live'
import { run as notifyRun } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import { FS, INK, SP } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * A server's disks: each filesystem drawn as its whole stack, from the disk
 * up through the partition, encryption and LVM to the filesystem, with the
 * room each layer could grow into, one answer per filesystem, and growing it
 * online.
 *
 * The agent reads sizes and layout only (the partition table, /sys, LVM's
 * report, the filesystem's superblock), never files. A grow names the
 * filesystem by the key the scan gave it, and the server checks every layer
 * again just before it writes, so a scan that is out of date stops the grow
 * rather than growing the wrong thing.
 */
export function DisksTab({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data: v, isLoading, error } = useQuery(assetDisksQuery(asset.id))

  if (!isConfigured()) {
    return (
      <Card padding="md">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          A server&apos;s disks are read by its agent, through a control plane. Connect this console to Argus to see them.
        </Text>
      </Card>
    )
  }
  if (error) {
    return (
      <Alert color="rose" title="Disks could not be read">
        <Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text>
      </Alert>
    )
  }
  if (isLoading || !v) return <Skeleton h={240} />

  const blocked = whyNot(v)
  const active = v.runs.find((r) => !diskRunFinished(r.state))
  const fss = v.report ? sortedFilesystems(v.report.filesystems) : []

  return (
    <Stack gap="md">
      {blocked ? (
        <Card padding="md">
          <Text size={FS.body} c="dimmed" lh={1.5}>{blocked}</Text>
        </Card>
      ) : (
        <>
          {active && <ActiveRun run={active} canEdit={canEdit} />}
          <Header view={v} assetId={asset.id} canEdit={canEdit} count={fss.length} />
          {fss.map((f) => (
            <FilesystemCard key={f.key} fs={f} view={v} asset={asset} canEdit={canEdit} busy={Boolean(active)} />
          ))}
        </>
      )}
      <RecentRuns runs={v.runs} />
    </Stack>
  )
}

function whyNot(v: DisksView): string | null {
  if (!v.enrolled) return 'No agent is enrolled on this server. Install one from Servers, Add a server, and its disks appear here.'
  if (!v.reported) {
    return "This server's agent is too old to report its disks. Update the agent from the Settings tab, and they appear here within a minute."
  }
  if (!v.supported) return `Disks cannot be grown on this server from the console: ${v.reason || 'its agent did not say why'}.`
  return null
}

/* ── Sizes ───────────────────────────────────────────────────────────────── */

/** How full a filesystem is, the way df counts it. */
export function fsPercent(f: Pick<DiskFS, 'used' | 'avail'>): number {
  const usable = f.used + f.avail
  return usable > 0 ? Math.ceil((f.used / usable) * 100) : 0
}

/** How full it would be after growing by room. */
export function percentAfter(f: Pick<DiskFS, 'used' | 'avail'>, room: number): number {
  const usable = f.used + f.avail + room
  return usable > 0 ? Math.ceil((f.used / usable) * 100) : 0
}

/** Fullest first. */
export function sortedFilesystems(list: DiskFS[]): DiskFS[] {
  return [...list].sort((a, b) => fsPercent(b) - fsPercent(a))
}

/** What every filesystem that can grow could take, in all: the tab's badge. */
export function growable(v: DisksView): number {
  return (v.report?.filesystems ?? []).filter((f) => f.answer === 'grow').reduce((n, f) => n + (f.room ?? 0), 0)
}

/** Whether a filesystem is past 85% with nowhere to grow. */
export function stuck(v: DisksView): boolean {
  return (v.report?.filesystems ?? []).some((f) => f.answer !== 'grow' && fsPercent(f) >= 85)
}

/** Where a grow's space comes from, in words. */
export function roomSources(p: GrowPath): string[] {
  const out: string[] = []
  if ((p.part_room ?? 0) > 0) out.push(`${bytes(p.part_room!)} unallocated after ${p.part}`)
  if ((p.crypt_room ?? 0) > 0) out.push(`${bytes(p.crypt_room!)} in ${p.part || 'the partition'} the encrypted volume does not use yet`)
  if ((p.pv_room ?? 0) > 0) out.push(`${bytes(p.pv_room!)} the LVM physical volume ${p.pv} does not use yet`)
  if ((p.vg_free ?? 0) > 0) out.push(`${bytes(p.vg_free!)} free in volume group ${p.vg}`)
  if ((p.fs_room ?? 0) > 0) out.push(`${bytes(p.fs_room!)} the volume has that the filesystem does not use yet`)
  if ((p.size_max ?? 0) > 0 && out.length === 0) out.push(`${bytes(p.size_max! - (p.part_size ?? 0))} free after the partition`)
  return out
}

/**
 * What the server runs for a grow, as root, in order. The agent decides the
 * commands itself from the layout it finds; this is the same choice, shown
 * before asking. Where growpart is not installed it uses sfdisk and partx.
 */
export function plannedCommands(os: string, p: GrowPath, lvBytes: number): string[] {
  if (os === 'windows') {
    const sel = `-DiskNumber ${p.disk_number ?? 0} -PartitionNumber ${p.partition_number ?? 0}`
    return [
      `# save the partition table`, `Get-Partition -DiskNumber ${p.disk_number ?? 0}`,
      `# grow the partition and NTFS into the free space`, `Resize-Partition ${sel} -Size (Get-PartitionSupportedSize ${sel}).SizeMax`,
    ]
  }
  const out: string[] = ['# check the layout still matches what you see here']
  let below = false
  if (p.disk && p.part) out.push('# keep the partition table with the grow', `sfdisk --dump /dev/${p.disk}`)
  if ((p.part_room ?? 0) > 0) {
    out.push('# grow the partition into the free space', `growpart /dev/${p.disk} ${p.part_num}`)
    below = true
  }
  if (p.crypt && (below || (p.crypt_room ?? 0) > 0)) {
    out.push('# grow the encrypted volume', `cryptsetup resize ${p.crypt}`)
    below = true
  }
  if (p.pv && (below || (p.pv_room ?? 0) > 0)) {
    out.push('# grow LVM', `lvm pvresize ${p.pv}`)
    below = true
  }
  if (p.lv && (below || (p.vg_free ?? 0) > 0)) {
    if (!p.pv || !(below || (p.pv_room ?? 0) > 0)) out.push('# grow LVM')
    out.push(lvBytes > 0 ? `lvm lvextend -L +${lvBytes}b ${p.lv}` : `lvm lvextend -l +100%FREE ${p.lv}`)
  }
  out.push('# grow the filesystem')
  switch (p.fs_type) {
    case 'xfs':
      out.push(`xfs_growfs ${p.mount}`)
      break
    case 'btrfs':
      out.push(`btrfs filesystem resize max ${p.mount}`)
      break
    default:
      out.push(`resize2fs ${p.lv || (p.fs_device ? `/dev/${p.fs_device}` : '')}`.trim())
  }
  return out
}

/* ── The scan ────────────────────────────────────────────────────────────── */

function Header({ view: v, assetId, canEdit, count }: { view: DisksView; assetId: string; canEdit: boolean; count: number }) {
  const scan = useScanDisks(assetId)
  const r = v.report
  const looking = v.scanning || v.scanAsked
  const ask = (rescan: boolean) => void notifyRun(() => scan.mutateAsync(rescan), {
    failure: 'Could not ask the server to scan',
    success: rescan
      ? ['Looking for more space', 'The agent asks each disk for its size and scans at its next check-in.']
      : ['Scanning', 'The agent reads its disks at its next check-in.'],
  })
  const buttons = canEdit && (
    <Group gap="sm" wrap="nowrap">
      <Button size="compact-sm" variant="default" leftSection={looking ? <Loader size={12} /> : <IconRefresh size={14} />}
        disabled={looking} onClick={() => ask(false)}>
        {looking ? 'Scanning…' : 'Scan now'}
      </Button>
      <Button size="compact-sm" variant="default" leftSection={<IconSearch size={14} />} disabled={looking}
        title="For a disk made bigger in the hypervisor or the cloud: ask each disk for its size again, then scan"
        onClick={() => ask(true)}>
        Look for more space
      </Button>
    </Group>
  )
  if (!r) {
    return (
      <Card padding="md">
        <Group justify="space-between" wrap="wrap" gap="sm">
          <Text size={FS.body} c="dimmed">
            {looking ? 'Reading the disks on this server…' : 'The agent has not read its disks yet. It does every six hours.'}
          </Text>
          {buttons}
        </Group>
      </Card>
    )
  }
  return (
    <Stack gap={SP.cozy}>
      <Group justify="space-between" wrap="wrap" gap="sm">
        <Group gap="sm" wrap="wrap">
          <Text fw={600}>{count} filesystem{count === 1 ? '' : 's'}</Text>
          <Text size={FS.meta} c="dimmed" title={absTime(r.scanned_at)}>
            fullest first · read {relTime(r.scanned_at)}{r.hypervisor ? ` · ${r.hypervisor}` : ''}
          </Text>
        </Group>
        {buttons}
      </Group>
      {r.error && (
        <Text size={FS.meta} c={INK.denied}>The last scan failed: {r.error}. What is shown is from the scan before.</Text>
      )}
      <Legend />
    </Stack>
  )
}

const HATCH = 'repeating-linear-gradient(135deg, var(--mantine-color-teal-2) 0 4px, var(--mantine-color-teal-1) 4px 8px)'

function Legend() {
  const swatch = (bg: string, border?: string) => (
    <Box w={10} h={10} style={{ background: bg, borderRadius: 2, border: border ? `1px dashed ${border}` : undefined }} />
  )
  return (
    <Group gap="md" wrap="wrap">
      {[
        [swatch('var(--mantine-color-slate-7)'), 'Used'],
        [swatch('var(--mantine-color-violet-5)'), 'LVM volume'],
        [swatch(HATCH, 'var(--mantine-color-teal-6)'), 'Room to grow into'],
      ].map(([s, l]) => (
        <Group key={String(l)} gap={SP.snug} wrap="nowrap">{s}<Text size={FS.micro} c="dimmed">{l}</Text></Group>
      ))}
    </Group>
  )
}

/* ── One filesystem ──────────────────────────────────────────────────────── */

const KIND_LABEL: Record<DiskLayer['kind'], string> = {
  disk: 'Disk', partition: 'Partition', crypt: 'Encrypted', pv: 'LVM PV', vg: 'LVM', lv: 'Volume', fs: 'Filesystem',
}

function FilesystemCard({ fs: f, view: v, asset, canEdit, busy }: {
  fs: DiskFS; view: DisksView; asset: Asset; canEdit: boolean; busy: boolean
}) {
  const pct = fsPercent(f)
  const tone = pct >= 95 ? 'rose' : pct >= 85 ? 'amber' : 'azure'
  const scale = Math.max(1, ...f.layers.map((l) => l.size + (l.room ?? 0)))
  const onLvm = f.lvm ? ' on LVM' : ''
  const wide = useMatches({ base: false, md: true })
  const divider = '1px solid var(--mantine-color-default-border)'
  return (
    <Card padding={0} withBorder>
      <Group px="md" py="sm" gap="sm" wrap="wrap" style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
        <Text fw={700} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{f.mount}</Text>
        <Text size={FS.meta} c="dimmed">{f.fs}{onLvm}</Text>
        <Badge size="sm" variant="light" color={tone === 'azure' ? 'slate' : tone}>{pct}% used</Badge>
        <Progress value={pct} color={tone} w={140} size="sm" aria-label={`${f.mount} is ${pct}% used`} />
        <Text size={FS.meta} c="dimmed">{bytes(f.used)} of {bytes(f.used + f.avail)}</Text>
      </Group>
      <Group align="stretch" gap={0} wrap="wrap">
        <Box p="md" style={{ flex: '2 1 420px', minWidth: 0 }}>
          <Stack gap={SP.cozy}>
            {f.layers.map((l, i) => <LayerRow key={`${l.kind}-${l.name}-${i}`} layer={l} scale={scale} />)}
          </Stack>
        </Box>
        <Box p="md" style={{ flex: '1 1 260px', ...(wide ? { borderLeft: divider } : { borderTop: divider }) }}>
          <Answer fs={f} view={v} asset={asset} canEdit={canEdit} busy={busy} />
        </Box>
      </Group>
    </Card>
  )
}

function LayerRow({ layer: l, scale }: { layer: DiskLayer; scale: number }) {
  const w = (n: number) => `${Math.max(0, Math.min(100, (n / scale) * 100))}%`
  const room = l.room ?? 0
  const fill = l.kind === 'lv' || l.kind === 'vg' ? 'var(--mantine-color-violet-5)' : 'var(--mantine-color-slate-6)'
  const right = l.kind === 'fs' ? `${bytes(Math.max(0, l.size - (l.used ?? 0)))} free`
    : l.kind === 'vg' ? (room > 0 ? `${bytes(room)} free` : 'none free')
      : room > 0 ? `+${bytes(room)}` : bytes(l.size)
  return (
    <Group gap="sm" wrap="nowrap" align="center">
      <Text size={FS.micro} c="dimmed" tt="uppercase" w={{ base: 64, sm: 78 }} style={{ flexShrink: 0, letterSpacing: 0.4 }}>{KIND_LABEL[l.kind]}</Text>
      <Text size={FS.meta} ff="monospace" w={{ base: 96, sm: 150 }} truncate="end" title={`${l.name}${l.detail ? ` · ${l.detail}` : ''}`}
        style={{ flexShrink: 0 }}>
        {l.name}{l.detail ? <Text span size={FS.micro} c="dimmed" ff="inherit"> · {l.detail}</Text> : null}
      </Text>
      <Box style={{ flex: 1, minWidth: 40, height: 16, borderRadius: 3, background: 'var(--mantine-color-default-hover)', display: 'flex', overflow: 'hidden' }}
        aria-label={`${KIND_LABEL[l.kind]} ${l.name}: ${bytes(l.size)}${room > 0 ? `, ${bytes(room)} to grow into` : ''}`}>
        {l.kind === 'fs' ? (
          <>
            <Box style={{ width: w(l.used ?? 0), background: 'var(--mantine-color-slate-7)' }} />
            <Box style={{ width: w(Math.max(0, l.size - (l.used ?? 0))), background: 'var(--mantine-color-slate-3)' }} />
          </>
        ) : (
          <Box style={{ width: w(l.kind === 'vg' ? (l.used ?? l.size) : l.size), background: fill }} />
        )}
        {room > 0 && <Box style={{ width: w(room), background: HATCH, outline: '1px dashed var(--mantine-color-teal-6)', outlineOffset: -1 }} />}
      </Box>
      <Text size={FS.meta} c={room > 0 ? INK.verified : 'dimmed'} w={{ base: 64, sm: 80 }} ta="right" style={{ flexShrink: 0 }}>{right}</Text>
    </Group>
  )
}

function Answer({ fs: f, view: v, asset, canEdit, busy }: {
  fs: DiskFS; view: DisksView; asset: Asset; canEdit: boolean; busy: boolean
}) {
  const [open, setOpen] = useState(false)
  const room = f.room ?? 0
  if (f.answer === 'grow' && f.path) {
    const sources = roomSources(f.path)
    // Room that reaches the volume group from below goes to this volume
    // unless someone gives it less: the usual press is all of it.
    const fromBelow = (f.path.part_room ?? 0) + (f.path.crypt_room ?? 0) + (f.path.pv_room ?? 0) + (f.path.fs_room ?? 0) > 0
    return (
      <Stack gap={SP.cozy}>
        <Group gap={SP.snug} wrap="nowrap">
          <ThemeIcon size={20} radius="xl" variant="light" color="teal"><IconCheck size={13} /></ThemeIcon>
          <Text fw={600}>{f.lvm && !fromBelow ? `Can grow by up to ${bytes(room)}` : `Can grow now by ${bytes(room)}`}</Text>
        </Group>
        <Text size={FS.body} c="dimmed" lh={1.5}>
          {sources.length > 0 ? `${capitalize(sources.join(', and '))}.` : ''} Growing is online: nothing is unmounted or restarted.
        </Text>
        <Text size={FS.meta}>After: <b>{f.mount}</b> at {percentAfter(f, room)}% · {bytes(f.avail + room)} free</Text>
        {canEdit && (
          <Group gap="sm">
            <Button size="sm" variant={f.lvm && !fromBelow ? 'default' : 'filled'} disabled={busy}
              title={busy ? 'Another grow on this server is going or waiting' : undefined} onClick={() => setOpen(true)}>
              {f.lvm && !fromBelow ? 'Grow…' : `Grow by ${bytes(room)}…`}
            </Button>
          </Group>
        )}
        {open && <GrowDialog fs={f} view={v} asset={asset} onClose={() => setOpen(false)} />}
      </Stack>
    )
  }
  const full = f.answer === 'full'
  return (
    <Stack gap={SP.cozy}>
      <Group gap={SP.snug} wrap="nowrap">
        <ThemeIcon size={20} radius="xl" variant="light" color={full ? 'slate' : 'amber'}>
          {full ? <IconMinus size={13} /> : <IconAlertTriangle size={13} />}
        </ThemeIcon>
        <Text fw={600}>{full ? "Can't grow here" : 'Something is in the way'}</Text>
      </Group>
      {f.why && <Text size={FS.body} c="dimmed" lh={1.5}>{f.why}</Text>}
      {full && fsPercent(f) >= 85 && (
        <Text size={FS.meta} c="dimmed" lh={1.5}>
          {f.rescan
            ? 'If you made this disk bigger in the hypervisor or the cloud, press Look for more space.'
            : 'Make the disk bigger where it lives, then press Look for more space; or free space from the Logs tab.'}
        </Text>
      )}
    </Stack>
  )
}

const capitalize = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)

/* ── Growing ─────────────────────────────────────────────────────────────── */

const GiB = 1024 ** 3

function GrowDialog({ fs: f, view: v, asset, onClose }: { fs: DiskFS; view: DisksView; asset: Asset; onClose: () => void }) {
  const start = useStartDiskGrow(asset.id)
  const room = f.room ?? 0
  const p = f.path!
  const fsRoom = p.fs_room ?? 0
  const lvRoom = Math.max(0, room - fsRoom)
  const partial = Boolean(f.lvm && p.lv && lvRoom >= GiB)
  const maxGb = Math.floor(lvRoom / GiB)
  const [gb, setGb] = useState<number>(maxGb)
  const lvBytes = partial && gb > 0 && gb < maxGb ? gb * GiB : 0
  const adds = lvBytes > 0 ? lvBytes + fsRoom : room
  const short = asset.hostname.split('.')[0] || asset.hostname
  const commands = plannedCommands(v.report?.os ?? 'linux', p, lvBytes)
  const needs = v.rule.needsApproval
  return (
    <Modal opened onClose={onClose} size="lg" title={<Text fw={700}>Grow {f.mount} on {short}</Text>}>
      <Stack gap="md">
        <Text size={FS.body} c="dimmed">Online. Nothing is unmounted, nothing restarts, nothing is removed.</Text>
        <Table withRowBorders={false} striped={false} highlightOnHover={false} verticalSpacing={2} fz="sm">
          <Table.Tbody>
            <Table.Tr>
              <Table.Td w={150} c="dimmed">Filesystem</Table.Td>
              <Table.Td><Text span ff="monospace" fw={600}>{f.fs} {f.mount}</Text> · {bytes(p.fs_size)} → <b>{bytes(p.fs_size + adds)}</b></Table.Td>
            </Table.Tr>
            <Table.Tr>
              <Table.Td c="dimmed">Space comes from</Table.Td>
              <Table.Td>{roomSources(p).join('; ') || '—'}</Table.Td>
            </Table.Tr>
            <Table.Tr>
              <Table.Td c="dimmed">After</Table.Td>
              <Table.Td>{fsPercent(f)}% → <Text span c={INK.verified} fw={600}>{percentAfter(f, adds)}% used</Text></Table.Td>
            </Table.Tr>
          </Table.Tbody>
        </Table>
        {partial && (
          <NumberInput label={`Give ${p.lv?.split('/').pop()} (GB)`} w={220} min={1} max={maxGb} value={gb}
            onChange={(n) => setGb(Math.max(1, Math.min(maxGb, Math.floor(Number(n) || 1))))}
            description={gb >= maxGb ? `all ${bytes(lvRoom)}` : `${bytes(lvRoom - gb * GiB)} stays free in ${p.vg}`} />
        )}
        <Stack gap={SP.snug}>
          <Text size={FS.body} fw={600}>What runs, as root, in this order</Text>
          <Code block style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }} data-testid="grow-commands">
            {commands.join('\n')}
          </Code>
        </Stack>
        {needs && (
          <Alert color="amber" p="xs" icon={<IconHourglass size={16} />}>
            <Text size={FS.body}>This server&apos;s rule asks for a second admin: the grow waits until another admin approves it.</Text>
          </Alert>
        )}
        <Group justify="space-between" gap="sm" wrap="wrap">
          <Text size={FS.micro} c="dimmed">If any layer changed since the scan, it stops before writing.</Text>
          <Group gap="sm">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button loading={start.isPending} onClick={() => void notifyRun(
              () => start.mutateAsync({ key: f.key, bytes: lvBytes }),
              {
                failure: 'Grow not started',
                success: needs
                  ? ['Waiting for approval', 'Another admin approves the grow before it starts.']
                  : ['Grow asked for', 'The server starts at its next check-in, within 30 seconds.'],
              },
            ).then((done) => done && onClose())}>
              {needs ? 'Ask for approval' : `Grow by ${bytes(adds)}`}
            </Button>
          </Group>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── A grow going ────────────────────────────────────────────────────────── */

const STATE: Record<DiskRun['state'], { label: string; color: string }> = {
  awaiting_approval: { label: 'Waiting for approval', color: 'amber' },
  queued: { label: 'Asked', color: 'azure' },
  running: { label: 'Growing', color: 'azure' },
  done: { label: 'Done', color: 'teal' },
  failed: { label: 'Failed', color: 'rose' },
  interrupted: { label: 'Interrupted', color: 'rose' },
  cancelled: { label: 'Taken back', color: 'slate' },
  denied: { label: 'Denied', color: 'slate' },
}

export function DiskRunState({ state }: { state: DiskRun['state'] }) {
  const s = STATE[state]
  return <Badge size="sm" variant="light" color={s.color}>{s.label}</Badge>
}

function who(r: DiskRun): string {
  return r.trigger === 'automatic' ? `Automatic${r.reason ? ` · ${r.reason}` : ''}` : r.requestedBy
}

function ActiveRun({ run: r, canEdit }: { run: DiskRun; canEdit: boolean }) {
  return (
    <SectionCard title="Grow" icon={IconDatabase} badge={<DiskRunState state={r.state} />}
      description={`${who(r)} · ${r.summary} · asked ${relTime(r.requestedAt)}`}
      action={<RunActions run={r} canEdit={canEdit} />}>
      <Stack gap={SP.cozy}>
        <Group gap={SP.cozy} wrap="nowrap">
          {r.state === 'running' && <Loader size={14} />}
          <Text size={FS.body} c="dimmed" lh={1.5}>
            {r.state === 'awaiting_approval' ? 'Another admin approves this grow before the server is asked to start it.'
              : r.state === 'queued' ? 'Asked. The server starts at its next check-in, within 30 seconds.'
                : r.message || (r.step ? `${r.step}…` : 'Growing. Each step only adds space.')}
          </Text>
        </Group>
        {r.steps.length > 0 && <Steps steps={r.steps} />}
      </Stack>
    </SectionCard>
  )
}

function Steps({ steps }: { steps: DiskRun['steps'] }) {
  return (
    <Stack gap={SP.snug}>
      {steps.map((s, i) => (
        <Box key={`${s.title}-${i}`}>
          <Group gap={SP.snug} wrap="nowrap">
            <ThemeIcon size={16} radius="xl" variant="light"
              color={s.state === 'done' ? 'teal' : s.state === 'failed' ? 'rose' : 'azure'}>
              {s.state === 'done' ? <IconCheck size={10} /> : s.state === 'failed' ? <IconX size={10} /> : <Loader size={8} />}
            </ThemeIcon>
            <Text size={FS.body} fw={600}>{s.title}</Text>
          </Group>
          {s.command && <Text size={FS.meta} ff="monospace" ml={22} style={{ overflowWrap: 'anywhere' }}>{s.command}</Text>}
          {s.output && (
            <Text size={FS.micro} c={s.state === 'failed' ? INK.denied : 'dimmed'} ff="monospace" ml={22}
              style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
              {s.output}
            </Text>
          )}
        </Box>
      ))}
    </Stack>
  )
}

function RunActions({ run: r, canEdit }: { run: DiskRun; canEdit: boolean }) {
  const { data: me } = useQuery(meQuery())
  const decide = useDecideDiskRun()
  if (!canEdit) return null
  const act = (action: 'approve' | 'deny' | 'cancel', ok: [string, string], failure: string) =>
    void notifyRun(() => decide.mutateAsync({ id: r.id, action }), { failure, success: ok })
  const mine = me?.email?.toLowerCase() === r.requestedBy.toLowerCase()
  const canTakeBack = r.state === 'awaiting_approval' || (r.state === 'queued' && !r.sentAt)
  return (
    <Group gap={SP.snug} wrap="nowrap">
      {r.state === 'awaiting_approval' && !mine && (
        <>
          <Button size="compact-xs" color="teal" leftSection={<IconCheck size={12} />} loading={decide.isPending}
            onClick={() => act('approve', ['Approved', 'The server starts at its next check-in.'], 'Not approved')}>
            Approve
          </Button>
          <Button size="compact-xs" variant="default" color="rose" leftSection={<IconX size={12} />} loading={decide.isPending}
            onClick={() => act('deny', ['Denied', 'Nothing is changed.'], 'Not denied')}>
            Deny
          </Button>
        </>
      )}
      {canTakeBack && (
        <Button size="compact-xs" variant="default" loading={decide.isPending}
          onClick={() => act('cancel', ['Taken back', 'Nothing was changed.'], 'Not taken back')}>
          Take back
        </Button>
      )}
    </Group>
  )
}

/* ── Past grows ──────────────────────────────────────────────────────────── */

function RecentRuns({ runs }: { runs: DiskRun[] }) {
  const [open, setOpen] = useState<string | null>(null)
  const done = runs.filter((r) => diskRunFinished(r.state))
  if (done.length === 0) return null
  return (
    <SectionCard title="Recent changes" flush description="Each with its commands and the partition table before, also in Activity.">
      <ScrollArea type="auto">
        <Table verticalSpacing={6} fz="sm" miw={560}>
          <Table.Tbody>
            {done.map((r) => (
              <RunRow key={r.id} run={r} open={open === r.id} onToggle={() => setOpen(open === r.id ? null : r.id)} />
            ))}
          </Table.Tbody>
        </Table>
      </ScrollArea>
    </SectionCard>
  )
}

function RunRow({ run: r, open, onToggle }: { run: DiskRun; open: boolean; onToggle: () => void }) {
  const outcome = r.sizeAfter > 0 ? `${bytes(r.sizeBefore)} → ${bytes(r.sizeAfter)}` : ''
  const steps = r.steps.filter((s) => s.command).length
  return (
    <>
      <Table.Tr style={{ cursor: 'pointer' }} onClick={onToggle} aria-expanded={open}>
        <Table.Td w={150}><Text size={FS.meta} title={absTime(r.requestedAt)}>{relTime(r.finishedAt ?? r.requestedAt)}</Text></Table.Td>
        <Table.Td w={140}><DiskRunState state={r.state} /></Table.Td>
        <Table.Td><Text size="sm">{who(r)} · {r.summary}{r.decidedBy && r.state !== 'denied' ? ` · approved by ${r.decidedBy}` : ''}</Text></Table.Td>
        <Table.Td ta="right"><Text size={FS.meta} c="dimmed">{[outcome, steps > 0 ? `${steps} step${steps === 1 ? '' : 's'}` : ''].filter(Boolean).join(' · ')}</Text></Table.Td>
      </Table.Tr>
      {open && (
        <Table.Tr>
          <Table.Td colSpan={4}>
            <RunDetail run={r} />
          </Table.Td>
        </Table.Tr>
      )}
    </>
  )
}

function RunDetail({ run: r }: { run: DiskRun }) {
  const { data: full, isLoading } = useQuery(diskRunQuery(r.id))
  const x = full ?? r
  return (
    <Stack gap={SP.cozy}>
      <Group justify="space-between" wrap="wrap" gap="xs">
        <Text size={FS.meta} c="dimmed">
          {x.trigger === 'automatic' ? 'Started by its rule' : `Asked by ${x.requestedBy}`}
          {x.decidedBy ? ` · ${x.state === 'denied' ? 'denied' : 'approved'} by ${x.decidedBy}` : ''}
          {x.finishedAt ? ` · finished ${absTime(x.finishedAt)}` : ''}
        </Text>
        {x.tableBefore && (
          <Button size="compact-xs" variant="default"
            onClick={() => downloadText(x.tableBefore, `partition-table-${x.assetHostname || x.hostname}-${x.id}.txt`)}>
            Partition table before
          </Button>
        )}
      </Group>
      {x.message && <Text size={FS.body} c={x.state === 'done' ? undefined : INK.denied}>{x.message}</Text>}
      {isLoading ? <Skeleton h={60} /> : x.steps.length > 0 && <Steps steps={x.steps} />}
    </Stack>
  )
}
