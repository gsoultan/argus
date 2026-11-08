import { ActionIcon, Alert, Badge, Box, Button, Card, Group, Menu, Modal, SegmentedControl, SimpleGrid, Stack, Text, Textarea, ThemeIcon, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import {
  IconCalendarTime, IconClockPause, IconDots, IconPencil, IconPlayerPause, IconPlayerPlay, IconPlayerStop, IconTrash, IconUserCheck,
} from '@tabler/icons-react'
import { useState } from 'react'
import { ButtonLink } from '~/components/links'
import { EmptyState } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import {
  DAY_SHORT, RUN_STATE, SCHEDULE_STATE, SCHEDULE_TAG, cloudAccountsQuery, cloudScheduleRunsQuery, cloudSchedulesQuery, money, scheduleOf,
  transitionWhen, useKeepRunning, useScheduleAction, weekRuns, windowWords,
  type CloudItem, type CloudKeepRunning, type CloudScheduleView, type CloudWindow,
} from '~/lib/cloud'
import { notifyError, notifyOk } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import { FS, INK } from '~/theme'

/**
 * The week a schedule runs, one row per day, Monday first: a bar where the
 * instances are on. Wall-clock, in the schedule's zone.
 */
export function WeekStrip({ windows, compact }: { windows: CloudWindow[]; compact?: boolean }) {
  const runs = weekRuns(windows)
  const h = compact ? 8 : 12
  return (
    <Box role="img" aria-label={windows.length ? 'Runs ' + windows.map(windowWords).join('; ') : 'Never runs'}>
      {runs.map((ranges, d) => (
        <Group key={d} gap={8} wrap="nowrap" mb={compact ? 3 : 5}>
          <Text size={FS.micro} c="dimmed" w={26} style={{ flexShrink: 0 }}>{DAY_SHORT[d]}</Text>
          <Box style={{ position: 'relative', flex: 1, height: h, borderRadius: h / 2, background: 'var(--mantine-color-default-hover)' }}>
            {ranges.map(([a, b], i) => (
              <Box key={i} style={{
                position: 'absolute', top: 0, bottom: 0, left: `${(a / 1440) * 100}%`, width: `${((b - a) / 1440) * 100}%`,
                background: 'var(--mantine-color-teal-filled)', opacity: 0.85,
                borderRadius: h / 2,
              }} />
            ))}
          </Box>
        </Group>
      ))}
      {!compact && (
        <Group gap={8} wrap="nowrap">
          <Box w={26} />
          <Group justify="space-between" style={{ flex: 1 }}>
            {['00', '06', '12', '18', '24'].map((t) => <Text key={t} size={FS.micro} c="dimmed">{t}</Text>)}
          </Group>
        </Group>
      )}
    </Box>
  )
}

export function SchedulesList({ schedules, keep, loading, names }: {
  schedules?: CloudScheduleView[]
  keep?: CloudKeepRunning[]
  loading?: boolean
  /** Instance names by id, for the kept-running list. */
  names: Map<string, string>
}) {
  const { data: me } = useQuery(meQuery())
  const admin = canSee(me?.role, 'admin')
  if (loading) return <Text c="dimmed" size={FS.meta}>Loading…</Text>
  return (
    <Stack gap="md">
      {(keep?.length ?? 0) > 0 && (
        <Alert color="sky" variant="light" icon={<IconClockPause size={16} />} title="Kept running">
          <Stack gap={4}>
            {keep!.map((k) => (
              <Text key={k.accountId + k.instanceId} size={FS.meta}>
                <b>{names.get(k.instanceId) ?? k.instanceId}</b> until {absTime(k.until)}, by {k.setBy}: “{k.reason}”
              </Text>
            ))}
          </Stack>
        </Alert>
      )}
      {!schedules?.length ? (
        <Card>
          <EmptyState icon={IconCalendarTime} title="No schedules yet"
            description={`A schedule stops instances outside the hours they are needed and starts them again, which AWS stops billing for. An instance joins one with the tag ${SCHEDULE_TAG}=<name>.`} />
          {admin && <Group justify="center" pb="md"><ButtonLink to="/cloud/schedule" size="sm" leftSection={<IconCalendarTime size={16} />}>Make a schedule</ButtonLink></Group>}
        </Card>
      ) : (
        <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md">
          {schedules.map((s) => <ScheduleCard key={s.id} s={s} admin={admin} me={me?.email ?? ''} decides={admin || me?.role === 'approver'} />)}
        </SimpleGrid>
      )}
    </Stack>
  )
}

function ScheduleCard({ s, admin, me, decides }: { s: CloudScheduleView; admin: boolean; me: string; decides: boolean }) {
  const act = useScheduleAction()
  const navigate = useNavigate()
  const [runsOpen, setRunsOpen] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const st = SCHEDULE_STATE[s.state]
  const asker = s.pendingBy ?? s.updatedBy
  const waiting = s.state === 'awaiting_approval' || Boolean(s.pending)
  const mine = asker.toLowerCase() === me.toLowerCase()
  const run = async (verb: 'approve' | 'decline' | 'pause' | 'resume' | 'delete') => {
    try {
      await act.mutateAsync({ id: s.id, verb })
      const done = { approve: 'Approved', decline: 'Declined', pause: 'Paused', resume: 'Resumed', delete: 'Deleted' }[verb]
      notifyOk(done, verb === 'resume' ? `${s.name} runs as you from the next transition.` : s.name)
    } catch (err) {
      notifyError('Could not do that', err)
    }
  }
  return (
    <Card padding="lg">
      <Group justify="space-between" align="flex-start" wrap="nowrap" mb="sm">
        <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
          <ThemeIcon variant="light" color={s.state === 'active' ? 'teal' : 'slate'} size={34} radius="md"><IconCalendarTime size={18} /></ThemeIcon>
          <Box style={{ minWidth: 0 }}>
            <Group gap="xs" wrap="wrap">
              <Text fw={700} size={FS.body} ff="monospace">{s.name}</Text>
              <Badge variant="light" color={st.color} radius="sm">{st.label}</Badge>
              {s.mode === 'enforce' && <Tooltip label="Every 15 minutes, instances in the wrong state are put right"><Badge variant="default" radius="sm" tt="none" fw={500}>Enforced</Badge></Tooltip>}
            </Group>
            <Text size={FS.micro} c="dimmed" mt={2}>{s.timezone} · {s.windows.map(windowWords).join('; ')}</Text>
          </Box>
        </Group>
        {admin && (
          <Menu position="bottom-end" withinPortal>
            <Menu.Target><ActionIcon variant="subtle" color="gray" aria-label={`More for ${s.name}`}><IconDots size={16} /></ActionIcon></Menu.Target>
            <Menu.Dropdown>
              <Menu.Item leftSection={<IconPencil size={14} />} onClick={() => void navigate({ to: '/cloud/schedule', search: { id: s.id } })}>Edit</Menu.Item>
              {s.state === 'active' && <Menu.Item leftSection={<IconPlayerPause size={14} />} onClick={() => void run('pause')}>Pause</Menu.Item>}
              {s.state === 'paused' && <Menu.Item leftSection={<IconPlayerPlay size={14} />} onClick={() => void run('resume')}>Resume as me</Menu.Item>}
              <Menu.Item leftSection={<IconCalendarTime size={14} />} onClick={() => setRunsOpen(true)}>Runs</Menu.Item>
              <Menu.Divider />
              <Menu.Item color="rose" leftSection={<IconTrash size={14} />} onClick={() => setConfirmDelete(true)}>Delete…</Menu.Item>
            </Menu.Dropdown>
          </Menu>
        )}
      </Group>

      {s.state === 'paused' && s.pausedReason && <Text size={FS.meta} c="dimmed" mb="sm">Paused: {s.pausedReason}.</Text>}
      {waiting && (
        <Alert color="amber" variant="light" p="xs" mb="sm" icon={<IconUserCheck size={16} />}>
          <Text size={FS.meta}>
            {s.pending ? <>An edit by <b>{asker}</b> waits for approval: {s.pending.windows.map(windowWords).join('; ')}. The version below keeps running.</>
              : <>Asked for by <b>{asker}</b>. It starts and stops Production instances, so someone else approves it.</>}
            {s.reason && <> “{s.reason}”</>}
          </Text>
          {decides && !mine && (
            <Group gap="xs" mt="xs">
              <Button size="compact-xs" variant="default" loading={act.isPending} onClick={() => void run('decline')}>Decline</Button>
              <Button size="compact-xs" loading={act.isPending} onClick={() => void run('approve')}>Approve</Button>
            </Group>
          )}
        </Alert>
      )}

      <WeekStrip windows={s.windows} />

      <SimpleGrid cols={3} spacing="sm" mt="md">
        <Stat label="Instances" value={String(s.matched)} hint={s.matched ? `${s.running} running now` : `None tagged ${SCHEDULE_TAG}=${s.name}`} />
        <Stat label="Runs" value={`${Math.round(s.hoursPerWeek)} h/week`} hint={`Off ${Math.round(168 - s.hoursPerWeek)} h`} />
        <Stat label="Saves about" value={s.saving.priced ? `${money(s.saving.monthly)}/mo` : '—'}
          hint={s.saving.unpriced ? `${s.saving.unpriced} size${s.saving.unpriced === 1 ? '' : 's'} not priced` : 'On-demand prices'} />
      </SimpleGrid>

      <Stack gap={4} mt="md">
        {s.state === 'active' && s.next.slice(0, 2).map((t) => (
          <Group key={t.at + t.action} gap={6} wrap="nowrap">
            {t.action === 'start' ? <IconPlayerPlay size={13} color="var(--mantine-color-teal-text)" /> : <IconPlayerStop size={13} color="var(--mantine-color-amber-text)" />}
            <Text size={FS.meta}>{t.action === 'start' ? 'Starts' : 'Stops'} {transitionWhen(t.at, s.timezone)}</Text>
            <Text size={FS.micro} c="dimmed">{relTime(t.at)}</Text>
          </Group>
        ))}
        {s.lastRun && (
          <Text size={FS.micro} c="dimmed">
            Last: {s.lastRun.action === 'start' ? 'start' : 'stop'} {relTime(s.lastRun.dueAt)} ·{' '}
            <Text span size={FS.micro} c={RUN_STATE[s.lastRun.state].color === 'rose' ? 'rose' : undefined}>{s.lastRun.detail}</Text>
          </Text>
        )}
        <Text size={FS.micro} c="dimmed">{s.state === 'awaiting_approval' ? 'Will run' : 'Runs'} as {s.owner}, so CloudTrail names them.</Text>
      </Stack>

      {runsOpen && <RunsModal s={s} onClose={() => setRunsOpen(false)} />}
      <Modal opened={confirmDelete} onClose={() => setConfirmDelete(false)} title={<Text fw={700}>Delete {s.name}?</Text>} centered>
        <Text size={FS.body}>Its instances stay as they are now, running or stopped, and nothing starts or stops them again. Their {SCHEDULE_TAG} tag stays.</Text>
        <Group justify="flex-end" mt="lg">
          <Button variant="default" onClick={() => setConfirmDelete(false)}>Keep it</Button>
          <Button color="rose" loading={act.isPending} onClick={() => { setConfirmDelete(false); void run('delete') }}>Delete</Button>
        </Group>
      </Modal>
    </Card>
  )
}

function Stat({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <Box style={{ minWidth: 0 }}>
      <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text fw={700} size="lg" lh={1.3}>{value}</Text>
      <Text size={FS.micro} c="dimmed" truncate>{hint}</Text>
    </Box>
  )
}

function RunsModal({ s, onClose }: { s: CloudScheduleView; onClose: () => void }) {
  const runs = useQuery(cloudScheduleRunsQuery(s.id))
  return (
    <Modal opened onClose={onClose} size="lg" title={<Text fw={700}>Runs of {s.name}</Text>}>
      {runs.isPending && <Text size={FS.meta} c="dimmed">Loading…</Text>}
      {runs.data?.length === 0 && <Text size={FS.meta} c="dimmed">Nothing has run yet. The first transition is {s.next[0] ? transitionWhen(s.next[0].at, s.timezone) : 'not scheduled'}.</Text>}
      <Stack gap="sm">
        {runs.data?.map((r) => (
          <Card key={r.id} withBorder padding="sm">
            <Group gap="xs">
              <Text fw={600} size={FS.body}>{r.action === 'start' ? 'Start' : 'Stop'} · {transitionWhen(r.dueAt, s.timezone)}</Text>
              <Badge variant="light" color={RUN_STATE[r.state].color} radius="sm">{RUN_STATE[r.state].label}</Badge>
            </Group>
            <Text size={FS.meta} mt={4}>{r.detail}</Text>
            {r.targets.filter((t) => t.status !== 'done').map((t) => (
              <Text key={t.accountId + t.id} size={FS.micro} c="dimmed">{t.name} ({t.id}): {t.status}{t.reason ? `, ${t.reason}` : ''}</Text>
            ))}
            {r.requestIds.length > 0 && <Text size={FS.micro} c="dimmed" ff="monospace" mt={2}>AWS {r.requestIds.join(', ')}</Text>}
          </Card>
        ))}
      </Stack>
    </Modal>
  )
}

const KEEP_FOR = [
  { value: '2', label: '2 hours' }, { value: '8', label: '8 hours' }, { value: '24', label: '1 day' },
  { value: '72', label: '3 days' }, { value: '168', label: '7 days' },
]

/**
 * In an instance's panel: the schedule it is on, what that does next, and
 * keep running for a while, which every schedule respects.
 */
export function InstanceSchedule({ it }: { it: CloudItem }) {
  const { data: me } = useQuery(meQuery())
  const q = useQuery(cloudSchedulesQuery())
  const accounts = useQuery(cloudAccountsQuery())
  const keepM = useKeepRunning()
  const [open, setOpen] = useState(false)
  const [hours, setHours] = useState('8')
  const [reason, setReason] = useState('')
  const name = scheduleOf(it.resource.tags)
  if (!name) return null
  const s = q.data?.schedules.find((x) => x.name.toLowerCase() === name.toLowerCase())
  const kept = q.data?.keepRunning.find((k) => k.accountId === it.accountId && k.instanceId === it.resource.id)
  const manage = canSee(me?.role, 'admin') && accounts.data?.accounts.find((a) => a.id === it.accountId)?.access === 'manage'
  const next = s?.state === 'active' ? s.next[0] : undefined
  const keep = async (until?: string) => {
    try {
      await keepM.mutateAsync({ it, until, reason })
      notifyOk(until ? 'Kept running' : 'Back on its schedule', until ? `${it.resource.name} stays up until ${absTime(until)}.` : it.resource.name)
      setOpen(false)
      setReason('')
    } catch (err) {
      notifyError('Could not do that', err)
    }
  }
  return (
    <Card withBorder padding="sm" radius="md">
      <Group gap="xs" wrap="nowrap" align="flex-start">
        <IconCalendarTime size={16} style={{ marginTop: 2, flexShrink: 0 }} />
        <Box style={{ flex: 1, minWidth: 0 }}>
          <Text size={FS.meta}>
            On schedule <Text span ff="monospace" fw={600} size={FS.meta}>{name}</Text>
            {!s && q.data && <Text span c={INK.pending} size={FS.meta}>, which does not exist; nothing starts or stops it</Text>}
            {s && s.state !== 'active' && <Text span c="dimmed" size={FS.meta}> ({SCHEDULE_STATE[s.state].label.toLowerCase()})</Text>}
          </Text>
          {next && !kept && <Text size={FS.micro} c="dimmed">{next.action === 'start' ? 'Starts' : 'Stops'} {transitionWhen(next.at, s!.timezone)} ({s!.timezone})</Text>}
          {kept && <Text size={FS.micro} c={INK.live}>Kept running until {absTime(kept.until)} by {kept.setBy}: “{kept.reason}”</Text>}
        </Box>
        {manage && s && (kept
          ? <Button size="compact-xs" variant="default" loading={keepM.isPending} onClick={() => void keep()}>Let it stop</Button>
          : <Button size="compact-xs" variant="default" leftSection={<IconClockPause size={13} />} onClick={() => setOpen(true)}>Keep running…</Button>)}
      </Group>
      <Modal opened={open} onClose={() => setOpen(false)} title={<Text fw={700}>Keep {it.resource.name} running</Text>} centered>
        <Text size={FS.meta} c="dimmed" mb="sm">Every schedule leaves it running until then, then acts on it at its next transition. No approval needed.</Text>
        <SegmentedControl fullWidth value={hours} onChange={setHours} data={KEEP_FOR} />
        <Textarea mt="md" label="Why" autosize minRows={2} value={reason} onChange={(e) => setReason(e.currentTarget.value)}
          placeholder="Load test runs overnight" />
        <Group justify="flex-end" mt="lg">
          <Button variant="default" onClick={() => setOpen(false)}>Cancel</Button>
          <Button disabled={reason.trim().length < 3} loading={keepM.isPending}
            onClick={() => void keep(new Date(Date.now() + Number(hours) * 3_600_000).toISOString())}>Keep running</Button>
        </Group>
      </Modal>
    </Card>
  )
}
