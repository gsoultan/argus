import { Alert, Badge, Box, Button, Group, Select, Stack, Tabs, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconBrandAws, IconCalendarTime, IconCirclePlus, IconInfoCircle, IconSearch } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { CloudOverview } from '~/components/cloud/CloudOverview'
import { ChangesList } from '~/components/cloud/ChangesList'
import { SchedulesList } from '~/components/cloud/Schedules'
import { InstancesTable, NetworkTables, StorageTables } from '~/components/cloud/CloudTables'
import { NewAddress, NewDisk } from '~/components/cloud/StorageActions'
import { CLOUD_TABS, CLOUD_TAB_LABEL, parseCloudTab, type CloudTab } from '~/components/cloud/tabs'
import { ButtonLink } from '~/components/links'
import { PageBody, PageHeader, Toolbar } from '~/components/page'
import { relTime } from '~/components/primitives'
import { cloudAccountsQuery, cloudChangesQuery, cloudItemsQuery, cloudSchedulesQuery, type CloudItem, type CloudKind } from '~/lib/cloud'
import { guard } from '~/lib/guard'
import { isConfigured } from '~/lib/live'
import { meQuery } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import { FS } from '~/theme'

/**
 * Cloud: the connected AWS accounts and what is in them, read every five
 * minutes. On an account Argus may change, admins launch, stop, resize and
 * remove instances from here; Changes lists every one, with who approved it.
 * See plans/cloud-management/design.md.
 */
export const Route = createFileRoute('/cloud/')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  validateSearch: (s: Record<string, unknown>): { tab?: CloudTab } => ({ tab: parseCloudTab(s.tab) }),
  component: Cloud,
})

const KINDS: CloudKind[] = ['instance', 'volume', 'firewall', 'network', 'subnet', 'address', 'snapshot', 'image']

