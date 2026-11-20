import {
  ActionIcon, Alert, Badge, Box, Button, Card, Checkbox, Group, NumberInput, Radio, Select, SimpleGrid, Stack, Switch, Text, TextInput, Textarea, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconBrandDebian, IconBrandUbuntu, IconBrandWindows, IconCirclePlus, IconCloud, IconCopy, IconPlus, IconServer2, IconUserCheck, IconX,
} from '@tabler/icons-react'
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { reportChange } from '~/components/cloud/InstanceActions'
import { EnvBadge } from '~/components/cloud/common'
import { ButtonLink } from '~/components/links'
import { PageBody, PageHeader } from '~/components/page'
import {
  SCHEDULE_TAG, cloudAccountsQuery, cloudItemsQuery, cloudLaunchOptionsQuery, cloudSchedulesQuery, openToInternet, useAskCloudChange, windowWords, type CloudImage,
} from '~/lib/cloud'
import { cloudReservationsQuery } from '~/lib/cloudReservations'
import { guard } from '~/lib/guard'
import { notifyError } from '~/lib/notify'
import { FS, INK, SP } from '~/theme'

/**
 * Launch an instance: where, what image, how big, which network, and
 * whether it comes up as an Argus server. One page with a summary that says
 * exactly what will be asked of AWS, and whether someone else approves it
 * first. The control plane checks all of it again.
 */
export const Route = createFileRoute('/cloud/launch')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  validateSearch: (s: Record<string, unknown>): { like?: string; account?: string } => ({
    like: typeof s.like === 'string' ? s.like : undefined,
    account: typeof s.account === 'string' ? s.account : undefined,
  }),
  component: Launch,
})

const COMMON_SIZES = ['t3.micro', 't3.small', 't3.medium', 't3.large', 'm7i.large', 'c7i.large']
const AZURE_COMMON_SIZES = ['Standard_B1s', 'Standard_B2s', 'Standard_B2ms', 'Standard_D2s_v5', 'Standard_D4s_v5', 'Standard_E2s_v5']
const GCP_COMMON_SIZES = ['e2-small', 'e2-medium', 'e2-standard-2', 'e2-standard-4', 'n2-standard-2', 'n2-standard-4']
const HOSTNAME = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/
const MAX_COUNT = 10
const MAX_EXTRA_DISKS = 4

/**
 * The hostnames a launch makes, as the control plane works them out: one is
 * the name; several number on from a name ending in -NN, or add -01 onwards.
 */
function launchNames(name: string, count: number): string[] {
  if (count <= 1) return [name]
  const m = /^(.*?)-(\d{1,4})$/.exec(name)
  const base = m?.[1] ?? name
  const width = m?.[2]?.length ?? 2
  const first = m?.[2] ? Number(m[2]) : 1
  return Array.from({ length: count }, (_, i) => `${base}-${String(first + i).padStart(width, '0')}`)
}

/** web-04 after web-03; web-01 after web. */
function nextName(name: string): string {
  const m = /^(.*?)-(\d{1,4})$/.exec(name)
  return m?.[2] ? `${m[1]}-${String(Number(m[2]) + 1).padStart(m[2].length, '0')}` : `${name}-02`
}

function imageIcon(img: CloudImage) {
  if (!img.linux) return IconBrandWindows
  if (img.family.startsWith('ubuntu')) return IconBrandUbuntu
  if (img.family.startsWith('debian')) return IconBrandDebian
  return IconServer2
}

