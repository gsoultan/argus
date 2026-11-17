import { Alert, Badge, Box, Button, Group, Select, SimpleGrid, Stack, Tabs, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconBrandAws, IconBrandGoogle, IconCalendarTime, IconCirclePlus, IconCloudPlus, IconInfoCircle, IconSearch, IconShieldPlus } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { AccessPanel } from '~/components/cloud/Access'
import { CloudOverview } from '~/components/cloud/CloudOverview'
import { CostsPanel } from '~/components/cloud/Costs'
import { ReservationsPanel } from '~/components/cloud/Reservations'
import { ChangesList } from '~/components/cloud/ChangesList'
import { SchedulesList } from '~/components/cloud/Schedules'
import { InstancesTable, NetworkTables, ScalingGroupsTable, StorageTables } from '~/components/cloud/CloudTables'
import { NewBalancer, NewTargetGroup } from '~/components/cloud/BalancerActions'
import { SuggestionsCard } from '~/components/cloud/Suggestion'
import { ScaleGroupsCard } from '~/components/cloud/ScaleGroups'
import { ReadReplicasCard } from '~/components/cloud/ReadReplicas'
import { cloudSuggestionsQuery } from '~/lib/cloudSuggestions'
import { scaleGroupsQuery } from '~/lib/cloudScale'
import { NewFirewall } from '~/components/cloud/NetworkActions'
import { NewAddress, NewDisk } from '~/components/cloud/StorageActions'
import { FindingsPanel } from '~/components/cloud/Findings'
import { cloudFindingsQuery } from '~/lib/cloudFindings'
import { CLOUD_TABS, CLOUD_TAB_LABEL, parseCloudTab, type CloudTab } from '~/components/cloud/tabs'
import { ButtonLink } from '~/components/links'
import { PageBody, PageHeader, Toolbar } from '~/components/page'
import { relTime } from '~/components/primitives'
import { allows, cloudAccountsQuery, cloudChangesQuery, cloudItemsQuery, cloudSchedulesQuery, type CloudItem, type CloudKind } from '~/lib/cloud'
import { cloudAccessQuery } from '~/lib/cloudAccess'
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

