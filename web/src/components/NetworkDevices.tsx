import {
  Alert, Badge, Box, Button, Checkbox, Drawer, Group, MultiSelect, NumberInput, PasswordInput, ScrollArea, SegmentedControl, Select,
  SimpleGrid, Skeleton, Stack, Switch, Table, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconCheck, IconEye, IconFolderPlus, IconInfoCircle, IconPlayerPause, IconPlayerPlay, IconPlus, IconRefresh, IconRouter, IconSearch, IconX } from '@tabler/icons-react'
import { useMemo, useState, type ReactNode } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { GroupForm, WhoCanSee } from '~/components/CheckAccess'
import { Figure, GroupMenu, RemoveButton, RollUpBadge, RoundStrip, sectionsOf, UptimeText } from '~/components/Websites'
import { assetName } from '~/lib/assetType'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import { assetsQuery, meQuery } from '~/lib/queries'
import {
  AUTH_PROTOS, discoverCommunity, formatBps, formatSpeed, formatUptimeSec, interfaceLabel, networkDeviceQuery, networkDevicesQuery, PRIV_PROTOS,
  securityWords, SNMP_DEFAULT_COMMUNITIES, testNetworkDevice, useAddNetworkDevice, useChangeNetworkDevice, useCheckDeviceNow, usePauseDevice, useRemoveNetworkDevice,
  type DeviceAttempt, type DeviceInterface, type NetworkDevice, type NetworkDeviceInput, type SnmpDiscovery, type SnmpVersion, type WatchedInterface,
} from '~/lib/networkDevices'
import { ago, checkGroupsQuery, formatDuration, formatUptime, outageMinutes, stateLabel, useMoveCheck, type CheckGroup } from '~/lib/websites'
import { FS, SP } from '~/theme'

/**
 * Network devices: switches, routers, firewalls, printers, UPSes and access
 * points, read over SNMP by a server's agent on the same network. Down means
 * the device stopped answering, or an interface someone chose to watch went
 * down, over two rounds. A restart is shown, and told in the bell, but is
 * not an outage: the device answered.
 */
export function NetworkDevicesPage({ openId }: { openId?: string }) {
  const navigate = useNavigate()
  const { data: me } = useQuery(meQuery())
  const canEdit = me?.role === 'admin' || me?.role === 'owner'
  const { data: list, isLoading, error } = useQuery({ ...networkDevicesQuery(), enabled: isConfigured() })
  const { data: groups } = useQuery({ ...checkGroupsQuery(), enabled: isConfigured() })
  const [editing, setEditing] = useState<NetworkDevice | 'new' | null>(null)
  const [grouping, setGrouping] = useState<CheckGroup | 'new' | null>(null)
  const [sharing, setSharing] = useState<CheckGroup | null>(null)
  const open = (id?: string) => void navigate({ to: '/network-devices', search: id ? { device: id } : {}, replace: true })

  const down = list?.filter((d) => !d.paused && d.state === 'down').length ?? 0
  const up = list?.filter((d) => !d.paused && d.state === 'up').length ?? 0
  const limited = list?.some((d) => d.limited) ?? false
  // A group holding only websites is not shown here; an empty one is, to admins.
  const sections = useMemo(() => {
    const used = new Set((list ?? []).map((d) => d.groupId))
    return sectionsOf(list ?? [], (groups ?? []).filter((g) => used.has(g.id) || g.checks === 0), canEdit)
  }, [list, groups, canEdit])

  return (
    <Box>
      <PageHeader
        title="Network devices"
        description={limited
          ? 'The devices you were given. A server next to each one reads it every minute, and you are told in the bell when one goes down and when it comes back.'
          : 'Switches, routers, firewalls and printers, read over SNMP by a server on the same network. Admins, and whoever was given a device, are told when one goes down or restarts.'}
        status={list && list.length > 0 ? (
          down > 0 ? <Badge color="rose" variant="filled">{down} down</Badge> : <Badge color="teal" variant="light">{up} up</Badge>
        ) : undefined}
        actions={canEdit && isConfigured() ? (
          <>
            <Button variant="default" leftSection={<IconFolderPlus size={14} />} onClick={() => setGrouping('new')}>New group</Button>
            <Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add device</Button>
          </>
        ) : undefined}
      />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to check network devices.</Text></Alert>
        ) : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read the network devices: {error.message}</Text></Alert>
        ) : (
          <Stack gap="lg">
            {isLoading || list?.length === 0 || sections.length === 0 ? (
              <DeviceTable list={list ?? []} loading={isLoading} onOpen={open}
                empty={<EmptyState icon={IconRouter} title="No network devices are checked yet."
                  description={canEdit
                    ? 'Add a switch, router or firewall, and choose a server on its network to read it from.'
                    : 'An admin can add the devices to check.'}
                  action={canEdit ? <Button size="xs" onClick={() => setEditing('new')}>Add device</Button> : undefined} />} />
            ) : sections.map((sec) => (
              <Box key={sec.group?.id ?? 'none'} data-testid="check-group">
                {(sections.length > 1 || sec.group) && (
                  <Group justify="space-between" mb={SP.snug} wrap="nowrap">
                    <Group gap="xs" wrap="nowrap">
                      <Text fw={600} size="sm">{sec.group?.name ?? 'Not in a group'}</Text>
                      <RollUpBadge list={sec.checks} />
                      <Text size={FS.meta} c="dimmed">{sec.checks.length} {sec.checks.length === 1 ? 'device' : 'devices'}</Text>
                    </Group>
                    {canEdit && sec.group && (
                      <GroupMenu group={sec.group} onShare={() => setSharing(sec.group!)} onRename={() => setGrouping(sec.group!)} />
                    )}
                  </Group>
                )}
                <DeviceTable list={sec.checks} onOpen={open}
                  empty={<Text size={FS.body} c="dimmed" p="md">No devices in this group yet. Open a device and choose this group for it.</Text>} />
              </Box>
            ))}
            <HowItReads />
          </Stack>
        )}
      </PageBody>
      {openId && (
        <DeviceDrawer id={openId} canEdit={canEdit} groups={groups ?? []} onClose={() => open()} onEdit={(d) => setEditing(d)} />
      )}
      {grouping && <GroupForm group={grouping === 'new' ? undefined : grouping} onClose={() => setGrouping(null)} />}
      {sharing && <WhoCanSee target="group" id={sharing.id} name={sharing.name} onClose={() => setSharing(null)} />}
      {editing && (
        <DeviceForm device={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)}
          onSaved={(d) => { setEditing(null); open(d.id) }} />
      )}
    </Box>
  )
}

