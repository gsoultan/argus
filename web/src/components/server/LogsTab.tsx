import { useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Checkbox, Group, Loader, Modal, NumberInput, Progress, ScrollArea, Skeleton,
  Stack, Table, Text, TextInput, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconAlertTriangle, IconCheck, IconFileText, IconLock, IconRefresh, IconX } from '@tabler/icons-react'
import { ButtonLink } from '~/components/links'
import { SectionCard } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import { bytes } from '~/components/server/SystemCard'
import { downloadText } from '~/lib/download'
import {
  isConfigured,
  type LogDisk, type LogEventLog, type LogItem, type LogOpenDeleted, type LogPick, type LogPreview, type LogRemoved,
  type LogRun, type LogsView,
} from '~/lib/live'
import { logRunFinished } from '~/lib/logApi'
import { assetLogsQuery, logRunQuery, useDecideLogRun, useScanLogs, useStartLogRun } from '~/lib/logQueries'
import { run as notifyRun } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import { FS, INK, SP } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * A server's logs: what they take on its disks, what cleaning each would
 * free, and cleaning them.
 *
 * The agent looks every six hours (or now, when asked) at names, sizes and
 * dates; it never reads what is inside a log. A cleanup is planned from that
 * scan's own file list, and the server checks every file again just before
 * it goes, so a list that is out of date skips files rather than removing
 * the wrong ones. Login records, the audit log and Argus's own files are
 * never offered.
 */
