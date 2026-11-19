import { Alert, Badge, Box, Button, Card, Flex, Group, List, Modal, SimpleGrid, Stack, Text, TextInput, ThemeIcon, Tooltip } from '@mantine/core'
import {
  IconAlertTriangle, IconBrandAws, IconBrandAzure, IconBrandGoogle, IconCheck, IconCircleCheck, IconCoin, IconMinus, IconCloud, IconDeviceDesktopOff, IconLockOpen, IconPencil, IconRefresh, IconServer2,
} from '@tabler/icons-react'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { EmptyState, SectionCard } from '~/components/page'
import { Stat, relTime } from '~/components/primitives'
import { ButtonLink } from '~/components/links'
import {
  PROVIDER_LABEL, SCHEDULE_TAG, accountIdText, openToInternet, prepareCloudAccount, staleness, useAllowCloudChanges, useDisconnectCloudAccount, useMakeCloudReadOnly, useSyncCloudAccount,
  type CloudAccount, type CloudBase, type CloudItem,
} from '~/lib/cloud'
import { run } from '~/lib/notify'
import { FS, INK, SP } from '~/theme'
import { EnvBadge } from './common'
import { dollars } from '~/lib/cloudCosts'
import { findingCounts, type CloudFindingsView } from '~/lib/cloudFindings'
import { ChangeIdentity } from './ChangeIdentity'
import { GcpSetup } from './GcpSetup'
import { RoleSetup } from './RoleSetup'
import type { CloudTab } from './tabs'

export function CloudOverview({ accounts, bases, instances, firewalls, findings, admin, platform = true, onTab }: {
  accounts: CloudAccount[]
  /** Risks & waste: the High risks are shown here, the rest there. */
  findings?: CloudFindingsView
  bases: CloudBase[]
  instances?: CloudItem[]
  firewalls?: CloudItem[]
  volumes?: CloudItem[]
  addresses?: CloudItem[]
  admin: boolean
  /** The session is in the default organization, where the cloud identity is set. */
  platform?: boolean
  onTab: (t: CloudTab) => void
}) {
  const aws = bases.filter((b) => (b.provider || 'aws') === 'aws')
  const base = aws.find((b) => b.default) ?? aws[0]
  if (accounts.length === 0) return <FirstAccount admin={admin} platform={platform} base={base} />
  const providers = new Set(accounts.map((a) => a.provider))

  const running = instances?.filter((i) => i.resource.state === 'running').length
  const unmatched = instances?.filter((i) => !i.server)
  const open = firewalls?.filter((f) => openToInternet(f.resource).length > 0)
  const counts = findings ? findingCounts(findings.items) : undefined
  const high = findings?.items.filter((f) => f.group === 'risk' && f.severity === 'high') ?? []
  const failing = accounts.filter((a) => Object.keys(a.syncErrors).length > 0 || staleness(a) === 'late')

  return (
    <Stack gap="lg">
      <SimpleGrid cols={{ base: 2, md: 4 }} spacing="md">
        <Stat label="Instances" value={instances?.length} icon={IconServer2}
          sub={instances ? `${running} running · ${instances.length - (running ?? 0)} not running` : undefined} onClick={() => onTab('instances')} />
        <Stat label="With an Argus server" value={instances ? `${instances.length - (unmatched?.length ?? 0)} of ${instances.length}` : undefined}
          icon={IconCircleCheck} tone={unmatched && unmatched.length > 0 ? 'warn' : 'ok'} color="amber"
          sub="Matched by address or name" onClick={() => onTab('instances')} />
        <Stat label="Risks" value={counts ? (counts.high ? `${counts.high} high` : counts.risks) : undefined} icon={IconLockOpen}
          tone={counts && counts.risks > 0 ? 'warn' : 'ok'} color="rose"
          sub={counts ? `${counts.otherRisks} medium or low · see Risks & waste` : undefined} onClick={() => onTab('risks')} />
        <Stat label="Not in use" value={findings ? dollars(findings.monthly) : undefined} icon={IconCoin}
          tone={findings && findings.monthly > 0 ? 'warn' : 'ok'} color="amber"
          sub={counts ? `a month, across ${counts.waste} resource${counts.waste === 1 ? '' : 's'}` : undefined} onClick={() => onTab('risks')} />
      </SimpleGrid>

      <Box style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 420px), 1fr))', gap: 16 }}>
        <SectionCard title="Accounts" icon={providers.size !== 1 ? IconCloud : providers.has('gcp') ? IconBrandGoogle : providers.has('azure') ? IconBrandAzure : IconBrandAws} flush
          action={admin ? <ButtonLink to="/cloud/connect" size="xs" variant="light">Connect another</ButtonLink> : undefined}>
          <Stack gap={0}>
            {accounts.map((a) => <AccountRow key={a.id} a={a} admin={admin} bases={bases} />)}
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
            {high.slice(0, 3).map((f) => (
              <Finding key={f.accountId + f.id} color="rose" icon={IconLockOpen} title={f.title} text={`${f.name} · ${f.accountName}. ${f.why}`}
                action={<Button size="xs" variant="default" onClick={() => onTab('risks')}>Review</Button>} />
            ))}
            {findings && findings.items.length > high.slice(0, 3).length && (
              <Box px="md" py="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
                <Button size="compact-sm" variant="subtle" onClick={() => onTab('risks')}>
                  See all {findings.items.length} in Risks &amp; waste
                </Button>
              </Box>
            )}
            {!findings && open && open.length > 0 && (
              <Finding color="rose" icon={IconLockOpen}
                title={`${open.length} ${open.length === 1 ? 'firewall is' : 'firewalls are'} open to the internet`}
                text={open.slice(0, 3).map((f) => `${f.resource.name} (${openToInternet(f.resource).join(', ')})`).join(', ') +
                  (open.length > 3 ? ` and ${open.length - 3} more` : '') + '. Servers with the agent need no inbound SSH or RDP.'}
                action={<Button size="xs" variant="default" onClick={() => onTab('network')}>Review</Button>} />
            )}
            {failing.map((a) => (
              <Finding key={a.id} color="amber" icon={IconRefresh}
                title={`${a.name} was not fully read`}
                text={Object.keys(a.syncErrors).length > 0
                  ? `${Object.keys(a.syncErrors).length} region${Object.keys(a.syncErrors).length === 1 ? '' : 's'} failed: ${Object.values(a.syncErrors)[0]}. What is shown there is from the last good read.`
                  : `Last read ${relTime(a.syncedAt)}. What is shown is from then.`} />
            ))}
            {unmatched?.length === 0 && (findings ? findings.items.length === 0 : open?.length === 0) && failing.length === 0 && (
              <Box p="md"><EmptyState compact icon={IconCircleCheck} title="Nothing needs a look"
                description="Every instance has an Argus server, Risks & waste found nothing, and every account was read." /></Box>
            )}
          </Stack>
        </SectionCard>
      </Box>
    </Stack>
  )
}