const DAY = 86_400_000

/** Whether its last read got an answer, so its uptime is current. */
const answered = (d: NetworkDevice) =>
  !!d.device && (!d.lastAt || Date.parse(d.device.at) >= Date.parse(d.lastAt) - 1000)

/** Whether it restarted in the last day. */
const restartedRecently = (d: NetworkDevice, now = Date.now()) =>
  !!d.device?.restartedAt && now - Date.parse(d.device.restartedAt) < DAY

function DeviceTable({ list, loading = false, empty, onOpen }: { list: NetworkDevice[]; loading?: boolean; empty: ReactNode; onOpen: (id: string) => void }) {
  return (
    <DataTable columns={['Device', 'Status', 'Up for', 'Interfaces', '7 days', '30 days', 'Checked']} minWidth={860}
      loading={loading} isEmpty={list.length === 0} empty={empty}>
      {list.map((d) => <DeviceRow key={d.id} d={d} onOpen={() => onOpen(d.id)} />)}
    </DataTable>
  )
}

function StateBadge({ d }: { d: NetworkDevice }) {
  const s = stateLabel(d)
  return <Badge color={s.color} variant={s.color === 'rose' ? 'filled' : 'light'}>{s.label}</Badge>
}

/** "22 of 24 up", and the watched ones that are down. */
function InterfacesCell({ d }: { d: NetworkDevice }) {
  const ifs = d.device?.interfaces ?? []
  if (ifs.length === 0) return <Text size="sm" c="dimmed">—</Text>
  const up = ifs.filter((i) => i.up).length
  const watchedDown = ifs.filter((i) => i.watched && !i.up)
  return (
    <Stack gap={SP.hair}>
      <Text size="sm">{up} of {ifs.length} up</Text>
      {watchedDown.length > 0 ? (
        <Text size={FS.micro} c="rose" fw={600}>{watchedDown.map(interfaceLabel).join(', ')} down</Text>
      ) : d.watch.length > 0 && (
        <Text size={FS.micro} c="dimmed">{d.watch.length} watched</Text>
      )}
    </Stack>
  )
}

