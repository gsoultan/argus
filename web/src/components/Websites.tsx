import {
  ActionIcon, Alert, Badge, Box, Button, Drawer, Group, Menu, Modal, MultiSelect, NumberInput, PasswordInput, SegmentedControl, Select,
  SimpleGrid, Skeleton, Stack, Switch, Table, Text, Textarea, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconBrandTelegram, IconDots, IconEye, IconFolderPlus, IconPlayerPause, IconPlayerPlay, IconPlus, IconRefresh, IconShieldCheck, IconWorld, IconX } from '@tabler/icons-react'
import { useMemo, useState, type ReactNode } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { GroupForm, WhoCanSee } from '~/components/CheckAccess'
import { assetName } from '~/lib/assetType'
import { isConfigured } from '~/lib/live'
import { notifyOk, run } from '~/lib/notify'
import {
  botLabel, botOptions, chatOption, chatsOf, telegramQuery, useSetRoute, type TelegramView,
} from '~/lib/telegram'
import { assetsQuery, meQuery } from '~/lib/queries'
import {
  ago, certDaysLeft, testWebsiteAlert, formatDuration, formatUptime, outageMinutes, requestSummary, sendsBody, sliceRounds, SITE_METHODS,
  checkGroupsQuery, rollUp, stateLabel, testWebsite, useAddWebsite, useChangeWebsite, useCheckWebsiteNow, useMoveCheck, usePauseWebsite,
  useRemoveCheckGroup, useRemoveWebsite, websiteQuery, websitesQuery, type CheckGroup,
  type HeaderInput, type SiteMethod, type SiteResult, type TelegramChoice, type WebsiteCheck, type WebsiteInput, type WebsiteRound,
} from '~/lib/websites'
import { FS, SP } from '~/theme'

/**
 * Websites: is each site up, and how much of the time.
 *
 * What the page claims comes from the control plane, which only calls a site
 * down after retries, a check of Argus's own network, the chosen servers and
 * a second round all agree. The page says that next to the list, because an
 * operator deciding whether to wake someone needs to know how sure "Down" is.
 */
export function WebsitesPage({ openId }: { openId?: string }) {
  const navigate = useNavigate()
  const { data: me } = useQuery(meQuery())
  const canEdit = me?.role === 'admin' || me?.role === 'owner'
  const { data: list, isLoading, error } = useQuery({ ...websitesQuery(), enabled: isConfigured() })
  const { data: groups } = useQuery({ ...checkGroupsQuery(), enabled: isConfigured() })
  const [editing, setEditing] = useState<WebsiteCheck | 'new' | null>(null)
  const [alerting, setAlerting] = useState(false)
  const [grouping, setGrouping] = useState<CheckGroup | 'new' | null>(null)
  const [sharing, setSharing] = useState<CheckGroup | null>(null)
  const open = (id?: string) => void navigate({ to: '/websites', search: id ? { check: id } : {}, replace: true })

  const down = list?.filter((c) => !c.paused && c.state === 'down').length ?? 0
  const up = list?.filter((c) => !c.paused && c.state === 'up').length ?? 0
  const limited = list?.some((c) => c.limited) ?? false
  const sections = useMemo(() => sectionsOf(list ?? [], groups ?? [], canEdit), [list, groups, canEdit])

  return (
    <Box>
      <PageHeader
        title="Websites"
        description={limited
          ? 'The websites you were given. Argus checks each one every minute and tells you in the bell when one goes down and when it comes back.'
          : 'Argus checks each address every minute and tells admins, and whoever was given a site, when it goes down and when it comes back.'}
        status={list && list.length > 0 ? (
          down > 0
            ? <Badge color="rose" variant="filled">{down} down</Badge>
            : <Badge color="teal" variant="light">{up} up</Badge>
        ) : undefined}
        actions={canEdit && isConfigured() ? (
          <>
            <Button variant="default" leftSection={<IconBrandTelegram size={14} />} onClick={() => setAlerting(true)}>Telegram alerts</Button>
            <Button variant="default" leftSection={<IconFolderPlus size={14} />} onClick={() => setGrouping('new')}>New group</Button>
            <Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add website</Button>
          </>
        ) : undefined}
      />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to check websites.</Text></Alert>
        ) : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read the website checks: {error.message}</Text></Alert>
        ) : (
          <Stack gap="lg">
            {isLoading || list?.length === 0 || sections.length === 0 ? (
              <SiteTable list={list ?? []} loading={isLoading} onOpen={open}
                empty={<EmptyState icon={IconWorld} title="No websites are checked yet."
                  description={canEdit ? 'Add an address and Argus starts checking it at once.' : 'An admin can add the addresses to check.'}
                  action={canEdit ? <Button size="xs" onClick={() => setEditing('new')}>Add website</Button> : undefined} />} />
            ) : sections.map((sec) => (
              <Box key={sec.group?.id ?? 'none'} data-testid="check-group">
                {(sections.length > 1 || sec.group) && (
                  <Group justify="space-between" mb={SP.snug} wrap="nowrap">
                    <Group gap="xs" wrap="nowrap">
                      <Text fw={600} size="sm">{sec.group?.name ?? 'Not in a group'}</Text>
                      <RollUpBadge list={sec.checks} />
                      <Text size={FS.meta} c="dimmed">{sec.checks.length} {sec.checks.length === 1 ? 'site' : 'sites'}</Text>
                    </Group>
                    {canEdit && sec.group && (
                      <GroupMenu group={sec.group} onShare={() => setSharing(sec.group!)} onRename={() => setGrouping(sec.group!)} />
                    )}
                  </Group>
                )}
                <SiteTable list={sec.checks} onOpen={open}
                  empty={<Text size={FS.body} c="dimmed" p="md">No checks in this group yet. Open a check and choose this group for it.</Text>} />
              </Box>
            ))}
            <HowSure />
          </Stack>
        )}
      </PageBody>
      {openId && (
        <WebsiteDrawer id={openId} canEdit={canEdit} groups={groups ?? []} onClose={() => open()}
          onEdit={(c) => setEditing(c)} />
      )}
      {alerting && <AlertSettings onClose={() => setAlerting(false)} />}
      {grouping && <GroupForm group={grouping === 'new' ? undefined : grouping} onClose={() => setGrouping(null)} />}
      {sharing && <WhoCanSee target="group" id={sharing.id} name={sharing.name} onClose={() => setSharing(null)} />}
      {editing && (
        <WebsiteForm check={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)}
          onSaved={(c) => { setEditing(null); open(c.id) }} />
      )}
    </Box>
  )
}

