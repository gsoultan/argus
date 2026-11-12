import { Alert, Badge, Box, Button, Card, Flex, Group, List, Modal, SimpleGrid, Stack, Text, TextInput, ThemeIcon, Tooltip } from '@mantine/core'
import {
  IconAlertTriangle, IconBrandAws, IconCheck, IconCircleCheck, IconMinus, IconCloud, IconDeviceDesktopOff, IconLockOpen, IconPencil, IconRefresh, IconServer2,
} from '@tabler/icons-react'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { EmptyState, SectionCard } from '~/components/page'
import { Stat, relTime } from '~/components/primitives'
import { ButtonLink } from '~/components/links'
import {
  SCHEDULE_TAG, openToInternet, prepareCloudAccount, staleness, useAllowCloudChanges, useDisconnectCloudAccount, useMakeCloudReadOnly, useSyncCloudAccount,
  type CloudAccount, type CloudBase, type CloudItem,
} from '~/lib/cloud'
import { run } from '~/lib/notify'
import { FS, INK, SP } from '~/theme'
import { EnvBadge } from './common'
import { RoleSetup } from './RoleSetup'
import type { CloudTab } from './tabs'

export function CloudOverview({ accounts, bases, instances, firewalls, volumes, addresses, admin, onTab }: {
  accounts: CloudAccount[]
  bases: CloudBase[]
  instances?: CloudItem[]
  firewalls?: CloudItem[]
  volumes?: CloudItem[]
  addresses?: CloudItem[]
  admin: boolean
  onTab: (t: CloudTab) => void
}) {
  const base = bases.find((b) => b.provider === 'aws')
  if (accounts.length === 0) return <FirstAccount admin={admin} base={base} />

  const running = instances?.filter((i) => i.resource.state === 'running').length
  const unmatched = instances?.filter((i) => !i.server)
  const open = firewalls?.filter((f) => openToInternet(f.resource).length > 0)
  const looseDisks = volumes?.filter((v) => v.resource.state === 'available')
  const looseIPs = addresses?.filter((a) => a.resource.state === 'unattached')
  const failing = accounts.filter((a) => Object.keys(a.syncErrors).length > 0 || staleness(a) === 'late')

  return (
    <Stack gap="lg">
      <SimpleGrid cols={{ base: 2, md: 4 }} spacing="md">
        <Stat label="Instances" value={instances?.length} icon={IconServer2}
          sub={instances ? `${running} running · ${instances.length - (running ?? 0)} not running` : undefined} onClick={() => onTab('instances')} />
        <Stat label="With an Argus server" value={instances ? `${instances.length - (unmatched?.length ?? 0)} of ${instances.length}` : undefined}
          icon={IconCircleCheck} tone={unmatched && unmatched.length > 0 ? 'warn' : 'ok'} color="amber"
          sub="Matched by address or name" onClick={() => onTab('instances')} />
        <Stat label="Open to the internet" value={open?.length} icon={IconLockOpen} tone={open && open.length > 0 ? 'warn' : 'ok'} color="rose"
          sub="Firewalls letting anyone reach SSH, RDP or a database" onClick={() => onTab('network')} />
        <Stat label="Accounts" value={accounts.length} icon={IconCloud}
          sub={`${accounts.reduce((n, a) => n + a.regions.length, 0)} regions read every 5 minutes`} />
      </SimpleGrid>

      <Box style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 420px), 1fr))', gap: 16 }}>
        <SectionCard title="Accounts" icon={IconBrandAws} flush
          action={admin ? <ButtonLink to="/cloud/connect" size="xs" variant="light">Connect another</ButtonLink> : undefined}>
          <Stack gap={0}>
            {accounts.map((a) => <AccountRow key={a.id} a={a} admin={admin} />)}
          </Stack>
        </SectionCard>

        <SectionCard title="Needs a look" icon={IconAlertTriangle} iconColor="amber" flush>
          <Stack gap={0}>
            {unmatched && unmatched.length > 0 && (
              <Finding color="amber" icon={IconDeviceDesktopOff}
                title={`${unmatched.length} ${unmatched.length === 1 ? 'instance has' : 'instances have'} no Argus server`}
                text={`Sessions on ${names(unmatched)} are not recorded. Add them as servers with the agent.`}
                action={<Button size="xs" variant="default" onClick={() => onTab('instances')}>See them</Button>} />
            )}
            {open && open.length > 0 && (
              <Finding color="rose" icon={IconLockOpen}
                title={`${open.length} ${open.length === 1 ? 'firewall is' : 'firewalls are'} open to the internet`}
                text={open.slice(0, 3).map((f) => `${f.resource.name} (${openToInternet(f.resource).join(', ')})`).join(', ') +
                  (open.length > 3 ? ` and ${open.length - 3} more` : '') + '. Servers with the agent need no inbound SSH or RDP.'}
                action={<Button size="xs" variant="default" onClick={() => onTab('network')}>Review</Button>} />
            )}
            {((looseDisks?.length ?? 0) + (looseIPs?.length ?? 0)) > 0 && (
              <Finding color="amber" icon={IconAlertTriangle}
                title="Paid for, not in use"
                text={[looseDisks?.length ? `${looseDisks.length} disk${looseDisks.length === 1 ? '' : 's'} not attached to anything` : '',
                  looseIPs?.length ? `${looseIPs.length} Elastic IP${looseIPs.length === 1 ? '' : 's'} not attached` : '']
                  .filter(Boolean).join(' and ') + '. AWS charges for both while they sit there.'}
                action={<Button size="xs" variant="default" onClick={() => onTab(looseDisks?.length ? 'disks' : 'network')}>See them</Button>} />
            )}
            {failing.map((a) => (
              <Finding key={a.id} color="amber" icon={IconRefresh}
                title={`${a.name} was not fully read`}
                text={Object.keys(a.syncErrors).length > 0
                  ? `${Object.keys(a.syncErrors).length} region${Object.keys(a.syncErrors).length === 1 ? '' : 's'} failed: ${Object.values(a.syncErrors)[0]}. What is shown there is from the last good read.`
                  : `Last read ${relTime(a.syncedAt)}. What is shown is from then.`} />
            ))}
            {unmatched?.length === 0 && open?.length === 0 && !looseDisks?.length && !looseIPs?.length && failing.length === 0 && (
              <Box p="md"><EmptyState compact icon={IconCircleCheck} title="Nothing needs a look"
                description="Every instance has an Argus server, no firewall is open to the internet, and every account was read." /></Box>
            )}
          </Stack>
        </SectionCard>
      </Box>
    </Stack>
  )
}

