import { useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, Checkbox, Drawer, Group, Loader, Menu, Modal, ScrollArea, SegmentedControl, Select, Skeleton,
  Stack, Switch, Table, Text, TextInput, UnstyledButton,
} from '@mantine/core'
import { useDebouncedValue, useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconArrowDown, IconArrowUp, IconCheck, IconDots, IconMinus, IconPlus, IconSearch, IconX } from '@tabler/icons-react'
import { EmptyState } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import { ScriptInstall } from '~/components/server/ScriptInstall'
import { RunState } from '~/components/server/UpdatesTab'
import type { PackageRun, ScriptReport } from '~/lib/live'
import { run as notifyRun } from '~/lib/notify'
import { runFinished } from '~/lib/packageApi'
import { useDecidePackageRun } from '~/lib/packageQueries'
import { meQuery } from '~/lib/queries'
import {
  sizeText, softwareRunText,
  type FoundPackage, type InstalledPackage, type SoftwareChange, type SoftwarePreview,
} from '~/lib/softwareApi'
import {
  installedSoftwareQuery, searchSoftwareQuery, softwareRunsQuery, usePreviewSoftware, useRefreshSoftware,
  useStartSoftwareRun, useUndoSoftwareRun,
} from '~/lib/softwareQueries'
import { FS, SP } from '~/theme'
import type { Asset, ServerApp } from '~/types/domain'

/**
 * The Installed segment of What runs here: every package someone asked for
 * on this server (libraries behind a switch), installing more from the
 * repositories the server already uses, and removing what is there.
 *
 * Nothing is changed without a preview the server's own package manager
 * simulated just now: the exact packages added, changed and removed. The run
 * is simulated again before it starts and stops if the answer changed. The
 * agent, the SSH server Argus reaches the server through, the running kernel
 * and packages the system cannot boot without are never removable here.
 */