function Launch() {
  const navigate = useNavigate()
  const { like, account: likeAccount } = Route.useSearch()
  const instances = useQuery({ ...cloudItemsQuery('instance'), enabled: Boolean(like) })
  const source = like ? instances.data?.find((i) => i.resource.id === like && (!likeAccount || i.accountId === likeAccount)) : undefined
  const accounts = useQuery(cloudAccountsQuery())
  const managed = (accounts.data?.accounts ?? []).filter((a) => a.access === 'manage')
  const [accountId, setAccountId] = useState<string | null>(null)
  const account = managed.find((a) => a.id === accountId)
  const gcp = account?.provider === 'gcp'
  const azure = account?.provider === 'azure'
  /** What the provider calls one: a VM in Google Cloud and Azure. */
  const vm = gcp || azure
  const cloudName = gcp ? 'Google' : azure ? 'Azure' : 'AWS'
  const regions = (account?.regions ?? []).filter((r) => r !== 'global')
  const [region, setRegion] = useState<string | null>(null)
  useEffect(() => {
    if (accountId) return
    if (like && !source) return
    const want = source && managed.find((a) => a.id === source.accountId)
    if (want) { setAccountId(want.id); setRegion(source.resource.region) } else if (managed[0]) setAccountId(managed[0].id)
  }, [managed.length, source?.resource.id, instances.isFetched])
  useEffect(() => { if (account && (!region || !regions.includes(region))) setRegion(regions[0] ?? null) }, [account?.id])
  const opts = useQuery(cloudLaunchOptionsQuery(accountId, region))
  const o = opts.data

  const [name, setName] = useState('')
  const [image, setImage] = useState<string | null>(null)
  const [size, setSize] = useState<string | null>('t3.small')
  const [disk, setDisk] = useState<number>(20)
  const [subnet, setSubnet] = useState<string | null>(null)
  const [zone, setZone] = useState<string | null>(null)
  const [group, setGroup] = useState<string | null>(null)
  const [firewalls, setFirewalls] = useState<string[]>([])
  const [publicIp, setPublicIp] = useState(false)
  const [protect, setProtect] = useState(true)
  const [count, setCount] = useState<number>(1)
  const [extraDisks, setExtraDisks] = useState<number[]>([])
  const [spot, setSpot] = useState(false)
  const [capacity, setCapacity] = useState<string | null>(null)
  const reservations = useQuery(cloudReservationsQuery())
  const [agent, setAgent] = useState(true)
  const [reason, setReason] = useState('')
  const [schedule, setSchedule] = useState<string | null>(null)
  const schedules = useQuery(cloudSchedulesQuery())
  const ask = useAskCloudChange()

  // A new region has other images, subnets and groups.
  // Launching another like an instance takes its size, subnet, groups and,
  // when it is one Argus offers, its image, once its region's options load.
  const prefilled = useRef(false)
  useEffect(() => {
    const ln = (kind: string) => (source?.resource.links ?? []).filter((l) => l.kind === kind).map((l) => l.id)
    if (source && o && !prefilled.current && source.accountId === accountId && source.resource.region === region) {
      prefilled.current = true
      const subnets = ln('subnet')
      setImage(o.images.find((i) => i.id === source.resource.details?.image)?.id ?? o.images[0]?.id ?? null)
      setSubnet(o.subnets.find((s) => subnets.includes(s.resource.id))?.resource.id ?? o.subnets[0]?.resource.id ?? null)
      setFirewalls(ln('firewall').filter((f) => o.firewalls.some((g) => g.resource.id === f)).slice(0, o.groups ? 1 : 5))
      if (source.resource.size) setSize(source.resource.size)
      setZone(o.zones?.find((z) => z === source.resource.zone) ?? (o.groups ? null : o.zones?.[0] ?? null))
      setGroup(o.groups?.find((g) => g === source.resource.details?.resourceGroup) ?? o.groups?.[0] ?? null)
      setName(nextName(source.resource.name))
      return
    }
    setImage(o?.images[0]?.id ?? null)
    setSubnet(o?.subnets[0]?.resource.id ?? null)
    // An Azure VM may have no zone; a Google one always has.
    setZone(o?.groups ? null : o?.zones?.[0] ?? null)
    setGroup(o?.groups?.[0] ?? null)
    setFirewalls([])
    // Each provider has its own machine types.
    if (o && size && !o.sizes.includes(size)) setSize(o.sizes.includes('e2-medium') ? 'e2-medium' : o.sizes.includes('t3.small') ? 't3.small' : o.sizes.includes('Standard_B2s') ? 'Standard_B2s' : o.sizes[0] ?? null)
  }, [o])
  const img = o?.images.find((i) => i.id === image)
  useEffect(() => {
    if (img && !img.linux) { setAgent(false); setDisk((d) => Math.max(d, gcp ? 50 : 30)) } else setAgent(true)
    if (img?.minDiskGb) setDisk((d) => Math.max(d, img.minDiskGb ?? 0))
  }, [img?.id])
  const sub = o?.subnets.find((s) => s.resource.id === subnet)
  const groups = useMemo(() => (o?.firewalls ?? []).filter((f) => !sub || azure || f.resource.network === sub.resource.network), [o, sub, azure])
  useEffect(() => { setFirewalls((fs) => fs.filter((f) => groups.some((g) => g.resource.id === f))) }, [groups])
  // Capacity reservations this launch fits in whole: same account, region,
  // zone and type, active, with room for every instance.
  const rooms = (reservations.data?.capacityReservations ?? []).filter((c) => c.accountId === accountId && c.resource.region === region &&
    c.resource.state === 'active' && c.resource.zone === sub?.resource.zone && c.resource.size === size &&
    Number(c.resource.details?.available ?? 0) >= count)
  const room = spot ? undefined : rooms.find((c) => c.resource.id === capacity)
  // A pick that no longer fits is dropped, not kept to come back silently.
  useEffect(() => { if (capacity && !room) setCapacity(null) }, [capacity, room])
  const imageMissing = Boolean(source && o && prefilled.current && source.resource.details?.image && !o.images.some((i) => i.id === source.resource.details?.image))

  const host = name.trim().toLowerCase()
  const names = launchNames(host, count)
  // A Google VM's name starts with a letter.
  const nameOk = HOSTNAME.test(host) && names.every((n) => HOSTNAME.test(n)) && (!gcp || /^[a-z]/.test(host))
  const minDisk = Math.max(gcp ? 10 : azure ? 30 : 8, img?.minDiskGb ?? 0)
  const maxExtra = gcp ? 65536 : azure ? 32767 : 16384
  const disksOk = extraDisks.every((g) => g >= (gcp ? 10 : 1) && g <= maxExtra)
  const approval = o?.needsApproval ?? false
  const ready = account && region && nameOk && img && size && disk >= minDisk && disksOk && count >= 1 && count <= MAX_COUNT && sub &&
    (gcp ? Boolean(zone) : azure ? Boolean(group) && img.linux : firewalls.length > 0) &&
    (!agent || !o?.agentProblem) && (!approval || reason.trim().length >= 5)
  const exposed = groups.filter((g) => firewalls.includes(g.resource.id) && openToInternet(g.resource).length > 0)

  const launch = async () => {
    try {
      const c = await ask.mutateAsync({
        accountId: accountId!, region: region!, action: 'launch', reason,
        spec: {
          name: host, image, size, diskGb: disk, subnet, firewalls, publicIp, protect: protect && !spot && !azure, agent, ...(vm && zone ? { zone } : {}), ...(azure && group ? { group } : {}),
          ...(count > 1 ? { count } : {}), ...(extraDisks.length ? { extraDisks } : {}), ...(spot && !azure ? { spot } : {}),
          ...(room ? { capacityReservation: room.resource.id } : {}),
          ...(schedule ? { tags: { [SCHEDULE_TAG]: schedule } } : {}),
        },
      })
      reportChange(c, 'Launching')
      void navigate({ to: '/cloud', search: { tab: 'changes' } })
    } catch (err) {
      notifyError('Could not launch', err)
    }
  }

  if (accounts.data && managed.length === 0) {
    return (
      <Box>
        <PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Launch an instance' }]} title="Launch an instance" />
        <PageBody>
          <Alert color="sky" variant="light" title="No account allows changes yet" maw={720}>
            <Text size={FS.meta}>Argus can only read the connected accounts. On the Cloud overview, choose Allow changes on an account, then come back.</Text>
            <ButtonLink to="/cloud" size="xs" variant="light" mt="sm">Back to Cloud</ButtonLink>
          </Alert>
        </PageBody>
      </Box>
    )
  }

  return (
    <Box>
      <PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Launch an instance' }]} title="Launch an instance"
        description={gcp
          ? 'A new Compute Engine VM with no service account, made as you. With the agent on, it comes up as an Argus server by itself.'
          : azure ? 'A new Azure Linux VM with no managed identity and no password, made as you. With the agent on, it comes up as an Argus server by itself.'
          : 'A new EC2 instance with an encrypted disk and instance metadata v2, made as you. With the agent on, it comes up as an Argus server by itself.'} />
      <PageBody>
        {like && (
          <Alert color="azure" variant="light" mb="md" icon={<IconCopy size={16} />} data-testid="launch-like">
            <Text size={FS.meta}>
              {source
                ? <>Like <b>{source.resource.name}</b>: its account, region, size, subnet and security groups{imageMissing ? '' : ' and image'}. Change anything before launching.</>
                : instances.isPending ? 'Reading the instance to copy…' : 'Argus no longer has that instance; fill in the form instead.'}
            </Text>
            {imageMissing && (
              <Text size={FS.meta} mt={4}>Its image ({source?.resource.details?.image}) is not one Argus launches, so pick the closest below.</Text>
            )}
          </Alert>
        )}
        <Box style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 340px), 1fr))', gap: 20, alignItems: 'start' }}>
          <Stack gap="md" style={{ gridColumn: 'span 2', minWidth: 0 }} className="argus-launch-form">
            <Section n={1} title="Where">
              <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                <Select label="Account" value={accountId} onChange={setAccountId} allowDeselect={false}
                  data={managed.map((a) => ({ value: a.id, label: a.name }))} />
                <Select label="Region" value={region} onChange={setRegion} allowDeselect={false} searchable
                  data={regions} />
              </SimpleGrid>
              {azure && (
                <Select mt="sm" label="Resource group" value={group} onChange={setGroup} allowDeselect={false} searchable
                  placeholder={o?.groups?.length ? 'Pick a resource group' : 'Argus has read no resource group yet'} data={o?.groups ?? []}
                  description="The VM, its disks, its network interface and its public address are made here. Argus offers the groups it has read something in; make a new one in the Azure portal." />
              )}
              {o && <Group gap={6} mt="xs"><Text size={FS.meta} c="dimmed">Environment</Text><EnvBadge env={o.environment} /></Group>}
              {opts.error && <Alert color="rose" variant="light" mt="sm">{(opts.error as Error).message}</Alert>}
            </Section>

            <Section n={2} title="Name and image">
              <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
                <TextInput style={{ gridColumn: 'span 2' }} label="Name" description="Also the server's hostname: lowercase letters, digits and dashes." placeholder="web-3"
                  value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={63} data-autofocus
                  error={name && !nameOk ? (gcp && !/^[a-z]/.test(host) ? 'A Google VM name starts with a letter' : HOSTNAME.test(host) ? 'Too long once numbered; shorten it' : 'Lowercase letters, digits and dashes, not starting or ending with a dash') : undefined} />
                <NumberInput label="How many" description={`1 to ${MAX_COUNT}, all alike`} min={1} max={MAX_COUNT} value={count}
                  onChange={(v) => setCount(Math.max(1, Math.min(MAX_COUNT, Number(v) || 1)))} />
              </SimpleGrid>
              {count > 1 && HOSTNAME.test(host) && (
                <Text size={FS.meta} c="dimmed" mt={6} data-testid="launch-names">
                  Named {names.length > 4 ? `${names.slice(0, 3).join(', ')} … ${names[names.length - 1]}` : names.join(', ')}. Each gets its own one-hour agent token.
                </Text>
              )}
              <Text size="sm" fw={500} mt="md" mb={6}>Image</Text>
              {opts.isPending && <Text size={FS.meta} c="dimmed">Asking {cloudName} for the newest images…</Text>}
              <Radio.Group value={image} onChange={setImage}>
                <SimpleGrid cols={{ base: 1, xs: 2, md: 3 }} spacing="sm">
                  {o?.images.map((i) => {
                    const Icon = imageIcon(i)
                    return (
                      <Radio.Card key={i.id} value={i.id} radius="md" p="sm">
                        <Group wrap="nowrap" gap="sm" align="flex-start">
                          <ThemeIcon variant="light" color={i.linux ? 'azure' : 'sky'} size={30} radius="md"><Icon size={17} /></ThemeIcon>
                          <Box style={{ minWidth: 0, flex: 1 }}>
                            <Text fw={600} size={FS.body}>{i.label}</Text>
                            <Text size={FS.micro} c="dimmed" ff="monospace" truncate>{i.id}</Text>
                          </Box>
                          <Radio.Indicator size="sm" />
                        </Group>
                      </Radio.Card>
                    )
                  })}
                </SimpleGrid>
              </Radio.Group>
              {azure && img && !img.linux && (
                <Text size={FS.meta} c={INK.denied} mt="xs">Argus launches Linux VMs in Azure for now. Launch {img.name} in the Azure portal.</Text>
              )}
            </Section>

            <Section n={3} title="Size">
              <Group gap={6}>
                {(gcp ? GCP_COMMON_SIZES : azure ? AZURE_COMMON_SIZES : COMMON_SIZES).filter((s) => !o || o.sizes.includes(s)).map((s) => (
                  <Button key={s} size="compact-sm" radius="xl" ff="monospace" fw={500} fz={FS.meta}
                    variant={size === s ? 'light' : 'default'} onClick={() => setSize(s)}>{s}</Button>
                ))}
              </Group>
              <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm" mt="sm">
                <Select label={gcp ? 'Machine type' : azure ? 'VM size' : 'Instance type'} searchable value={size} onChange={setSize} data={o?.sizes ?? COMMON_SIZES}
                  nothingFoundMessage="No such type" styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
                <NumberInput hideControls label="Disk" suffix=" GB" min={minDisk} max={gcp ? 65536 : azure ? 4095 : 2048} value={disk} onChange={(v) => setDisk(Number(v) || 0)}
                  description={gcp ? 'Balanced persistent disk, deleted with the VM' : azure ? 'Standard SSD, deleted with the VM' : 'gp3, encrypted, deleted with the instance'}
                  error={disk < minDisk ? `At least ${minDisk} GB` : undefined} />
              </SimpleGrid>
              <Text size="sm" fw={500} mt="md">More disks</Text>
              <Text size={FS.meta} c="dimmed" mb={6}>{gcp
                ? 'Each is a separate balanced persistent disk, deleted with the VM, seen as /dev/disk/by-id/google-<name>-data-1 onwards. Format and mount it on the server.'
                : azure ? 'Each is a separate Standard SSD disk named <name>-data-1 onwards, deleted with the VM, seen under /dev/disk/azure/scsi1 by its LUN, from lun0. Format and mount it on the server.'
                : 'Each is a separate encrypted gp3 disk, attached as /dev/sdf onwards and deleted with the instance. Format and mount it on the server.'}</Text>
              <Stack gap={6}>
                {extraDisks.map((gb, i) => (
                  <Group key={i} gap="xs" wrap="nowrap">
                    <Text size={FS.meta} ff="monospace" c="dimmed" w={72}>{vm ? `data-${i + 1}` : `/dev/sd${String.fromCharCode(102 + i)}`}</Text>
                    <NumberInput hideControls size="xs" w={140} suffix=" GB" min={gcp ? 10 : 1} max={maxExtra} value={gb} aria-label={`Disk ${i + 2} size`}
                      onChange={(v) => setExtraDisks((ds) => ds.map((d, j) => j === i ? Number(v) || 0 : d))} />
                    <ActionIcon variant="subtle" color="gray" aria-label="Remove this disk" onClick={() => setExtraDisks((ds) => ds.filter((_, j) => j !== i))}><IconX size={14} /></ActionIcon>
                  </Group>
                ))}
                {extraDisks.length < MAX_EXTRA_DISKS && (
                  <Button size="xs" variant="default" w="fit-content" leftSection={<IconPlus size={14} />} onClick={() => setExtraDisks((ds) => [...ds, 100])}>Add a disk</Button>
                )}
              </Stack>
              {!azure && <Switch mt="md" checked={spot} onChange={(e) => setSpot(e.currentTarget.checked)} label="Use Spot capacity"
                description={gcp
                  ? 'Google\'s spare capacity, often far cheaper, but Google can take it back with thirty seconds\' notice and the VM is then deleted. Suits work that can stop and start again; not a database.'
                  : 'AWS\'s spare capacity, often far cheaper, but AWS can take it back with two minutes\' notice and the instance is then terminated. Suits work that can stop and start again; not a database.'} />}
              {!vm && rooms.length > 0 && (
                <Select mt="md" label="Capacity reservation" placeholder="None, wherever AWS has room" clearable disabled={spot}
                  value={room ? capacity : null} onChange={setCapacity} data-testid="launch-capacity"
                  description={spot ? 'Spot instances cannot use a capacity reservation.' : 'Room already reserved for this type in the subnet\'s zone. A reservation only for launches that name it is used only when picked here.'}
                  data={rooms.map((c) => ({ value: c.resource.id,
                    label: `${c.resource.name} · ${c.resource.details?.available} of ${c.resource.details?.total} free${c.resource.details?.match === 'targeted' ? ' · named launches only' : ''}` }))} />
              )}
            </Section>

            <Section n={4} title="Network">
              {gcp && (
                <Select label="Zone" value={zone} onChange={setZone} allowDeselect={false} mb="sm" placeholder={o?.zones?.length ? 'Pick a zone' : 'No zones read yet'}
                  data={o?.zones ?? []} description="A Google subnet spans the region; the VM and its disks live in one zone." />
              )}
              {azure && (
                <Select label="Zone" value={zone} onChange={setZone} clearable mb="sm" placeholder="No zone" data={o?.zones ?? []}
                  description="Optional. Azure refuses a zone the region or the size does not have. The VM's disks are made in the same zone." />
              )}
              <Select label="Subnet" value={subnet} onChange={setSubnet} allowDeselect={false}
                data={(o?.subnets ?? []).map((s) => ({
                  value: s.resource.id,
                  label: `${s.resource.name} · ${s.resource.size} · ${s.resource.zone ?? s.resource.region}${s.resource.details?.freeAddresses ? ` · ${s.resource.details.freeAddresses} free` : ''}`,
                }))} />
              <Text size="sm" fw={500} mt="md">{gcp ? 'Firewall rules' : azure ? 'Network security group' : 'Security groups'}</Text>
              <Text size={FS.meta} c="dimmed" mb={6}>{gcp
                ? 'Up to five, in the subnet\'s network. A rule reaches the VM through its network tags; rules with no target tags reach every VM already. With the agent, nothing needs inbound SSH or RDP.'
                : azure ? 'One, or none: it goes on the VM\'s network interface, on top of the group the subnet has, if any. With the agent, nothing needs inbound SSH.'
                : 'One to five, in the subnet\'s network. With the agent, nothing needs inbound SSH or RDP.'}</Text>
              <Stack gap={6}>
                {groups.map((g) => {
                  const open = openToInternet(g.resource)
                  return (
                    <Checkbox.Card key={g.resource.id} radius="md" p="xs" checked={firewalls.includes(g.resource.id)}
                      onClick={() => setFirewalls((fs) => fs.includes(g.resource.id) ? fs.filter((f) => f !== g.resource.id) : azure ? [g.resource.id] : [...fs, g.resource.id].slice(0, 5))}>
                      <Group wrap="nowrap" gap="sm">
                        <Checkbox.Indicator />
                        <Box style={{ flex: 1, minWidth: 0 }}>
                          <Text size={FS.body} fw={600} truncate>{g.resource.name}</Text>
                          <Text size={FS.micro} c="dimmed" truncate>{g.resource.details?.description && g.resource.details.description !== g.resource.name ? g.resource.details.description : g.resource.id}</Text>
                        </Box>
                        {open.length > 0 && <Badge color="rose" variant="light" radius="sm">{open.join(', ')} from anywhere</Badge>}
                      </Group>
                    </Checkbox.Card>
                  )
                })}
                {o && groups.length === 0 && <Text size={FS.meta} c="dimmed">{gcp ? 'No firewall rules in this network.' : azure ? 'No network security groups in this region.' : 'No security groups in this network.'}</Text>}
              </Stack>
              <Switch mt="md" checked={publicIp} onChange={(e) => setPublicIp(e.currentTarget.checked)} label="Give it a public address"
                description="Off keeps it reachable only inside the network. The agent needs only outbound access." />
            </Section>

            <Section n={5} title="Argus">
              <Switch checked={agent} onChange={(e) => setAgent(e.currentTarget.checked)} disabled={img && !img.linux}
                label="Install the Argus agent" description={img && !img.linux
                  ? 'On Windows, add the server from Servers › Add a server once it is up.'
                  : azure && !agent ? 'Without it nobody can sign in to the VM: Azure requires an administrator account, and Argus gives it a key that nobody holds and no password. Access is then reset in the Azure portal.'
                  : 'It enrolls itself with a one-hour token that only works for this hostname, so sessions on it are recorded from the first minute.'} />
              {agent && o?.agentProblem && (
                <Alert color="amber" variant="light" mt="sm" icon={<IconAlertTriangle size={16} />}>
                  <Text size={FS.meta}>{o.agentProblem}</Text>
                </Alert>
              )}
              {azure ? (
                <Text size={FS.meta} c="dimmed" mt="md">Azure protects a VM from deletion with a resource lock, which Argus cannot set. Put one on in the Azure portal; Argus will not remove a VM that has one.</Text>
              ) : (
                <Switch mt="md" checked={protect && !spot} disabled={spot} onChange={(e) => setProtect(e.currentTarget.checked)} label={gcp ? 'Deletion protection' : 'Termination protection'}
                  description={spot
                    ? gcp ? 'Google deletes a Spot VM when it takes the capacity back, so protection is off.' : 'AWS does not allow it on Spot instances, so it is off.'
                    : `Nobody can remove it, from Argus or ${gcp ? 'Google' : 'AWS'}, until this is turned off.`} />
              )}
              {(schedules.data?.schedules.length ?? 0) > 0 && (
                <Select mt="md" label="Schedule" placeholder="None: it runs until someone stops it" clearable value={schedule} onChange={setSchedule}
                  description={`Tags it ${SCHEDULE_TAG}=<name>, so the schedule starts and stops it.`}
                  data={schedules.data!.schedules.filter((s) => s.state !== 'awaiting_approval').map((s) => ({ value: s.name, label: `${s.name} · ${s.windows.map(windowWords).join('; ')}` }))} />
              )}
            </Section>
          </Stack>

          <Card padding="lg" style={{ position: 'sticky', top: 80 }}>
            <Group gap="sm" mb="md">
              <ThemeIcon variant="light" color="azure" size={32} radius="md"><IconCloud size={18} /></ThemeIcon>
              <Box>
                <Text fw={700}>{!nameOk ? (vm ? 'New VM' : 'New instance') : count > 1 ? `${count} ${vm ? 'VMs' : 'instances'}, ${names[0]} to ${names[names.length - 1]}` : host}</Text>
                <Text size={FS.micro} c="dimmed">{account?.name ?? '—'} · {region ?? '—'}{azure && group ? ` · ${group}` : ''}</Text>
              </Box>
            </Group>
            <Stack gap={SP.snug}>
              <Row k="Image" v={img?.label} />
              <Row k="Type" v={size} mono />
              <Row k="Disk" v={gcp ? `${disk} GB balanced` : azure ? `${disk} GB Standard SSD` : `${disk} GB gp3, encrypted`} />
              {extraDisks.length > 0 && <Row k="More disks" v={extraDisks.map((g) => `${g} GB`).join(', ')} />}
              {spot && !azure && <Row k="Capacity" v="Spot" />}
              {room && <Row k="Capacity" v={`Reserved, ${room.resource.name}`} />}
              <Row k="Subnet" v={sub ? `${sub.resource.name} (${azure ? zone ?? 'no zone' : gcp ? zone ?? '' : sub.resource.zone ?? ''})` : undefined} />
              <Row k={gcp ? 'Firewall rules' : azure ? 'Security group' : 'Security groups'} v={azure && !firewalls.length ? 'The subnet\'s only' : firewalls.length ? groups.filter((g) => firewalls.includes(g.resource.id)).map((g) => g.resource.name).join(', ') : undefined} />
              <Row k="Public address" v={publicIp ? 'Yes' : 'No'} />
              <Row k="Argus agent" v={agent ? 'Installs and enrolls' : 'No'} />
              {!azure && <Row k="Protection" v={protect && !spot ? 'On' : 'Off'} />}
              {schedule && <Row k="Schedule" v={schedule} mono />}
            </Stack>
            {exposed.length > 0 && (
              <Text size={FS.meta} c={INK.denied} mt="sm">{exposed.map((g) => g.resource.name).join(', ')} let anyone on the internet in.</Text>
            )}
            {approval && (
              <Alert color="amber" variant="light" mt="md" p="sm" icon={<IconUserCheck size={16} />}>
                <Text size={FS.meta}>In {o?.environment}, someone else approves a launch first. It then runs as you.</Text>
              </Alert>
            )}
            <Textarea mt="md" label={approval ? 'Why' : 'Why (optional)'} autosize minRows={2} value={reason}
              onChange={(e) => setReason(e.currentTarget.value)} placeholder="Third web node for the spring sale" />
            <Button fullWidth mt="md" size="md" leftSection={approval ? <IconUserCheck size={17} /> : <IconCirclePlus size={17} />}
              disabled={!ready} loading={ask.isPending} onClick={() => void launch()}>
              {approval ? 'Ask for approval' : 'Launch'}
            </Button>
            <Text size={FS.micro} c="dimmed" mt="xs" ta="center">{cloudName} bills {count > 1 ? `each ${vm ? 'VM' : 'instance'}` : `the ${vm ? 'VM' : 'instance'}`} from the moment it starts.</Text>
          </Card>
        </Box>
      </PageBody>
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

function Row({ k, v, mono }: { k: string; v?: string | null; mono?: boolean }) {
  return (
    <Group justify="space-between" wrap="nowrap" gap="md">
      <Text size={FS.meta} c="dimmed" style={{ flexShrink: 0 }}>{k}</Text>
      <Text size={FS.meta} ff={mono ? 'monospace' : undefined} ta="right" truncate c={v ? undefined : 'dimmed'}>{v || 'Not chosen'}</Text>
    </Group>
  )
}