export function LogsTab({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const q = assetLogsQuery(asset.id)
  const { data: v, isLoading, error } = useQuery(q)

  if (!isConfigured()) {
    return (
      <Card padding="md">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          A server&apos;s logs are read by its agent, through a control plane. Connect this console to Argus to see them.
        </Text>
      </Card>
    )
  }
  if (error) {
    return (
      <Alert color="rose" title="Logs could not be read">
        <Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text>
      </Alert>
    )
  }
  if (isLoading || !v) return <Skeleton h={240} />

  const blocked = whyNot(v)
  const active = v.runs.find((r) => !logRunFinished(r.state))

  return (
    <Stack gap="md">
      {blocked ? (
        <Card padding="md">
          <Text size={FS.body} c="dimmed" lh={1.5}>{blocked}</Text>
        </Card>
      ) : (
        <>
          {active && <ActiveRun run={active} canEdit={canEdit} />}
          <Disks view={v} assetId={asset.id} canEdit={canEdit} />
          {v.report && <Items view={v} assetId={asset.id} canEdit={canEdit} busy={Boolean(active)} />}
          {v.report && v.report.open.length > 0 && (
            <OpenDeleted view={v} assetId={asset.id} canEdit={canEdit} busy={Boolean(active)} />
          )}
          {v.report?.event_logs && v.report.event_logs.length > 0 && (
            <EventLogs view={v} assetId={asset.id} canEdit={canEdit} busy={Boolean(active)} />
          )}
          {v.report && <Protected view={v} />}
        </>
      )}
      <RecentRuns runs={v.runs} />
    </Stack>
  )
}

function whyNot(v: LogsView): string | null {
  if (!v.enrolled) return 'No agent is enrolled on this server. Install one from Servers, Add a server, and its logs appear here.'
  if (!v.reported) {
    return "This server's agent is too old to report its logs. Update the agent from the Settings tab, and they appear here within a minute."
  }
  if (!v.supported) return `Logs cannot be cleaned on this server from the console: ${v.reason || 'its agent did not say why'}.`
  return null
}

/* ── Sizes ───────────────────────────────────────────────────────────────── */

/** How full a disk is, the way df counts it. */
export function diskPercent(used: number, avail: number): number {
  const usable = used + avail
  return usable > 0 ? Math.ceil((used / usable) * 100) : 0
}

/** The disk a path is on: the longest mount point it sits under. */
export function mountOf(path: string, disks: LogDisk[]): string | undefined {
  const norm = (s: string) => s.replace(/\\/g, '/').toLowerCase()
  const p = norm(path)
  let best: string | undefined
  for (const d of disks) {
    const m = norm(d.mount)
    const under = m === '/' || p === m || p.startsWith(m.endsWith('/') ? m : `${m}/`)
    if (under && (!best || m.length > norm(best).length)) best = d.mount
  }
  return best
}

function itemMount(it: LogItem, disks: LogDisk[]): string | undefined {
  return it.mount || mountOf(it.root, disks)
}

/** What can be freed, in all: the tab's badge. */
export function cleanable(v: LogsView): number {
  const items = v.previews.reduce((n, p) => n + p.frees, 0)
  const open = (v.report?.open ?? []).reduce((n, o) => n + o.bytes, 0)
  return items + open
}

/** Whether any disk is past the rule's line, or 85% where the rule has none. */
export function diskFilling(v: LogsView): boolean {
  const line = v.rule.diskPercent || 85
  return (v.report?.disks ?? []).some((d) => diskPercent(d.used, d.avail) >= line)
}

/** The kinds picked unless someone unticks them: old files nobody writes to. */
const PICKED_FIRST = new Set(['journal', 'rotated', 'winlogs', 'iis'])

/* ── Disks ───────────────────────────────────────────────────────────────── */

function Disks({ view: v, assetId, canEdit }: { view: LogsView; assetId: string; canEdit: boolean }) {
  const scan = useScanLogs(assetId)
  const r = v.report
  const looking = v.scanning || v.scanAsked
  const disks = useMemo(() => {
    if (!r) return []
    // The disks logs are on, fullest first; the fullest of all if none.
    const withLogs = r.disks.filter((d) => r.items.some((it) => itemMount(it, r.disks) === d.mount))
    const list = withLogs.length > 0 ? withLogs : r.disks.slice(0, 1)
    return [...list].sort((a, b) => diskPercent(b.used, b.avail) - diskPercent(a.used, a.avail))
  }, [r])
  const scanButton = canEdit && (
    <Button size="compact-sm" variant="default" leftSection={looking ? <Loader size={12} /> : <IconRefresh size={14} />}
      disabled={looking} onClick={() => void notifyRun(() => scan.mutateAsync(), {
        failure: 'Could not ask the server to scan',
        success: ['Scanning', "The agent looks at its logs' sizes at its next check-in."],
      })}>
      {looking ? 'Scanning…' : 'Scan now'}
    </Button>
  )

  if (!r) {
    return (
      <Card padding="md">
        <Group justify="space-between" wrap="wrap" gap="sm">
          <Text size={FS.body} c="dimmed">
            {looking ? 'Looking at the logs on this server…' : 'The agent has not looked at its logs yet. It does every six hours.'}
          </Text>
          {scanButton}
        </Group>
      </Card>
    )
  }
  return (
    <Card padding="md">
      <Stack gap="md">
        {r.error && (
          <Text size={FS.meta} c={INK.denied}>The last scan failed: {r.error}. What is shown is from the scan before.</Text>
        )}
        {disks.map((d, i) => (
          <DiskBar key={d.mount} disk={d} view={v}
            right={i === 0 ? (
              <Group gap="sm" wrap="nowrap">
                <Text size={FS.meta} c="dimmed" title={absTime(r.scanned_at)}>scanned {relTime(r.scanned_at)}</Text>
                {scanButton}
              </Group>
            ) : undefined} />
        ))}
      </Stack>
    </Card>
  )
}

function DiskBar({ disk: d, view: v, right }: { disk: LogDisk; view: LogsView; right?: React.ReactNode }) {
  const r = v.report!
  const logs = r.items.filter((it) => itemMount(it, r.disks) === d.mount).reduce((n, it) => n + it.bytes, 0)
  const open = r.open.filter((o) => mountOf(o.path, r.disks) === d.mount).reduce((n, o) => n + o.bytes, 0)
  const usable = d.used + d.avail || 1
  const pct = diskPercent(d.used, d.avail)
  const line = v.rule.diskPercent || 85
  const tone = pct >= 95 ? 'rose' : pct >= line ? 'amber' : undefined
  const share = (n: number) => Math.max(0, Math.min(100, (n / usable) * 100))
  const rest = Math.max(0, d.used - logs - open)
  return (
    <Box data-testid={`log-disk-${d.mount}`}>
      <Group justify="space-between" wrap="wrap" gap="xs" mb={SP.cozy}>
        <Group gap="sm" wrap="wrap">
          <Text size="sm" fw={600}>Disk <Text span inherit ff="monospace" fw={400}>{d.mount}</Text></Text>
          <Badge size="sm" variant="light" color={tone ?? 'slate'}>{pct}% used</Badge>
          <Text size={FS.meta} c="dimmed">{bytes(d.used)} of {bytes(d.total)} · {bytes(d.avail)} free</Text>
        </Group>
        {right}
      </Group>
      <Progress.Root size="md" aria-label={`Disk ${d.mount}`}>
        <Progress.Section value={share(logs)} color="azure.6" />
        <Progress.Section value={share(open)} color="amber.5" />
        <Progress.Section value={share(rest)} color="slate.4" />
      </Progress.Root>
      <Group gap="md" mt={SP.cozy} wrap="wrap">
        <Legend color="azure.6" label={`Logs ${bytes(logs)}`} />
        {open > 0 && <Legend color="amber.5" label={`Deleted but still open ${bytes(open)}`} />}
        <Legend color="slate.4" label={`Everything else ${bytes(rest)}`} />
        <Legend color="slate.1" label={`Free ${bytes(d.avail)}`} />
      </Group>
    </Box>
  )
}

function Legend({ color, label }: { color: string; label: string }) {
  const [name, shade] = color.split('.')
  return (
    <Group gap={SP.snug} wrap="nowrap">
      <Box w={10} h={10} style={{ borderRadius: 2, background: `var(--mantine-color-${name}-${shade})` }} />
      <Text size={FS.meta} c="dimmed">{label}</Text>
    </Group>
  )
}

/* ── What can be cleaned ─────────────────────────────────────────────────── */

function itemDetail(it: LogItem): string {
  const parts = [it.detail, it.count > 1 ? `${it.count} files` : '', it.oldest ? `oldest ${shortDay(it.oldest)}` : '']
  return parts.filter(Boolean).join(' · ')
}

function shortDay(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

function Items({ view: v, assetId, canEdit, busy }: { view: LogsView; assetId: string; canEdit: boolean; busy: boolean }) {
  const r = v.report!
  const previews = useMemo(() => new Map(v.previews.map((p) => [p.key, p])), [v.previews])
  const items = r.items
  const [picked, setPicked] = useState<Set<string> | null>(null)
  const chosen = picked ?? new Set(items.filter((it) => PICKED_FIRST.has(it.kind) && (previews.get(it.key)?.frees ?? 0) > 0).map((it) => it.key))
  const [confirm, setConfirm] = useState<LogPick | null>(null)
  const frees = items.filter((it) => chosen.has(it.key)).reduce((n, it) => n + (previews.get(it.key)?.frees ?? 0), 0)
  const after = projected(r.disks, items.filter((it) => chosen.has(it.key)), previews)
  const allChosen = items.length > 0 && items.every((it) => chosen.has(it.key))
  const toggle = (key: string, on: boolean) => {
    const next = new Set(chosen)
    if (on) next.add(key)
    else next.delete(key)
    setPicked(next)
  }

  const actions = (
    <Group gap={SP.cozy} wrap="wrap">
      {canEdit && (
        <ButtonLink size="compact-sm" variant="default" to="/settings" search={{ section: 'logs' }}>Change what&apos;s kept</ButtonLink>
      )}
      {canEdit && items.length > 0 && (
        <Button size="compact-sm" disabled={busy || chosen.size === 0 || frees === 0}
          onClick={() => setConfirm({ items: [...chosen], open: [], restart: [], eventLogs: [] })}>
          Clean {frees > 0 ? bytes(frees) : ''}…
        </Button>
      )}
    </Group>
  )

  return (
    <SectionCard title="Logs" icon={IconFileText} flush action={actions}
      description={chosen.size > 0 && frees > 0
        ? <>{chosen.size} of {items.length} selected · frees about <b>{bytes(frees)}</b>{after ? ` · ${after}` : ''}</>
        : 'Pick what to clean. Each line says what cleaning it does under this server\'s rule.'}>
      {items.length === 0 ? (
        <Box p="md">
          <Text size={FS.body} c="dimmed">No logs to clean on this server.</Text>
        </Box>
      ) : (
        <ScrollArea type="auto">
          <Table verticalSpacing={8} fz="sm" miw={680}>
            <Table.Thead>
              <Table.Tr>
                {canEdit && (
                  <Table.Th w={36}>
                    <Checkbox size="xs" aria-label="Select every log" checked={allChosen}
                      indeterminate={!allChosen && chosen.size > 0}
                      onChange={(e) => setPicked(e.currentTarget.checked ? new Set(items.map((it) => it.key)) : new Set())} />
                  </Table.Th>
                )}
                <Table.Th>What</Table.Th>
                <Table.Th ta="right">Size</Table.Th>
                <Table.Th>What cleaning does</Table.Th>
                <Table.Th ta="right">Frees</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {items.map((it) => {
                const p = previews.get(it.key)
                const on = chosen.has(it.key)
                return (
                  <Table.Tr key={it.key}>
                    {canEdit && (
                      <Table.Td>
                        <Checkbox size="xs" aria-label={`Clean ${it.title}`} checked={on}
                          onChange={(e) => toggle(it.key, e.currentTarget.checked)} />
                      </Table.Td>
                    )}
                    <Table.Td>
                      <Text size="sm" fw={600} style={{ overflowWrap: 'anywhere' }}>{it.title}</Text>
                      <Text size={FS.micro} c="dimmed">{itemDetail(it)}</Text>
                    </Table.Td>
                    <Table.Td ta="right"><Text size="sm" fw={600}>{bytes(it.bytes)}</Text></Table.Td>
                    <Table.Td>
                      <Group gap={SP.tight} wrap="wrap">
                        <Text size={FS.meta} c={on || !canEdit ? undefined : 'dimmed'}>{p?.does || 'Nothing to clean'}</Text>
                        {p?.automatic && <Badge size="xs" variant="light" color="slate">Automatic</Badge>}
                      </Group>
                    </Table.Td>
                    <Table.Td ta="right">
                      <Text size="sm" fw={on ? 600 : 400} c={on ? undefined : 'dimmed'}>{on && p && p.frees > 0 ? bytes(p.frees) : '—'}</Text>
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </ScrollArea>
      )}
      {confirm && <ConfirmClean assetId={assetId} view={v} pick={confirm} onClose={() => setConfirm(null)} />}
    </SectionCard>
  )
}

/** "disk / would be 77%", for the fullest disk the cleaning touches. */
function projected(disks: LogDisk[], items: LogItem[], previews: Map<string, LogPreview>): string {
  const freed = new Map<string, number>()
  for (const it of items) {
    const m = itemMount(it, disks)
    if (m) freed.set(m, (freed.get(m) ?? 0) + (previews.get(it.key)?.frees ?? 0))
  }
  let best: { mount: string; before: number; after: number } | null = null
  for (const d of disks) {
    const f = freed.get(d.mount)
    if (!f) continue
    const before = diskPercent(d.used, d.avail)
    const after = diskPercent(Math.max(0, d.used - f), d.avail + f)
    if (!best || before > best.before) best = { mount: d.mount, before, after }
  }
  return best ? `disk ${best.mount} would be ${best.after}%` : ''
}

/* ── Deleted but still open ──────────────────────────────────────────────── */

function OpenDeleted({ view: v, assetId, canEdit, busy }: { view: LogsView; assetId: string; canEdit: boolean; busy: boolean }) {
  const open = v.report!.open
  const total = open.reduce((n, o) => n + o.bytes, 0)
  const [confirm, setConfirm] = useState<LogPick | null>(null)
  return (
    <SectionCard title={`Deleted but still open · ${bytes(total)}`} icon={IconAlertTriangle} iconColor="amber" flush
      description="Someone deleted these files, but a process still holds each one, so the space is not free yet.">
      <Table verticalSpacing={8} fz="sm">
        <Table.Tbody>
          {open.map((o) => (
            <Table.Tr key={`${o.pid}:${o.fd}`}>
              <Table.Td>
                <Text size="sm" fw={600} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{o.path} (deleted)</Text>
                <Text size={FS.micro} c="dimmed">held by {o.process} (pid {o.pid}{o.unit ? `, ${o.unit}` : ''})</Text>
              </Table.Td>
              <Table.Td ta="right" w={90}><Text size="sm" fw={600}>{bytes(o.bytes)}</Text></Table.Td>
              {canEdit && (
                <Table.Td w={1}>
                  <Group gap={SP.cozy} wrap="nowrap" justify="flex-end">
                    {o.unit && (
                      <Button size="compact-xs" variant="default" disabled={busy}
                        onClick={() => setConfirm({ items: [], open: [], restart: [o.unit!], eventLogs: [] })}>
                        Restart {o.unit}
                      </Button>
                    )}
                    <Button size="compact-xs" variant="default" disabled={busy}
                      onClick={() => setConfirm({ items: [], open: [{ pid: o.pid, fd: o.fd }], restart: [], eventLogs: [] })}>
                      Empty the open file
                    </Button>
                  </Group>
                </Table.Td>
              )}
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      {confirm && <ConfirmClean assetId={assetId} view={v} pick={confirm} onClose={() => setConfirm(null)} />}
    </SectionCard>
  )
}

/* ── Windows event logs ──────────────────────────────────────────────────── */

const EVENT_LOG_CAP_MB = 4096

function EventLogs({ view: v, assetId, canEdit, busy }: { view: LogsView; assetId: string; canEdit: boolean; busy: boolean }) {
  const logs = v.report!.event_logs ?? []
  const [confirm, setConfirm] = useState<LogPick | null>(null)
  return (
    <SectionCard title="Event logs" flush
      description="Windows keeps these at their maximum size, overwriting the oldest events. Argus never clears them; it can only raise a maximum, so less is overwritten.">
      <ScrollArea type="auto">
        <Table verticalSpacing={8} fz="sm" miw={520}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Log</Table.Th>
              <Table.Th ta="right">Size</Table.Th>
              <Table.Th ta="right">Maximum</Table.Th>
              {canEdit && <Table.Th w={1} />}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {logs.map((l) => (
              <Table.Tr key={l.name}>
                <Table.Td>
                  <Group gap={SP.tight}>
                    <Text size="sm" fw={600}>{l.name}</Text>
                    {l.protected && <Badge size="xs" variant="light" color="slate">Evidence</Badge>}
                    {nearlyFull(l) && <Badge size="xs" variant="light" color="amber">Overwriting</Badge>}
                  </Group>
                  {l.records ? <Text size={FS.micro} c="dimmed">{l.records.toLocaleString()} events</Text> : null}
                </Table.Td>
                <Table.Td ta="right">{bytes(l.bytes)}</Table.Td>
                <Table.Td ta="right">{bytes(l.max_bytes)}</Table.Td>
                {canEdit && (
                  <Table.Td>
                    <Button size="compact-xs" variant="default" disabled={busy || l.max_bytes >= EVENT_LOG_CAP_MB * 1024 ** 2}
                      onClick={() => setConfirm({ items: [], open: [], restart: [], eventLogs: [{ name: l.name, maxMb: suggestMax(l) }] })}>
                      Raise maximum
                    </Button>
                  </Table.Td>
                )}
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </ScrollArea>
      {confirm && <ConfirmClean assetId={assetId} view={v} pick={confirm} onClose={() => setConfirm(null)} />}
    </SectionCard>
  )
}

function nearlyFull(l: LogEventLog): boolean {
  return l.max_bytes > 0 && l.bytes >= l.max_bytes * 0.9
}

/** Twice the maximum, within what Argus allows. */
function suggestMax(l: LogEventLog): number {
  const now = Math.ceil(l.max_bytes / 1024 ** 2)
  return Math.min(EVENT_LOG_CAP_MB, Math.max(64, now * 2))
}

/* ── Never cleaned ───────────────────────────────────────────────────────── */

function Protected({ view: v }: { view: LogsView }) {
  const list = v.report!.protected
  if (list.length === 0) return null
  return (
    <SectionCard title="Never cleaned" icon={IconLock} iconColor="slate" flush
      description="Kept as evidence of who did what on this server. Settings can add to this list, not take from it.">
      <Table verticalSpacing={8} fz="sm">
        <Table.Tbody>
          {list.map((p) => (
            <Table.Tr key={p.title}>
              <Table.Td>
                <Text size="sm"><b>{p.title}</b> <Text span size={FS.meta} c="dimmed">{p.detail}</Text></Text>
              </Table.Td>
              <Table.Td ta="right" w={100}><Text size="sm" c="dimmed">{p.bytes > 0 ? bytes(p.bytes) : ''}</Text></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </SectionCard>
  )
}

/* ── Confirm ─────────────────────────────────────────────────────────────── */

function ConfirmClean({ assetId, view: v, pick: initial, onClose }: {
  assetId: string
  view: LogsView
  pick: LogPick
  onClose: () => void
}) {
  const start = useStartLogRun(assetId)
  const [pick, setPick] = useState(initial)
  const [typed, setTyped] = useState('')
  const r = v.report!
  const previews = new Map(v.previews.map((p) => [p.key, p]))
  const items = r.items.filter((it) => pick.items.includes(it.key))
  const open = pick.open.map((o) => r.open.find((x) => x.pid === o.pid && x.fd === o.fd)).filter((x): x is LogOpenDeleted => Boolean(x))
  const restartBytes = (unit: string) => r.open.filter((o) => o.unit === unit).reduce((n, o) => n + o.bytes, 0)
  const frees = items.reduce((n, it) => n + (previews.get(it.key)?.frees ?? 0), 0) +
    open.reduce((n, o) => n + o.bytes, 0) + pick.restart.reduce((n, u) => n + restartBytes(u), 0)
  const resizeOnly = pick.eventLogs.length > 0 && frees === 0
  const after = projected(r.disks, items, previews)
  const ok = typed.trim().toLowerCase() === v.confirm
  const title = resizeOnly ? `Raise an event log's maximum on ${v.confirm}` : `Clean ${bytes(frees)} on ${v.confirm}`

  return (
    <Modal opened onClose={onClose} title={title} size="lg">
      <Stack gap="sm">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          {resizeOnly
            ? 'Nothing is removed. Windows keeps more events before it overwrites the oldest.'
            : 'These files are removed for good. Each one is checked again just before it goes, and one that changed since the scan is skipped. The list of what was removed is kept with the run in Activity.'}
        </Text>
        <Card padding={0} withBorder>
          <Table verticalSpacing={8} fz="sm">
            <Table.Tbody>
              {items.map((it) => {
                const p = previews.get(it.key)
                return (
                  <ConfirmRow key={it.key} title={it.title}
                    detail={`${p?.does ?? ''}${p && p.files > 0 ? ` · ${p.files} file${p.files === 1 ? '' : 's'}` : ''}`}
                    size={p?.frees ?? 0} />
                )
              })}
              {open.map((o) => (
                <ConfirmRow key={`${o.pid}:${o.fd}`} title={`${o.path} (deleted)`}
                  detail={`emptied while ${o.process} keeps it open`} size={o.bytes} />
              ))}
              {pick.restart.map((u) => (
                <ConfirmRow key={u} title={`Restart ${u}`} detail="it lets go of the deleted files it holds" size={restartBytes(u)} />
              ))}
              {pick.eventLogs.map((e, i) => {
                const l = r.event_logs?.find((x) => x.name === e.name)
                return (
                  <Table.Tr key={e.name}>
                    <Table.Td>
                      <Text size="sm" fw={600}>{e.name} event log</Text>
                      <Text size={FS.micro} c="dimmed">now at most {l ? bytes(l.max_bytes) : '—'}</Text>
                    </Table.Td>
                    <Table.Td w={170}>
                      <NumberInput size="xs" aria-label={`New maximum for ${e.name}, in MB`} suffix=" MB"
                        min={l ? Math.ceil(l.max_bytes / 1024 ** 2) + 1 : 1} max={EVENT_LOG_CAP_MB} value={e.maxMb}
                        onChange={(x) => setPick({
                          ...pick,
                          eventLogs: pick.eventLogs.map((y, j) => (j === i ? { ...y, maxMb: Number(x) || 0 } : y)),
                        })} />
                    </Table.Td>
                  </Table.Tr>
                )
              })}
              {after && (
                <Table.Tr>
                  <Table.Td colSpan={2}><Text size={FS.meta} c="dimmed" ta="right">{after}</Text></Table.Td>
                </Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Card>
        <TextInput label={<>Type <Text span ff="monospace" fw={600}>{v.confirm}</Text> to confirm</>} value={typed}
          onChange={(e) => setTyped(e.currentTarget.value)} ff="monospace" data-autofocus autoComplete="off" />
        {v.rule.needsApproval && (
          <Alert color="amber" p="xs">
            <Text size={FS.body}>This server&apos;s rule asks for a second admin: the cleanup waits until another admin approves it.</Text>
          </Alert>
        )}
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!ok} loading={start.isPending} onClick={() => void notifyRun(
            () => start.mutateAsync({ ...pick, confirm: typed.trim() }),
            {
              failure: 'Cleanup not started',
              success: v.rule.needsApproval
                ? ['Waiting for approval', 'Another admin approves the cleanup before it starts.']
                : ['Cleanup asked for', 'The server starts at its next check-in, within 30 seconds.'],
            },
          ).then((done) => done && onClose())}>
            {v.rule.needsApproval ? 'Ask for approval' : resizeOnly ? 'Raise maximum' : `Clean ${bytes(frees)}`}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function ConfirmRow({ title, detail, size }: { title: string; detail: string; size: number }) {
  return (
    <Table.Tr>
      <Table.Td>
        <Text size="sm" fw={600} style={{ overflowWrap: 'anywhere' }}>{title}</Text>
        <Text size={FS.micro} c="dimmed">{detail}</Text>
      </Table.Td>
      <Table.Td ta="right" w={100}><Text size="sm" fw={600}>{size > 0 ? bytes(size) : '—'}</Text></Table.Td>
    </Table.Tr>
  )
}

/* ── A cleanup going ─────────────────────────────────────────────────────── */

const STATE: Record<LogRun['state'], { label: string; color: string }> = {
  awaiting_approval: { label: 'Waiting for approval', color: 'amber' },
  queued: { label: 'Asked', color: 'azure' },
  running: { label: 'Cleaning', color: 'azure' },
  done: { label: 'Done', color: 'teal' },
  failed: { label: 'Failed', color: 'rose' },
  interrupted: { label: 'Interrupted', color: 'rose' },
  cancelled: { label: 'Taken back', color: 'slate' },
  denied: { label: 'Denied', color: 'slate' },
}

export function LogRunState({ state }: { state: LogRun['state'] }) {
  const s = STATE[state]
  return <Badge size="sm" variant="light" color={s.color}>{s.label}</Badge>
}

function who(r: LogRun): string {
  return r.trigger === 'automatic' ? `Automatic${r.reason ? ` · ${r.reason}` : ''}` : r.requestedBy
}

function ActiveRun({ run: r, canEdit }: { run: LogRun; canEdit: boolean }) {
  return (
    <SectionCard title="Cleanup" icon={IconFileText} badge={<LogRunState state={r.state} />}
      description={`${who(r)} · ${r.summary} · asked ${relTime(r.requestedAt)}`}
      action={<RunActions run={r} canEdit={canEdit} />}>
      <Group gap={SP.cozy} wrap="nowrap">
        {r.state === 'running' && <Loader size={14} />}
        <Text size={FS.body} c="dimmed" lh={1.5}>
          {r.state === 'awaiting_approval' ? 'Another admin approves this cleanup before the server is asked to start it.'
            : r.state === 'queued' ? 'Asked. The server starts at its next check-in, within 30 seconds.'
              : r.message || `Cleaning about ${bytes(r.plannedBytes)}. The server checks each file again before it goes.`}
        </Text>
      </Group>
    </SectionCard>
  )
}

function RunActions({ run: r, canEdit }: { run: LogRun; canEdit: boolean }) {
  const { data: me } = useQuery(meQuery())
  const decide = useDecideLogRun()
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
            onClick={() => act('deny', ['Denied', 'Nothing is removed.'], 'Not denied')}>
            Deny
          </Button>
        </>
      )}
      {canTakeBack && (
        <Button size="compact-xs" variant="default" loading={decide.isPending}
          onClick={() => act('cancel', ['Taken back', 'Nothing was removed.'], 'Not taken back')}>
          Take back
        </Button>
      )}
    </Group>
  )
}

/* ── Past cleanups ───────────────────────────────────────────────────────── */

function RecentRuns({ runs }: { runs: LogRun[] }) {
  const [open, setOpen] = useState<string | null>(null)
  const done = runs.filter((r) => logRunFinished(r.state))
  if (done.length === 0) return null
  return (
    <SectionCard title="Recent cleanups" flush description="Each with the list of what was removed, also in Activity.">
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

function RunRow({ run: r, open, onToggle }: { run: LogRun; open: boolean; onToggle: () => void }) {
  const outcome = r.state === 'done' || r.freed > 0
    ? `freed ${bytes(r.freed)}${r.removedCount > 0 ? ` · ${r.removedCount} file${r.removedCount === 1 ? '' : 's'}` : ''}`
    : ''
  return (
    <>
      <Table.Tr style={{ cursor: 'pointer' }} onClick={onToggle} aria-expanded={open}>
        <Table.Td w={150}><Text size={FS.meta} title={absTime(r.requestedAt)}>{relTime(r.finishedAt ?? r.requestedAt)}</Text></Table.Td>
        <Table.Td w={140}><LogRunState state={r.state} /></Table.Td>
        <Table.Td><Text size="sm">{who(r)} · {r.summary}</Text></Table.Td>
        <Table.Td ta="right"><Text size={FS.meta} c="dimmed">{outcome}</Text></Table.Td>
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

const ACTION_LABEL: Record<string, string> = {
  deleted: 'deleted',
  truncated: 'truncated',
  emptied: 'emptied',
  vacuumed: 'vacuumed',
  restarted: 'restarted',
  resized: 'maximum raised',
}

function sizeText(x: LogRemoved): string {
  if (x.action === 'restarted') return bytes(x.bytes)
  return x.after !== undefined && x.after > 0 ? `${bytes(x.bytes)} → ${bytes(x.after)}` : bytes(x.bytes)
}

/** The manifest, as a file to keep. */
export function manifestCsv(rows: LogRemoved[]): string {
  const q = (s: string) => (/[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s)
  const lines = rows.map((x) => [x.path, x.action, String(x.bytes), String(x.after ?? ''), x.modified ?? ''].map(q).join(','))
  return ['path,action,bytes,bytes_after,modified', ...lines].join('\n') + '\n'
}

const SHOWN = 50

function RunDetail({ run: r }: { run: LogRun }) {
  const { data: full, isLoading } = useQuery(logRunQuery(r.id))
  const [all, setAll] = useState(false)
  const removed = full?.removed ?? []
  const shown = all ? removed : removed.slice(0, SHOWN)
  return (
    <Stack gap={SP.cozy}>
      <Group justify="space-between" wrap="wrap" gap="xs">
        <Text size={FS.meta} c="dimmed">
          {r.trigger === 'automatic' ? 'Started by its rule' : `Asked by ${r.requestedBy}`}
          {r.decidedBy ? ` · ${r.state === 'denied' ? 'denied' : 'approved'} by ${r.decidedBy}` : ''}
          {r.finishedAt ? ` · finished ${absTime(r.finishedAt)}` : ''}
        </Text>
        {removed.length > 0 && (
          <Button size="compact-xs" variant="default"
            onClick={() => downloadText(manifestCsv(removed), `log-cleanup-${r.assetHostname || r.hostname}-${r.id}.csv`, 'text/csv')}>
            Download CSV
          </Button>
        )}
      </Group>
      {r.message && <Text size={FS.body} c={r.state === 'done' ? undefined : INK.denied}>{r.message}</Text>}
      {r.skipped.length > 0 && (
        <Group gap={SP.cozy} wrap="nowrap" align="flex-start">
          <ThemeIcon size={18} radius="xl" variant="light" color="amber"><IconAlertTriangle size={12} /></ThemeIcon>
          <Box>
            <Text size={FS.body} fw={600}>{r.skipped.length} file{r.skipped.length === 1 ? '' : 's'} skipped</Text>
            <Text size={FS.micro} c="dimmed" style={{ whiteSpace: 'pre-line', overflowWrap: 'anywhere' }}>
              {r.skipped.slice(0, 10).map((s) => `${s.path}: ${s.why}`).join('\n')}
              {r.skipped.length > 10 ? `\nand ${r.skipped.length - 10} more` : ''}
            </Text>
          </Box>
        </Group>
      )}
      {isLoading ? <Skeleton h={60} /> : removed.length > 0 && (
        <ScrollArea type="auto">
          <Table verticalSpacing={4} fz="xs" miw={520}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Path</Table.Th>
                <Table.Th ta="right">Modified</Table.Th>
                <Table.Th ta="right">Size</Table.Th>
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {shown.map((x, i) => (
                <Table.Tr key={`${x.path}-${i}`}>
                  <Table.Td><Text size={FS.meta} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{x.path}</Text></Table.Td>
                  <Table.Td ta="right"><Text size={FS.meta}>{x.modified && !x.modified.startsWith('0001') ? shortDay(x.modified) : ''}</Text></Table.Td>
                  <Table.Td ta="right"><Text size={FS.meta}>{sizeText(x)}</Text></Table.Td>
                  <Table.Td><Text size={FS.meta} c="dimmed">{ACTION_LABEL[x.action] ?? x.action}</Text></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
          {(removed.length > SHOWN && !all) || r.removedMore > 0 ? (
            <Group gap="sm" mt={SP.cozy}>
              {removed.length > SHOWN && !all && (
                <Button size="compact-xs" variant="subtle" onClick={() => setAll(true)}>Show {removed.length - SHOWN} more</Button>
              )}
              {r.removedMore > 0 && (
                <Text size={FS.micro} c="dimmed">{r.removedMore} more not listed; the server kept only the first ones.</Text>
              )}
            </Group>
          ) : null}
        </ScrollArea>
      )}
    </Stack>
  )
}