function names(list: CloudItem[]): string {
  const n = list.slice(0, 3).map((i) => i.resource.name).join(', ')
  return list.length > 3 ? `${n} and ${list.length - 3} more` : n
}

function Finding({ color, icon: Icon, title, text, action }: {
  color: string
  icon: typeof IconAlertTriangle
  title: string
  text: string
  action?: React.ReactNode
}) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start" px="md" py="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
      <ThemeIcon variant="light" color={color} size={26} radius="md"><Icon size={14} /></ThemeIcon>
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Text fw={600} size={FS.body}>{title}</Text>
        <Text size={FS.meta} c="dimmed" lh={1.5}>{text}</Text>
      </Box>
      {action}
    </Group>
  )
}

function AccountRow({ a, admin }: { a: CloudAccount; admin: boolean }) {
  const sync = useSyncCloudAccount()
  const [confirm, setConfirm] = useState(false)
  const [access, setAccess] = useState(false)
  const [update, setUpdate] = useState(false)
  const stale = staleness(a)
  const counts = [
    [a.counts.instance ?? 0, 'instance'], [a.counts.volume ?? 0, 'disk'], [a.counts.firewall ?? 0, 'firewall'],
  ] as const
  return (
    <Box px="md" py="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
      <Flex justify="space-between" align={{ base: 'stretch', sm: 'flex-start' }} direction={{ base: 'column', sm: 'row' }} gap="xs">
        <Box style={{ minWidth: 0 }}>
          <Group gap="xs" wrap="wrap">
            <Text fw={600} size={FS.body}>{a.name}</Text>
            <EnvBadge env={a.environment} />
            {a.access === 'manage'
              ? <Badge variant="light" color="azure" radius="sm" tt="none" fw={500}>Can change</Badge>
              : <Badge variant="default" radius="sm" tt="none" fw={500}>Read only</Badge>}
            {a.access === 'manage' && a.manageMissing?.length > 0 && (
              <Tooltip label={`The manage role lacks ${a.manageMissing.join(', ')}, so Argus cannot change disks, images or addresses here yet.`} multiline w={300}>
                <Badge variant="light" color="amber" radius="sm" tt="none" fw={500}>Role needs an update</Badge>
              </Tooltip>
            )}
          </Group>
          <Text size={FS.micro} c="dimmed" ff="monospace" mt={2}>{a.accountId.replace(/(\d{4})(\d{4})(\d{4})/, '$1 $2 $3')} · {a.regions.length} region{a.regions.length === 1 ? '' : 's'}</Text>
          <Text size={FS.meta} c="dimmed" mt={SP.tight}>
            {counts.map(([n, w]) => `${n} ${w}${n === 1 ? '' : 's'}`).join(' · ')}
          </Text>
          <Text size={FS.micro} mt={2} c={stale === 'fresh' ? 'dimmed' : INK.pending}>
            {stale === 'never' ? 'Being read for the first time…' : `Read ${relTime(a.syncedAt)}`}
          </Text>
        </Box>
        {admin && (
          <Group gap={4} wrap="nowrap">
            <Button size="xs" variant="subtle" leftSection={<IconRefresh size={13} />} loading={sync.isPending}
              onClick={() => void run(() => sync.mutateAsync(a.id), { failure: 'Could not read the account again',
                success: ['Reading again', `${a.name} is being read; this page updates when it is done.`] })}>
              Read now
            </Button>
            {a.access === 'manage' && a.manageMissing?.length > 0 && (
              <Button size="xs" variant="light" color="amber" leftSection={<IconPencil size={13} />} onClick={() => setUpdate(true)}>Update role</Button>
            )}
            {a.access === 'manage'
              ? <Button size="xs" variant="subtle" color="gray" onClick={() => setAccess(true)}>Make read only</Button>
              : <Button size="xs" variant="subtle" leftSection={<IconPencil size={13} />} onClick={() => setAccess(true)}>Allow changes</Button>}
            <Button size="xs" variant="subtle" color="rose" onClick={() => setConfirm(true)}>Disconnect</Button>
          </Group>
        )}
      </Flex>
      <Disconnect a={a} opened={confirm} onClose={() => setConfirm(false)} />
      {access && (a.access === 'manage'
        ? <ReadOnly a={a} onClose={() => setAccess(false)} />
        : <AllowChanges a={a} onClose={() => setAccess(false)} />)}
      {update && <AllowChanges a={a} update onClose={() => setUpdate(false)} />}
    </Box>
  )
}