/** The regions an account was read in; Google's "global" is not one. */
function regionCount(a: CloudAccount): number {
  return a.regions.filter((r) => r !== 'global').length
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

function AccountRow({ a, admin, bases: all }: { a: CloudAccount; admin: boolean; bases: CloudBase[] }) {
  // Only this cloud's identities: an AWS account cannot move to a Google one.
  const bases = all.filter((b) => (b.provider || 'aws') === a.provider)
  const aws = a.provider === 'aws'
  // Argus only reads an Azure subscription so far, so changes are not offered there.
  const azure = a.provider === 'azure'
  const sync = useSyncCloudAccount()
  const [confirm, setConfirm] = useState(false)
  const [moving, setMoving] = useState(false)
  const identity = bases.find((b) => b.identity === a.identity)
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
            {!aws && <Badge variant="light" color="slate" radius="sm" tt="none" fw={500}>{PROVIDER_LABEL[a.provider]}</Badge>}
            <EnvBadge env={a.environment} />
            {a.access === 'manage'
              ? <Badge variant="light" color="azure" radius="sm" tt="none" fw={500}>Can change</Badge>
              : azure
                ? <Tooltip label="Argus only reads Azure subscriptions so far. Changes from Argus come later for Azure." multiline w={280}>
                    <Badge variant="default" radius="sm" tt="none" fw={500}>Read only</Badge>
                  </Tooltip>
                : <Badge variant="default" radius="sm" tt="none" fw={500}>Read only</Badge>}
            {a.access === 'manage' && a.manageMissing?.length > 0 && (
              <Tooltip label={`The manage role lacks ${a.manageMissing.join(', ')}, so Argus cannot change disks, images or addresses here yet.`} multiline w={300}>
                <Badge variant="light" color="amber" radius="sm" tt="none" fw={500}>Role needs an update</Badge>
              </Tooltip>
            )}
            {a.identityMissing && (
              <Tooltip label={`Its identity (${a.identity}) is no longer available here, so Argus is not reading it. Change identity to reach it again.`} multiline w={300}>
                <Badge variant="light" color="rose" radius="sm" tt="none" fw={500}>Identity missing</Badge>
              </Tooltip>
            )}
          </Group>
          <Text size={FS.micro} c="dimmed" ff="monospace" mt={2}>{accountIdText(a.provider, a.accountId)} · {regionCount(a)} region{regionCount(a) === 1 ? '' : 's'}</Text>
          {!a.identityMissing && bases.length > 1 && (
            <Tooltip label={identity?.principal ?? a.identityArn} multiline w={320}>
              <Text size={FS.micro} c="dimmed" mt={2} w="fit-content">Connects as {identity?.name ?? a.identity}</Text>
            </Tooltip>
          )}
          <Text size={FS.meta} c="dimmed" mt={SP.tight}>
            {counts.map(([n, w]) => `${n} ${w}${n === 1 ? '' : 's'}`).join(' · ')}
          </Text>
          <Text size={FS.micro} mt={2} c={stale === 'fresh' ? 'dimmed' : INK.pending}>
            {stale === 'never' ? 'Being read for the first time…' : `Read ${relTime(a.syncedAt)}`}
          </Text>
        </Box>
        {admin && (
          <Group gap={4} wrap="wrap" justify="flex-end">
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
              : !azure && <Button size="xs" variant="subtle" leftSection={<IconPencil size={13} />} onClick={() => setAccess(true)}>Allow changes</Button>}
            {aws && (a.identityMissing || bases.length > 1) && (
              <Button size="xs" variant={a.identityMissing ? 'light' : 'subtle'} color={a.identityMissing ? 'rose' : undefined}
                onClick={() => setMoving(true)}>
                Change identity
              </Button>
            )}
            <Button size="xs" variant="subtle" color="rose" onClick={() => setConfirm(true)}>Disconnect</Button>
          </Group>
        )}
      </Flex>
      {moving && <ChangeIdentity a={a} bases={bases} onClose={() => setMoving(false)} />}
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
 * role, which Argus finds by its name and assumes before saving.
 */
function AllowChanges({ a, update, onClose }: { a: CloudAccount; update?: boolean; onClose: () => void }) {
  const gcp = a.provider === 'gcp'
  const prepared = useQuery({ queryKey: ['cloud', 'prepare-manage', a.provider, a.accountId],
    queryFn: () => prepareCloudAccount(a.accountId, true, '', '', a.provider), gcTime: 0 })
  const allow = useAllowCloudChanges()
  // The template's role has a fixed name, so the control plane finds it;
  // a role kept from before is checked again as it is.
  const arn = update ? a.manageRoleArn ?? '' : ''
  return (
    <Modal opened onClose={onClose} size="xl" centered title={<Text fw={700}>{update ? `Update the manage role in ${a.name}` : `Allow changes in ${a.name}`}</Text>}>
      <Stack gap="md">
        {gcp ? (
          <Text size={FS.body} lh={1.55}>
            Argus gets a second service account, <Text span ff="monospace">argus-manage</Text>, that can change Compute Engine and
            nothing else: no IAM, so it cannot grant itself more. For now Argus starts, stops and resets VMs and grows disks with it.
            Every change is made for the person who asked, waits for approval where the environment asks for it, and goes in the audit log.
          </Text>
        ) : update ? (
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
        {prepared.data ? (gcp ? <GcpSetup prepared={prepared.data} /> : <RoleSetup prepared={prepared.data} update />)
          : !prepared.error && <Text size={FS.meta} c="dimmed">{gcp ? 'Making the command…' : 'Making the template…'}</Text>}
        <Text size={FS.meta} c="dimmed">{gcp
          ? 'Once the command says Done, press the button. Argus finds argus-manage by its name; there is nothing to paste.'
          : 'Once the stack says UPDATE_COMPLETE, press the button. Argus finds the role by its name; there is nothing to paste.'}</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={allow.isPending}
            onClick={() => void run(() => allow.mutateAsync({ id: a.id, roleArn: arn }), { failure: 'Argus could not use that role',
              success: update ? ['Checking the role', `Argus is reading what the role in ${a.name} may do now.`]
                : ['Changes allowed', gcp ? `Admins can now start, stop and reset VMs, change their machine type, labels and deletion protection, make, grow, attach, detach, snapshot and delete disks, reserve and assign static addresses, launch and remove VMs, make and delete images, and make and change firewall rules in ${a.name}.`
                  : `Admins can now launch and change instances, disks and addresses in ${a.name}.`] }).then((ok) => ok && onClose())}>
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
          for approval can no longer run. {a.provider === 'gcp'
            ? 'argus-manage stays in the project until someone deletes it under IAM › Service accounts.'
            : 'The role stays in AWS until the stack is updated without it.'}
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
  const azure = a.provider === 'azure'
  const cloud = PROVIDER_LABEL[a.provider] ?? 'AWS'
  if (azure) {
    return (
      <Modal opened={opened} onClose={onClose} title={<Text fw={700}>Disconnect {a.name}?</Text>} centered>
        <Stack gap="md">
          <Text size={FS.body}>Disconnecting makes no call to Azure. Argus only forgets the subscription.</Text>
          <Box>
            <Text size={FS.meta} fw={700} mb={4}>Stays in Azure, untouched</Text>
            <List fz={FS.meta} spacing={4} icon={<ThemeIcon size={16} radius="xl" variant="light" color="teal"><IconCheck size={11} /></ThemeIcon>}>
              <List.Item>Every VM, disk, snapshot and image, as they are now</List.Item>
              <List.Item>Network security groups and every rule</List.Item>
              <List.Item>The Reader role Argus was given, until someone removes it under Access control (IAM)</List.Item>
              <List.Item>Argus agents on VMs, until you uninstall them</List.Item>
            </List>
          </Box>
          <Box>
            <Text size={FS.meta} fw={700} mb={4}>Changes in Argus only</Text>
            <List fz={FS.meta} spacing={4} icon={<ThemeIcon size={16} radius="xl" variant="light" color="gray"><IconMinus size={11} /></ThemeIcon>}>
              <List.Item>Argus stops reading the subscription, and its resources leave these lists</List.Item>
            </List>
            <Text size={FS.micro} c="dimmed" mt={6}>Reconnect it any time.</Text>
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
  return (
    <Modal opened={opened} onClose={onClose} title={<Text fw={700}>Disconnect {a.name}?</Text>} centered>
      <Stack gap="md">
        <Text size={FS.body}>Disconnecting makes no call to {cloud}. Argus only forgets the account.</Text>
        <Box>
          <Text size={FS.meta} fw={700} mb={4}>Stays in {cloud}, untouched</Text>
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

function FirstAccount({ admin, platform, base }: { admin: boolean; platform: boolean; base?: CloudBase }) {
  return (
    <Card padding="xl">
      <Stack align="center" gap="lg" maw={640} mx="auto" py="lg">
        <ThemeIcon size={56} radius="xl" variant="light" color="azure"><IconCloud size={30} /></ThemeIcon>
        <Box ta="center">
          <Text fw={700} size="lg">Connect your first AWS account, Google Cloud project or Azure subscription</Text>
          <Text size={FS.body} c="dimmed" mt={SP.tight} lh={1.6}>
            See every instance, disk and firewall across your accounts and regions, which ones have an Argus server, and what is
            open to the internet. Argus never asks for access keys: it uses a role or service account made in the account, or the Reader role you give it in a subscription, read only unless you allow changes.
          </Text>
        </Box>
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm" w="100%">
          {[['1', 'Name the account', 'Its AWS account id, Google Cloud project id or Azure subscription id, and whether it is production.'],
            ['2', 'Give Argus access', 'One command in CloudShell or Cloud Shell. Argus connects as soon as it works.']].map(([n, t, d]) => (
            <Box key={n} p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 10 }}>
              <Text size={FS.micro} c={INK.brand} fw={700}>STEP {n}</Text>
              <Text fw={600} size={FS.body} mt={2}>{t}</Text>
              <Text size={FS.meta} c="dimmed">{d}</Text>
            </Box>
          ))}
        </SimpleGrid>
        {base?.error && (
          <Alert color="amber" variant="light" w="100%" title={`argus-control's AWS identity ${base.name} does not answer`}>
            <Text size={FS.meta}>
              It needs one to assume roles in your accounts. {platform
                ? "On EC2 it uses the instance profile; anywhere else, give it an IAM user's access key under Settings › Cloud identities."
                : "Ask the platform's owner: it is set from the default organization."}
            </Text>
            <Text size={FS.micro} c="dimmed" mt={4}>{base.error}</Text>
            {admin && platform && (
              <ButtonLink to="/settings" search={{ section: 'cloud' }} size="compact-sm" variant="light" mt="xs">
                Set the AWS identity
              </ButtonLink>
            )}
          </Alert>
        )}
        {admin ? (
          <Group gap="sm" justify="center">
            <ButtonLink to="/cloud/connect" size="sm" leftSection={<IconBrandAws size={16} />}>Connect an AWS account</ButtonLink>
            <ButtonLink to="/cloud/connect" search={{ provider: 'gcp' }} size="sm" variant="default" leftSection={<IconBrandGoogle size={15} />}>
              Connect a Google Cloud project
            </ButtonLink>
            <ButtonLink to="/cloud/connect" search={{ provider: 'azure' }} size="sm" variant="default" leftSection={<IconBrandAzure size={15} />}>
              Connect an Azure subscription
            </ButtonLink>
          </Group>
        ) : (
          <Text size={FS.meta} c="dimmed">An administrator connects accounts.</Text>
        )}
      </Stack>
    </Card>
  )
}
