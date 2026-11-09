import {
  ActionIcon, Alert, Badge, Box, Button, Card, Group, MultiSelect, SegmentedControl, Select, SimpleGrid, Stack, Text, TextInput, Textarea, ThemeIcon, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconCalendarTime, IconPlayerPlay, IconPlayerStop, IconPlus, IconTrash, IconUserCheck, IconX } from '@tabler/icons-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { WeekStrip } from '~/components/cloud/Schedules'
import { EnvBadge } from '~/components/cloud/common'
import { PageBody, PageHeader } from '~/components/page'
import {
  DAY_SHORT, SCHEDULE_PRESETS, SCHEDULE_TAG, cloudAccountsQuery, cloudSchedulesQuery, money, previewSchedule, transitionWhen, useSaveSchedule,
  windowWords, type CloudSchedulePreview, type CloudScheduleVersion, type CloudWindow,
} from '~/lib/cloud'
import { guard } from '~/lib/guard'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS, INK, SP } from '~/theme'

/**
 * Make or change a schedule: its name (what instances are tagged with), its
 * zone, the weekly windows, which accounts and regions, and how strictly it
 * holds. The summary previews what it matches and saves, live, from the
 * control plane, which checks all of it again.
 */
export const Route = createFileRoute('/cloud/schedule')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  validateSearch: (s: Record<string, unknown>): { id?: string } => ({ id: typeof s.id === 'string' ? s.id : undefined }),
  component: ScheduleEditor,
})

const NAME = /^[A-Za-z0-9-]{1,63}$/

function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}

function allZones(): string[] {
  try {
    const z = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone')
    if (z?.length) return z.includes('UTC') ? z : ['UTC', ...z]
  } catch { /* older browsers */ }
  return ['UTC', 'Asia/Jakarta', 'Asia/Singapore', 'Asia/Tokyo', 'Australia/Sydney', 'Europe/Berlin', 'Europe/London', 'America/New_York', 'America/Los_Angeles']
}

const EMPTY: CloudScheduleVersion = {
  name: '', timezone: browserZone(), windows: SCHEDULE_PRESETS[0]!.windows, skipDates: [], accountIds: [], regions: [], mode: 'transitions',
}