function DeviceRow({ d, onOpen }: { d: NetworkDevice; onOpen: () => void }) {
  const since = d.state === 'down' && d.openOutage ? d.openOutage.startedAt : d.stateSince
  return (
    <Table.Tr style={{ cursor: 'pointer' }} onClick={onOpen}>
      <Table.Td>
        <UnstyledButton onClick={onOpen} aria-label={`Open ${d.name}`}>
          <Text size="sm" fw={600}>{d.name}</Text>
          <Text size={FS.micro} c="dimmed" ff="monospace">{d.host}{d.device?.description ? ` · ${d.device.description.split(/[,\n]/)[0]}` : ''}</Text>
        </UnstyledButton>
      </Table.Td>
      <Table.Td>
        <Stack gap={SP.hair}>
          <Group gap={SP.tight}>
            <StateBadge d={d} />
            {restartedRecently(d) && !d.paused && (
              <Tooltip label={`Restarted ${ago(d.device!.restartedAt)}. Not counted as an outage: it answered.`} multiline maw={300}>
                <Badge color="amber" variant="light">Restarted</Badge>
              </Tooltip>
            )}
          </Group>
          {!d.paused && d.state === 'up' && d.lastOutcome === 'partial' ? (
            <Tooltip label={d.lastDetail} multiline maw={360}>
              <Text size={FS.micro} c="amber">One server cannot reach it; another can</Text>
            </Tooltip>
          ) : since && !d.paused && d.state !== 'pending' && d.failingRounds === 0 && (
            <Text size={FS.micro} c="dimmed">for {formatDuration(Math.max(1, Math.round((Date.now() - Date.parse(since)) / 60_000)))}</Text>
          )}
        </Stack>
      </Table.Td>
      <Table.Td><Text size="sm">{answered(d) ? formatUptimeSec(d.device!.uptimeSec) : '—'}</Text></Table.Td>
      <Table.Td><InterfacesCell d={d} /></Table.Td>
      <Table.Td><UptimeText v={d.uptime.week} /></Table.Td>
      <Table.Td><UptimeText v={d.uptime.month} /></Table.Td>
      <Table.Td><Text size={FS.meta} c="dimmed">{d.paused ? 'paused' : ago(d.lastAt)}</Text></Table.Td>
    </Table.Tr>
  )
}

function HowItReads() {
  return (
    <SectionCard title="How Argus reads a device" icon={IconInfoCircle} iconColor="teal">
      <Stack gap={SP.cozy}>
        {[
          ['A server next to it asks.', 'The agent of a server you choose, on the same network, reads the device over SNMP. Argus itself never needs to reach it. Choose up to three; any one getting an answer means it is up.'],
          ['Down needs two rounds.', 'No answer, refused credentials, or an interface you watch not being up, a minute apart. The outage then counts from the first.'],
          ['A restart is not an outage.', 'When its uptime goes back, Argus tells you it restarted, but it answered, so it stays up.'],
          ['Only standard values are read.', 'Its name, description, uptime and interface table (IF-MIB). Nothing on the device is changed.'],
          ['Credentials stay sealed.', 'They are kept encrypted and are never shown again, not even to admins. Use SNMPv3 with authentication and encryption when the device supports it.'],
        ].map(([t, d]) => (
          <Text key={t} size={FS.body}><Text span fw={600}>{t}</Text> <Text span c="dimmed">{d}</Text></Text>
        ))}
      </Stack>
    </SectionCard>
  )
}

/* ── One device ──────────────────────────────────────────────────────────── */