/**
 * Lets Argus change an account: the same stack, updated to add the manage
 * role, and that role pasted back. Argus assumes it before saving.
 */
function AllowChanges({ a, update, onClose }: { a: CloudAccount; update?: boolean; onClose: () => void }) {
  const prepared = useQuery({ queryKey: ['cloud', 'prepare-manage', a.accountId], queryFn: () => prepareCloudAccount(a.accountId, true), gcTime: 0 })
  const allow = useAllowCloudChanges()
  const [arn, setArn] = useState(update ? a.manageRoleArn ?? '' : '')
  return (
    <Modal opened onClose={onClose} size="xl" centered title={<Text fw={700}>{update ? `Update the manage role in ${a.name}` : `Allow changes in ${a.name}`}</Text>}>
      <Stack gap="md">
        {update ? (
          <Text size={FS.body} lh={1.55}>
            The <Text span ff="monospace">ArgusManage</Text> role in this account was made before Argus could change disks, snapshots,
            images and Elastic IPs. It lacks {a.manageMissing.join(', ')}. Deploy the template below over the same stack; the role
            keeps its ARN, so check it again below once the update is done. Argus also notices by itself at its next read.
          </Text>
        ) : (
          <Text size={FS.body} lh={1.55}>
            Argus gets a second role, <Text span ff="monospace">ArgusManage</Text>, that can change instances, disks, snapshots, images
            and Elastic IPs and nothing else: no IAM, so it cannot grant itself more. Every change is made as the person who asked,
            waits for approval where the environment asks for it, and goes in the audit log.
          </Text>
        )}
        {prepared.error && <Alert color="rose" variant="light">{(prepared.error as Error).message}</Alert>}
        {prepared.data ? <RoleSetup prepared={prepared.data} update /> : !prepared.error && <Text size={FS.meta} c="dimmed">Making the template…</Text>}
        <TextInput label="Manage role ARN" description="The stack's ManageRoleArn output, once the update is done."
          placeholder={`arn:aws:iam::${a.accountId}:role/ArgusManage`} value={arn} onChange={(e) => setArn(e.currentTarget.value)}
          styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!arn.trim().startsWith('arn:')} loading={allow.isPending}
            onClick={() => void run(() => allow.mutateAsync({ id: a.id, roleArn: arn.trim() }), { failure: 'Argus could not use that role',
              success: update ? ['Checking the role', `Argus is reading what the role in ${a.name} may do now.`]
                : ['Changes allowed', `Admins can now launch and change instances, disks and addresses in ${a.name}.`] }).then((ok) => ok && onClose())}>
            {update ? 'Check again' : 'Check and allow'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function ReadOnly({ a, onClose }: { a: CloudAccount; onClose: () => void }) {
  const ro = useMakeCloudReadOnly()
  return (
    <Modal opened onClose={onClose} centered title={<Text fw={700}>Make {a.name} read only?</Text>}>
      <Stack gap="md">
        <Text size={FS.body} lh={1.55}>
          Argus stops using the manage role, and nobody can change instances in this account from Argus. Changes waiting
          for approval can no longer run. The role stays in AWS until the stack is updated without it.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="amber" loading={ro.isPending}
            onClick={() => void run(() => ro.mutateAsync(a.id), { failure: 'Could not make it read only',
              success: ['Read only', `${a.name} can no longer be changed from Argus.`] }).then((ok) => ok && onClose())}>
            Make read only
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function Disconnect({ a, opened, onClose }: { a: CloudAccount; opened: boolean; onClose: () => void }) {
  const del = useDisconnectCloudAccount()
  const [typed, setTyped] = useState('')
  return (
    <Modal opened={opened} onClose={onClose} title={<Text fw={700}>Disconnect {a.name}?</Text>} centered>
      <Stack gap="md">
        <Text size={FS.body}>Disconnecting makes no call to AWS. Argus only forgets the account.</Text>
        <Box>
          <Text size={FS.meta} fw={700} mb={4}>Stays in AWS, untouched</Text>
          <List fz={FS.meta} spacing={4} icon={<ThemeIcon size={16} radius="xl" variant="light" color="teal"><IconCheck size={11} /></ThemeIcon>}>
            <List.Item>Every instance, disk, snapshot and image, as they are now</List.Item>
            <List.Item>Security groups and every rule, including rules changed from Argus</List.Item>
            <List.Item>Tags Argus wrote, like {SCHEDULE_TAG}; nothing reads them once disconnected</List.Item>
            <List.Item>The roles Argus used, until someone deletes their <Text span ff="monospace" size={FS.meta}>argus-access</Text> stack</List.Item>
            <List.Item>Argus agents on instances, until you uninstall them</List.Item>
          </List>
        </Box>
        <Box>
          <Text size={FS.meta} fw={700} mb={4}>Changes in Argus only</Text>
          <List fz={FS.meta} spacing={4} icon={<ThemeIcon size={16} radius="xl" variant="light" color="gray"><IconMinus size={11} /></ThemeIcon>}>
            <List.Item>Argus stops reading the account, and its resources leave these lists</List.Item>
            <List.Item>Changes waiting for approval are cancelled</List.Item>
            <List.Item>Schedules stop acting on its instances</List.Item>
          </List>
          <Text size={FS.micro} c="dimmed" mt={6}>Reconnect it any time. Instances still tagged for a schedule are picked up by that schedule again.</Text>
        </Box>
        <TextInput label={<>Type <Text span ff="monospace" fw={700}>{a.name}</Text> to confirm</>} value={typed}
          onChange={(e) => setTyped(e.currentTarget.value)} data-autofocus />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" disabled={typed !== a.name} loading={del.isPending}
            onClick={() => void run(() => del.mutateAsync(a.id), { failure: 'Could not disconnect',
              success: ['Disconnected', `${a.name} is no longer read. The audit log has the entry.`] }).then((ok) => ok && onClose())}>
            Disconnect
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function FirstAccount({ admin, base }: { admin: boolean; base?: CloudBase }) {
  return (
    <Card padding="xl">
      <Stack align="center" gap="lg" maw={640} mx="auto" py="lg">
        <ThemeIcon size={56} radius="xl" variant="light" color="azure"><IconBrandAws size={30} /></ThemeIcon>
        <Box ta="center">
          <Text fw={700} size="lg">Connect your first AWS account</Text>
          <Text size={FS.body} c="dimmed" mt={SP.tight} lh={1.6}>
            See every instance, disk and firewall across your accounts and regions, which ones have an Argus server, and what is
            open to the internet. Argus never asks for access keys: it uses a role you create in the account with one click, read only unless you allow changes.
          </Text>
        </Box>
        <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm" w="100%">
          {[['1', 'Name the account', 'Its 12-digit id and whether it is production.'],
            ['2', 'Create the role', 'One CloudFormation stack, free, made for this Argus.'],
            ['3', 'Paste the role', 'Argus checks it and starts reading.']].map(([n, t, d]) => (
            <Box key={n} p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 10 }}>
              <Text size={FS.micro} c={INK.brand} fw={700}>STEP {n}</Text>
              <Text fw={600} size={FS.body} mt={2}>{t}</Text>
              <Text size={FS.meta} c="dimmed">{d}</Text>
            </Box>
          ))}
        </SimpleGrid>
        {base?.error && (
          <Alert color="amber" variant="light" w="100%" title="argus-control has no AWS identity yet">
            <Text size={FS.meta}>
              It needs one to assume roles in your accounts. On EC2 it uses the instance profile; anywhere else, give it
              an IAM user's access key under Settings › Cloud identities.
            </Text>
            <Text size={FS.micro} c="dimmed" mt={4}>{base.error}</Text>
            {admin && (
              <ButtonLink to="/settings" search={{ section: 'cloud' }} size="compact-sm" variant="light" mt="xs">
                Set the AWS identity
              </ButtonLink>
            )}
          </Alert>
        )}
        {admin ? (
          <ButtonLink to="/cloud/connect" size="sm" leftSection={<IconBrandAws size={16} />}>Connect an AWS account</ButtonLink>
        ) : (
          <Text size={FS.meta} c="dimmed">An administrator connects accounts.</Text>
        )}
      </Stack>
    </Card>
  )
}