/** What sectionsOf needs of a check, of any kind. */
type Sectioned = Pick<WebsiteCheck, 'state' | 'paused' | 'groupId'>

const SEVERITY: Record<string, number> = { down: 0, unknown: 1, pending: 2, up: 3, paused: 4, empty: 5 }

/**
 * The checks by group, problems first: groups with a site down come before
 * the rest, and inside a group a site down comes before one that is up.
 * Checks in no group come last. An empty group is shown to admins only, so
 * they can fill it.
 */
export function sectionsOf<C extends Sectioned>(list: C[], groups: CheckGroup[], showEmpty: boolean): { group?: CheckGroup; checks: C[] }[] {
  const rank = (c: C) => SEVERITY[c.paused ? 'paused' : c.state] ?? 9
  const byState = (a: C, b: C) => rank(a) - rank(b)
  const known = new Set(groups.map((g) => g.id))
  const out = groups
    .map((g) => ({ group: g, checks: list.filter((c) => c.groupId === g.id).sort(byState) }))
    .filter((s) => s.checks.length > 0 || showEmpty)
    .sort((a, b) => SEVERITY[rollUp(a.checks)]! - SEVERITY[rollUp(b.checks)]! || a.group.name.localeCompare(b.group.name))
  const loose = list.filter((c) => !c.groupId || !known.has(c.groupId)).sort(byState)
  return loose.length > 0 ? [...out, { checks: loose }] : out
}

export function RollUpBadge({ list }: { list: Pick<WebsiteCheck, 'state' | 'paused' | 'failingRounds'>[] }) {
  const r = rollUp(list)
  if (r === 'empty') return null
  const n = list.filter((c) => !c.paused && c.state === 'down').length
  const [label, color] = r === 'down' ? [`${n} down`, 'rose'] : r === 'unknown' ? ['Not sure', 'amber']
    : r === 'pending' ? ['Waiting', 'slate'] : r === 'paused' ? ['Paused', 'slate'] : ['All up', 'teal']
  return <Badge size="sm" color={color} variant={r === 'down' ? 'filled' : 'light'}>{label}</Badge>
}

export function GroupMenu({ group, onShare, onRename }: { group: CheckGroup; onShare: () => void; onRename: () => void }) {
  const remove = useRemoveCheckGroup()
  const [asking, setAsking] = useState(false)
  return (
    <>
      <Group gap="xs" wrap="nowrap">
        <Button size="compact-sm" variant="default" leftSection={<IconEye size={14} />} onClick={onShare}>Who can see this</Button>
        <Menu position="bottom-end" withinPortal>
          <Menu.Target>
            <ActionIcon variant="subtle" color="gray" aria-label={`More for ${group.name}`}><IconDots size={16} /></ActionIcon>
          </Menu.Target>
          <Menu.Dropdown>
            <Menu.Item onClick={onRename}>Rename</Menu.Item>
            <Menu.Item color="rose" onClick={() => setAsking(true)}>Remove group</Menu.Item>
          </Menu.Dropdown>
        </Menu>
      </Group>
      <Modal opened={asking} onClose={() => setAsking(false)} title={`Remove ${group.name}?`}>
        <Stack>
          <Text size={FS.body}>Its checks stay, in no group. Whoever was given the group stops seeing them, unless they were given a check on its own.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setAsking(false)}>Keep it</Button>
            <Button color="rose" loading={remove.isPending} onClick={() => void run(() => remove.mutateAsync(group.id), {
              failure: 'Not removed', success: ['Removed', `${group.name} is removed. Its checks are in no group now.`],
            }).then(() => setAsking(false))}>Remove</Button>
          </Group>
        </Stack>
      </Modal>
    </>
  )
}

function SiteTable({ list, loading = false, empty, onOpen }: { list: WebsiteCheck[]; loading?: boolean; empty: ReactNode; onOpen: (id: string) => void }) {
  return (
    <DataTable
      columns={['Website', 'Status', 'Response', 'Last 24 hours', '7 days', '30 days', 'Checked']}
      minWidth={820}
      loading={loading}
      isEmpty={list.length === 0}
      empty={empty}
    >
      {list.map((c) => <WebsiteRow key={c.id} c={c} onOpen={() => onOpen(c.id)} />)}
    </DataTable>
  )
}