function InterfaceTable({ list, compact = false }: { list: DeviceInterface[]; compact?: boolean }) {
  const [all, setAll] = useState(false)
  // Watched first, then what is up, then by number.
  const sorted = useMemo(() => [...list].sort((a, b) =>
    Number(!!b.watched) - Number(!!a.watched) || Number(b.up) - Number(a.up) || a.index - b.index), [list])
  const shown = all ? sorted : sorted.slice(0, 12)
  return (
    <Box>
      <Table.ScrollContainer minWidth={compact ? 360 : 520}>
        <Table verticalSpacing={4} data-testid="interfaces">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Interface</Table.Th><Table.Th>Status</Table.Th><Table.Th>Speed</Table.Th>
              {!compact && <><Table.Th>In</Table.Th><Table.Th>Out</Table.Th><Table.Th>New errors</Table.Th></>}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {shown.map((i) => (
              <Table.Tr key={i.index}>
                <Table.Td>
                  <Group gap={6} wrap="nowrap">
                    {i.watched && <Tooltip label="Watched: down when this is down"><IconEye size={14} aria-label="Watched" /></Tooltip>}
                    <Text size={FS.meta} fw={i.watched ? 600 : undefined}>{interfaceLabel(i)}</Text>
                  </Group>
                </Table.Td>
                <Table.Td>
                  {i.up ? <Badge size="sm" color="teal" variant="light">Up</Badge>
                    : !i.adminUp ? <Badge size="sm" color="slate" variant="light">Turned off</Badge>
                      : <Badge size="sm" color={i.watched ? 'rose' : 'gray'} variant={i.watched ? 'filled' : 'light'}>Down</Badge>}
                </Table.Td>
                <Table.Td><Text size={FS.meta}>{formatSpeed(i.speedMbps)}</Text></Table.Td>
                {!compact && (
                  <>
                    <Table.Td><Text size={FS.meta}>{formatBps(i.inBps)}</Text></Table.Td>
                    <Table.Td><Text size={FS.meta}>{formatBps(i.outBps)}</Text></Table.Td>
                    <Table.Td><Text size={FS.meta} c={i.errorsUp ? 'amber' : 'dimmed'} fw={i.errorsUp ? 600 : undefined}>{i.errorsUp ?? 0}</Text></Table.Td>
                  </>
                )}
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
      {sorted.length > 12 && (
        <Button variant="subtle" size="compact-sm" mt={SP.tight} onClick={() => setAll(!all)}>
          {all ? 'Show fewer' : `Show all ${sorted.length} interfaces`}
        </Button>
      )}
    </Box>
  )
}

function DeviceDrawer({ id, canEdit, groups, onClose, onEdit }: {
  id: string; canEdit: boolean; groups: CheckGroup[]; onClose: () => void; onEdit: (d: NetworkDevice) => void
}) {
  const { data, isLoading, error } = useQuery(networkDeviceQuery(id))
  const pause = usePauseDevice()
  const move = useMoveCheck()
  const checkNow = useCheckDeviceNow()
  const remove = useRemoveNetworkDevice()
  const [sharing, setSharing] = useState(false)
  const d = data?.device
  const info = d?.device

  return (
    <Drawer opened onClose={onClose} position="right" size="xl"
      title={d ? <Group gap="xs"><Text fw={700} size="lg">{d.name}</Text><StateBadge d={d} /></Group> : null}>
      {isLoading ? <Skeleton h={300} /> : error || !data || !d ? (
        <Alert color="rose"><Text size={FS.body}>Could not read this device: {error?.message ?? 'it may have been removed'}</Text></Alert>
      ) : (
        <Stack gap="md">
          <Text size={FS.meta} ff="monospace" c="dimmed">{d.host}{d.port !== 161 ? `:${d.port}` : ''}</Text>
          {d.lastDetail && (
            <Alert color={d.state === 'down' ? 'rose' : d.state === 'unknown' || d.failingRounds > 0 ? 'amber' : 'slate'} variant="light">
              <Text size={FS.body}>
                {d.state === 'down' && d.openOutage
                  ? `Down since ${new Date(d.openOutage.startedAt).toLocaleString()}: `
                  : d.lastAt ? `Last read ${ago(d.lastAt)}: ` : ''}
                {d.lastDetail}
              </Text>
            </Alert>
          )}
          {info?.restartedAt && Date.now() - Date.parse(info.restartedAt) < 7 * DAY && (
            <Alert color="amber" variant="light">
              <Text size={FS.body}>It restarted at {new Date(info.restartedAt).toLocaleString()}. Not counted as an outage, because it answered.</Text>
            </Alert>
          )}
          <SimpleGrid cols={{ base: 2, sm: 4 }} spacing="sm">
            <Figure label="Up for" value={info && answered(d) ? formatUptimeSec(info.uptimeSec) : '—'} />
            <Figure label="Last 24 hours" value={formatUptime(d.uptime.day)} />
            <Figure label="7 days" value={formatUptime(d.uptime.week)} />
            <Figure label="30 days" value={formatUptime(d.uptime.month)} />
          </SimpleGrid>
          <RoundStrip rounds={data.rounds} />

          {info && (
            <Box>
              <Text fw={600} size="sm" mb={SP.snug}>What it says about itself</Text>
              <Stack gap={SP.tight}>
                {info.name && <Text size={FS.body}><Text span c="dimmed">Name </Text>{info.name}</Text>}
                {info.location && <Text size={FS.body}><Text span c="dimmed">Location </Text>{info.location}</Text>}
                {info.description && <Text size={FS.meta} c="dimmed" style={{ whiteSpace: 'pre-line' }} lineClamp={3}>{info.description}</Text>}
              </Stack>
            </Box>
          )}

          <Box>
            <Group justify="space-between" mb={SP.snug}>
              <Text fw={600} size="sm">Interfaces</Text>
              {info && <Text size={FS.micro} c="dimmed">Traffic since the read before, as of {ago(info.at)}</Text>}
            </Group>
            {info && info.interfaces.length > 0
              ? <InterfaceTable list={info.interfaces} />
              : <Text size={FS.body} c="dimmed">{d.state === 'pending' ? 'Not read yet.' : 'It has not shared its interfaces.'}</Text>}
          </Box>

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
                          {o.confirmed && <Text size={FS.micro} c="dimmed">Seen from {o.confirmed}</Text>}
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            )}
          </Box>

          <Box>
            <Text fw={600} size="sm" mb={SP.snug}>How it is read</Text>
            <Stack gap={SP.tight}>
              <Text size={FS.body}>Every {d.everySeconds / 60} minute{d.everySeconds === 60 ? '' : 's'}, waiting up to {d.timeoutSeconds} seconds for an answer.</Text>
              {d.watch.length > 0
                ? <Text size={FS.body}>Down when {d.watch.length === 1 ? d.watch[0]!.name : `any of ${d.watch.map((w) => w.name).join(', ')}`} is not up, as well as when it does not answer.</Text>
                : <Text size={FS.body}>Down when it does not answer. No interface is watched.</Text>}
              {!d.limited && (
                <>
                  <Text size={FS.body}>{securityWords(d)}{d.user ? `, as ${d.user}` : ''}. The credentials are kept encrypted and never shown.</Text>
                  <Text size={FS.body}>Read from {d.checkHosts.join(', ')}.</Text>
                </>
              )}
            </Stack>
          </Box>

          {canEdit && (
            <Box>
              <Text fw={600} size="sm" mb={SP.snug}>Group and who can see it</Text>
              <Group gap="xs" align="flex-end">
                <Select size="xs" w={220} aria-label="Group" placeholder="No group" clearable value={d.groupId || null}
                  data={groups.map((g) => ({ value: g.id, label: g.name }))}
                  nothingFoundMessage="No groups yet: add one with New group"
                  onChange={(v) => void run(() => move.mutateAsync({ id: d.id, groupId: v ?? '' }), {
                    failure: 'Not moved',
                    success: ['Moved', v ? `${d.name} is in ${groups.find((g) => g.id === v)?.name}.` : `${d.name} is in no group.`],
                  })} />
                <Button size="xs" variant="default" leftSection={<IconEye size={14} />} onClick={() => setSharing(true)}>Who can see this</Button>
              </Group>
              {sharing && <WhoCanSee target="check" id={d.id} name={d.name} onClose={() => setSharing(false)} />}
            </Box>
          )}

          {(canEdit || d.canOperate) && (
            <Group gap="xs">
              <Button size="xs" variant="default" leftSection={<IconRefresh size={14} />} disabled={d.paused} loading={checkNow.isPending}
                onClick={() => void run(() => checkNow.mutateAsync(d.id), { failure: 'Not checked', success: ['Checking', 'The result shows here in a few seconds.'] })}>
                Check now
              </Button>
              <Button size="xs" variant="default" loading={pause.isPending}
                leftSection={d.paused ? <IconPlayerPlay size={14} /> : <IconPlayerPause size={14} />}
                onClick={() => void run(() => pause.mutateAsync({ id: d.id, paused: !d.paused }), {
                  failure: d.paused ? 'Not resumed' : 'Not paused',
                  success: d.paused ? ['Resumed', `${d.name} is read again.`] : ['Paused', `${d.name} is not read, and sends no alerts, until it is resumed.`],
                })}>
                {d.paused ? 'Resume' : 'Pause'}
              </Button>
              {canEdit && (
                <>
                  <Button size="xs" variant="default" onClick={() => onEdit(d)}>Edit</Button>
                  <RemoveButton name={d.name} onRemove={() => run(() => remove.mutateAsync(d.id), {
                    failure: 'Not removed', success: ['Removed', `${d.name} and its history are removed.`],
                  }).then((ok) => { if (ok) onClose() })} />
                </>
              )}
            </Group>
          )}
        </Stack>
      )}
    </Drawer>
  )
}

/* ── Adding and editing ──────────────────────────────────────────────────── */

function inputOf(d: NetworkDevice): NetworkDeviceInput {
  return {
    name: d.name, host: d.host, port: d.port, version: d.version, community: '', user: d.user, authProto: d.authProto, authPass: '',
    privProto: d.privProto, privPass: '', watch: d.watch, everySeconds: d.everySeconds, timeoutSeconds: d.timeoutSeconds,
    checkFrom: d.checkFrom, paused: d.paused,
  }
}

const BLANK: NetworkDeviceInput = {
  name: '', host: '', port: 161, version: 'v3', community: '', user: '', authProto: 'SHA256', authPass: '', privProto: 'AES', privPass: '',
  watch: [], everySeconds: 60, timeoutSeconds: 10, checkFrom: [], paused: false,
}

/** Whether the form has what a read needs. A saved device keeps its passwords. */
export function readyToRead(v: NetworkDeviceInput, saved: boolean): boolean {
  if (!v.host.trim() || v.checkFrom.length === 0) return false
  if (v.version === 'v2c') return saved || v.community !== ''
  if (!v.user.trim()) return false
  if (saved) return true
  return (!v.authProto || v.authPass.length >= 8) && (!v.privProto || v.privPass.length >= 8)
}

const MONO = { label: { fontFamily: 'monospace', textTransform: 'none' as const } }

/**
 * Finds a v2c community string for a device the admin owns but can't recall:
 * it asks the first chosen server to try the common defaults plus any strings
 * the admin adds, read-only, and fills in the one that answers.
 */
function FindCommunity({ host, port, checkFrom, onUse, onSwitchV3 }: {
  host: string; port: number; checkFrom: string[]; onUse: (c: string) => void; onSwitchV3: () => void
}) {
  const [extra, setExtra] = useState<string[]>([])
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<SnmpDiscovery | null>(null)
  const [reveal, setReveal] = useState(true)
  const ready = host.trim() !== '' && checkFrom.length > 0
  const defaults = SNMP_DEFAULT_COMMUNITIES as readonly string[]
  const addDraft = () => {
    const c = draft.trim()
    if (c && !extra.includes(c) && !defaults.includes(c)) setExtra((p) => [...p, c])
    setDraft('')
  }
  const find = () => {
    setBusy(true)
    setResult(null)
    void run(async () => setResult(await discoverCommunity(host.trim(), port || 161, checkFrom, extra)),
      { failure: 'Could not search' }).finally(() => setBusy(false))
  }
  const tried = [...defaults, ...extra]

  if (result?.found && result.community) {
    const s = result.snmp
    return (
      <Alert color="teal" variant="light" p="sm" data-testid="device-found">
        <Group gap={6} mb={6} wrap="nowrap"><IconCheck size={15} /><Text fw={700} size={FS.body}>Found it</Text></Group>
        {s && (
          <Text size={FS.body} mb={SP.snug}>
            <b>{s.name || host}</b> answered. {s.description ? `${s.description} · ` : ''}up for {formatUptimeSec(s.uptimeSec)}.
          </Text>
        )}
        <Group gap={8} wrap="nowrap" mb={SP.snug}>
          <Text size={FS.meta} c="dimmed">Community that worked</Text>
          <Badge variant="light" color="teal" styles={MONO}>{reveal ? result.community : '••••••'}</Badge>
          <UnstyledButton onClick={() => setReveal((x) => !x)}><Text size={FS.meta} c="blue">{reveal ? 'Hide' : 'Show'}</Text></UnstyledButton>
        </Group>
        <Text size={FS.meta} c="dimmed" mb={SP.snug}>Tried {result.tried} string{result.tried === 1 ? '' : 's'}. Check it is the right device before you use it.</Text>
        <Group gap="xs">
          <Button size="compact-sm" onClick={() => onUse(result.community as string)}>Use this community</Button>
          <Button size="compact-sm" variant="subtle" onClick={() => setResult(null)}>Try other strings</Button>
        </Group>
      </Alert>
    )
  }

  return (
    <Box style={{ border: '1px solid var(--mantine-color-blue-3)', background: 'var(--mantine-color-blue-0)', borderRadius: 8, padding: 12 }}>
      <Group gap={6} mb={4} wrap="nowrap"><IconSearch size={14} /><Text fw={700} size={FS.body}>Find the community string</Text></Group>
      <Text size={FS.meta} c="dimmed" mb={SP.snug}>
        A server you chose tries each string below against the device, read-only, and stops at the first that answers. Nothing on the device changes.
      </Text>
      <Group gap={6} mb={SP.snug}>
        {SNMP_DEFAULT_COMMUNITIES.map((c) => <Badge key={c} variant="light" color="blue" styles={MONO}>{c}</Badge>)}
        {extra.map((c) => (
          <Badge key={c} variant="outline" color="gray" styles={MONO}
            rightSection={<IconX size={11} style={{ cursor: 'pointer', display: 'block' }} onClick={() => setExtra((p) => p.filter((x) => x !== c))} />}>{c}</Badge>
        ))}
      </Group>
      <Group gap="xs" mb={SP.snug} wrap="nowrap" align="flex-end">
        <TextInput style={{ flex: 1 }} placeholder="Add a string you know, from a runbook or the vendor default" value={draft}
          onChange={(e) => setDraft(e.currentTarget.value)} onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); addDraft() } }} />
        <Button variant="default" onClick={addDraft} disabled={!draft.trim()}>Add</Button>
      </Group>
      {result && !result.found && (
        <Alert color="rose" variant="light" mb={SP.snug} p="xs" data-testid="device-none">
          <Text size={FS.meta}>None of the {result.tried} strings was accepted. The device may use a different community, speak only SNMPv3, have SNMP turned off, or be read better from another server.{result.detail ? ` (${result.detail})` : ''}</Text>
        </Alert>
      )}
      <Group gap="xs">
        <Button size="compact-sm" loading={busy} disabled={!ready} onClick={find}>Find it</Button>
        {result && !result.found && <Button size="compact-sm" variant="subtle" onClick={onSwitchV3}>Switch to SNMPv3</Button>}
        <Text size={FS.meta} c="dimmed">{ready ? `Tries ${tried.length} string${tried.length === 1 ? '' : 's'} on port ${port || 161}, through the first server you chose.` : 'Fill in the address and a server first.'}</Text>
      </Group>
    </Box>
  )
}