function Cloud() {
  const navigate = useNavigate({ from: Route.fullPath })
  const tab: CloudTab = parseCloudTab(Route.useSearch().tab) ?? 'overview'
  const setTab = (t: CloudTab) => void navigate({ search: { tab: t === 'overview' ? undefined : t }, replace: true })
  const { data: me } = useQuery(meQuery())
  const admin = canSee(me?.role, 'admin')
  const accounts = useQuery(cloudAccountsQuery())
  const changes = useQuery(cloudChangesQuery({ enabled: tab === 'changes' }))
  const pending = useQuery(cloudChangesQuery({ pending: true }))
  const schedules = useQuery(cloudSchedulesQuery({ enabled: tab === 'schedules' || tab === 'instances' }))
  const waitingSchedules = schedules.data?.schedules.filter((s) => s.state === 'awaiting_approval' || s.pending).length ?? 0
  const q: Record<CloudKind, { data?: CloudItem[]; isPending: boolean }> = {
    instance: useQuery(cloudItemsQuery('instance')),
    volume: useQuery(cloudItemsQuery('volume')),
    firewall: useQuery(cloudItemsQuery('firewall')),
    network: useQuery(cloudItemsQuery('network')),
    subnet: useQuery(cloudItemsQuery('subnet')),
    address: useQuery(cloudItemsQuery('address')),
    snapshot: useQuery(cloudItemsQuery('snapshot')),
    image: useQuery(cloudItemsQuery('image')),
  }
  const [making, setMaking] = useState<'disk' | 'address' | null>(null)
  const [account, setAccount] = useState<string | null>(null)
  const [search, setSearch] = useState('')

  // Every id to a readable name, so a disk says which instance it is on.
  const names = useMemo(() => {
    const m = new Map<string, string>()
    for (const k of KINDS) for (const it of q[k].data ?? []) if (it.resource.name !== it.resource.id) m.set(it.resource.id, it.resource.name)
    return m
  }, [q.instance.data, q.volume.data, q.firewall.data, q.network.data, q.subnet.data, q.address.data, q.snapshot.data, q.image.data])

  const narrow = (list?: CloudItem[]) => {
    if (!list) return list
    const needle = search.trim().toLowerCase()
    return list.filter((it) => (!account || it.accountId === account) && (!needle ||
      [it.resource.name, it.resource.id, it.resource.publicIp ?? '', ...(it.resource.addresses ?? []),
        ...Object.entries(it.resource.tags ?? {}).map(([k, v]) => `${k}=${v}`)].some((s) => s.toLowerCase().includes(needle))))
  }
  const filtered = Boolean(account || search.trim())
  const list = accounts.data?.accounts ?? []
  const newest = list.map((a) => a.syncedAt).filter(Boolean).sort().at(-1) ?? null

  const description = 'Every instance, disk, image and firewall in your AWS accounts, and which instances have an Argus server. Launch, stop and resize instances, and make, grow, snapshot and remove disks, with approval where the environment asks for it.'
  if (!isConfigured()) {
    return (
      <Box>
        <PageHeader title="Cloud" description={description} />
        <PageBody>
          <Alert icon={<IconInfoCircle size={16} />} color="amber" variant="light">
            No control plane is configured, so there are no cloud accounts to show.
          </Alert>
        </PageBody>
      </Box>
    )
  }

  const count = (k: CloudKind) => q[k].data?.length
  const badge = (n?: number) => (n ? <Badge size="xs" variant="light" color="slate" circle={false}>{n}</Badge> : undefined)
  const hasAccounts = list.length > 0
  const canChange = list.some((a) => a.access === 'manage')

  return (
    <Box>
      <PageHeader
        title="Cloud"
        status={<Badge variant="light" color="slate" radius="sm" leftSection={<IconBrandAws size={12} />}>AWS</Badge>}
        description={description}
        actions={admin && hasAccounts ? (
          <Group gap="xs">
            <ButtonLink to="/cloud/connect" size="sm" variant="default" leftSection={<IconBrandAws size={16} />}>Connect an account</ButtonLink>
            {canChange && tab === 'schedules' && <ButtonLink to="/cloud/schedule" size="sm" leftSection={<IconCalendarTime size={16} />}>Make a schedule</ButtonLink>}
            {canChange && tab === 'disks' && <Button size="sm" leftSection={<IconCirclePlus size={16} />} onClick={() => setMaking('disk')}>New disk</Button>}
            {canChange && tab === 'network' && <Button size="sm" leftSection={<IconCirclePlus size={16} />} onClick={() => setMaking('address')}>Allocate an Elastic IP</Button>}
            {canChange && tab !== 'schedules' && tab !== 'disks' && tab !== 'network' && <ButtonLink to="/cloud/launch" size="sm" leftSection={<IconCirclePlus size={16} />}>Launch an instance</ButtonLink>}
          </Group>
        ) : undefined}
        tabs={hasAccounts ? (
          <Tabs value={tab} onChange={(v) => v && setTab(v as CloudTab)} keepMounted={false}>
            <Tabs.List className="argus-tabs-scroll" style={{ borderBottom: 0 }}>
              {CLOUD_TABS.map((t) => (
                <Tabs.Tab key={t} value={t} fz="sm" rightSection={
                  t === 'instances' ? badge(count('instance')) : t === 'disks' ? badge(count('volume')) : t === 'network' ? badge(count('firewall'))
                    : t === 'changes' && pending.data?.length ? <Badge size="xs" variant="filled" color="amber" circle={false}>{pending.data.length}</Badge>
                    : t === 'schedules' && waitingSchedules ? <Badge size="xs" variant="filled" color="amber" circle={false}>{waitingSchedules}</Badge> : undefined
                }>
                  {CLOUD_TAB_LABEL[t]}
                </Tabs.Tab>
              ))}
            </Tabs.List>
          </Tabs>
        ) : undefined}
      />
      <PageBody>
        <Stack gap="md">
          {accounts.error && <Alert color="rose" variant="light">{(accounts.error as Error).message}</Alert>}
          {tab !== 'overview' && tab !== 'changes' && tab !== 'schedules' && hasAccounts && (
            <Toolbar right={<Text size={FS.meta} c="dimmed">{newest ? `Read ${relTime(newest)}` : null}</Text>}>
              <TextInput placeholder="Name, id, address or tag" leftSection={<IconSearch size={14} />} value={search}
                onChange={(e) => setSearch(e.currentTarget.value)} w={{ base: '100%', sm: 300 }} aria-label="Search cloud resources" />
              {list.length > 1 && (
                <Select placeholder="All accounts" clearable value={account} onChange={setAccount} w={{ base: '100%', sm: 220 }}
                  data={list.map((a) => ({ value: a.id, label: a.name }))} aria-label="Account" />
              )}
            </Toolbar>
          )}
          {tab === 'overview' && (
            accounts.data ? (
              <CloudOverview accounts={list} bases={accounts.data.bases} admin={admin} onTab={setTab}
                instances={q.instance.data} firewalls={q.firewall.data} volumes={q.volume.data} addresses={q.address.data} />
            ) : (
              <Group justify="center" p="xl"><Text c="dimmed" size={FS.body}>Loading…</Text></Group>
            )
          )}
          {tab === 'instances' && <InstancesTable items={narrow(q.instance.data)} loading={q.instance.isPending} names={names} filtered={filtered} />}
          {tab === 'schedules' && (
            <SchedulesList schedules={schedules.data?.schedules} keep={schedules.data?.keepRunning} loading={schedules.isPending} names={names} />
          )}
          {tab === 'changes' && (
            <ChangesList changes={changes.data} loading={changes.isPending}
              empty={{ title: 'No changes yet', text: 'Launching, stopping, resizing and removing instances from Argus shows up here, with who asked, who approved and what AWS said.' }} />
          )}
          {tab === 'disks' && (
            <StorageTables volumes={narrow(q.volume.data)} snapshots={narrow(q.snapshot.data)} images={narrow(q.image.data)}
              loading={q.volume.isPending} names={names} filtered={filtered} />
          )}
          {tab === 'network' && (
            <NetworkTables firewalls={narrow(q.firewall.data)} networks={narrow(q.network.data)} subnets={q.subnet.data}
              addresses={narrow(q.address.data)} loading={q.firewall.isPending} names={names} filtered={filtered} />
          )}
        </Stack>
        {making === 'disk' && <NewDisk onClose={() => setMaking(null)} />}
        {making === 'address' && <NewAddress onClose={() => setMaking(null)} />}
      </PageBody>
    </Box>
  )
}