function StateBadge({ c }: { c: WebsiteCheck }) {
  const s = stateLabel(c)
  return <Badge color={s.color} variant={s.color === 'rose' ? 'filled' : 'light'}>{s.label}</Badge>
}

function WebsiteRow({ c, onOpen }: { c: WebsiteCheck; onOpen: () => void }) {
  const since = c.state === 'down' && c.openOutage ? c.openOutage.startedAt : c.stateSince
  return (
    <Table.Tr style={{ cursor: 'pointer' }} onClick={onOpen}>
      <Table.Td>
        <UnstyledButton onClick={onOpen} aria-label={`Open ${c.name}`}>
          <Text size="sm" fw={600}>{c.name}</Text>
          <Text size={FS.micro} c="dimmed" ff="monospace" style={{ overflowWrap: 'anywhere' }}>{c.url}</Text>
        </UnstyledButton>
      </Table.Td>
      <Table.Td>
        <Stack gap={SP.hair}>
          <Group gap={SP.tight}><StateBadge c={c} /></Group>
          {!c.paused && c.state === 'up' && c.lastOutcome === 'partial' ? (
            <Tooltip label={c.lastDetail} multiline maw={360}>
              <Text size={FS.micro} c="amber">Argus cannot reach it; another server can</Text>
            </Tooltip>
          ) : since && !c.paused && c.state !== 'pending' && c.failingRounds === 0 && (
            <Text size={FS.micro} c="dimmed">for {formatDuration(Math.max(1, Math.round((Date.now() - Date.parse(since)) / 60_000)))}</Text>
          )}
        </Stack>
      </Table.Td>
      <Table.Td><Text size="sm">{c.lastMs != null && c.state === 'up' && (c.lastOutcome === 'up' || c.lastOutcome === 'partial') ? `${c.lastMs} ms` : '—'}</Text></Table.Td>
      <Table.Td><UptimeText v={c.uptime.day} /></Table.Td>
      <Table.Td><UptimeText v={c.uptime.week} /></Table.Td>
      <Table.Td><UptimeText v={c.uptime.month} /></Table.Td>
      <Table.Td><Text size={FS.meta} c="dimmed">{c.paused ? 'paused' : ago(c.lastAt)}</Text></Table.Td>
    </Table.Tr>
  )
}

export function UptimeText({ v }: { v: number | null }) {
  const color = v == null ? 'dimmed' : v >= 99.9 ? undefined : v >= 99 ? 'amber' : 'rose'
  return <Text size="sm" c={color} fw={v != null && v < 99 ? 600 : undefined}>{formatUptime(v)}</Text>
}

/** How sure "Down" is, in the words the alert uses. */
export function HowSure() {
  return (
    <SectionCard title="How Argus makes sure a site is really down" icon={IconShieldCheck} iconColor="teal">
      <Stack gap={SP.cozy}>
        {[
          ['One blip never counts.', 'A failed check is tried twice more, 20 seconds apart. Any success and the site is up.'],
          ['Argus checks its own connection first.', 'If Argus cannot reach the internet or look up names either, the result is "Cannot check". That is never down, and is left out of uptime.'],
          ['Other places must agree.', 'The servers you choose for a site check it too. If any of them can reach it, it is not down; the site shows which server could.'],
          ['It must still fail a minute later.', 'Down needs two failed rounds in a row. The outage then counts from the first one.'],
          ['Slow is not down.', 'Only no answer within the timeout (30 seconds unless you change it), a connection or certificate error, a wrong status, or a missing word counts as a failure.'],
        ].map(([t, d]) => (
          <Text key={t} size={FS.body}><Text span fw={600}>{t}</Text> <Text span c="dimmed">{d}</Text></Text>
        ))}
      </Stack>
    </SectionCard>
  )
}

/* ── One site ─────────────────────────────────────────────────────────────── */

