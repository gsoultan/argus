import {
  Alert, Badge, Box, Button, Card, Checkbox, Group, NumberInput, Radio, Select, SimpleGrid, Stack, Switch, Text, TextInput, Textarea, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconBrandDebian, IconBrandUbuntu, IconBrandWindows, IconCirclePlus, IconCloud, IconServer2, IconUserCheck,
} from '@tabler/icons-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { reportChange } from '~/components/cloud/InstanceActions'
import { EnvBadge } from '~/components/cloud/common'
import { ButtonLink } from '~/components/links'
import { PageBody, PageHeader } from '~/components/page'
import {
  SCHEDULE_TAG, cloudAccountsQuery, cloudLaunchOptionsQuery, cloudSchedulesQuery, openToInternet, useAskCloudChange, windowWords, type CloudImage,
} from '~/lib/cloud'
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
  component: Launch,
})

const COMMON_SIZES = ['t3.micro', 't3.small', 't3.medium', 't3.large', 'm7i.large', 'c7i.large']
const HOSTNAME = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/

function imageIcon(img: CloudImage) {
  if (!img.linux) return IconBrandWindows
  if (img.family.startsWith('ubuntu')) return IconBrandUbuntu
  if (img.family.startsWith('debian')) return IconBrandDebian
  return IconServer2
}

