import { useState } from 'react'
import {
  Alert, Anchor, Badge, Box, Button, Card, Group, Progress, Radio, Skeleton, Stack, Text, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconArrowRight, IconCheck, IconCircleDashed, IconExclamationMark, IconLoader2,
  IconRefresh, IconSearch, IconX,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { ErrorState } from '~/components/ErrorState'
import { Mono, absTime, relTime } from '~/components/primitives'
import {
  controlUpdate, type ControlUpdateState, type ControlUpdateStatus, type ControlUpdateView,
} from '~/lib/live'
import { notifyOk, notifyWarn, run } from '~/lib/notify'
import {
  controlUpdateKey, useCancelControlUpdate, useCheckControlUpdate, useRequestControlUpdate,
} from '~/lib/queries'
import { FS, SP } from '~/theme'

/**
 * Settings › Control plane › Updates: argus-control updating itself.
 *
 * Updating used to be a package install and a restart by hand, which ended
 * every open session and had no way back. From here the control plane hands
 * the update to a root helper on its host, which checks and backs up first,
 * restarts, and puts the old version back if the new one does not come up.
 * This page shows, before anything starts, what the restart would do: which
 * sessions pause and resume, which end, and whether going back would be clean
 * for the data. Everything on it is what the control plane reported.
 */
export function ControlUpdateSection() {
  const { data, error, isPending, refetch } = useQuery({
    queryKey: controlUpdateKey,
    queryFn: controlUpdate,
    // Fast while an update runs, so each step shows as it happens; the
    // request fails while the control plane restarts, and the last answer
    // stays on screen until it is back.
    refetchInterval: (q) => (inProgress(q.state.data?.update) ? 2_000 : 30_000),
    retry: false,
  })
  if (isPending) return <Skeleton h={420} />
  if (!data) {
    return (
      <ErrorState title="argus-control did not say whether it can update" error={error}
        reset={() => void refetch()} />
    )
  }
  return <ControlUpdatePanel v={data} unreachable={Boolean(error)} />
}

const ACTIVE: ControlUpdateState[] = [
  'waiting', 'requested', 'downloading', 'backing_up', 'installing', 'restarting', 'rolling_back',
]

function inProgress(u: ControlUpdateStatus | null | undefined): boolean {
  return Boolean(u && ACTIVE.includes(u.state))
}

export function ControlUpdatePanel({ v, unreachable }: { v: ControlUpdateView; unreachable?: boolean }) {
  const u = v.update
  const running = inProgress(u)
  // The last update's outcome is shown until someone starts another, or for
  // a day, whichever comes first.
  const recent = u && !running && Date.now() - new Date(u.at).getTime() < 24 * 3600_000 ? u : null
  return (
    <Stack gap="sm" data-testid="control-update">
      <Versions v={v} busy={running} />
      {running && u && <Progressing u={u} v={v} unreachable={unreachable} />}
      {!running && recent && <Outcome u={recent} />}
      {!running && <Offer v={v} />}
    </Stack>
  )
}

function Versions({ v, busy }: { v: ControlUpdateView; busy: boolean }) {
  return (
    <SectionCard title="This control plane"
      description={<>argus-control on <Mono>{v.hostname || 'this host'}</Mono>. With more than one replica, each is
        updated from its own address.</>}
      action={v.releaseChecks ? <CheckNow disabled={busy} /> : undefined}>
      <Group gap="xl" align="flex-end" wrap="wrap">
        <Box>
          <Text size={FS.meta} c="dimmed">Running</Text>
          <Text fz={26} fw={650} data-testid="update-running">{v.running}</Text>
        </Box>
        {v.newest && v.newer && (
          <>
            <IconArrowRight size={20} style={{ marginBottom: 8, opacity: 0.5 }} />
            <Box>
              <Text size={FS.meta} c="dimmed">Newest release</Text>
              <Text fz={26} fw={650} c="azure" data-testid="update-newest">{v.newest.version}</Text>
            </Box>
            <Badge variant="light" mb={8}>Released {relTime(v.newest.publishedAt)}</Badge>
          </>
        )}
        {!v.newer && (
          <Badge color="teal" variant="light" mb={8} leftSection={<IconCheck size={12} />}>
            Up to date
          </Badge>
        )}
      </Group>
      <Text size={FS.meta} c="dimmed" mt={SP.cozy}>
        {v.checkFailure
          ? <>Could not check for releases: {v.checkFailure}</>
          : v.checkedAt ? <>Checked for releases {relTime(v.checkedAt)}.</> : <>Release checks are off.</>}
      </Text>
    </SectionCard>
  )
}

/**
 * Looks for a newer release now rather than at the next check, and says what
 * it found: a release to update to, nothing newer, or why it could not ask.
 */
function CheckNow({ disabled }: { disabled: boolean }) {
  const check = useCheckControlUpdate()
  return (
    <Button size="xs" variant="default" leftSection={<IconSearch size={14} />} loading={check.isPending}
      disabled={disabled} data-testid="update-check"
      onClick={() => void run(async () => {
        const v = await check.mutateAsync()
        if (v.checkFailure) notifyWarn('Could not check for releases', v.checkFailure)
        else if (v.newer && v.newest) notifyOk(`argus-control ${v.newest.version} is out`, `This control plane runs ${v.running}.`)
        else notifyOk('Up to date', `${v.running} is the newest release.`)
      }, { failure: 'Could not check for releases' })}>
      Check for updates
    </Button>
  )
}

/* ── Before it starts ───────────────────────────────────────────────────── */

type Tone = 'ok' | 'warn' | 'bad'

function Check({ tone, title, children, testId }: {
  tone: Tone; title: string; children?: React.ReactNode; testId?: string
}) {
  const color = tone === 'ok' ? 'teal' : tone === 'warn' ? 'amber' : 'rose'
  const Icon = tone === 'ok' ? IconCheck : tone === 'warn' ? IconExclamationMark : IconX
  return (
    <Group gap="sm" align="flex-start" wrap="nowrap" data-testid={testId} data-tone={tone}>
      <ThemeIcon size={22} radius="xl" color={color} variant="light"><Icon size={13} /></ThemeIcon>
      <Box style={{ minWidth: 0 }}>
        <Text size={FS.body} fw={600}>{title}</Text>
        {children && <Text size={FS.meta} c="dimmed" lh={1.5}>{children}</Text>}
      </Box>
    </Group>
  )
}

function Offer({ v }: { v: ControlUpdateView }) {
  const request = useRequestControlUpdate()
  const s = v.sessions
  const [when, setWhen] = useState<'idle' | 'now'>('idle')
  if (!v.newer || !v.newest) return null
  const target = v.newest.version

  const ending: string[] = []
  if (s.terminals) ending.push(plural(s.terminals, 'terminal on an older agent', 'terminals on older agents'))
  if (s.desktops) ending.push(plural(s.desktops, 'desktop', 'desktops'))
  if (s.databases) ending.push(plural(s.databases, 'database session', 'database sessions'))

  const rb = v.rollback
  const rollbackTone: Tone = !rb || !rb.known ? 'warn' : rb.breaking.length > 0 ? 'warn' : 'ok'

  return (
    <SectionCard title={`Update to ${target}`}
      description="Checked now, and again by the helper just before the restart."
      action={v.newest.page ? <Anchor href={v.newest.page} target="_blank" size={FS.meta}>What's new</Anchor> : undefined}>
      <Stack gap="md">
        {!v.supported && (
          <Alert color="amber" icon={<IconAlertTriangle size={16} />} title="This install cannot update itself"
            p="xs" data-testid="update-unsupported">
            <Text size={FS.body} lh={1.5}>{v.reason}</Text>
          </Alert>
        )}
        <Stack gap="sm">
          <Check tone="ok" title="Package checked before anything changes">
            The helper downloads {target} itself and checks it against the release's checksums.
          </Check>
          <Check tone={rollbackTone} title={rb?.known && rb.breaking.length === 0 ? 'Rolls back cleanly' : 'Going back would need the backup'}
            testId="update-rollback">
            {!rb || !rb.known
              ? rb?.reason ?? 'The release does not say which migrations it carries.'
              : rb.breaking.length === 0
                ? rb.added === 0
                  ? `No new migrations, so ${v.running} runs on the database as it is.`
                  : `${plural(rb.added, 'new migration', 'new migrations')}, all additive. ${v.running} runs on the new schema, so going back needs no restore.`
                : <>{target} {rb.breaking.map((m) => m.why || m.name).join('; ')}. If it has to go back, {v.running} may not
                  start on that database until the backup is restored.</>}
          </Check>
          <Check tone="ok" title="Backup first">
            The backup service runs just before the restart. If it fails, nothing is changed.
          </Check>
          <Check tone={s.ending > 0 ? 'warn' : 'ok'} testId="update-sessions"
            title={s.ending > 0 ? `${ending.join(', ')} would end` : 'No open session would end'}>
            {s.held > 0
              ? `${plural(s.held, 'terminal pauses', 'terminals pause')} for a few seconds and picks up where it was.`
              : 'Terminals on current agents pause for a few seconds and pick up where they were.'}
            {s.ending > 0 && ' Desktops, database sessions, and terminals on agents too old to hold a shell end with the restart.'}
          </Check>
        </Stack>

        <Radio.Group value={when} onChange={(x) => setWhen(x as 'idle' | 'now')} label="When">
          <Stack gap={SP.snug} mt={SP.snug}>
            <Radio value="idle" data-testid="update-when-idle"
              label="When nothing would be cut off"
              description={s.ending > 0
                ? `Waits for the ${plural(s.ending, 'session', 'sessions')} above to end, up to 2 hours, then gives up without changing anything.`
                : 'Nothing would end now, so it starts at once.'} />
            <Radio value="now" data-testid="update-when-now"
              label="Now"
              description={s.ending > 0
                ? `${ending.join(', ')} end. Everything else pauses for a few seconds.`
                : 'Open terminals pause for a few seconds and pick up where they were.'} />
          </Stack>
        </Radio.Group>

        <Group>
          <Button leftSection={<IconRefresh size={15} />} disabled={!v.supported} loading={request.isPending}
            data-testid="update-start"
            onClick={() => void run(() => request.mutateAsync({ version: target, when }), {
              failure: 'The update did not start',
            })}>
            Update to {target}
          </Button>
        </Group>
      </Stack>
    </SectionCard>
  )
}

/* ── While it runs ──────────────────────────────────────────────────────── */

const ORDER: ControlUpdateState[] = [
  'waiting', 'requested', 'downloading', 'backing_up', 'installing', 'restarting', 'done',
]

const LABEL: Record<ControlUpdateState, string> = {
  waiting: 'Waiting for sessions to end',
  requested: 'Handed to the update helper',
  downloading: 'Downloading and checking',
  backing_up: 'Backing up',
  installing: 'Installing',
  restarting: 'Restarting and checking the new version',
  rolling_back: 'Going back',
  done: 'Done',
  rolled_back: 'Rolled back',
  failed: 'Failed',
  cancelled: 'Cancelled',
}

function Progressing({ u, v, unreachable }: { u: ControlUpdateStatus; v: ControlUpdateView; unreachable?: boolean }) {
  const cancel = useCancelControlUpdate()
  const at = Math.max(0, ORDER.indexOf(u.state))
  const pct = u.state === 'rolling_back' ? 90 : Math.round(((at + 1) / ORDER.length) * 100)
  return (
    <SectionCard title={`Updating to ${u.target}`}
      description={<>Asked by {u.requested_by || 'someone'} {relTime(u.steps?.[0]?.at ?? u.at)}. This page follows it
        through the restart by itself.</>}
      action={u.state === 'waiting'
        ? <Button size="xs" variant="default" loading={cancel.isPending} data-testid="update-cancel"
            onClick={() => void run(() => cancel.mutateAsync(), { failure: 'The update was not cancelled' })}>
            Cancel
          </Button>
        : undefined}>
      <Stack gap="sm" data-testid="update-progress" data-state={u.state}>
        <Progress value={pct} color={u.state === 'rolling_back' ? 'amber' : 'azure'} animated />
        {unreachable && (
          <Alert color="azure" icon={<IconLoader2 size={16} />} p="xs">
            <Text size={FS.body} lh={1.5}>The control plane is restarting. This page reconnects when it is back.</Text>
          </Alert>
        )}
        {v.helperLate && (
          <Alert color="amber" icon={<IconAlertTriangle size={16} />} title="The update helper has not answered" p="xs">
            <Text size={FS.body} lh={1.5}>
              On the control plane's host, check that the watcher is on:{' '}
              <Mono>sudo systemctl enable --now argus-control-update.path</Mono>
            </Text>
          </Alert>
        )}
        <Steps u={u} />
      </Stack>
    </SectionCard>
  )
}

function Steps({ u }: { u: ControlUpdateStatus }) {
  const steps = u.steps ?? []
  return (
    <Stack gap={6}>
      {steps.map((s, i) => {
        const last = i === steps.length - 1
        const bad = s.state === 'failed' || s.state === 'rolling_back' || s.state === 'rolled_back'
        const Icon = last && ACTIVE.includes(s.state) ? IconCircleDashed : bad ? IconX : IconCheck
        const color = bad ? (s.state === 'rolled_back' || s.state === 'rolling_back' ? 'amber' : 'rose') : last && ACTIVE.includes(s.state) ? 'azure' : 'teal'
        return (
          <Group key={i} gap="sm" wrap="nowrap" align="flex-start">
            <ThemeIcon size={20} radius="xl" variant="light" color={color}><Icon size={12} /></ThemeIcon>
            <Box style={{ flex: 1, minWidth: 0 }}>
              <Text size={FS.body} fw={500}>{LABEL[s.state] ?? s.state}</Text>
              <Text size={FS.meta} c="dimmed" lh={1.45}>{s.message}</Text>
            </Box>
            <Text size={FS.meta} c="dimmed" title={absTime(s.at)}>{new Date(s.at).toLocaleTimeString()}</Text>
          </Group>
        )
      })}
    </Stack>
  )
}

function Outcome({ u }: { u: ControlUpdateStatus }) {
  const tone = u.state === 'done' ? 'teal' : u.state === 'cancelled' ? 'slate' : u.state === 'rolled_back' ? 'amber' : 'rose'
  const title = {
    done: `Updated to ${u.target}`,
    rolled_back: `The update to ${u.target} was rolled back`,
    failed: `The update to ${u.target} failed`,
    cancelled: `The update to ${u.target} was cancelled`,
  }[u.state as 'done' | 'rolled_back' | 'failed' | 'cancelled'] ?? LABEL[u.state]
  return (
    <Card padding="md" data-testid="update-outcome" data-state={u.state}>
      <Stack gap="sm">
        <Alert color={tone} p="xs" title={title}
          icon={u.state === 'done' ? <IconCheck size={16} /> : <IconAlertTriangle size={16} />}>
          <Text size={FS.body} lh={1.5}>{u.message}</Text>
          {u.state === 'rolled_back' && (
            <Text size={FS.meta} lh={1.5} mt={4}>Still running {u.from}. Nothing is retried on its own; the helper's
              log is in <Mono>journalctl -u argus-control-update</Mono> on the control plane's host.</Text>
          )}
        </Alert>
        <Steps u={u} />
      </Stack>
    </Card>
  )
}

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`
}
