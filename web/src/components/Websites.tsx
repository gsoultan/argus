import {
  Alert, Badge, Box, Button, Drawer, Group, Modal, MultiSelect, NumberInput, Select, SimpleGrid, Skeleton, Stack, Switch,
  Table, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconPlus, IconRefresh, IconShieldCheck, IconWorld } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { assetName } from '~/lib/assetType'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import { assetsQuery, meQuery } from '~/lib/queries'
import {
  ago, certDaysLeft, formatDuration, formatUptime, outageMinutes, sliceRounds, stateLabel, testWebsite, useAddWebsite,
  useChangeWebsite, useCheckWebsiteNow, useRemoveWebsite, websiteQuery, websitesQuery,
  type SiteResult, type WebsiteCheck, type WebsiteInput, type WebsiteRound,
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
  const [editing, setEditing] = useState<WebsiteCheck | 'new' | null>(null)
  const open = (id?: string) => void navigate({ to: '/websites', search: id ? { check: id } : {}, replace: true })

  const down = list?.filter((c) => !c.paused && c.state === 'down').length ?? 0
  const up = list?.filter((c) => !c.paused && c.state === 'up').length ?? 0

  return (
    <Box>
      <PageHeader
        title="Websites"
        description="Argus checks each address every minute and tells admins when a site goes down and when it comes back."
        status={list && list.length > 0 ? (
          down > 0
            ? <Badge color="rose" variant="filled">{down} down</Badge>
            : <Badge color="teal" variant="light">{up} up</Badge>
        ) : undefined}
        actions={canEdit && isConfigured() ? (
          <Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add website</Button>
        ) : undefined}
      />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to check websites.</Text></Alert>
        ) : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read the website checks: {error.message}</Text></Alert>
        ) : (
          <Stack gap="lg">
            <DataTable
              columns={['Website', 'Status', 'Response', 'Last 24 hours', '7 days', '30 days', 'Checked']}
              minWidth={820}
              loading={isLoading}
              isEmpty={list?.length === 0}
              empty={<EmptyState icon={IconWorld} title="No websites are checked yet."
                description={canEdit ? 'Add an address and Argus starts checking it at once.' : 'An admin can add the addresses to check.'}
                action={canEdit ? <Button size="xs" onClick={() => setEditing('new')}>Add website</Button> : undefined} />}
            >
              {list?.map((c) => <WebsiteRow key={c.id} c={c} onOpen={() => open(c.id)} />)}
            </DataTable>
            <HowSure />
          </Stack>
        )}
      </PageBody>
      {openId && (
        <WebsiteDrawer id={openId} canEdit={canEdit} onClose={() => open()}
          onEdit={(c) => setEditing(c)} />
      )}
      {editing && (
        <WebsiteForm check={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)}
          onSaved={(c) => { setEditing(null); open(c.id) }} />
      )}
    </Box>
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

function UptimeText({ v }: { v: number | null }) {
  const color = v == null ? 'dimmed' : v >= 99.9 ? undefined : v >= 99 ? 'amber' : 'rose'
  return <Text size="sm" c={color} fw={v != null && v < 99 ? 600 : undefined}>{formatUptime(v)}</Text>
}

/** How sure "Down" is, in the words the alert uses. */
function HowSure() {
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

function WebsiteDrawer({ id, canEdit, onClose, onEdit }: {
  id: string; canEdit: boolean; onClose: () => void; onEdit: (c: WebsiteCheck) => void
}) {
  const { data, isLoading, error } = useQuery(websiteQuery(id))
  const checkNow = useCheckWebsiteNow()
  const remove = useRemoveWebsite()
  const change = useChangeWebsite()
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
              <Text size={FS.body}>{c.expectStatus ? `Must answer ${c.expectStatus}.` : 'Any answer below 400, after redirects, is up.'}{c.keyword ? ` The page must contain "${c.keyword}".` : ''}</Text>
              <Text size={FS.body}>{c.confirmHosts.length > 0
                ? `Before calling it down, Argus asks ${c.confirmHosts.join(', ')} to check too.`
                : 'Only Argus checks it. Choose servers to confirm from, so a problem on Argus\'s side is never called the site being down.'}</Text>
            </Stack>
          </Box>

          {canEdit && (
            <Group gap="xs">
              <Button size="xs" variant="default" leftSection={<IconRefresh size={14} />} disabled={c.paused}
                loading={checkNow.isPending}
                onClick={() => void run(() => checkNow.mutateAsync(c.id), { failure: 'Not checked', success: ['Checking', 'The result shows here in a few seconds.'] })}>
                Check now
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

function Figure({ label, value, warn = false }: { label: string; value: string; warn?: boolean }) {
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
function RoundStrip({ rounds }: { rounds: WebsiteRound[] }) {
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

function RemoveButton({ name, onRemove }: { name: string; onRemove: () => void }) {
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
  }
}

const BLANK: WebsiteInput = { name: '', url: '', expectStatus: 0, keyword: '', everySeconds: 60, timeoutSeconds: 30, confirmFrom: [], paused: false }

function WebsiteForm({ check, onClose, onSaved }: { check?: WebsiteCheck; onClose: () => void; onSaved: (c: WebsiteCheck) => void }) {
  const [v, setV] = useState<WebsiteInput>(check ? inputOf(check) : BLANK)
  const [tested, setTested] = useState<SiteResult | null>(null)
  const [testing, setTesting] = useState(false)
  const add = useAddWebsite()
  const change = useChangeWebsite()
  const { data: assets } = useQuery(assetsQuery())
  const servers = (assets ?? []).filter((a) => (a.kind ?? 'server') !== 'android')
    .map((a) => ({ value: a.id, label: assetName(a) }))
  const set = <K extends keyof WebsiteInput>(k: K, val: WebsiteInput[K]) => { setV((p) => ({ ...p, [k]: val })); setTested(null) }
  const saving = add.isPending || change.isPending

  const save = async () => {
    const c = check ? await change.mutateAsync({ id: check.id, input: v }) : await add.mutateAsync(v)
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
            onChange={(e) => set('keyword', e.currentTarget.value)} />
        </SimpleGrid>
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
              void run(async () => setTested(await testWebsite(v)), { failure: 'Could not test' }).finally(() => setTesting(false))
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