function DeviceForm({ device, onClose, onSaved }: { device?: NetworkDevice; onClose: () => void; onSaved: (d: NetworkDevice) => void }) {
  const [v, setV] = useState<NetworkDeviceInput>(device ? inputOf(device) : BLANK)
  const [tested, setTested] = useState<DeviceAttempt | null>(null)
  const [testing, setTesting] = useState(false)
  const [finding, setFinding] = useState(false)
  const [autofilled, setAutofilled] = useState(false)
  const add = useAddNetworkDevice()
  const change = useChangeNetworkDevice()
  const { data: assets } = useQuery(assetsQuery())
  const servers = (assets ?? []).filter((a) => (a.kind ?? 'server') !== 'android').map((a) => ({ value: a.id, label: assetName(a) }))
  const set = <K extends keyof NetworkDeviceInput>(k: K, val: NetworkDeviceInput[K]) => { setV((p) => ({ ...p, [k]: val })); setTested(null) }
  const saving = add.isPending || change.isPending
  const savedSecret = !!device?.hasSecret
  const ready = readyToRead(v, savedSecret && device?.version === v.version && device?.user === v.user)
  // The interfaces to choose from: the test's answer, else the last read.
  const choices: DeviceInterface[] = tested?.snmp?.interfaces ?? device?.device?.interfaces ?? []
  const watched = new Set(v.watch.map((w) => w.index))
  const toggle = (i: DeviceInterface, on: boolean) =>
    setV((p) => ({ ...p, watch: on ? [...p.watch, { index: i.index, name: interfaceLabel(i) }] : p.watch.filter((w) => w.index !== i.index) }))
  const keepNote = savedSecret ? 'Saved. Leave empty to keep it.' : undefined

  const save = async () => {
    const d = device ? await change.mutateAsync({ id: device.id, input: v }) : await add.mutateAsync(v)
    onSaved(d)
  }

  return (
    <Drawer opened onClose={onClose} position="right" size="lg" title={<Text fw={700}>{device ? `Edit ${device.name}` : 'Add a network device'}</Text>}>
      <Stack gap="sm">
        <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
          <TextInput label="Address" placeholder="10.0.0.2 or core-sw1.lan" required data-autofocus style={{ gridColumn: 'span 2' }}
            value={v.host} onChange={(e) => set('host', e.currentTarget.value.trim())} />
          <NumberInput label="Port" min={1} max={65535} value={v.port} onChange={(x) => set('port', Number(x) || 161)} />
        </SimpleGrid>
        <TextInput label="Name" placeholder="The address, if left empty" value={v.name} maxLength={100}
          onChange={(e) => set('name', e.currentTarget.value)} />
        <MultiSelect label="Read it from these servers" required placeholder={v.checkFrom.length < 3 ? 'Choose one to three' : undefined}
          data={servers} value={v.checkFrom} onChange={(ids) => set('checkFrom', ids)} maxValues={3} searchable clearable
          description="Servers with an Argus agent on the device's network. Each reads it on its own; any one getting an answer means it is up, so a second server keeps one server's trouble from calling the device down." />

        <Box>
          <Text size="sm" fw={500} mb={4}>SNMP version</Text>
          <SegmentedControl value={v.version} onChange={(x) => set('version', x as SnmpVersion)}
            data={[{ value: 'v3', label: 'SNMPv3 (recommended)' }, { value: 'v2c', label: 'SNMP v2c' }]} />
        </Box>
        {v.version === 'v2c' ? (
          <>
            <Box>
              <Group justify="space-between" mb={4} wrap="nowrap">
                <Text size="sm" fw={500}>Community string {!savedSecret && <Text span c="red">*</Text>}
                  {autofilled && <Badge ml={6} size="xs" color="teal" variant="light">Found by trying common strings</Badge>}</Text>
                <UnstyledButton onClick={() => setFinding((f) => !f)}>
                  <Group gap={4} wrap="nowrap"><IconSearch size={13} /><Text size={FS.meta} fw={600} c="blue">{finding ? 'Close' : "Don't know it? Find it"}</Text></Group>
                </UnstyledButton>
              </Group>
              {finding ? (
                <FindCommunity host={v.host} port={v.port} checkFrom={v.checkFrom}
                  onUse={(c) => { set('community', c); setAutofilled(true); setFinding(false) }}
                  onSwitchV3={() => { set('version', 'v3'); setFinding(false) }} />
              ) : (
                <PasswordInput required={!savedSecret} placeholder={keepNote} value={v.community} autoComplete="off"
                  onChange={(e) => { set('community', e.currentTarget.value); setAutofilled(false) }} />
              )}
              {autofilled && !finding && (
                <Text size={FS.meta} c="dimmed" mt={4}>Filled from the device. Sealed on save and never shown again, like any Argus secret.</Text>
              )}
            </Box>
            <Alert color="amber" variant="light">
              <Text size={FS.body}>SNMP v2c sends the community string in the clear, so anyone on the network between the server and the device can read it. Use a read-only community, or SNMPv3 if the device supports it.</Text>
            </Alert>
          </>
        ) : (
          <>
            <TextInput label="User" required value={v.user} autoComplete="off" onChange={(e) => set('user', e.currentTarget.value.trim())}
              description="An SNMPv3 user with read-only access on the device." />
            <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
              <Select label="Authentication" allowDeselect={false} value={v.authProto || 'none'}
                data={[{ value: 'none', label: 'None' }, ...AUTH_PROTOS.map((p) => ({ value: p, label: p }))]}
                onChange={(x) => setV((p) => ({ ...p, authProto: x === 'none' || !x ? '' : x, ...(x === 'none' ? { privProto: '', authPass: '', privPass: '' } : {}) }))} />
              <PasswordInput label="Authentication password" disabled={!v.authProto} placeholder={v.authProto ? keepNote ?? 'At least 8 characters' : undefined}
                value={v.authPass} autoComplete="new-password" onChange={(e) => set('authPass', e.currentTarget.value)} />
              <Select label="Encryption" allowDeselect={false} value={v.privProto || 'none'} disabled={!v.authProto}
                data={[{ value: 'none', label: 'None' }, ...PRIV_PROTOS.map((p) => ({ value: p, label: p }))]}
                onChange={(x) => set('privProto', x === 'none' || !x ? '' : x)} />
              <PasswordInput label="Encryption password" disabled={!v.privProto} placeholder={v.privProto ? keepNote ?? 'At least 8 characters' : undefined}
                value={v.privPass} autoComplete="new-password" onChange={(e) => set('privPass', e.currentTarget.value)} />
            </SimpleGrid>
            {!v.privProto && (
              <Text size={FS.meta} c="amber">Without encryption, what the device answers travels in the clear.</Text>
            )}
          </>
        )}
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <Select label="Check every" data={[{ value: '60', label: '1 minute' }, { value: '300', label: '5 minutes' }, { value: '600', label: '10 minutes' }]}
            value={String(v.everySeconds)} onChange={(x) => set('everySeconds', Number(x ?? 60))} allowDeselect={false} />
          <NumberInput label="Wait for an answer (seconds)" min={5} max={60} value={v.timeoutSeconds}
            onChange={(x) => set('timeoutSeconds', Number(x) || 10)} />
        </SimpleGrid>
        {device && (
          <Switch label="Paused: not read, and no alerts" checked={v.paused} onChange={(e) => set('paused', e.currentTarget.checked)} />
        )}

        <Box>
          <Group justify="space-between" mb={4}>
            <Text size="sm" fw={500}>Watch interfaces</Text>
            <Button size="compact-sm" variant="light" loading={testing} disabled={!ready}
              onClick={() => {
                setTesting(true)
                void run(async () => setTested(await testNetworkDevice(v, device?.id)), { failure: 'Could not test' }).finally(() => setTesting(false))
              }}>
              {choices.length > 0 ? 'Read it again' : 'Test and find interfaces'}
            </Button>
          </Group>
          <Text size={FS.meta} c="dimmed" mb={SP.snug}>
            The device is down when it stops answering. Tick an interface, such as an uplink, to call it down when that interface is down too.
          </Text>
          {tested && (
            <Alert color={tested.ok || tested.fail === 'interface' ? 'teal' : 'rose'} variant="light" mb={SP.snug} data-testid="device-test">
              <Text size={FS.body}>
                {tested.snmp ? `${tested.snmp.name || v.host} answered${tested.from ? ` from ${tested.from}` : ''}` : 'No answer'}
                {tested.snmp ? `, up for ${formatUptimeSec(tested.snmp.uptimeSec)}, with ${tested.snmp.interfaces.length} interfaces.` : `: ${tested.detail}.`}
                {tested.snmp && tested.fail === 'interface' ? ` ${tested.detail}.` : ''}
              </Text>
            </Alert>
          )}
          {choices.length > 0 ? (
            <ScrollArea.Autosize mah={260} type="auto" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
              <Stack gap={4} p="xs">
                {[...choices].sort((a, b) => Number(b.up) - Number(a.up) || a.index - b.index).map((i) => (
                  <Checkbox key={i.index} checked={watched.has(i.index)} onChange={(e) => toggle(i, e.currentTarget.checked)}
                    label={<Group gap={6} wrap="nowrap"><Text size="sm">{interfaceLabel(i)}</Text>
                      <Badge size="xs" color={i.up ? 'teal' : 'gray'} variant="light">{i.up ? 'Up' : 'Down'}</Badge>
                      {i.speedMbps ? <Text size={FS.micro} c="dimmed">{formatSpeed(i.speedMbps)}</Text> : null}</Group>} />
                ))}
              </Stack>
            </ScrollArea.Autosize>
          ) : v.watch.length > 0 ? (
            <Text size={FS.body}>Watching {v.watch.map((w: WatchedInterface) => w.name).join(', ')}. Test it to change them.</Text>
          ) : (
            <Text size={FS.body} c="dimmed">{ready ? 'Test it to list its interfaces.' : 'Fill in the address, a server and the credentials to test it.'}</Text>
          )}
        </Box>

        <Group justify="flex-end" gap="xs" mt="xs">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={saving} disabled={!ready}
            onClick={() => void run(save, {
              failure: device ? 'Not saved' : 'Not added',
              success: device ? ['Saved', `${v.name || v.host} is read with the new settings.`] : ['Added', 'Argus is reading it now.'],
            })}>
            {device ? 'Save' : 'Add device'}
          </Button>
        </Group>
      </Stack>
    </Drawer>
  )
}