export function InstalledSoftware({ asset, canEdit, apps, active }: {
  asset: Asset
  canEdit: boolean
  /** What runs here, to say what is running and listening. */
  apps: ServerApp[]
  /** Only asked of the agent while the segment is open. */
  active: boolean
}) {
  const [all, setAll] = useState(false)
  const [filter, setFilter] = useState('')
  const [installing, setInstalling] = useState(false)
  const [preview, setPreview] = useState<SoftwarePreview | null>(null)
  const { data, isLoading, error } = useQuery(installedSoftwareQuery(asset.id, all, active))
  const { data: runs } = useQuery(softwareRunsQuery(asset.id, active))
  const askPreview = usePreviewSoftware(asset.id)
  const busy = (runs?.runs ?? []).some((r) => !runFinished(r.state))

  const running = useMemo(() => runningIndex(apps), [apps])
  const rows = useMemo(() => {
    const f = filter.trim().toLowerCase()
    return (data?.installed ?? []).filter((p) => !f || p.name.toLowerCase().includes(f) || (p.summary ?? '').toLowerCase().includes(f))
  }, [data, filter])
  const shown = rows.slice(0, 300)

  const remove = (p: InstalledPackage) => void notifyRun(async () => {
    setPreview(await askPreview.mutateAsync({ action: 'remove', items: [{ name: p.name }] }))
  }, { failure: `Could not preview removing ${p.name}` })
  const reinstall = (p: InstalledPackage) => void notifyRun(async () => {
    setPreview(await askPreview.mutateAsync({ action: 'install', items: [{ name: p.name, version: p.version }] }))
  }, { failure: `Could not preview reinstalling ${p.name}` })

  const header = (
    <Group justify="space-between" wrap="wrap" gap="xs" mb="xs">
      <Text size={FS.micro} c="dimmed">
        {data?.state === 'ok'
          ? `${data.manager ?? ''} · ${data.manual ?? 0} asked for, ${data.total ?? 0} with libraries`
          : 'Read from the server’s package database'}
      </Text>
      {canEdit && (
        <Button size="compact-sm" leftSection={<IconPlus size={14} />} disabled={busy}
          title={busy ? 'One change at a time on a server: wait for the one going' : undefined}
          onClick={() => setInstalling(true)}>
          Install…
        </Button>
      )}
    </Group>
  )

  let body
  if (error) {
    body = <Alert color="rose" p="xs"><Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text></Alert>
  } else if (isLoading || !data) {
    body = <Skeleton h={160} />
  } else if (data.state !== 'ok') {
    body = <EmptyState compact icon={IconX} title="The server did not answer." description={data.error ?? 'Its agent did not say why.'} />
  } else {
    body = (
      <>
        <Group gap="sm" mb="xs" wrap="wrap">
          <TextInput size="xs" placeholder="Filter installed software" value={filter} w={260}
            leftSection={<IconSearch size={12} />} onChange={(e) => setFilter(e.currentTarget.value)}
            aria-label="Filter installed software" />
          <Switch size="xs" label="Show libraries and dependencies" checked={all} onChange={(e) => setAll(e.currentTarget.checked)} />
        </Group>
        <Table.ScrollContainer minWidth={640} type="native">
          <Table verticalSpacing={6} fz="sm">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Package</Table.Th>
                <Table.Th>Version</Table.Th>
                <Table.Th visibleFrom="sm">From</Table.Th>
                <Table.Th visibleFrom="sm">Size</Table.Th>
                <Table.Th visibleFrom="sm">Installed</Table.Th>
                <Table.Th>State</Table.Th>
                {canEdit && <Table.Th w={40} />}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {shown.map((p) => (
                <Table.Tr key={p.name + (p.arch ?? '')}>
                  <Table.Td>
                    <Text size="sm" fw={600}>{p.name}</Text>
                    {p.summary && <Text size={FS.micro} c="dimmed" lineClamp={1} maw={260}>{p.summary}</Text>}
                  </Table.Td>
                  <Table.Td><Text size={FS.meta} ff="monospace" c="dimmed">{p.version}</Text></Table.Td>
                  <Table.Td visibleFrom="sm"><Text size={FS.meta} c="dimmed">{p.origin || '—'}</Text></Table.Td>
                  <Table.Td visibleFrom="sm"><Text size={FS.meta} c="dimmed">{sizeText(p.size_kb)}</Text></Table.Td>
                  <Table.Td visibleFrom="sm">
                    <Text size={FS.meta} c="dimmed" title={p.installed_at ? absTime(new Date(p.installed_at * 1000).toISOString()) : undefined}>
                      {p.installed_at ? relTime(new Date(p.installed_at * 1000).toISOString()) : '—'}
                    </Text>
                  </Table.Td>
                  <Table.Td><PackageState p={p} running={running.get(p.name)} /></Table.Td>
                  {canEdit && (
                    <Table.Td>
                      {!p.protected && (
                        <Menu position="bottom-end" withinPortal>
                          <Menu.Target>
                            <ActionIcon variant="default" size="sm" aria-label={`Actions for ${p.name}`} disabled={busy}>
                              <IconDots size={14} />
                            </ActionIcon>
                          </Menu.Target>
                          <Menu.Dropdown>
                            <Menu.Item onClick={() => remove(p)}>Remove…</Menu.Item>
                            <Menu.Item onClick={() => reinstall(p)}>Reinstall</Menu.Item>
                          </Menu.Dropdown>
                        </Menu>
                      )}
                    </Table.Td>
                  )}
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
        <Text size={FS.micro} c="dimmed" mt={SP.cozy}>
          Showing {shown.length} of {rows.length}{data.truncated ? ' (the server has more than Argus lists; filter to narrow)' : ''}
          {askPreview.isPending && <> · <Loader size={10} /> asking the server what that would change…</>}
        </Text>
      </>
    )
  }

  return (
    <Stack gap="md">
      <Box>
        {header}
        {body}
      </Box>
      <RecentChanges assetId={asset.id} runs={runs?.runs ?? []} canEdit={canEdit} onPreview={setPreview} />
      {installing && (
        <InstallDrawer asset={asset} manager={data?.manager} onClose={() => setInstalling(false)} />
      )}
      {preview && (
        <ConfirmPreview asset={asset} preview={preview} apps={apps} onClose={() => setPreview(null)} />
      )}
    </Stack>
  )
}

/** Package name → what runs from it, as What runs here found it. */
function runningIndex(apps: ServerApp[]): Map<string, ServerApp> {
  const m = new Map<string, ServerApp>()
  for (const a of apps) {
    if (a.package && !a.container) m.set(a.package, a)
  }
  return m
}

function PackageState({ p, running }: { p: InstalledPackage; running?: ServerApp }) {
  if (p.protected) {
    return (
      <Group gap={SP.tight} wrap="nowrap">
        <Badge size="xs" variant="light" color="slate">Protected</Badge>
        <Text size={FS.micro} c="dimmed" lineClamp={1}>{p.protected}</Text>
      </Group>
    )
  }
  if (running?.running) {
    return (
      <Group gap={SP.tight} wrap="nowrap">
        <Badge size="xs" variant="light" color="teal">running</Badge>
        <Text size={FS.micro} c="dimmed" ff="monospace">{running.listen.map((l) => `:${l.port}`).join(' ')}</Text>
      </Group>
    )
  }
  return <Text size={FS.micro} c="dimmed">{p.manual ? '—' : 'dependency'}</Text>
}

/* ── Install ─────────────────────────────────────────────────────────────── */

function InstallDrawer({ asset, manager, onClose }: { asset: Asset; manager?: string; onClose: () => void }) {
  const narrow = useMediaQuery('(max-width: 48em)') ?? false
  const [q, setQ] = useState('')
  const [debounced] = useDebouncedValue(q.trim(), 400)
  const { data, isFetching } = useQuery(searchSoftwareQuery(asset.id, debounced))
  const [picked, setPicked] = useState<FoundPackage | null>(null)
  const [version, setVersion] = useState<string | null>(null)
  const [start, setStart] = useState(true)
  const ask = usePreviewSoftware(asset.id)
  const [preview, setPreview] = useState<SoftwarePreview | null>(null)
  const go = useStartSoftwareRun(asset.id)
  const [source, setSource] = useState<'repo' | 'script'>('repo')

  const choose = (p: FoundPackage, v: string | null, s: boolean) => {
    setPicked(p)
    setVersion(v)
    setStart(s)
    setPreview(null)
    void notifyRun(async () => {
      setPreview(await ask.mutateAsync({ action: 'install', items: [{ name: p.name, ...(v ? { version: v } : {}) }], startServices: s }))
    }, { failure: `Could not preview installing ${p.name}` })
  }
  const found = data?.state === 'ok' ? data.found : []
  const blocked = preview?.blocked
  return (
    <Drawer opened onClose={onClose} position={narrow ? 'bottom' : 'right'} size={narrow ? '90%' : 760} padding={0}
      title={<Box><Text fw={600}>Install on {asset.hostname}</Text>
        <Text size={FS.micro} c="dimmed">{source === 'repo'
          ? `From the repositories this server already uses${manager ? ` · ${manager}` : ''}`
          : 'From the reviewed scripts in Settings › Software'}</Text></Box>}>
      <Box px="sm" pb="sm">
        <SegmentedControl size="xs" value={source} onChange={(v) => setSource(v as 'repo' | 'script')}
          data={[{ value: 'repo', label: 'Repositories' }, { value: 'script', label: 'Script' }]} />
      </Box>
      {source === 'script' ? <ScriptInstall asset={asset} onClose={onClose} /> : <>
      <Group align="stretch" gap={0} wrap={narrow ? 'wrap' : 'nowrap'} style={{ minHeight: 480 }}>
        <Box w={narrow ? '100%' : 300} style={{ borderRight: narrow ? undefined : '1px solid var(--mantine-color-default-border)' }}>
          <Box p="sm">
            <TextInput placeholder="Search packages" value={q} onChange={(e) => setQ(e.currentTarget.value)} autoFocus
              aria-label="Search packages" rightSection={isFetching ? <Loader size={12} /> : <IconSearch size={14} />} />
          </Box>
          <ScrollArea h={narrow ? 220 : 520} type="auto">
            {debounced.length < 2 ? (
              <Text size={FS.meta} c="dimmed" px="sm">Type at least two letters of a package's name.</Text>
            ) : data && data.state !== 'ok' ? (
              <Text size={FS.meta} c="rose" px="sm">{data.error}</Text>
            ) : found.length === 0 && !isFetching ? (
              <Text size={FS.meta} c="dimmed" px="sm">Nothing by that name in this server's repositories.</Text>
            ) : found.map((p) => (
              <UnstyledButton key={p.name} w="100%" px="sm" py={SP.cozy} onClick={() => choose(p, null, start)}
                bg={picked?.name === p.name ? 'var(--mantine-color-default-hover)' : undefined}
                style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
                <Group gap={SP.tight} wrap="nowrap">
                  <Text size="sm" fw={600}>{p.name}</Text>
                  {p.installed && <Badge size="xs" variant="light" color="teal">installed</Badge>}
                </Group>
                {p.summary && <Text size={FS.micro} c="dimmed" lineClamp={1}>{p.summary}</Text>}
                <Text size={FS.micro} c="dimmed" ff="monospace">{p.version}{p.origin ? ` · ${p.origin}` : ''}</Text>
              </UnstyledButton>
            ))}
            {data?.truncated && <Text size={FS.micro} c="dimmed" p="sm">More match; type more to narrow.</Text>}
          </ScrollArea>
        </Box>
        <Box p="md" style={{ flex: 1, minWidth: 0 }}>
          {!picked ? (
            <Text size={FS.body} c="dimmed">Pick a package to see exactly what installing it would change on this server.</Text>
          ) : (
            <Stack gap="sm">
              <Group gap={SP.snug}>
                <Text fw={600}>{picked.name}</Text>
                {picked.summary && <Text size={FS.micro} c="dimmed">{picked.summary}</Text>}
              </Group>
              {(picked.versions?.length ?? 0) > 1 && (
                <Select label="Version" size="xs" w={320} allowDeselect={false}
                  value={version ?? picked.versions![0]!}
                  data={picked.versions!.map((v, i) => ({ value: v, label: i === 0 ? `${v} (newest)` : v }))}
                  onChange={(v) => choose(picked, v && v !== picked.versions![0] ? v : null, start)} />
              )}
              {ask.isPending ? (
                <Group gap={SP.snug}><Loader size={14} /><Text size={FS.body} c="dimmed">Simulating on {asset.hostname}…</Text></Group>
              ) : preview ? (
                <>
                  <Changes preview={preview} hostname={asset.hostname} />
                  {!blocked && (preview.result.services?.length ?? 0) > 0 && (
                    <Switch size="sm" checked={start} onChange={(e) => choose(picked, version, e.currentTarget.checked)}
                      label={<>Start {preview.result.services!.join(', ')} now and at boot</>}
                      description="Off: installed but left stopped, the same on every distribution." />
                  )}
                  {preview.needsApproval && !blocked && <ApprovalNote hostname={asset.hostname} />}
                </>
              ) : null}
            </Stack>
          )}
        </Box>
      </Group>
      <Group justify="space-between" p="md" style={{ borderTop: '1px solid var(--mantine-color-default-border)' }} wrap="wrap">
        <Text size={FS.micro} c="dimmed">Simulated again before it runs; stops if anything differs.</Text>
        <Group gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!preview || Boolean(blocked) || ask.isPending} loading={go.isPending}
            onClick={() => preview && void notifyRun(() => go.mutateAsync(preview.id), {
              failure: 'Not installed',
              success: preview.needsApproval
                ? ['Waiting for approval', 'Another admin approves it before the server installs anything.']
                : ['Install asked for', 'The server starts at its next check-in, within 30 seconds.'],
            }).then((ok) => ok && onClose())}>
            {preview?.needsApproval ? 'Ask for approval' : 'Install'}
          </Button>
        </Group>
      </Group>
      </>}
    </Drawer>
  )
}

function ApprovalNote({ hostname }: { hostname: string }) {
  return (
    <Alert color="amber" p="xs">
      <Text size={FS.body}>{hostname}'s update rule asks for a second admin: another admin approves before it runs.</Text>
    </Alert>
  )
}

const KIND: Record<SoftwareChange['kind'], { icon: typeof IconPlus; color: string; label: string }> = {
  add: { icon: IconPlus, color: 'teal', label: 'added' },
  upgrade: { icon: IconArrowUp, color: 'azure', label: 'upgraded' },
  downgrade: { icon: IconArrowDown, color: 'amber', label: 'downgraded' },
  remove: { icon: IconMinus, color: 'rose', label: 'removed' },
}

/** What a preview would change, line by line, and why it cannot go ahead. */
export function Changes({ preview: pv, hostname }: { preview: SoftwarePreview; hostname: string }) {
  const r = pv.result
  const removes = r.changes.filter((c) => c.kind === 'remove').length
  return (
    <Stack gap={SP.cozy}>
      {pv.blocked && (
        <Alert color="rose" p="xs" title="This cannot go ahead">
          <Text size={FS.body}>{pv.blocked}</Text>
        </Alert>
      )}
      {(r.warnings ?? []).map((w) => (
        <Alert key={w} color="amber" p="xs"><Text size={FS.body}>{w}</Text></Alert>
      ))}
      {r.changes.length > 0 && (
        <>
          <Text size={FS.meta} fw={600}>What will change <Text span size={FS.micro} c="dimmed" fw={400}>· simulated on {hostname} {relTime(pv.createdAt)}</Text></Text>
          <Box style={{ border: '1px solid var(--mantine-color-default-border)', borderRadius: 6 }}>
            {r.changes.map((c) => {
              const k = KIND[c.kind]
              const Icon = k.icon
              return (
                <Group key={c.kind + c.name} justify="space-between" wrap="nowrap" px="sm" py={6}
                  style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
                  <Group gap={SP.snug} wrap="nowrap">
                    <Icon size={14} color={`var(--mantine-color-${k.color}-6)`} aria-label={k.label} />
                    <Text size="sm" fw={c.asked ? 600 : 400}>{c.name}</Text>
                  </Group>
                  <Text size={FS.micro} ff="monospace" c="dimmed">
                    {c.kind === 'add' ? c.to : c.kind === 'remove' ? c.from : `${c.from} → ${c.to}`}
                  </Text>
                </Group>
              )
            })}
          </Box>
          <Text size={FS.micro} c="dimmed">
            {[r.download_kb ? `Download ${sizeText(r.download_kb)}` : '',
              r.disk_kb ? `Disk ${r.disk_kb > 0 ? '+' : '−'}${sizeText(Math.abs(r.disk_kb))}` : '',
              pv.action === 'install' && removes === 0 ? 'Nothing removed' : ''].filter(Boolean).join(' · ')}
          </Text>
        </>
      )}
    </Stack>
  )
}

/* ── Remove, reinstall and undo: a preview to confirm ────────────────────── */

function ConfirmPreview({ asset, preview, apps, onClose }: {
  asset: Asset
  preview: SoftwarePreview
  apps: ServerApp[]
  onClose: () => void
}) {
  const [pv, setPv] = useState(preview)
  const [typed, setTyped] = useState('')
  const ask = usePreviewSoftware(asset.id)
  const go = useStartSoftwareRun(asset.id)
  const removing = pv.action === 'remove'
  const names = pv.items.map((i) => i.name).join(', ')
  const short = asset.hostname.split('.')[0] ?? asset.hostname
  const confirmed = !removing || typed.trim() === short || typed.trim() === asset.hostname
  // What runs here knows what is listening; say it before it stops.
  const live = removing ? apps.filter((a) => a.running && !a.container && a.package && pv.items.some((i) => i.name === a.package)) : []
  const title = pv.undoOf
    ? (removing ? `Remove what that installed on ${short}?` : `Put back what that removed on ${short}?`)
    : removing ? `Remove ${names} from ${short}?` : `Reinstall ${names} on ${short}?`
  const purge = (on: boolean) => void notifyRun(async () => {
    setPv(await ask.mutateAsync({ action: 'remove', items: pv.items, purge: on }))
  }, { failure: 'Could not preview that' })
  return (
    <Modal opened onClose={onClose} title={<Box><Text fw={600}>{title}</Text>
      <Text size={FS.micro} c="dimmed">Simulated on the server {relTime(pv.createdAt)}</Text></Box>} size="lg">
      <Stack gap="sm">
        {live.map((a) => (
          <Alert key={a.name} color="rose" p="xs">
            <Text size={FS.body}><b>{a.name} is running</b>{a.listen.length > 0 ? <> and listening on {a.listen.map((l) => `${l.address}:${l.port}`).join(', ')}</> : null}. It stops{a.listen.length > 0 ? ', and those ports close' : ''}.</Text>
          </Alert>
        ))}
        {ask.isPending ? <Skeleton h={120} /> : <Changes preview={pv} hostname={short} />}
        {removing && !pv.blocked && pv.result.purge_allowed && (
          <Checkbox size="sm" checked={pv.purge} disabled={ask.isPending} onChange={(e) => purge(e.currentTarget.checked)}
            label="Also delete its configuration" description="Purge: the configuration files go too, and cannot be put back." />
        )}
        {removing && !pv.blocked && (
          <TextInput label="Type the server name to confirm" placeholder={short} value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)} />
        )}
        {pv.needsApproval && !pv.blocked && <ApprovalNote hostname={short} />}
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color={removing ? 'rose' : undefined} disabled={Boolean(pv.blocked) || !confirmed || ask.isPending}
            loading={go.isPending}
            onClick={() => void notifyRun(() => go.mutateAsync(pv.id), {
              failure: removing ? 'Not removed' : 'Not installed',
              success: pv.needsApproval
                ? ['Waiting for approval', 'Another admin approves it before the server changes anything.']
                : ['Asked for', 'The server starts at its next check-in, within 30 seconds.'],
            }).then((ok) => ok && onClose())}>
            {pv.needsApproval ? (removing ? 'Ask to remove' : 'Ask for approval') : removing ? 'Remove' : 'Install'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── Recent changes ──────────────────────────────────────────────────────── */

export function RecentChanges({ assetId, runs, canEdit, onPreview }: {
  assetId: string
  runs: PackageRun[]
  canEdit: boolean
  onPreview: (p: SoftwarePreview) => void
}) {
  const refresh = useRefreshSoftware()
  // A run that just ended changed what is installed: read it again.
  const ended = runs.filter((r) => runFinished(r.state)).map((r) => r.id).join(',')
  const last = useRef<string | null>(null)
  useEffect(() => {
    if (last.current !== null && last.current !== ended) refresh()
    last.current = ended
  }, [assetId, ended, refresh])
  if (runs.length === 0) return null
  return (
    <Box>
      <Text size={FS.body} fw={600} mb={SP.cozy}>Recent changes <Text span size={FS.micro} c="dimmed" fw={400}>· all in Activity</Text></Text>
      <Table verticalSpacing={6} fz="sm">
        <Table.Tbody>
          {runs.map((r) => <ChangeRow key={r.id} run={r} canEdit={canEdit} onPreview={onPreview} />)}
        </Table.Tbody>
      </Table>
    </Box>
  )
}

function ChangeRow({ run: r, canEdit, onPreview }: { run: PackageRun; canEdit: boolean; onPreview: (p: SoftwarePreview) => void }) {
  const { data: me } = useQuery(meQuery())
  const decide = useDecidePackageRun()
  const undo = useUndoSoftwareRun()
  const refresh = useRefreshSoftware()
  const mine = me?.email?.toLowerCase() === r.requestedBy.toLowerCase()
  const act = (action: 'approve' | 'deny' | 'cancel', ok: [string, string], failure: string) =>
    void notifyRun(() => decide.mutateAsync({ id: r.id, action }).then(refresh), { failure, success: ok })
  const canTakeBack = r.state === 'awaiting_approval' || (r.state === 'queued' && !r.sentAt)
  const added = r.upgraded.some((u) => !u.from && (r.items ?? []).some((i) => i.name === u.name))
  const took = r.upgraded.some((u) => !u.to && (r.items ?? []).some((i) => i.name === u.name))
  const undoLabel = r.mode === 'install' ? (added ? 'Remove what this installed' : '') : r.mode === 'remove' && took ? 'Reinstall' : ''
  return (
    <Table.Tr>
      <Table.Td w={150}><Text size={FS.meta} title={absTime(r.requestedAt)}>{relTime(r.finishedAt ?? r.requestedAt)}</Text></Table.Td>
      <Table.Td w={150}><RunState state={r.state} /></Table.Td>
      <Table.Td>
        <Text size="sm">{r.requestedBy} {softwareRunText(r)}</Text>
        {r.message && r.state !== 'done' && <Text size={FS.micro} c={runFinished(r.state) ? 'rose' : 'dimmed'}>{r.message}</Text>}
        {(r.started?.length ?? 0) > 0 && <Text size={FS.micro} c="dimmed">Started {r.started!.join(', ')}</Text>}
        {r.scriptReport && <ScriptReportLines report={r.scriptReport} />}
      </Table.Td>
      <Table.Td ta="right">
        {canEdit && (
          <Group gap={SP.snug} justify="flex-end" wrap="nowrap">
            {r.state === 'awaiting_approval' && !mine && (
              <>
                <Button size="compact-xs" color="teal" leftSection={<IconCheck size={12} />} loading={decide.isPending}
                  onClick={() => act('approve', ['Approved', 'The server starts at its next check-in.'], 'Not approved')}>
                  Approve
                </Button>
                <Button size="compact-xs" variant="default" leftSection={<IconX size={12} />} loading={decide.isPending}
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
            {r.state === 'done' && undoLabel && (
              <Button size="compact-xs" variant="subtle" loading={undo.isPending}
                onClick={() => void notifyRun(async () => onPreview(await undo.mutateAsync(r.id)),
                  { failure: 'Could not preview undoing it' })}>
                {undoLabel}
              </Button>
            )}
          </Group>
        )}
      </Table.Td>
    </Table.Tr>
  )
}

/** What a script changed, as the server saw it before and after. */
export function ScriptReportLines({ report: s }: { report: ScriptReport }) {
  const lines: string[] = []
  const pk = s.packages ?? []
  if (pk.length) lines.push(`Packages: ${pk.map((p) => p.kind === 'remove' ? `−${p.name}` : `${p.name} ${p.to ?? ''}`.trim()).join(', ')}`)
  if (s.services_added?.length) lines.push(`New services: ${s.services_added.join(', ')}`)
  if (s.services_removed?.length) lines.push(`Services gone: ${s.services_removed.join(', ')}`)
  if (s.ports_opened?.length) lines.push(`Now listening on ${s.ports_opened.join(', ')}`)
  if (s.ports_closed?.length) lines.push(`No longer listening on ${s.ports_closed.join(', ')}`)
  if (s.folders_added?.length) lines.push(`New folders: ${s.folders_added.join(', ')}`)
  if (s.folders_removed?.length) lines.push(`Folders gone: ${s.folders_removed.join(', ')}`)
  if (s.checked) lines.push(s.installed ? `Check: installed${s.check_version ? ` ${s.check_version}` : ''}` : `Check: not installed${s.check_error ? ` (${s.check_error})` : ''}`)
  if (!lines.length) lines.push('Nothing it changed was seen.')
  return <>{lines.map((l) => <Text key={l} size={FS.micro} c="dimmed">{l}</Text>)}</>
}