function ScheduleEditor() {
  const navigate = useNavigate()
  const { id } = Route.useSearch()
  const list = useQuery(cloudSchedulesQuery())
  const accounts = useQuery(cloudAccountsQuery())
  const existing = list.data?.schedules.find((s) => s.id === id)
  const [v, setV] = useState<CloudScheduleVersion>(EMPTY)
  const [reason, setReason] = useState('')
  const [loaded, setLoaded] = useState(!id)
  useEffect(() => {
    if (existing && !loaded) {
      const src = existing.pending ?? existing
      setV({ name: src.name, timezone: src.timezone, windows: src.windows, skipDates: src.skipDates, accountIds: src.accountIds, regions: src.regions, mode: src.mode })
      setLoaded(true)
    }
  }, [existing, loaded])
  const set = <K extends keyof CloudScheduleVersion>(k: K, val: CloudScheduleVersion[K]) => setV((x) => ({ ...x, [k]: val }))

  // The preview follows the form, a moment after the last keystroke.
  const [preview, setPreview] = useState<CloudSchedulePreview | null>(null)
  const [previewErr, setPreviewErr] = useState<string | null>(null)
  useEffect(() => {
    const t = window.setTimeout(() => {
      previewSchedule(v).then((p) => { setPreview(p); setPreviewErr(null) }).catch((e: Error) => setPreviewErr(e.message))
    }, 350)
    return () => window.clearTimeout(t)
  }, [v])

  const zones = useMemo(allZones, [])
  const acctList = accounts.data?.accounts ?? []
  const regions = useMemo(() => [...new Set(acctList.flatMap((a) => a.regions))].sort(), [acctList])
  const nameOk = NAME.test(v.name)
  const windowsOk = v.windows.length > 0 && v.windows.every((w) => w.days.length > 0 && w.start && w.stop && (w.stopDay || w.start !== w.stop))
  const approval = preview?.needsApproval ?? false
  const ready = nameOk && windowsOk && Boolean(v.timezone) && (!approval || reason.trim().length >= 5)
  const save = useSaveSchedule()
  const submit = async () => {
    try {
      const s = await save.mutateAsync({ id, version: v, reason })
      if (s.state === 'awaiting_approval' || s.pending) notifyOk('Asked for approval', `${s.name} starts and stops Production instances; someone else approves it on Cloud › Schedules.`)
      else notifyOk(id ? 'Saved' : 'Schedule made', `${s.name} runs from its next transition, as you.`)
      void navigate({ to: '/cloud', search: { tab: 'schedules' } })
    } catch (err) {
      notifyError('Could not save the schedule', err)
    }
  }
  const title = id ? `Edit ${existing?.name ?? 'schedule'}` : 'Make a schedule'

  return (
    <Box>
      <PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Schedules', to: '/cloud?tab=schedules' }, { label: title }]} title={title}
        description={`Stops instances outside the hours they are needed and starts them again, as you. An instance joins with the tag ${SCHEDULE_TAG}=<name>.`} />
      <PageBody>
        <Box style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 340px), 1fr))', gap: 20, alignItems: 'start' }}>
          <Stack gap="md" style={{ gridColumn: 'span 2', minWidth: 0 }} className="argus-launch-form">
            <Section n={1} title="Name and time zone">
              <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                <TextInput label="Name" description={`Instances are tagged ${SCHEDULE_TAG}=${v.name || '<name>'}`} placeholder="office-hours"
                  value={v.name} onChange={(e) => set('name', e.currentTarget.value.trim())} maxLength={63} data-autofocus
                  styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
                  error={v.name && !nameOk ? 'Letters, digits and dashes' : undefined} />
                <Select label="Time zone" description="Windows are in this zone's clock, daylight saving included" searchable
                  value={v.timezone} onChange={(z) => z && set('timezone', z)} data={zones} allowDeselect={false} />
              </SimpleGrid>
            </Section>

            <Section n={2} title="When it runs">
              <Text size={FS.meta} c="dimmed" mb="xs">Start from one of these, then change anything.</Text>
              <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm" mb="md">
                {SCHEDULE_PRESETS.map((p) => {
                  const on = JSON.stringify(p.windows) === JSON.stringify(v.windows)
                  return (
                    <Card key={p.key} withBorder padding="sm" radius="md" style={{ cursor: 'pointer', borderColor: on ? 'var(--mantine-color-azure-filled)' : undefined }}
                      onClick={() => set('windows', p.windows)} role="button" aria-pressed={on} tabIndex={0}
                      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); set('windows', p.windows) } }}>
                      <Text fw={600} size={FS.body}>{p.label}</Text>
                      <Text size={FS.micro} c="dimmed">{p.text}</Text>
                    </Card>
                  )
                })}
              </SimpleGrid>
              <Stack gap="sm">
                {v.windows.map((w, i) => (
                  <WindowRow key={i} w={w} onChange={(nw) => set('windows', v.windows.map((x, j) => (j === i ? nw : x)))}
                    onRemove={v.windows.length > 1 ? () => set('windows', v.windows.filter((_, j) => j !== i)) : undefined} />
                ))}
              </Stack>
              <Button mt="sm" size="xs" variant="default" leftSection={<IconPlus size={14} />} disabled={v.windows.length >= 21}
                onClick={() => set('windows', [...v.windows, { days: [6], start: '09:00', stop: '13:00' }])}>Add a window</Button>
            </Section>

            <Section n={3} title="Which instances">
              <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                <MultiSelect label="Accounts" placeholder={v.accountIds.length ? undefined : 'Every account'} value={v.accountIds}
                  onChange={(x) => set('accountIds', x)} data={acctList.map((a) => ({ value: a.id, label: a.name }))} clearable />
                <MultiSelect label="Regions" placeholder={v.regions.length ? undefined : 'Every region'} value={v.regions}
                  onChange={(x) => set('regions', x)} data={regions} searchable clearable />
              </SimpleGrid>
              <Text size="sm" fw={500} mt="md" mb={6}>Tagged {SCHEDULE_TAG}={v.name || '<name>'} now</Text>
              {!preview && <Text size={FS.meta} c="dimmed">Looking…</Text>}
              {preview && preview.instances.length === 0 && (
                <Text size={FS.meta} c="dimmed">No instance has the tag yet. Tag one from its panel under Cloud › Instances, or pick this schedule when you launch one.</Text>
              )}
              <Stack gap={6}>
                {preview?.instances.map((it) => (
                  <Group key={it.accountId + it.id} gap="sm" wrap="nowrap">
                    <Box style={{ flex: 1, minWidth: 0 }}>
                      <Text size={FS.body} fw={600} truncate>{it.name}</Text>
                      <Text size={FS.micro} c="dimmed" truncate>{it.accountName} · {it.region} · <Text span ff="monospace" size={FS.micro}>{it.size}</Text></Text>
                      {it.note && <Text size={FS.micro} c={INK.pending}>{it.note}</Text>}
                    </Box>
                    <EnvBadge env={it.environment} />
                    <Badge variant="light" color={it.state === 'running' ? 'teal' : 'slate'} radius="sm">{it.state}</Badge>
                  </Group>
                ))}
              </Stack>
            </Section>

            <Section n={4} title="Mode and skip dates">
              <SegmentedControl value={v.mode} onChange={(m) => set('mode', m as CloudScheduleVersion['mode'])}
                data={[{ value: 'transitions', label: 'At each start and stop' }, { value: 'enforce', label: 'Enforce every 15 minutes' }]} />
              <Text size={FS.meta} c="dimmed" mt={6}>
                {v.mode === 'enforce'
                  ? 'Also puts instances back every quarter hour: one started by hand at night is stopped again. Keep running still wins.'
                  : 'Acts only when a window opens or closes. An instance started by hand in between stays up until the next stop.'}
              </Text>
              <SkipDates dates={v.skipDates} onChange={(d) => set('skipDates', d)} />
            </Section>
          </Stack>

          <Card padding="lg" style={{ position: 'sticky', top: 80 }}>
            <Group gap="sm" mb="md">
              <ThemeIcon variant="light" color="teal" size={32} radius="md"><IconCalendarTime size={18} /></ThemeIcon>
              <Box style={{ minWidth: 0 }}>
                <Text fw={700} ff="monospace" truncate>{nameOk ? v.name : 'New schedule'}</Text>
                <Text size={FS.micro} c="dimmed">{v.timezone}</Text>
              </Box>
            </Group>
            <WeekStrip windows={v.windows} />
            <Stack gap={SP.snug} mt="md">
              <Row k="Instances" v={preview ? String(preview.instances.length) : '—'} />
              <Row k="Runs" v={preview ? `${Math.round(preview.hoursPerWeek)} hours a week` : '—'} />
              <Row k="Saves about" v={preview?.saving.priced ? `${money(preview.saving.monthly)} a month` : '—'} />
              <Row k="Mode" v={v.mode === 'enforce' ? 'Enforced' : 'At transitions'} />
            </Stack>
            {preview && preview.saving.unpriced > 0 && (
              <Text size={FS.micro} c="dimmed" mt={4}>{preview.saving.unpriced} instance{preview.saving.unpriced === 1 ? '' : 's'} of a size the estimate does not price.</Text>
            )}
            <Text size={FS.micro} c="dimmed" mt={4}>At us-east-1 on-demand prices; disks are billed either way.</Text>
            {preview && preview.transitions.length > 0 && (
              <Box mt="md">
                <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} mb={4}>Next</Text>
                {preview.transitions.slice(0, 4).map((t) => (
                  <Group key={t.at + t.action} gap={6} wrap="nowrap">
                    {t.action === 'start' ? <IconPlayerPlay size={13} color="var(--mantine-color-teal-text)" /> : <IconPlayerStop size={13} color="var(--mantine-color-amber-text)" />}
                    <Text size={FS.meta}>{t.action === 'start' ? 'Start' : 'Stop'} {transitionWhen(t.at, v.timezone)}</Text>
                  </Group>
                ))}
              </Box>
            )}
            {previewErr && <Text size={FS.meta} c={INK.denied} mt="sm">{previewErr}</Text>}
            {approval && (
              <Alert color="amber" variant="light" mt="md" p="sm" icon={<IconUserCheck size={16} />}>
                <Text size={FS.meta}>It reaches Production, so someone else approves it first{id ? '; the version in effect keeps running until then' : ''}. It then runs as you.</Text>
              </Alert>
            )}
            <Textarea mt="md" label={approval ? 'Why' : 'Why (optional)'} autosize minRows={2} value={reason}
              onChange={(e) => setReason(e.currentTarget.value)} placeholder="Staging is idle at night and on weekends" />
            <Button fullWidth mt="md" size="md" leftSection={approval ? <IconUserCheck size={17} /> : <IconCalendarTime size={17} />}
              disabled={!ready} loading={save.isPending} onClick={() => void submit()}>
              {approval ? 'Ask for approval' : id ? 'Save' : 'Make the schedule'}
            </Button>
            <Text size={FS.micro} c="dimmed" mt="xs" ta="center">Someone with a session open on an instance gets up to an hour before it stops.</Text>
          </Card>
        </Box>
      </PageBody>
    </Box>
  )
}