function WebsiteDrawer({ id, canEdit, groups, onClose, onEdit }: {
  id: string; canEdit: boolean; groups: CheckGroup[]; onClose: () => void; onEdit: (c: WebsiteCheck) => void
}) {
  const pause = usePauseWebsite()
  const move = useMoveCheck()
  const [sharing, setSharing] = useState(false)
  const { data, isLoading, error } = useQuery(websiteQuery(id))
  const checkNow = useCheckWebsiteNow()
  const remove = useRemoveWebsite()
  const change = useChangeWebsite()
  const [testingAlert, setTestingAlert] = useState(false)
  const { data: tg } = useQuery({ ...telegramQuery(), enabled: canEdit && isConfigured() })
  const c = data?.check
  const cert = certDaysLeft(c?.certNotAfter)

  return (
    <Drawer opened onClose={onClose} position="right" size="lg"
      title={c ? <Group gap="xs"><Text fw={700} size="lg">{c.name}</Text><StateBadge c={c} /></Group> : null}>
      {isLoading ? <Skeleton h={300} /> : error || !data || !c ? (
        <Alert color="rose"><Text size={FS.body}>Could not read this check: {error?.message ?? 'it may have been removed'}</Text></Alert>
      ) : (
        <Stack gap="md">
          <Text size={FS.meta} ff="monospace" c="dimmed" style={{ overflowWrap: 'anywhere' }}>{c.url}</Text>
          {c.lastDetail && (
            <Alert color={c.state === 'down' ? 'rose' : c.state === 'unknown' || c.failingRounds > 0 ? 'amber' : 'slate'} variant="light">
              <Text size={FS.body}>
                {c.state === 'down' && c.openOutage
                  ? `Down since ${new Date(c.openOutage.startedAt).toLocaleString()}: `
                  : c.lastAt ? `Last checked ${ago(c.lastAt)}: ` : ''}
                {c.lastDetail}
              </Text>
            </Alert>
          )}
          <SimpleGrid cols={{ base: 2, sm: 4 }} spacing="sm">
            <Figure label="Last 24 hours" value={formatUptime(c.uptime.day)} />
            <Figure label="7 days" value={formatUptime(c.uptime.week)} />
            <Figure label="30 days" value={formatUptime(c.uptime.month)} />
            <Figure label="Certificate" value={cert == null ? '—' : cert < 0 ? 'Expired' : `${cert} days left`}
              warn={cert != null && cert < 14} />
          </SimpleGrid>
          <RoundStrip rounds={data.rounds} />

          <Box>
            <Text fw={600} size="sm" mb={SP.snug}>Outages</Text>
            {data.outages.length === 0 ? (
              <Text size={FS.body} c="dimmed">No outages recorded.</Text>
            ) : (
              <Table.ScrollContainer minWidth={420}>
                <Table verticalSpacing="xs">
                  <Table.Thead><Table.Tr><Table.Th>Started</Table.Th><Table.Th>Lasted</Table.Th><Table.Th>Why</Table.Th></Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {data.outages.map((o) => (
                      <Table.Tr key={o.id}>
                        <Table.Td><Text size={FS.meta}>{new Date(o.startedAt).toLocaleString()}</Text></Table.Td>
                        <Table.Td><Text size={FS.meta} c={o.endedAt ? undefined : 'rose'} fw={o.endedAt ? undefined : 600}>
                          {o.endedAt ? formatDuration(outageMinutes(o)) : `ongoing, ${formatDuration(outageMinutes(o))}`}
                        </Text></Table.Td>
                        <Table.Td>
                          <Text size={FS.meta}>{o.cause}</Text>
                          {o.confirmed && <Text size={FS.micro} c="dimmed">Confirmed by {o.confirmed}</Text>}
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            )}
          </Box>

          <Box>
            <Text fw={600} size="sm" mb={SP.snug}>How it is checked</Text>
            <Stack gap={SP.tight}>
              <Text size={FS.body}>Every {c.everySeconds / 60} minute{c.everySeconds === 60 ? '' : 's'}, waiting up to {c.timeoutSeconds} seconds for an answer.</Text>
              {requestSummary(c) && (
                <Text size={FS.body}>Sends {requestSummary(c)}.{(c.headerNames ?? []).length > 0 ? ' Header values are kept encrypted and never shown.' : ''}</Text>
              )}
              <Text size={FS.body}>{c.expectStatus ? `Must answer ${c.expectStatus}.` : 'Any answer below 400, after redirects, is up.'}{c.keyword ? ` The page must contain "${c.keyword}".` : ''}</Text>
              {!c.limited && (
                <>
                  <Text size={FS.body}>{c.confirmHosts.length > 0
                    ? `Before calling it down, Argus asks ${c.confirmHosts.join(', ')} to check too.`
                    : 'Only Argus checks it. Choose servers to confirm from, so a problem on Argus\'s side is never called the site being down.'}</Text>
                  <Text size={FS.body}>{telegramSentence(c, tg)}</Text>
                </>
              )}
            </Stack>
          </Box>

          {canEdit && (
            <Box>
              <Text fw={600} size="sm" mb={SP.snug}>Group and who can see it</Text>
              <Group gap="xs" align="flex-end">
                <Select size="xs" w={220} aria-label="Group" placeholder="No group" clearable
                  value={c.groupId || null}
                  data={groups.map((g) => ({ value: g.id, label: g.name }))}
                  nothingFoundMessage="No groups yet: add one with New group"
                  onChange={(v) => void run(() => move.mutateAsync({ id: c.id, groupId: v ?? '' }), {
                    failure: 'Not moved',
                    success: ['Moved', v ? `${c.name} is in ${groups.find((g) => g.id === v)?.name}.` : `${c.name} is in no group.`],
                  })} />
                <Button size="xs" variant="default" leftSection={<IconEye size={14} />} onClick={() => setSharing(true)}>Who can see this</Button>
              </Group>
              {sharing && <WhoCanSee target="check" id={c.id} name={c.name} onClose={() => setSharing(false)} />}
            </Box>
          )}

          {!canEdit && c.canOperate && (
            <Group gap="xs">
              <Button size="xs" variant="default" leftSection={<IconRefresh size={14} />} disabled={c.paused}
                loading={checkNow.isPending}
                onClick={() => void run(() => checkNow.mutateAsync(c.id), { failure: 'Not checked', success: ['Checking', 'The result shows here in a few seconds.'] })}>
                Check now
              </Button>
              <Button size="xs" variant="default" loading={pause.isPending}
                leftSection={c.paused ? <IconPlayerPlay size={14} /> : <IconPlayerPause size={14} />}
                onClick={() => void run(() => pause.mutateAsync({ id: c.id, paused: !c.paused }), {
                  failure: c.paused ? 'Not resumed' : 'Not paused',
                  success: c.paused ? ['Resumed', `${c.name} is checked again.`] : ['Paused', `${c.name} is not checked, and sends no alerts, until it is resumed.`],
                })}>
                {c.paused ? 'Resume' : 'Pause'}
              </Button>
            </Group>
          )}

          {canEdit && (
            <Group gap="xs">
              <Button size="xs" variant="default" leftSection={<IconRefresh size={14} />} disabled={c.paused}
                loading={checkNow.isPending}
                onClick={() => void run(() => checkNow.mutateAsync(c.id), { failure: 'Not checked', success: ['Checking', 'The result shows here in a few seconds.'] })}>
                Check now
              </Button>
              <Button size="xs" variant="default" leftSection={<IconBrandTelegram size={14} />} disabled={c.telegram === 'off'}
                loading={testingAlert}
                onClick={() => {
                  setTestingAlert(true)
                  void run(async () => {
                    const r = await testWebsiteAlert(c.id)
                    notifyOk('Sent', `${r.bot} posted a test alert to ${r.chats.length} chat${r.chats.length === 1 ? '' : 's'}.`)
                  }, { failure: 'Test alert not sent' }).finally(() => setTestingAlert(false))
                }}>
                Send test alert
              </Button>
              <Button size="xs" variant="default" onClick={() => onEdit(c)}>Edit</Button>
              <Button size="xs" variant="default" loading={change.isPending}
                onClick={() => void run(() => change.mutateAsync({ id: c.id, input: { ...inputOf(c), paused: !c.paused } }), {
                  failure: c.paused ? 'Not resumed' : 'Not paused',
                  success: c.paused ? ['Resumed', `${c.name} is checked again.`] : ['Paused', `${c.name} is not checked, and sends no alerts, until you resume it.`],
                })}>
                {c.paused ? 'Resume' : 'Pause'}
              </Button>
              <RemoveButton name={c.name} onRemove={() => run(() => remove.mutateAsync(c.id), {
                failure: 'Not removed', success: ['Removed', `${c.name} and its history are removed.`],
              }).then((ok) => { if (ok) onClose() })} />
            </Group>
          )}
        </Stack>
      )}
    </Drawer>
  )
}

export function Figure({ label, value, warn = false }: { label: string; value: string; warn?: boolean }) {
  return (
    <Box>
      <Text size={FS.micro} c="dimmed">{label}</Text>
      <Text fw={700} size="lg" c={warn ? 'rose' : undefined}>{value}</Text>
    </Box>
  )
}

const OUTCOME_COLOR: Record<WebsiteRound['outcome'], string> = {
  up: 'var(--mantine-color-teal-6)',
  partial: 'var(--mantine-color-teal-4)',
  pending: 'var(--mantine-color-amber-6)',
  down: 'var(--mantine-color-rose-6)',
  unknown: 'var(--mantine-color-gray-5)',
}

const OUTCOME_WORD: Record<WebsiteRound['outcome'], string> = {
  up: 'Up', partial: 'Up from another server', pending: 'Failed, checking again', down: 'Down', unknown: 'Argus could not check',
}

/**
 * The last day in 96 slices of 15 minutes: each as tall as the answer was
 * slow, coloured by the worst thing that happened in it, so a two-minute
 * outage still shows on a phone.
 */
export function RoundStrip({ rounds }: { rounds: WebsiteRound[] }) {
  const slices = useMemo(() => sliceRounds(rounds, 96), [rounds])
  const upMs = slices.flatMap((s) => (s?.ms != null ? [s.ms] : []))
  const max = Math.max(200, ...upMs)
  const avg = upMs.length ? Math.round(upMs.reduce((a, b) => a + b, 0) / upMs.length) : null
  const bad = slices.filter((s) => s && s.outcome === 'down').length
  return (
    <Box>
      <Group justify="space-between" mb={SP.snug}>
        <Text fw={600} size="sm">Last 24 hours</Text>
        <Text size={FS.micro} c="dimmed">{avg != null ? `Average answer ${avg} ms` : 'No answers yet'}</Text>
      </Group>
      {rounds.length === 0 ? (
        <Text size={FS.body} c="dimmed">No checks yet.</Text>
      ) : (
        <>
          <Group gap={2} align="flex-end" wrap="nowrap" h={56} role="img"
            aria-label={bad ? `Down in ${bad} of the last 96 quarter hours` : 'Not down in the last 24 hours'}>
            {slices.map((sl, i) => sl ? (
              <Tooltip key={i} withinPortal
                label={`${new Date(sl.from).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}: ${OUTCOME_WORD[sl.outcome]}${sl.ms != null ? `, ${sl.ms} ms on average` : ''}`}>
                <Box style={{
                  flex: 1, minWidth: 1, borderRadius: 1, background: OUTCOME_COLOR[sl.outcome],
                  height: sl.outcome === 'up' || sl.outcome === 'partial' ? `${Math.max(14, ((sl.ms ?? 0) / max) * 100)}%` : '100%',
                }} />
              </Tooltip>
            ) : (
              <Box key={i} style={{ flex: 1, minWidth: 1, height: 3, borderRadius: 1, background: 'var(--mantine-color-gray-3)' }} />
            ))}
          </Group>
          <Group justify="space-between" mt={SP.tight}>
            <Text size={FS.micro} c="dimmed">24 hours ago</Text>
            <Group gap="sm">
              {(['up', 'pending', 'down', 'unknown'] as const).map((o) => (
                <Group key={o} gap={4} wrap="nowrap">
                  <Box w={8} h={8} style={{ borderRadius: 2, background: OUTCOME_COLOR[o] }} />
                  <Text size={FS.micro} c="dimmed">{{ up: 'Up', pending: 'Failed once', down: 'Down', unknown: 'Could not check' }[o]}</Text>
                </Group>
              ))}
            </Group>
            <Text size={FS.micro} c="dimmed">now</Text>
          </Group>
        </>
      )}
    </Box>
  )
}

export function RemoveButton({ name, onRemove }: { name: string; onRemove: () => void }) {
  const [asking, setAsking] = useState(false)
  return (
    <>
      <Button size="xs" color="rose" variant="light" onClick={() => setAsking(true)}>Remove</Button>
      <Modal opened={asking} onClose={() => setAsking(false)} title={`Remove ${name}?`}>
        <Stack>
          <Text size={FS.body}>Argus stops checking it, and its uptime and outage history are deleted. This is in the audit log.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setAsking(false)}>Keep it</Button>
            <Button color="rose" onClick={() => { setAsking(false); onRemove() }}>Remove</Button>
          </Group>
        </Stack>
      </Modal>
    </>
  )
}

/* ── Adding and editing ──────────────────────────────────────────────────── */

function inputOf(c: WebsiteCheck): WebsiteInput {
  return {
    name: c.name, url: c.url, expectStatus: c.expectStatus, keyword: c.keyword, everySeconds: c.everySeconds,
    timeoutSeconds: c.timeoutSeconds, confirmFrom: c.confirmFrom, paused: c.paused,
    telegram: c.telegram, telegramBot: c.telegramBot ?? '', telegramChats: c.telegramChats,
    method: c.method || 'GET', body: c.body ?? '',
  }
}

const BLANK: WebsiteInput = {
  name: '', url: '', expectStatus: 0, keyword: '', everySeconds: 60, timeoutSeconds: 30, confirmFrom: [], paused: false,
  telegram: 'default', telegramBot: '', telegramChats: [], method: 'GET', body: '',
}

/** A header row in the form. saved: its value is the one already kept, unseen. */
interface HeaderRow { name: string; value: string; saved: boolean }

function headersOut(rows: HeaderRow[]): HeaderInput[] {
  return rows.filter((r) => r.name.trim() || r.value)
    .map((r) => (r.saved && !r.value ? { name: r.name.trim(), value: '', keep: true } : { name: r.name.trim(), value: r.value }))
}

function WebsiteForm({ check, onClose, onSaved }: { check?: WebsiteCheck; onClose: () => void; onSaved: (c: WebsiteCheck) => void }) {
  const [v, setV] = useState<WebsiteInput>(check ? inputOf(check) : BLANK)
  const [rows, setRows] = useState<HeaderRow[]>((check?.headerNames ?? []).map((name) => ({ name, value: '', saved: true })))
  const [showRequest, setShowRequest] = useState(() => !!check && requestSummary(check) !== '')
  const [tested, setTested] = useState<SiteResult | null>(null)
  const [testing, setTesting] = useState(false)
  const add = useAddWebsite()
  const change = useChangeWebsite()
  const { data: assets } = useQuery(assetsQuery())
  const servers = (assets ?? []).filter((a) => (a.kind ?? 'server') !== 'android')
    .map((a) => ({ value: a.id, label: assetName(a) }))
  const set = <K extends keyof WebsiteInput>(k: K, val: WebsiteInput[K]) => { setV((p) => ({ ...p, [k]: val })); setTested(null) }
  const saving = add.isPending || change.isPending

  const setRow = (i: number, r: Partial<HeaderRow>) => { setRows((p) => p.map((x, j) => (j === i ? { ...x, ...r } : x))); setTested(null) }
  const input = (): WebsiteInput => ({ ...v, body: sendsBody(v.method) ? v.body : '', headers: headersOut(rows) })

  const save = async () => {
    const c = check ? await change.mutateAsync({ id: check.id, input: input() }) : await add.mutateAsync(input())
    onSaved(c)
  }

  return (
    <Modal opened onClose={onClose} title={check ? `Edit ${check.name}` : 'Add a website'} size="lg">
      <Stack gap="sm">
        <TextInput label="Address" placeholder="https://shop.example.com/health" required data-autofocus
          value={v.url} onChange={(e) => set('url', e.currentTarget.value)}
          description="A page that only loads when the site really works, such as a health check, is the best one to watch." />
        <TextInput label="Name" placeholder="The address's host, if left empty" value={v.name}
          onChange={(e) => set('name', e.currentTarget.value)} />
        <MultiSelect label="Confirm from these servers" placeholder={v.confirmFrom.length < 3 ? 'Choose up to three' : undefined}
          data={servers} value={v.confirmFrom} onChange={(ids) => set('confirmFrom', ids)} maxValues={3} searchable clearable
          description="Before calling the site down, Argus asks these servers' agents to load it too. If any of them can, it is not down. Servers in other networks or regions make the best second opinions." />
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <Select label="Check every" data={[{ value: '60', label: '1 minute' }, { value: '300', label: '5 minutes' }, { value: '600', label: '10 minutes' }]}
            value={String(v.everySeconds)} onChange={(x) => set('everySeconds', Number(x ?? 60))} allowDeselect={false} />
          <NumberInput label="Wait for an answer (seconds)" min={5} max={60} value={v.timeoutSeconds}
            onChange={(x) => set('timeoutSeconds', Number(x) || 30)}
            description="A slower answer is a failure. Slow alone is not." />
          <NumberInput label="Expected status" placeholder="Any below 400" min={100} max={599} value={v.expectStatus || ''}
            onChange={(x) => set('expectStatus', Number(x) || 0)} />
          <TextInput label="Page must contain" placeholder="Optional word or phrase" value={v.keyword} maxLength={200}
            disabled={v.method === 'HEAD'} description={v.method === 'HEAD' ? 'A HEAD request gets no page to look in.' : undefined}
            onChange={(e) => set('keyword', e.currentTarget.value)} />
        </SimpleGrid>
        {showRequest ? (
          <RequestFields method={v.method} body={v.body} rows={rows}
            onMethod={(m) => setV((p) => ({ ...p, method: m, keyword: m === 'HEAD' ? '' : p.keyword }))}
            onBody={(b) => set('body', b)} onRow={setRow}
            onAdd={() => setRows((p) => [...p, { name: '', value: '', saved: false }])}
            onRemove={(i) => { setRows((p) => p.filter((_, j) => j !== i)); setTested(null) }} />
        ) : (
          <Group>
            <Button variant="subtle" size="compact-sm" leftSection={<IconPlus size={14} />} onClick={() => setShowRequest(true)}>
              Send a method, headers or a body
            </Button>
            <Text size={FS.meta} c="dimmed">For an endpoint that wants a POST or a key. Otherwise Argus sends a plain GET.</Text>
          </Group>
        )}
        <TelegramFields value={v.telegram} bot={v.telegramBot} chats={v.telegramChats}
          onChange={(telegram, telegramBot, telegramChats) => setV((p) => ({ ...p, telegram, telegramBot, telegramChats }))} />
        {check && (
          <Switch label="Paused: not checked, and no alerts" checked={v.paused} onChange={(e) => set('paused', e.currentTarget.checked)} />
        )}
        {tested && (
          <Alert color={tested.ok ? 'teal' : 'rose'} variant="light">
            <Text size={FS.body}>
              {tested.ok ? 'Up from Argus: ' : 'Failed from Argus: '}{tested.detail}{tested.ms ? ` in ${tested.ms} ms` : ''}.
              {!tested.ok && ' Saved like this, it would be checked again and confirmed before anyone is told.'}
            </Text>
          </Alert>
        )}
        <Group justify="space-between" mt="xs">
          <Button variant="default" loading={testing} disabled={!v.url.trim()}
            onClick={() => {
              setTesting(true)
              void run(async () => setTested(await testWebsite(input(), check?.id)), { failure: 'Could not test' }).finally(() => setTesting(false))
            }}>
            Test now
          </Button>
          <Group gap="xs">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button loading={saving} disabled={!v.url.trim()}
              onClick={() => void run(save, {
                failure: check ? 'Not saved' : 'Not added',
                success: check ? ['Saved', `${v.name || v.url} is checked with the new settings.`] : ['Added', 'Argus is checking it now.'],
              })}>
              {check ? 'Save' : 'Add website'}
            </Button>
          </Group>
        </Group>
      </Stack>
    </Modal>
  )
}

/** The request a check sends: method, headers, body. */
function RequestFields({ method, body, rows, onMethod, onBody, onRow, onAdd, onRemove }: {
  method: SiteMethod
  body: string
  rows: HeaderRow[]
  onMethod: (m: SiteMethod) => void
  onBody: (b: string) => void
  onRow: (i: number, r: Partial<HeaderRow>) => void
  onAdd: () => void
  onRemove: (i: number) => void
}) {
  return (
    <Stack gap="xs">
      <Box>
        <Text size="sm" fw={500} mb={4} id="request-method">Request</Text>
        <SegmentedControl aria-labelledby="request-method" size="xs" value={method}
          data={SITE_METHODS.map((m) => ({ value: m, label: m }))} onChange={(m) => onMethod(m as SiteMethod)} />
      </Box>
      <Box>
        <Text size="sm" fw={500}>Headers</Text>
        <Text size={FS.meta} c="dimmed" mb={4}>
          Values are encrypted and never shown again; a saved one is sent until you type a new one. They go only to this
          address's host, never to one it redirects to.
        </Text>
        <Stack gap={6}>
          {rows.map((r, i) => (
            <Group key={i} gap="xs" wrap="nowrap" align="flex-start">
              <TextInput aria-label={`Header ${i + 1} name`} placeholder="X-Api-Key" value={r.name} style={{ flex: 2, minWidth: 0 }}
                disabled={r.saved && !r.value} onChange={(e) => onRow(i, { name: e.currentTarget.value })} />
              <PasswordInput aria-label={`Header ${i + 1} value`} style={{ flex: 3, minWidth: 0 }}
                placeholder={r.saved ? 'Saved; type to replace' : 'Value'} value={r.value}
                onChange={(e) => onRow(i, { value: e.currentTarget.value })} />
              <ActionIcon variant="subtle" color="gray" mt={6} aria-label={`Remove header ${r.name || i + 1}`} onClick={() => onRemove(i)}>
                <IconX size={16} />
              </ActionIcon>
            </Group>
          ))}
          <Group>
            <Button variant="subtle" size="compact-sm" leftSection={<IconPlus size={14} />} onClick={onAdd} disabled={rows.length >= 20}>
              Add header
            </Button>
          </Group>
        </Stack>
      </Box>
      {sendsBody(method) && (
        <Textarea label="Body" value={body} onChange={(e) => onBody(e.currentTarget.value)} autosize minRows={3} maxRows={10}
          placeholder={'{"ping": true}'} styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
          description="Sent as JSON when it starts with { or [, otherwise as text, unless a Content-Type header says otherwise. Everyone who can see this check can read it, so put keys in a header." />
      )}
    </Stack>
  )
}

/* ── Telegram ────────────────────────────────────────────────────────────── */

/** Where a site's alerts go, in a sentence for its drawer. */
function telegramSentence(c: WebsiteCheck, tg: TelegramView | undefined): string {
  if (c.telegram === 'off') return 'Alerts go to the bell only, not Telegram.'
  if (c.telegram === 'chats') {
    const names = c.telegramChats.map((id) => {
      const ch = chatsOf(tg, c.telegramBot).find((x) => x.id === id)
      return ch ? chatOption(ch).label : id
    })
    const bot = tg ? botLabel(tg, c.telegramBot) : 'its own bot'
    return `Alerts go to the bell and, through ${bot}, to ${names.join(', ')}.`
  }
  if (!tg) return 'Alerts go to the bell, and to Telegram as all websites are set.'
  const r = tg.routes.find((x) => x.kind === 'website')
  if (!r || r.bot === 'off') return 'Alerts go to the bell only: Telegram is off for all websites.'
  return `Alerts go to the bell and, as all websites are set, through ${botLabel(tg, r.bot)}.`
}

/** One site's Telegram choice: as all websites, its own bot and chats, or off. */
function TelegramFields({ value, bot, chats, onChange }: {
  value: TelegramChoice; bot: string; chats: string[]
  onChange: (v: TelegramChoice, bot: string, chats: string[]) => void
}) {
  const { data: tg } = useQuery(telegramQuery())
  const route = tg?.routes.find((r) => r.kind === 'website')
  const feature = !route || route.bot === 'off'
    ? 'off'
    : `${botLabel(tg, route.bot)}${route.chats.length ? `, ${route.chats.length} chat${route.chats.length === 1 ? '' : 's'}` : ''}`
  const own = chatsOf(tg, bot)
  const options = Array.from(new Map([...chats.map((id) => ({ value: id, label: id })), ...own.map(chatOption)]
    .map((o) => [o.value, o] as const)).values())
  return (
    <Stack gap="xs">
      <Select label="Telegram" allowDeselect={false} value={value}
        data={[
          { value: 'default', label: `Same as all websites (${feature})` },
          { value: 'chats', label: 'Its own bot and chats' },
          { value: 'off', label: 'Off: bell only' },
        ]}
        onChange={(x) => onChange((x ?? 'default') as TelegramChoice, '', [])}
        description="Down and back-up alerts for this site. Bots are added under Settings, Notifications." />
      {value === 'chats' && (
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <Select label="Bot" allowDeselect={false} value={bot}
            data={botOptions(tg, { withOff: false })}
            onChange={(x) => onChange('chats', x ?? '', [])} />
          <MultiSelect label="Chats" data={options} value={chats} onChange={(x) => onChange('chats', bot, x)}
            placeholder={chats.length ? undefined : 'All of its chats'} clearable
            description={own.length === 0 ? 'This bot has no chats yet.' : 'Only the chats set on this bot.'} />
        </SimpleGrid>
      )}
    </Stack>
  )
}

/** All websites' Telegram choice: a bot and some of its chats, or off. */
function AlertSettings({ onClose }: { onClose: () => void }) {
  const { data: tg, isLoading } = useQuery(telegramQuery())
  const save = useSetRoute()
  const route = tg?.routes.find((r) => r.kind === 'website')
  const [bot, setBot] = useState<string | null>(null)
  const [chats, setChats] = useState<string[] | null>(null)
  const b = bot ?? route?.bot ?? ''
  const chosen = chats ?? route?.chats ?? []
  const options = chatsOf(tg, b).map(chatOption)
  return (
    <Modal opened onClose={onClose} title="Website alerts">
      {isLoading || !tg ? <Skeleton h={160} /> : (
        <Stack gap="sm">
          <Text size={FS.body} c="dimmed">
            Every admin sees website alerts in the bell. This decides which Telegram bot and chats get them too. A website can follow this, use its own bot and chats, or turn Telegram off when you edit it.
          </Text>
          <Select label="Bot" allowDeselect={false} value={b} data={botOptions(tg)}
            onChange={(x) => { setBot(x ?? ''); setChats([]) }}
            description="Bots are added under Settings, Notifications." />
          {b !== 'off' && (
            <MultiSelect label="Chats" placeholder={chosen.length ? undefined : 'All of its chats'}
              data={options} value={chosen} onChange={setChats} clearable
              description={options.length === 0 ? 'This bot has no chats yet.' : 'Leave empty for all of the bot\'s chats.'} />
          )}
          <Group justify="flex-end" mt="xs">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button loading={save.isPending}
              onClick={() => void run(() => save.mutateAsync({ kind: 'website', bot: b, chats: b === 'off' ? [] : chosen }), {
                failure: 'Not saved',
                success: ['Saved', b === 'off' ? 'Website alerts go to the bell only, unless a site chose its own bot.' : `Website alerts go through ${botLabel(tg, b)}.`],
              }).then((ok) => { if (ok) onClose() })}>
              Save
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  )
}
