import { useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Checkbox, Code, Group, List, Loader, Modal, Radio, ScrollArea, SegmentedControl,
  SimpleGrid, Skeleton, Stack, Table, Text, TextInput, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconArrowBigUpLines, IconCheck, IconCircleDashed, IconPackage, IconPower, IconRefresh, IconX,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { absTime, AssetDialogTitle, relTime, Stat } from '~/components/primitives'
import {
  isConfigured,
  type PackageReboot, type PackageRule, type PackageRun, type PackageUpdate, type PackagesView, type ReleaseInfo,
} from '~/lib/live'
import { run as notifyRun } from '~/lib/notify'
import {
  assetPackagesQuery, packageRunQuery, useCheckPackages, useDecidePackageRun, useStartPackageRun,
} from '~/lib/packageQueries'
import { meQuery } from '~/lib/queries'
import { runFinished } from '~/lib/packageApi'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'
import { assetName } from '~/lib/assetType'

/**
 * A server's operating system updates: what its package manager says can be
 * upgraded, which of it fixes a security issue, and installing it.
 *
 * The agent looks every six hours (or now, when asked) and reports here. An
 * install runs on the server in a job of its own, outside the agent, so it
 * goes on if the agent restarts or the connection drops; the agent reports
 * how it went once it can. A server that restarts half way has its package
 * manager repaired and the run marked interrupted.
 */
export function UpdatesTab({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const linux = asset.protocol !== 'rdp'
  const q = assetPackagesQuery(asset.id)
  const { data: v, isLoading, error } = useQuery({ ...q, enabled: q.enabled !== false && linux })

  if (!isConfigured()) {
    return (
      <Card padding="md">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          Operating system updates come from the server's agent, through a control plane. Connect this
          console to Argus to see them.
        </Text>
      </Card>
    )
  }
  if (!linux) {
    return (
      <Card padding="md">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          Updates from the console are for Linux servers. This one is reached over remote desktop.
        </Text>
      </Card>
    )
  }
  if (error) {
    return (
      <Alert color="rose" title="Updates could not be read">
        <Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text>
      </Alert>
    )
  }
  if (isLoading || !v) return <Skeleton h={240} />

  const blocked = whyNot(v)
  const active = v.runs.find((r) => !runFinished(r.state))

  return (
    <Stack gap="md">
      {blocked ? (
        <Card padding="md">
          <Text size={FS.body} c="dimmed" lh={1.5}>{blocked}</Text>
        </Card>
      ) : (
        <>
          {active && <ActiveRun run={active} canEdit={canEdit} />}
          <Summary view={v} />
          {v.report?.reboot_required && active?.mode !== 'reboot' && active?.mode !== 'release_upgrade' && (
            <RebootNeeded view={v} assetId={asset.id} name={assetName(asset)} canEdit={canEdit} busy={Boolean(active)} />
          )}
          {v.report?.release && active?.mode !== 'release_upgrade' && (
            <ReleaseUpgrade view={v} release={v.report.release} assetId={asset.id} name={assetName(asset)} canEdit={canEdit} busy={Boolean(active)} />
          )}
          <Packages view={v} assetId={asset.id} name={assetName(asset)} canEdit={canEdit} busy={Boolean(active)} />
        </>
      )}
      {v.runs.length > 0 && <RecentRuns runs={v.runs} canEdit={canEdit} />}
    </Stack>
  )
}

function whyNot(v: PackagesView): string | null {
  if (!v.enrolled) return 'No agent is enrolled on this server. Install one from Servers, Add a server, and its updates appear here.'
  if (!v.reported) {
    return "This server's agent is too old to report operating system updates. Update the agent from the Settings tab, and they appear here within a minute."
  }
  if (!v.supported) return `Updates from the console are not available on this server: ${v.reason ?? 'its agent did not say why'}.`
  return null
}

/* ── Summary ─────────────────────────────────────────────────────────────── */

const DAY = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

export function hhmm(minute: number): string {
  return `${String(Math.floor(minute / 60) % 24).padStart(2, '0')}:${String(minute % 60).padStart(2, '0')}`
}

/** A rule's window, in words: "Sun, Wed 02:00–04:00". */
export function windowText(r: Pick<PackageRule, 'windowDays' | 'windowStartMinute' | 'windowMinutes'>): string {
  const days = r.windowDays.length === 7 ? 'Daily' : r.windowDays.map((d) => DAY[d]).join(', ')
  return `${days} ${hhmm(r.windowStartMinute)}–${hhmm(r.windowStartMinute + r.windowMinutes)}`
}

export const MODE_LABEL: Record<PackageRule['mode'], string> = {
  off: 'Off',
  notify: 'Notify only',
  security: 'Security updates',
  all: 'All updates',
}

/** The window's next start, by the server's clock. */
function serverClock(iso: string, offsetMinutes: number): string {
  const d = new Date(new Date(iso).getTime() + offsetMinutes * 60_000)
  return `${DAY[d.getUTCDay()]} ${hhmm(d.getUTCHours() * 60 + d.getUTCMinutes())}`
}

function Summary({ view: v }: { view: PackagesView }) {
  const r = v.report
  const updates = r?.updates ?? []
  const security = updates.filter((u) => u.security).length || (r?.security_patches ?? 0)
  const kernel = updates.find((u) => u.reboot)
  const auto = v.rule.mode === 'security' || v.rule.mode === 'all'
  return (
    <SimpleGrid cols={{ base: 2, md: 4 }} spacing="sm">
      <Stat label="Updates waiting" value={r ? updates.length : '—'}
        sub={r ? `${r.manager ?? 'package manager'} · checked ${relTime(r.checked_at)}` : 'not looked yet'} />
      <Stat label="Security" value={r ? security : '—'} tone={security > 0 ? 'warn' : 'ok'}
        sub={security > 0 ? 'fix a known issue' : 'none waiting'} />
      <Stat label={auto ? 'Next automatic run' : 'Automatic updates'}
        value={auto ? (v.nextWindow ? serverClock(v.nextWindow, r?.utc_offset_minutes ?? 0) : '—') : MODE_LABEL[v.rule.mode]}
        sub={auto ? `${MODE_LABEL[v.rule.mode].toLowerCase()} · ${windowText(v.rule)}` : 'Settings, Updates turns them on'} />
      <Stat label="Reboot" tone={r?.reboot_required || kernel ? 'warn' : 'ok'}
        value={r?.reboot_required ? 'Needed now' : kernel ? 'Will be needed' : 'Not needed'}
        sub={r?.reboot_required ? 'an installed update waits for one' : kernel ? `${kernel.name} ${kernel.available}` : undefined} />
    </SimpleGrid>
  )
}

/* ── Reboot ──────────────────────────────────────────────────────────────── */

/**
 * The server says installed updates wait for a reboot: say so where it is
 * seen, with the button that does it.
 */
function RebootNeeded({ view: v, assetId, name, canEdit, busy }: {
  view: PackagesView
  assetId: string
  name: string
  canEdit: boolean
  busy: boolean
}) {
  const [asking, setAsking] = useState(false)
  const why = !canEdit ? 'An admin or owner can reboot it from here.'
    : !v.canReboot ? "This server's agent is too old to reboot from the console. Update the agent from the Settings tab first."
      : busy ? 'It can be rebooted once the run going now has ended.'
        : null
  return (
    <Alert color="amber" p="sm" icon={<IconPower size={18} />} title="This server needs a reboot">
      <Group justify="space-between" align="center" gap="sm" wrap="wrap">
        <Text size={FS.body} lh={1.5} style={{ flex: '1 1 260px' }}>
          Updates are installed and take effect once it restarts.{why ? ` ${why}` : ''}
        </Text>
        {canEdit && v.canReboot && (
          <Button size="sm" color="amber" leftSection={<IconPower size={16} />} disabled={busy}
            onClick={() => setAsking(true)}>
            Reboot now…
          </Button>
        )}
      </Group>
      {asking && <ConfirmReboot assetId={assetId} name={name} view={v} onClose={() => setAsking(false)} />}
    </Alert>
  )
}

function ConfirmReboot({ assetId, name, view: v, onClose }: { assetId: string; name: string; view: PackagesView; onClose: () => void }) {
  const start = useStartPackageRun(assetId)
  return (
    <Modal opened onClose={onClose} title={<AssetDialogTitle text={`Reboot ${name}?`} name={name} hostname={v.hostname} />} size="md">
      <Stack gap="sm">
        <Text size={FS.body} lh={1.5}>
          The server restarts now. Sessions to it drop, and what it serves is down until it is back, usually a minute
          or two. Nothing is installed. Argus confirms it came back on a new boot, then looks for updates again.
        </Text>
        {v.rule.needsApproval && (
          <Alert color="azure" p="xs">
            <Text size={FS.body}>This server's rule asks for a second admin: the reboot waits until another admin approves it.</Text>
          </Alert>
        )}
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" leftSection={<IconPower size={16} />} loading={start.isPending} onClick={() => void notifyRun(
            () => start.mutateAsync({ mode: 'reboot', reboot: 'if_needed' }),
            {
              failure: 'Reboot not started',
              success: v.rule.needsApproval
                ? ['Waiting for approval', 'Another admin approves the reboot before it starts.']
                : ['Reboot asked for', 'The server reboots at its next check-in, within 30 seconds.'],
            },
          ).then((ok) => ok && onClose())}>
            Reboot
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── A new release ───────────────────────────────────────────────────────── */

const RELEASE_TOOL: Record<string, string> = {
  'do-release-upgrade': "Ubuntu's own do-release-upgrade, without removing anything the new release no longer ships",
  apt: "Debian's documented way: apt is pointed at the new release, then upgrade and full-upgrade",
  leapp: 'leapp, after its preupgrade check finds nothing that stops it; the upgrade itself runs during the reboot',
}

/**
 * The next major release of the server's operating system: what it is, what
 * stands in the way, and the button that moves the server to it. Only for a
 * release the agent says the server offers, and only when the server is
 * ready: the reasons it is not are the agent's own, each something to fix.
 */
function ReleaseUpgrade({ view: v, release: rel, assetId, name, canEdit, busy }: {
  view: PackagesView
  release: ReleaseInfo
  assetId: string
  name: string
  canEdit: boolean
  busy: boolean
}) {
  const [asking, setAsking] = useState(false)
  const [installing, setInstalling] = useState(false)
  // A distribution Argus does not upgrade says nothing here; one it does,
  // without its tool or with upgrades turned off, says what to change.
  if (!rel.next) {
    if (!rel.reason || !rel.tool) return null
    return (
      <Card padding="md">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          <Text span inherit fw={600}>Release upgrades.</Text> {rel.current}: {rel.reason}.
        </Text>
      </Card>
    )
  }
  const blockers = rel.blockers ?? []
  // What stands in the way and can be cleared from here: the updates waiting
  // for the current release. Held ones too, or the blocker never clears.
  const waiting = (v.report?.updates ?? []).map((u) => u.name)
  const anyHeld = waiting.some((n) => heldBy(n, v.holds))
  const why = !canEdit ? 'An admin or owner can upgrade it from here.'
    : !v.canUpgradeRelease ? "This server's agent is too old to upgrade the operating system. Update the agent from the Settings tab first."
      : busy ? 'It can be upgraded once the run going now has ended.'
        : null
  return (
    <SectionCard title="New release available" icon={IconArrowBigUpLines}
      badge={<Badge size="sm" variant="light" color={blockers.length ? 'amber' : 'azure'}>{rel.next_name}</Badge>}
      description={`${rel.current} can move to ${rel.next_name}.`}
      action={canEdit && v.canUpgradeRelease ? (
        <Button size="compact-sm" leftSection={<IconArrowBigUpLines size={14} />}
          disabled={busy || blockers.length > 0} onClick={() => setAsking(true)}>
          Upgrade to {rel.next_name}…
        </Button>
      ) : undefined}>
      <Stack gap={SP.cozy}>
        {blockers.length > 0 ? (
          <Alert color="amber" p="xs" title="Not ready yet">
            <Stack gap={SP.cozy}>
              <List size="sm" spacing={4}>
                {blockers.map((b) => <List.Item key={b}>{b[0]?.toUpperCase() + b.slice(1)}.</List.Item>)}
              </List>
              {canEdit && waiting.length > 0 && (
                <Group gap={SP.cozy} wrap="wrap">
                  <Button size="compact-sm" variant="white" color="amber" leftSection={<IconPackage size={14} />}
                    disabled={busy} onClick={() => setInstalling(true)}>
                    Install {waiting.length === 1 ? 'it' : `the ${waiting.length} updates`} now
                  </Button>
                  <Text size={FS.meta} c="dimmed">
                    The agent looks again once they are in, and the upgrade button turns on when nothing else is in the way.
                  </Text>
                </Group>
              )}
            </Stack>
          </Alert>
        ) : (
          <Text size={FS.body} lh={1.5}>
            Ready. The upgrade uses {RELEASE_TOOL[rel.tool ?? ''] ?? rel.tool}. The server reboots into the new
            release, and Argus calls it done only once the server says it is on {rel.next_name}.
          </Text>
        )}
        {why && <Text size={FS.meta} c="dimmed">{why}</Text>}
      </Stack>
      {asking && <ConfirmRelease assetId={assetId} name={name} view={v} release={rel} onClose={() => setAsking(false)} />}
      {installing && (
        <ConfirmRun assetId={assetId} name={name} mode={anyHeld ? 'selected' : 'all'} names={waiting} view={v}
          onClose={() => setInstalling(false)} />
      )}
    </SectionCard>
  )
}

function ConfirmRelease({ assetId, name, view: v, release: rel, onClose }: {
  assetId: string
  name: string
  view: PackagesView
  release: ReleaseInfo
  onClose: () => void
}) {
  const start = useStartPackageRun(assetId)
  const [typed, setTyped] = useState('')
  const short = v.hostname.split('.')[0] ?? v.hostname
  const confirmed = typed.trim() === short || typed.trim() === v.hostname
  return (
    <Modal opened onClose={onClose} title={<AssetDialogTitle text={`Upgrade ${name} to ${rel.next_name}?`} name={name} hostname={v.hostname} />} size="lg">
      <Stack gap="sm">
        <Text size={FS.body} lh={1.5}>
          {rel.current} becomes {rel.next_name}. Every package moves to the new release, then the server reboots into
          it. Expect it to be busy for an hour or more; what it serves may stop working part way, and sessions to it
          drop at the reboot. Once it starts it cannot be stopped or undone from here, so take a backup or snapshot
          of the server first.
        </Text>
        <List size="sm" spacing={4}>
          <List.Item>The upgrade uses {RELEASE_TOOL[rel.tool ?? ''] ?? rel.tool}.</List.Item>
          <List.Item>Configuration files changed on the server are kept, including the SSH settings that send logins through Argus.</List.Item>
          <List.Item>Holds do not apply: a new release replaces every package.</List.Item>
          <List.Item>
            After the reboot the agent checks the server is on {rel.next_name}, and installs again what it uses there
            (the SSH server, the database clients) if the upgrade took it away.
          </List.Item>
        </List>
        {v.rule.needsApproval && (
          <Alert color="azure" p="xs">
            <Text size={FS.body}>This server's rule asks for a second admin: the upgrade waits until another admin approves it.</Text>
          </Alert>
        )}
        <TextInput label="Type the server name to confirm" placeholder={short} value={typed}
          onChange={(e) => setTyped(e.currentTarget.value)} autoComplete="off" />
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" leftSection={<IconArrowBigUpLines size={16} />} disabled={!confirmed}
            loading={start.isPending} onClick={() => void notifyRun(
              () => start.mutateAsync({ mode: 'release_upgrade', release: rel.next, reboot: 'if_needed' }),
              {
                failure: 'Upgrade not started',
                success: v.rule.needsApproval
                  ? ['Waiting for approval', 'Another admin approves the upgrade before it starts.']
                  : ['Upgrade asked for', 'The server starts at its next check-in, within 30 seconds.'],
              },
            ).then((ok) => ok && onClose())}>
            Upgrade to {rel.next_name}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── The package list ────────────────────────────────────────────────────── */

/** Whether a hold pattern names a package, as the agent matches it. */
export function heldBy(name: string, holds: string[]): string | undefined {
  const base = name.split('.')[0] ?? name
  return holds.find((h) => {
    const re = new RegExp(`^${h.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.')}$`)
    return re.test(name) || re.test(base)
  })
}

type Filter = 'all' | 'security' | 'held'

function Packages({ view: v, assetId, name, canEdit, busy }: {
  view: PackagesView
  assetId: string
  name: string
  canEdit: boolean
  busy: boolean
}) {
  const check = useCheckPackages(assetId)
  const updates = useMemo(() => v.report?.updates ?? [], [v.report])
  const held = useMemo(() => new Map(updates.map((u) => [u.name, heldBy(u.name, v.holds)])), [updates, v.holds])
  // Ticked: everything waiting but the held and the newly pulled in.
  const [picked, setPicked] = useState<Set<string> | null>(null)
  const chosen = picked ?? new Set(updates.filter((u) => !held.get(u.name) && !u.new).map((u) => u.name))
  const [filter, setFilter] = useState<Filter>('all')
  const [confirm, setConfirm] = useState<null | { mode: 'all' | 'security' | 'selected'; names: string[] }>(null)

  const shown = updates.filter((u) =>
    filter === 'security' ? u.security : filter === 'held' ? Boolean(held.get(u.name)) : true)
  const security = updates.filter((u) => u.security && !held.get(u.name) && !u.new)
  const heldCount = updates.filter((u) => held.get(u.name)).length
  const looking = v.checking || v.checkAsked
  const toggle = (name: string, on: boolean) => {
    const next = new Set(chosen)
    if (on) next.add(name)
    else next.delete(name)
    setPicked(next)
  }
  const allChosen = updates.length > 0 && updates.every((u) => chosen.has(u.name))
  const chosenList = updates.filter((u) => chosen.has(u.name)).map((u) => u.name)
  // Exactly what "all" would install: then say all, so the agent installs
  // what is waiting when it gets there rather than this list.
  const everythingDefault = chosenList.length > 0 &&
    chosenList.length === updates.filter((u) => !held.get(u.name) && !u.new).length &&
    chosenList.every((n) => !held.get(n))

  const actions = (
    <Group gap={SP.cozy} wrap="wrap">
      {canEdit && v.rule.mode !== 'off' && (
        <Button size="compact-sm" variant="default" leftSection={looking ? <Loader size={12} /> : <IconRefresh size={14} />}
          disabled={looking} onClick={() => void notifyRun(() => check.mutateAsync(), {
            failure: 'Could not ask the server to look',
            success: ['Looking for updates', 'The agent refreshes its package lists at its next check-in.'],
          })}>
          {looking ? 'Looking…' : 'Check now'}
        </Button>
      )}
      {canEdit && security.length > 0 && (
        <Button size="compact-sm" variant="default" disabled={busy}
          onClick={() => setConfirm({ mode: 'security', names: security.map((u) => u.name) })}>
          Security only
        </Button>
      )}
      {canEdit && updates.length > 0 && (
        <Button size="compact-sm" disabled={busy || chosenList.length === 0}
          onClick={() => setConfirm({ mode: everythingDefault ? 'all' : 'selected', names: chosenList })}>
          Update {chosenList.length} package{chosenList.length === 1 ? '' : 's'}
        </Button>
      )}
    </Group>
  )

  return (
    <SectionCard title="Waiting to install" icon={IconPackage} flush action={actions}
      description={v.report?.error
        ? <Text span size={FS.meta} c="rose">The last look failed: {v.report.error}. This list is from the look before.</Text>
        : undefined}>
      {updates.length === 0 ? (
        <Box p="md">
          <Text size={FS.body} c="dimmed">
            {v.report ? 'Nothing to install. This server is up to date.' : looking
              ? 'Looking for updates…'
              : 'The agent has not looked for updates yet. It does every six hours while updates are on for this server.'}
          </Text>
        </Box>
      ) : (
        <>
          <Group px="md" py="xs" justify="space-between" wrap="wrap" gap="xs">
            <SegmentedControl size="xs" value={filter} onChange={(f) => setFilter(f as Filter)} data={[
              { value: 'all', label: `All ${updates.length}` },
              { value: 'security', label: `Security ${updates.filter((u) => u.security).length}` },
              { value: 'held', label: `Held ${heldCount}` },
            ]} />
            {canEdit && (
              <Text size={FS.meta} c="dimmed">{chosenList.length} of {updates.length} selected</Text>
            )}
          </Group>
          <ScrollArea type="auto">
            <Table verticalSpacing={6} fz="sm" miw={640}>
              <Table.Thead>
                <Table.Tr>
                  {canEdit && (
                    <Table.Th w={36}>
                      <Checkbox size="xs" aria-label="Select every package" checked={allChosen}
                        indeterminate={!allChosen && chosen.size > 0}
                        onChange={(e) => setPicked(e.currentTarget.checked ? new Set(updates.map((u) => u.name)) : new Set())} />
                    </Table.Th>
                  )}
                  <Table.Th>Package</Table.Th>
                  <Table.Th>Installed → available</Table.Th>
                  <Table.Th>Why</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {shown.map((u) => (
                  <PackageRow key={u.name} u={u} hold={held.get(u.name)} canEdit={canEdit}
                    checked={chosen.has(u.name)} onToggle={(on) => toggle(u.name, on)} />
                ))}
              </Table.Tbody>
            </Table>
          </ScrollArea>
        </>
      )}
      {confirm && (
        <ConfirmRun assetId={assetId} name={name} mode={confirm.mode} names={confirm.names}
          view={v} onClose={() => setConfirm(null)} />
      )}
    </SectionCard>
  )
}

function PackageRow({ u, hold, canEdit, checked, onToggle }: {
  u: PackageUpdate
  hold: string | undefined
  canEdit: boolean
  checked: boolean
  onToggle: (on: boolean) => void
}) {
  return (
    <Table.Tr style={hold ? { opacity: 0.7 } : undefined}>
      {canEdit && (
        <Table.Td>
          <Checkbox size="xs" aria-label={`Install ${u.name}`} checked={checked}
            onChange={(e) => onToggle(e.currentTarget.checked)} />
        </Table.Td>
      )}
      <Table.Td><Text size="sm" fw={600} ff="monospace">{u.name}</Text></Table.Td>
      <Table.Td>
        <Text size={FS.meta} ff="monospace" c="dimmed">
          {u.installed ? `${u.installed} → ` : 'new · '}<Text span inherit c="var(--mantine-color-text)">{u.available}</Text>
        </Text>
      </Table.Td>
      <Table.Td>
        <Group gap={SP.tight} wrap="wrap">
          {u.security && <Badge size="xs" variant="light" color="rose">Security</Badge>}
          {hold && <Badge size="xs" variant="light" color="slate">Held · {hold}</Badge>}
          {u.reboot && <Badge size="xs" variant="light" color="amber">Reboot</Badge>}
          {u.new && <Badge size="xs" variant="light" color="slate">Pulled in</Badge>}
          {u.origin && <Text size={FS.micro} c="dimmed">{u.origin}</Text>}
        </Group>
      </Table.Td>
    </Table.Tr>
  )
}

function ConfirmRun({ assetId, name, mode, names, view: v, onClose }: {
  assetId: string
  name: string
  mode: 'all' | 'security' | 'selected'
  names: string[]
  view: PackagesView
  onClose: () => void
}) {
  const start = useStartPackageRun(assetId)
  const updates = v.report?.updates ?? []
  const kernel = updates.some((u) => names.includes(u.name) && u.reboot)
  const [reboot, setReboot] = useState<PackageReboot>(v.rule.reboot)
  const what = mode === 'security' ? `${names.length} security update${names.length === 1 ? '' : 's'}`
    : mode === 'all' ? `all ${names.length} waiting update${names.length === 1 ? '' : 's'}`
      : `${names.length} package${names.length === 1 ? '' : 's'}`
  const heldPicked = mode === 'selected' ? names.filter((n) => heldBy(n, v.holds)) : []
  return (
    <Modal opened onClose={onClose} title={<AssetDialogTitle text={`Install ${what} on ${name}?`} name={name} hostname={v.hostname} />} size="md">
      <Stack gap="sm">
        <Text size={FS.body} lh={1.5}>
          The server refreshes its package lists, installs {mode === 'selected' ? 'the packages picked' : 'what is waiting then'},
          and restarts the services still using replaced files. Changed configuration files are kept as they are. The
          run goes on if the agent restarts or the connection drops, and cannot be stopped half way once it starts.
        </Text>
        {heldPicked.length > 0 && (
          <Alert color="amber" p="xs">
            <Text size={FS.body}>Picked by hand although held: {heldPicked.join(', ')}. They are installed this time.</Text>
          </Alert>
        )}
        {v.rule.needsApproval && (
          <Alert color="azure" p="xs">
            <Text size={FS.body}>This server's rule asks for a second admin: the run waits until another admin approves it.</Text>
          </Alert>
        )}
        <Radio.Group label="If an update needs a reboot" value={reboot} onChange={(x) => setReboot(x as PackageReboot)}>
          <Stack gap={SP.cozy} mt={SP.snug}>
            <Radio value="if_needed" label="Reboot at the end of the run" description={kernel ? 'A new kernel is in this run.' : undefined} />
            <Radio value="never" label="Do not reboot; tell me it is needed" />
          </Stack>
        </Radio.Group>
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={start.isPending} onClick={() => void notifyRun(
            () => start.mutateAsync({ mode, reboot, ...(mode === 'selected' ? { packages: names } : {}) }),
            {
              failure: 'Updates not started',
              success: v.rule.needsApproval
                ? ['Waiting for approval', 'Another admin approves the run before it starts.']
                : ['Updates asked for', 'The server starts at its next check-in, within 30 seconds.'],
            },
          ).then((ok) => ok && onClose())}>
            Install {what}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── A run going ─────────────────────────────────────────────────────────── */

const RELEASE_STEPS: { key: string; label: string; detail: string }[] = [
  { key: 'preflight', label: 'Check the server is ready', detail: 'and that it still offers the release approved' },
  { key: 'upgrade', label: 'Upgrade every package', detail: 'changed configuration files are kept as they are' },
  { key: 'reboot', label: 'Reboot into the new release', detail: 'Argus confirms the server is on it once it is back' },
]

const REBOOT_STEPS: { key: string; label: string; detail: string }[] = [
  { key: 'reboot', label: 'Reboot', detail: 'the server restarts; Argus confirms it came back on a new boot' },
]

const STEPS: { key: string; label: string; detail: string }[] = [
  { key: 'refresh', label: 'Refresh package lists', detail: 'and work out what to install' },
  { key: 'install', label: 'Install', detail: 'changed configuration files are kept as they are' },
  { key: 'services', label: 'Restart services', detail: 'those still using replaced files' },
  { key: 'reboot', label: 'Reboot', detail: 'only if asked, and only if an update needs it' },
]

export function runTitle(r: PackageRun): string {
  if (r.mode === 'reboot') return 'Reboot'
  if (r.mode === 'release_upgrade') return `Release upgrade · to ${r.release ?? 'the next release'}`
  if (r.mode === 'script') return `Script · ${r.scriptName ?? ''}`
  if (r.mode === 'install' || r.mode === 'remove') {
    const names = (r.items ?? []).map((i) => i.name)
    return `${r.mode === 'install' ? 'Install' : 'Remove'} · ${names.length <= 3 ? names.join(', ') : `${names.length} packages`}`
  }
  const what = r.mode === 'all' ? 'all updates' : r.mode === 'security' ? 'security updates'
    : r.packages.length <= 3 ? r.packages.join(', ') : `${r.packages.length} packages`
  return `${r.trigger === 'automatic' ? 'Automatic' : 'By hand'} · ${what}`
}

const STATE: Record<PackageRun['state'], { label: string; color: string }> = {
  awaiting_approval: { label: 'Waiting for approval', color: 'amber' },
  waiting: { label: 'Waiting its turn', color: 'slate' },
  queued: { label: 'Asked', color: 'azure' },
  running: { label: 'Running', color: 'azure' },
  rebooting: { label: 'Rebooting', color: 'azure' },
  done: { label: 'Done', color: 'teal' },
  failed: { label: 'Failed', color: 'rose' },
  interrupted: { label: 'Interrupted', color: 'rose' },
  cancelled: { label: 'Taken back', color: 'slate' },
  denied: { label: 'Denied', color: 'slate' },
}

export function RunState({ state }: { state: PackageRun['state'] }) {
  const s = STATE[state]
  return <Badge size="sm" variant="light" color={s.color}>{s.label}</Badge>
}

function ActiveRun({ run: r, canEdit }: { run: PackageRun; canEdit: boolean }) {
  const started = r.state === 'running' || r.state === 'rebooting'
  const { data: full } = useQuery(packageRunQuery(r.id, started))
  const reboot = r.mode === 'reboot'
  const release = r.mode === 'release_upgrade'
  const steps = reboot ? REBOOT_STEPS : release ? RELEASE_STEPS : STEPS
  const at = steps.findIndex((s) => s.key === (r.state === 'rebooting' || reboot ? 'reboot' : r.step))
  return (
    <SectionCard title={reboot ? 'Reboot' : release ? 'Release upgrade' : 'Update run'}
      icon={reboot ? IconPower : release ? IconArrowBigUpLines : IconPackage} badge={<RunState state={r.state} />}
      description={reboot || release
        ? `${release ? `To ${r.release ?? 'the next release'} · ` : ''}Asked by ${r.requestedBy} ${relTime(r.requestedAt)}`
        : `${runTitle(r)} · ${r.trigger === 'automatic' ? 'started by its rule' : `asked by ${r.requestedBy}`} ${relTime(r.requestedAt)}${r.reboot === 'if_needed' ? ' · reboot if needed' : ''}`}
      action={<RunActions run={r} canEdit={canEdit} />}>
      {!started ? (
        <Text size={FS.body} c="dimmed" lh={1.5}>
          {r.state === 'awaiting_approval' ? 'Another admin approves this run before the server is asked to start it.'
            : r.state === 'waiting' ? 'Servers in this batch go one at a time; this one goes when the one before it is done.'
              : reboot ? 'Asked. The server reboots at its next check-in, within 30 seconds.'
                : release ? 'Asked. The server starts the upgrade at its next check-in, within 30 seconds.'
                : 'Asked. The server starts at its next check-in, within 30 seconds.'}
        </Text>
      ) : (
        <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
          <Stack gap={SP.cozy}>
            {steps.map((s, i) => {
              const done = at > i
              const now = at === i
              const skipped = s.key === 'reboot' && r.reboot === 'never'
              return (
                <Group key={s.key} gap={SP.cozy} wrap="nowrap" align="flex-start">
                  <ThemeIcon size={18} radius="xl" variant={done ? 'filled' : 'light'} color={done ? 'teal' : now ? 'azure' : 'slate'}>
                    {done ? <IconCheck size={12} /> : now ? <Loader size={10} color="azure" /> : <IconCircleDashed size={12} />}
                  </ThemeIcon>
                  <Box>
                    <Text size={FS.body} fw={now ? 600 : 400} c={done || now ? undefined : 'dimmed'}>
                      {s.label}{s.key === 'install' && r.planned > 0 ? ` ${r.planned} package${r.planned === 1 ? '' : 's'}` : ''}
                    </Text>
                    <Text size={FS.micro} c="dimmed">{skipped ? 'not asked for this run' : now && r.message ? r.message : s.detail}</Text>
                  </Box>
                </Group>
              )
            })}
          </Stack>
          <RunLog log={full?.log} />
        </SimpleGrid>
      )}
    </SectionCard>
  )
}

function RunLog({ log }: { log: string | undefined }) {
  // Follow the newest line, as a terminal would.
  const view = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = view.current
    if (el) el.scrollTop = el.scrollHeight
  }, [log])
  return (
    <ScrollArea h={220} type="auto" viewportRef={view} style={{ borderRadius: 6 }}>
      <Code block fz={FS.micro} style={{ minHeight: 220, whiteSpace: 'pre-wrap' }}>
        {log || 'Output from the server appears here as it runs.'}
      </Code>
    </ScrollArea>
  )
}

function RunActions({ run: r, canEdit }: { run: PackageRun; canEdit: boolean }) {
  const { data: me } = useQuery(meQuery())
  const decide = useDecidePackageRun()
  if (!canEdit) return null
  const act = (action: 'approve' | 'deny' | 'cancel', ok: [string, string], failure: string) =>
    void notifyRun(() => decide.mutateAsync({ id: r.id, action }), { failure, success: ok })
  const mine = me?.email?.toLowerCase() === r.requestedBy.toLowerCase()
  const canTakeBack = r.state === 'awaiting_approval' || r.state === 'waiting' || (r.state === 'queued' && !r.sentAt)
  return (
    <Group gap={SP.snug} wrap="nowrap">
      {r.state === 'awaiting_approval' && !mine && (
        <>
          <Button size="compact-xs" color="teal" leftSection={<IconCheck size={12} />} loading={decide.isPending}
            onClick={() => act('approve', ['Approved', 'The server starts at its next check-in.'], 'Not approved')}>
            Approve
          </Button>
          <Button size="compact-xs" variant="default" color="rose" leftSection={<IconX size={12} />} loading={decide.isPending}
            onClick={() => act('deny', ['Denied', 'Nothing is installed.'], 'Not denied')}>
            Deny
          </Button>
        </>
      )}
      {canTakeBack && (
        <Button size="compact-xs" variant="default" loading={decide.isPending}
          onClick={() => act('cancel', ['Taken back', 'Nothing was installed.'], 'Not taken back')}>
          Take back
        </Button>
      )}
    </Group>
  )
}

/* ── Past runs ───────────────────────────────────────────────────────────── */

function RecentRuns({ runs, canEdit }: { runs: PackageRun[]; canEdit: boolean }) {
  const [open, setOpen] = useState<string | null>(null)
  const done = runs.filter((r) => runFinished(r.state))
  if (done.length === 0) return null
  return (
    <SectionCard title="Recent runs" flush description="Every run is also in Activity.">
      <Table verticalSpacing={6} fz="sm">
        <Table.Tbody>
          {done.map((r) => (
            <RunRow key={r.id} run={r} open={open === r.id} canEdit={canEdit}
              onToggle={() => setOpen(open === r.id ? null : r.id)} />
          ))}
        </Table.Tbody>
      </Table>
    </SectionCard>
  )
}

function RunRow({ run: r, open, onToggle }: { run: PackageRun; open: boolean; canEdit: boolean; onToggle: () => void }) {
  const { data: full } = useQuery(packageRunQuery(r.id, open))
  const outcome = r.mode === 'release_upgrade'
    ? [r.state === 'done' && r.rebooted ? `on ${r.release ?? 'the new release'}` : '', r.rebootRequired ? 'reboot needed' : '']
      .filter(Boolean).join(' · ')
    : [
      r.upgraded.length > 0 ? `${r.upgraded.length} upgraded` : r.state === 'done' ? 'nothing to install' : '',
      r.rebooted ? 'rebooted' : r.rebootRequired ? 'reboot needed' : '',
    ].filter(Boolean).join(' · ')
  return (
    <>
      <Table.Tr style={{ cursor: 'pointer' }} onClick={onToggle} aria-expanded={open}>
        <Table.Td w={150}><Text size={FS.meta} title={absTime(r.requestedAt)}>{relTime(r.finishedAt ?? r.requestedAt)}</Text></Table.Td>
        <Table.Td w={140}><RunState state={r.state} /></Table.Td>
        <Table.Td><Text size="sm">{runTitle(r)}</Text></Table.Td>
        <Table.Td><Text size={FS.meta} c="dimmed">{outcome}</Text></Table.Td>
      </Table.Tr>
      {open && (
        <Table.Tr>
          <Table.Td colSpan={4}>
            <Stack gap={SP.cozy}>
              {r.message && <Text size={FS.body} c={r.state === 'done' ? undefined : 'rose'}>{r.message}</Text>}
              {r.upgraded.length > 0 && (
                <Text size={FS.meta} ff="monospace" c="dimmed" style={{ whiteSpace: 'pre-line' }}>
                  {r.upgraded.map((u) => `${u.name} ${u.from ?? ''} → ${u.to}`).join('\n')}
                </Text>
              )}
              {r.restarted.length > 0 && <Text size={FS.meta} c="dimmed">Restarted {r.restarted.join(', ')}</Text>}
              <RunLog log={full?.log} />
            </Stack>
          </Table.Td>
        </Table.Tr>
      )}
    </>
  )
}