function WindowRow({ w, onChange, onRemove }: { w: CloudWindow; onChange: (w: CloudWindow) => void; onRemove?: () => void }) {
  const toggle = (d: number) => {
    if (w.stopDay) { onChange({ ...w, days: [d] }); return }
    onChange({ ...w, days: w.days.includes(d) ? w.days.filter((x) => x !== d) : [...w.days, d].sort((a, b) => a - b) })
  }
  return (
    <Card withBorder padding="sm" radius="md">
      <Group justify="space-between" wrap="nowrap" mb="xs">
        <Text size={FS.meta} fw={600}>{windowWords(w)}</Text>
        {onRemove && <Tooltip label="Remove this window"><ActionIcon variant="subtle" color="gray" onClick={onRemove} aria-label="Remove this window"><IconTrash size={15} /></ActionIcon></Tooltip>}
      </Group>
      <Group gap={4} mb="sm">
        {DAY_SHORT.map((d, i) => (
          <Button key={d} size="compact-sm" radius="xl" fz={FS.meta} fw={500} variant={w.days.includes(i + 1) ? 'light' : 'default'}
            aria-pressed={w.days.includes(i + 1)} onClick={() => toggle(i + 1)}>{d}</Button>
        ))}
      </Group>
      <SimpleGrid cols={{ base: 1, xs: 3 }} spacing="sm">
        <TextInput type="time" label="Starts" value={w.start} onChange={(e) => onChange({ ...w, start: e.currentTarget.value })} />
        <Select label="Stops on" value={w.stopDay ? String(w.stopDay) : ''} allowDeselect={false}
          data={[{ value: '', label: 'Same day (or next)' }, ...DAY_SHORT.map((d, i) => ({ value: String(i + 1), label: d }))]}
          onChange={(x) => onChange({ ...w, stopDay: x ? Number(x) : undefined, days: x ? [w.days[0] ?? 1] : w.days })} />
        <TextInput type="time" label="Stops" value={w.stop} onChange={(e) => onChange({ ...w, stop: e.currentTarget.value })} />
      </SimpleGrid>
    </Card>
  )
}