function Launch() {
  const navigate = useNavigate()
  const accounts = useQuery(cloudAccountsQuery())
  const managed = (accounts.data?.accounts ?? []).filter((a) => a.access === 'manage')
  const [accountId, setAccountId] = useState<string | null>(null)
  const account = managed.find((a) => a.id === accountId)
  const [region, setRegion] = useState<string | null>(null)
  useEffect(() => { if (!accountId && managed[0]) setAccountId(managed[0].id) }, [managed.length])
  useEffect(() => { if (account && (!region || !account.regions.includes(region))) setRegion(account.regions[0] ?? null) }, [account?.id])
  const opts = useQuery(cloudLaunchOptionsQuery(accountId, region))
  const o = opts.data

  const [name, setName] = useState('')
  const [image, setImage] = useState<string | null>(null)
  const [size, setSize] = useState<string | null>('t3.small')
  const [disk, setDisk] = useState<number>(20)
  const [subnet, setSubnet] = useState<string | null>(null)
  const [firewalls, setFirewalls] = useState<string[]>([])
  const [publicIp, setPublicIp] = useState(false)
  const [protect, setProtect] = useState(true)
  const [agent, setAgent] = useState(true)
  const [reason, setReason] = useState('')
  const [schedule, setSchedule] = useState<string | null>(null)
  const schedules = useQuery(cloudSchedulesQuery())
  const ask = useAskCloudChange()

  // A new region has other images, subnets and groups.
  useEffect(() => {
    setImage(o?.images[0]?.id ?? null)
    setSubnet(o?.subnets[0]?.resource.id ?? null)
    setFirewalls([])
  }, [o])
  const img = o?.images.find((i) => i.id === image)
  useEffect(() => { if (img && !img.linux) { setAgent(false); setDisk((d) => Math.max(d, 30)) } else setAgent(true) }, [img?.id])
  const sub = o?.subnets.find((s) => s.resource.id === subnet)
  const groups = useMemo(() => (o?.firewalls ?? []).filter((f) => !sub || f.resource.network === sub.resource.network), [o, sub])
  useEffect(() => { setFirewalls((fs) => fs.filter((f) => groups.some((g) => g.resource.id === f))) }, [groups])

  const host = name.trim().toLowerCase()
  const nameOk = HOSTNAME.test(host)
  const approval = o?.needsApproval ?? false
  const ready = account && region && nameOk && img && size && disk >= 8 && sub && firewalls.length > 0 &&
    (!agent || !o?.agentProblem) && (!approval || reason.trim().length >= 5)
  const exposed = groups.filter((g) => firewalls.includes(g.resource.id) && openToInternet(g.resource).length > 0)

  const launch = async () => {
    try {
      const c = await ask.mutateAsync({
        accountId: accountId!, region: region!, action: 'launch', reason,
        spec: { name: host, image, size, diskGb: disk, subnet, firewalls, publicIp, protect, agent, ...(schedule ? { tags: { [SCHEDULE_TAG]: schedule } } : {}) },
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
        description="A new EC2 instance with an encrypted disk and instance metadata v2, made as you. With the agent on, it comes up as an Argus server by itself." />
      <PageBody>
        <Box style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 340px), 1fr))', gap: 20, alignItems: 'start' }}>
          <Stack gap="md" style={{ gridColumn: 'span 2', minWidth: 0 }} className="argus-launch-form">
            <Section n={1} title="Where">
              <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                <Select label="Account" value={accountId} onChange={setAccountId} allowDeselect={false}
                  data={managed.map((a) => ({ value: a.id, label: a.name }))} />
                <Select label="Region" value={region} onChange={setRegion} allowDeselect={false} searchable
                  data={account?.regions ?? []} />
              </SimpleGrid>
              {o && <Group gap={6} mt="xs"><Text size={FS.meta} c="dimmed">Environment</Text><EnvBadge env={o.environment} /></Group>}
              {opts.error && <Alert color="rose" variant="light" mt="sm">{(opts.error as Error).message}</Alert>}
            </Section>

            <Section n={2} title="Name and image">
              <TextInput label="Name" description="Also the server's hostname: lowercase letters, digits and dashes." placeholder="web-3"
                value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={63} data-autofocus
                error={name && !nameOk ? 'Lowercase letters, digits and dashes, not starting or ending with a dash' : undefined} />
              <Text size="sm" fw={500} mt="md" mb={6}>Image</Text>
              {opts.isPending && <Text size={FS.meta} c="dimmed">Asking AWS for the newest images…</Text>}
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
            </Section>

            <Section n={3} title="Size">
              <Group gap={6}>
                {COMMON_SIZES.filter((s) => !o || o.sizes.includes(s)).map((s) => (
                  <Button key={s} size="compact-sm" radius="xl" ff="monospace" fw={500} fz={FS.meta}
                    variant={size === s ? 'light' : 'default'} onClick={() => setSize(s)}>{s}</Button>
                ))}
              </Group>
              <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm" mt="sm">
                <Select label="Instance type" searchable value={size} onChange={setSize} data={o?.sizes ?? COMMON_SIZES}
                  nothingFoundMessage="No such type" styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
                <NumberInput hideControls label="Disk" suffix=" GB" min={8} max={2048} value={disk} onChange={(v) => setDisk(Number(v) || 0)}
                  description="gp3, encrypted, deleted with the instance" />
              </SimpleGrid>
            </Section>

            <Section n={4} title="Network">
              <Select label="Subnet" value={subnet} onChange={setSubnet} allowDeselect={false}
                data={(o?.subnets ?? []).map((s) => ({
                  value: s.resource.id,
                  label: `${s.resource.name} · ${s.resource.size} · ${s.resource.zone ?? s.resource.region}${s.resource.details?.freeAddresses ? ` · ${s.resource.details.freeAddresses} free` : ''}`,
                }))} />
              <Text size="sm" fw={500} mt="md">Security groups</Text>
              <Text size={FS.meta} c="dimmed" mb={6}>One to five, in the subnet's network. With the agent, nothing needs inbound SSH or RDP.</Text>
              <Stack gap={6}>
                {groups.map((g) => {
                  const open = openToInternet(g.resource)
                  return (
                    <Checkbox.Card key={g.resource.id} radius="md" p="xs" checked={firewalls.includes(g.resource.id)}
                      onClick={() => setFirewalls((fs) => fs.includes(g.resource.id) ? fs.filter((f) => f !== g.resource.id) : [...fs, g.resource.id].slice(0, 5))}>
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
                {o && groups.length === 0 && <Text size={FS.meta} c="dimmed">No security groups in this network.</Text>}
              </Stack>
              <Switch mt="md" checked={publicIp} onChange={(e) => setPublicIp(e.currentTarget.checked)} label="Give it a public address"
                description="Off keeps it reachable only inside the network. The agent needs only outbound access." />
            </Section>

            <Section n={5} title="Argus">
              <Switch checked={agent} onChange={(e) => setAgent(e.currentTarget.checked)} disabled={img && !img.linux}
                label="Install the Argus agent" description={img && !img.linux
                  ? 'On Windows, add the server from Servers › Add a server once it is up.'
                  : 'It enrolls itself with a one-hour token that only works for this hostname, so sessions on it are recorded from the first minute.'} />
              {agent && o?.agentProblem && (
                <Alert color="amber" variant="light" mt="sm" icon={<IconAlertTriangle size={16} />}>
                  <Text size={FS.meta}>{o.agentProblem}</Text>
                </Alert>
              )}
              <Switch mt="md" checked={protect} onChange={(e) => setProtect(e.currentTarget.checked)} label="Termination protection"
                description="Nobody can remove it, from Argus or AWS, until this is turned off." />
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
                <Text fw={700}>{nameOk ? host : 'New instance'}</Text>
                <Text size={FS.micro} c="dimmed">{account?.name ?? '—'} · {region ?? '—'}</Text>
              </Box>
            </Group>
            <Stack gap={SP.snug}>
              <Row k="Image" v={img?.label} />
              <Row k="Type" v={size} mono />
              <Row k="Disk" v={`${disk} GB gp3, encrypted`} />
              <Row k="Subnet" v={sub ? `${sub.resource.name} (${sub.resource.zone ?? ''})` : undefined} />
              <Row k="Security groups" v={firewalls.length ? groups.filter((g) => firewalls.includes(g.resource.id)).map((g) => g.resource.name).join(', ') : undefined} />
              <Row k="Public address" v={publicIp ? 'Yes' : 'No'} />
              <Row k="Argus agent" v={agent ? 'Installs and enrolls' : 'No'} />
              <Row k="Protection" v={protect ? 'On' : 'Off'} />
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
            <Text size={FS.micro} c="dimmed" mt="xs" ta="center">AWS bills the instance from the moment it starts.</Text>
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