const KINDS: CloudKind[] = ['instance', 'volume', 'firewall', 'network', 'subnet', 'address', 'snapshot', 'image', 'load-balancer', 'target-group', 'scaling-group']

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
  const access = useQuery(cloudAccessQuery({ waiting: true }))
  const waitingAccess = access.data?.grants.length ?? 0
  const findings = useQuery(cloudFindingsQuery())
  const suggestions = useQuery(cloudSuggestionsQuery(tab === 'scaling'))
  const scaleGroups = useQuery(scaleGroupsQuery(tab === 'scaling'))
  const highRisks = findings.data?.items.filter((f) => f.group === 'risk' && f.severity === 'high').length ?? 0
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
    'load-balancer': useQuery(cloudItemsQuery('load-balancer')),
    'target-group': useQuery(cloudItemsQuery('target-group')),
    'scaling-group': useQuery(cloudItemsQuery('scaling-group')),
    database: useQuery({ ...cloudItemsQuery('database'), enabled: tab === 'scaling' }),
  }
  const [making, setMaking] = useState<'disk' | 'address' | 'firewall' | 'balancer' | 'target-group' | null>(null)
  const [account, setAccount] = useState<string | null>(null)
  const [search, setSearch] = useState('')

  // Every id to a readable name, so a disk says which instance it is on.
  const names = useMemo(() => {
    const m = new Map<string, string>()
    for (const k of KINDS) for (const it of q[k].data ?? []) if (it.resource.name !== it.resource.id) m.set(it.resource.id, it.resource.name)
    return m
  }, [q.instance.data, q.volume.data, q.firewall.data, q.network.data, q.subnet.data, q.address.data, q.snapshot.data, q.image.data, q['load-balancer'].data, q['target-group'].data, q['scaling-group'].data])

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

  const description = 'Every instance, disk, image, firewall and load balancer in your AWS accounts and Google Cloud projects, and which instances have an Argus server. Launch, stop and resize instances, manage disks, firewall rules and load balancer targets, with approval where the environment asks for it, and see what it all costs.'
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
  const can = (action: string) => list.some((a) => a.access === 'manage' && allows(a, action))
  const allocators = new Set(list.filter((a) => a.access === 'manage' && allows(a, 'address-allocate')).map((a) => a.provider))
  const addressButton = allocators.size > 1 ? 'New public address' : allocators.has('gcp') ? 'Reserve a static address' : 'Allocate an Elastic IP'

  return (
    <Box>
      <PageHeader
        title="Cloud"
        status={<Group gap={6}>
          {(list.length === 0 || list.some((a) => a.provider === 'aws')) && <Badge variant="light" color="slate" radius="sm" leftSection={<IconBrandAws size={12} />}>AWS</Badge>}
          {list.some((a) => a.provider === 'gcp') && <Badge variant="light" color="slate" radius="sm" tt="none" leftSection={<IconBrandGoogle size={11} />}>Google Cloud</Badge>}
        </Group>}
        description={description}
        actions={admin && hasAccounts ? (
          <Group gap="xs">
            <ButtonLink to="/cloud/connect" size="sm" variant="default" leftSection={<IconCloudPlus size={16} />}>Connect an account</ButtonLink>
            {canChange && tab === 'schedules' && <ButtonLink to="/cloud/schedule" size="sm" leftSection={<IconCalendarTime size={16} />}>Make a schedule</ButtonLink>}
            {can('volume-create') && tab === 'disks' && <Button size="sm" leftSection={<IconCirclePlus size={16} />} onClick={() => setMaking('disk')}>New disk</Button>}
            {can('address-allocate') && tab === 'network' && <Button size="sm" variant="default" leftSection={<IconCirclePlus size={16} />} onClick={() => setMaking('address')}>{addressButton}</Button>}
            {can('firewall-create') && tab === 'network' && <Button size="sm" leftSection={<IconShieldPlus size={16} />} onClick={() => setMaking('firewall')}>New security group</Button>}
            {can('launch') && tab !== 'schedules' && tab !== 'disks' && tab !== 'network' && tab !== 'access' && tab !== 'costs' && tab !== 'reservations' && tab !== 'risks' && <ButtonLink to="/cloud/launch" size="sm" leftSection={<IconCirclePlus size={16} />}>Launch an instance</ButtonLink>}
          </Group>
        ) : undefined}
        tabs={hasAccounts ? (
          <Tabs value={tab} onChange={(v) => v && setTab(v as CloudTab)} keepMounted={false}>
            <Tabs.List className="argus-tabs-scroll" style={{ borderBottom: 0 }}>
              {CLOUD_TABS.map((t) => (
                <Tabs.Tab key={t} value={t} fz="sm" rightSection={
                  t === 'instances' ? badge(count('instance')) : t === 'disks' ? badge(count('volume')) : t === 'network' ? badge(count('firewall')) : t === 'scaling' ? badge(count('scaling-group'))
                    : t === 'changes' && pending.data?.length ? <Badge size="xs" variant="filled" color="amber" circle={false}>{pending.data.length}</Badge>
                    : t === 'risks' && highRisks ? <Badge size="xs" variant="filled" color="rose" circle={false}>{highRisks} high</Badge>
                    : t === 'access' && waitingAccess ? <Badge size="xs" variant="filled" color="amber" circle={false}>{waitingAccess}</Badge>
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
          {tab !== 'overview' && tab !== 'changes' && tab !== 'schedules' && tab !== 'access' && tab !== 'costs' && tab !== 'reservations' && tab !== 'risks' && hasAccounts && (
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
              <CloudOverview accounts={list} bases={accounts.data.bases} admin={admin} platform={!me?.org || me.org.default} onTab={setTab}
                findings={findings.data}
                instances={q.instance.data} firewalls={q.firewall.data} volumes={q.volume.data} addresses={q.address.data} />
            ) : (
              <Group justify="center" p="xl"><Text c="dimmed" size={FS.body}>Loading…</Text></Group>
            )
          )}
          {tab === 'instances' && <InstancesTable items={narrow(q.instance.data)} loading={q.instance.isPending} names={names} filtered={filtered} />}
          {tab === 'schedules' && (
            <SchedulesList schedules={schedules.data?.schedules} keep={schedules.data?.keepRunning} loading={schedules.isPending} names={names} />
          )}
          {tab === 'access' && <AccessPanel accounts={list} />}
          {tab === 'risks' && <FindingsPanel names={names} admin={admin} />}
          {tab === 'costs' && <CostsPanel accounts={list} onTab={setTab} />}
          {tab === 'reservations' && <ReservationsPanel onTab={setTab} />}
          {tab === 'changes' && (
            <ChangesList changes={changes.data} loading={changes.isPending}
              empty={{ title: 'No changes yet', text: 'Launching, stopping, resizing and removing instances from Argus shows up here, with who asked, who approved and what AWS said.' }} />
          )}
          {tab === 'disks' && (
            <StorageTables volumes={narrow(q.volume.data)} snapshots={narrow(q.snapshot.data)} images={narrow(q.image.data)}
              loading={q.volume.isPending} names={names} filtered={filtered} />
          )}
          {tab === 'network' && (
            <NetworkTables firewalls={narrow(q.firewall.data)} instances={q.instance.data} networks={narrow(q.network.data)} subnets={q.subnet.data}
              addresses={narrow(q.address.data)} balancers={narrow(q['load-balancer'].data)} targetGroups={narrow(q['target-group'].data)}
              loading={q.firewall.isPending} names={names} filtered={filtered} onMake={admin && can('firewall-create') ? setMaking : undefined} />
          )}
          {tab === 'scaling' && (
            <Stack gap="md">
              <SimpleGrid cols={{ base: 1, lg: (suggestions.data?.length ?? 0) > 0 ? 3 : 1 }} spacing="md">
                <Box style={{ gridColumn: (suggestions.data?.length ?? 0) > 0 ? 'span 2' : undefined, minWidth: 0 }}>
                  <ScaleGroupsCard groups={scaleGroups.data} loading={scaleGroups.isPending} instances={q.instance.data} canMake={admin && canChange} />
                </Box>
                <SuggestionsCard items={suggestions.data} />
              </SimpleGrid>
              <Text size={FS.meta} c="dimmed">
                Auto Scaling groups in your AWS accounts. Each keeps how many servers it wants running, between its minimum and maximum, and replaces one that fails.
              </Text>
              <ScalingGroupsTable items={narrow(q['scaling-group'].data)} loading={q['scaling-group'].isPending} names={names} filtered={filtered}
                targetGroups={q['target-group'].data} />
              <ReadReplicasCard items={narrow(q.database.data)} loading={q.database.isPending} canChange={admin && canChange} />
            </Stack>
          )}
        </Stack>
        {making === 'disk' && <NewDisk onClose={() => setMaking(null)} />}
        {making === 'address' && <NewAddress onClose={() => setMaking(null)} />}
        {making === 'firewall' && <NewFirewall onClose={() => setMaking(null)} />}
        {making === 'balancer' && <NewBalancer onClose={() => setMaking(null)} />}
        {making === 'target-group' && <NewTargetGroup onClose={() => setMaking(null)} />}
      </PageBody>
    </Box>
  )
}