function SkipDates({ dates, onChange }: { dates: string[]; onChange: (d: string[]) => void }) {
  const [d, setD] = useState('')
  const add = () => { if (/^\d{4}-\d{2}-\d{2}$/.test(d) && !dates.includes(d)) onChange([...dates, d].sort()); setD('') }
  return (
    <Box mt="md">
      <Text size="sm" fw={500}>Skip dates</Text>
      <Text size={FS.meta} c="dimmed" mb={6}>Holidays: no window opens on these days, in the schedule's zone.</Text>
      <Group gap="xs" align="flex-end">
        <TextInput type="date" value={d} onChange={(e) => setD(e.currentTarget.value)} aria-label="A date to skip" w={180} />
        <Button size="sm" variant="default" onClick={add} disabled={!d}>Add</Button>
      </Group>
      {dates.length > 0 && (
        <Group gap={6} mt="xs">
          {dates.map((x) => (
            <Badge key={x} variant="default" radius="sm" tt="none" fw={500} rightSection={
              <ActionIcon size={14} variant="transparent" color="gray" aria-label={`Do not skip ${x}`} onClick={() => onChange(dates.filter((y) => y !== x))}><IconX size={11} /></ActionIcon>
            }>{x}</Badge>
          ))}
        </Group>
      )}
    </Box>
  )
}

function Section({ n, title, children }: { n: number; title: string; children: ReactNode }) {
  return (
    <Card padding="lg">
      <Group gap="sm" mb="md">
        <ThemeIcon size={24} radius="xl" variant="light" color="azure"><Text size={FS.micro} fw={700}>{n}</Text></ThemeIcon>
        <Text fw={700}>{title}</Text>
      </Group>
      {children}
    </Card>
  )
}

function Row({ k, v }: { k: string; v?: string | null }) {
  return (
    <Group justify="space-between" wrap="nowrap" gap="md">
      <Text size={FS.meta} c="dimmed" style={{ flexShrink: 0 }}>{k}</Text>
      <Text size={FS.meta} ta="right" truncate>{v}</Text>
    </Group>
  )
}
