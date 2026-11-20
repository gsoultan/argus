import { ActionIcon, Alert, Button, Checkbox, Group, Menu, NumberInput, Select, Stack, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconCamera, IconDots, IconLink, IconLinkOff, IconPlus, IconResize, IconTag, IconTrash, IconUserCheck,
} from '@tabler/icons-react'
import { useState } from 'react'
import { Field, relTime } from '~/components/primitives'
import {
  ACTION_LEVEL, AZURE_DISK_MAX_GB, AZURE_DISK_NAME, AZURE_DISK_TYPES, GCP_DISK_TYPES, GCP_NAME, PRICE, VOLUME_TYPES, allows, allowsTags, attachment, cloudAccountsQuery, cloudChangesQuery, cloudItemsQuery, envOf, freeDevice, gcpDevicePath, money,
  roleLacks, volumeMonthly, volumeOf, type CloudAccount, type CloudAction, type CloudEnvironment, type CloudItem,
} from '~/lib/cloud'
import { meQuery } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import { FS } from '~/theme'
import { ChangeModal, TagsChange, useSubmit } from './ChangeModal'

/*
 * What can be done to a disk, snapshot, image or Elastic IP, from its panel,
 * and the two things made from nothing: a new disk and a new address. Shown
 * to admins and owners on an account Argus may change. Each one is a cloud
 * change like an instance's: the control plane decides whether it waits for
 * someone else, and runs it as the person who asked.
 */

const KIND_WORD: Record<string, string> = {
  volume: 'disk', snapshot: 'snapshot', image: 'image', address: 'Elastic IP', firewall: 'security group', 'target-group': 'target group', 'scaling-group': 'Auto Scaling group',
}

/** The account an item is in, when the person may change it there. */
export function useChangeable(it: CloudItem) {
  const { data: me } = useQuery(meQuery())
  const accounts = useQuery(cloudAccountsQuery())
  const pending = useQuery(cloudChangesQuery({ pending: true }))
  const account = accounts.data?.accounts.find((a) => a.id === it.accountId)
  const waiting = pending.data?.find((c) => c.accountId === it.accountId && c.resourceId === it.resource.id)
  return { admin: canSee(me?.role, 'admin'), account, waiting }
}

export function Gate({ it, account, children }: { it: CloudItem; account?: CloudAccount; children: React.ReactNode }) {
  if (!account) return null
  if (account.access !== 'manage') {
    return (
      <Text size={FS.meta} c="dimmed">
        {account.name} is read only. Allow changes on the Cloud overview to change this {KIND_WORD[it.resource.kind] ?? 'resource'} from here.
      </Text>
    )
  }
  return <>{children}</>
}

/** Says the manage role predates this and what to do, when it does. */
export function RoleNeedsUpdate({ account, action }: { account?: CloudAccount; action: CloudAction }) {
  if (!roleLacks(account, action)) return null
  return (
    <Alert color="amber" variant="light" p="sm">
      <Text size={FS.meta}>
        The manage role in {account!.name} was made before Argus could do this. Update it from the account's row on
        the Cloud overview; Argus sees the update at its next read.
      </Text>
    </Alert>
  )
}

export function StorageActions({ it }: { it: CloudItem }) {
  const { admin, account, waiting } = useChangeable(it)
  const [open, setOpen] = useState<CloudAction | 'tags' | 'new-disk' | null>(null)
  if (!admin) return null
  const r = it.resource
  const env = envOf(r.tags, it.environment)
  const close = () => setOpen(null)
  const busy = Boolean(waiting) || r.state === 'deleting' || r.state === 'deleted'
  const attached = attachment(r)
  const can = (a: CloudAction) => allows(account, a)
  const gcp = account?.provider === 'gcp'
  const azure = account?.provider === 'azure'

  const buttons = (() => {
    switch (r.kind) {
      case 'volume':
        return (
          <>
            {can('volume-grow') && (
              <Button size="xs" variant="default" leftSection={<IconResize size={14} />} disabled={busy} onClick={() => setOpen('volume-grow')}>
                {gcp || azure ? 'Grow' : 'Grow or change'}
              </Button>
            )}
            {r.state === 'available'
              ? can('volume-attach') && <Button size="xs" variant="default" leftSection={<IconLink size={14} />} disabled={busy} onClick={() => setOpen('volume-attach')}>Attach</Button>
              : can('volume-detach') && <Button size="xs" variant="default" leftSection={<IconLinkOff size={14} />} disabled={busy || !attached} onClick={() => setOpen('volume-detach')}>Detach</Button>}
            {can('snapshot-create') && <Button size="xs" variant="default" leftSection={<IconCamera size={14} />} disabled={busy} onClick={() => setOpen('snapshot-create')}>Snapshot</Button>}
          </>
        )
      case 'snapshot':
        return can('volume-create') && <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} disabled={r.state !== (gcp ? 'ready' : 'completed')} onClick={() => setOpen('new-disk')}>New disk from it</Button>
      case 'address':
        return r.state === 'attached'
          ? can('address-disassociate') && <Button size="xs" variant="default" leftSection={<IconLinkOff size={14} />} disabled={busy} onClick={() => setOpen('address-disassociate')}>Take it off</Button>
          : can('address-associate') && !(gcp && (!r.publicIp || r.region === 'global')) && <Button size="xs" variant="default" leftSection={<IconLink size={14} />} disabled={busy} onClick={() => setOpen('address-associate')}>Attach to an instance</Button>
      default:
        return null
    }
  })()
  const remove: Record<string, { action: CloudAction; label: string }> = {
    volume: { action: 'volume-delete', label: 'Delete disk…' },
    snapshot: { action: 'snapshot-delete', label: 'Delete snapshot…' },
    image: { action: 'image-delete', label: 'Delete image…' },
    address: { action: 'address-release', label: 'Release address…' },
  }
  const rm = remove[r.kind] && can(remove[r.kind]!.action) ? remove[r.kind] : undefined

  return (
    <Gate it={it} account={account}>
      <Stack gap="xs">
        {waiting && (
          <Alert color="amber" variant="light" p="xs" icon={<IconUserCheck size={16} />}>
            <Text size={FS.meta}><b>{waiting.summary}</b> is waiting for approval, asked by {waiting.requestedBy} {relTime(waiting.requestedAt)}.</Text>
          </Alert>
        )}
        <Group gap="xs">
          {buttons}
          {allowsTags(account, r.kind) && <Button size="xs" variant="default" leftSection={<IconTag size={14} />} disabled={busy} onClick={() => setOpen('tags')}>{gcp ? 'Labels' : 'Tags'}</Button>}
          {rm && (
            <Menu position="bottom-end" withinPortal>
              <Menu.Target>
                <ActionIcon variant="default" size={30} aria-label="More actions" disabled={busy}><IconDots size={15} /></ActionIcon>
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Item color="rose" leftSection={<IconTrash size={14} />} onClick={() => setOpen(rm.action)}>{rm.label}</Menu.Item>
              </Menu.Dropdown>
            </Menu>
          )}
        </Group>
        {open === 'tags' && <TagsChange it={it} env={env} gcp={gcp} onClose={close} />}
        {open === 'volume-grow' && (gcp || azure
          ? <SizeOnlyGrow it={it} env={env} account={account} onClose={close} />
          : <VolumeGrow it={it} env={env} account={account} onClose={close} />)}
        {(open === 'volume-attach' || open === 'address-associate') && <AttachTo it={it} env={env} account={account} action={open} onClose={close} />}
        {(open === 'volume-detach' || open === 'address-disassociate') && <Detach it={it} env={env} account={account} action={open} onClose={close} />}
        {open === 'snapshot-create' && <SnapshotCreate it={it} env={env} account={account} onClose={close} />}
        {open === 'new-disk' && <NewDisk accountId={it.accountId} region={r.region} snapshot={it} onClose={close} />}
        {rm && open === rm.action && <Delete it={it} env={env} account={account} action={rm.action} onClose={close} />}
      </Stack>
    </Gate>
  )
}

/** Whether an Azure disk type is kept in every zone of its region, and so attaches to a VM in any of them. */
const zoneRedundant = (type?: string) => Boolean(type?.endsWith('_ZRS'))

/**
 * Instances in the same account and region (and zone, for a disk) that are not being removed. An Azure disk may
 * have no zone, and then goes only on a VM without one.
 */
function useTargets(it: CloudItem, sameZone: boolean) {
  const instances = useQuery(cloudItemsQuery('instance'))
  const anyZone = zoneRedundant(it.resource.details?.type)
  return (instances.data ?? []).filter((x) => x.accountId === it.accountId && x.resource.region === it.resource.region &&
    x.resource.state !== 'terminated' && x.resource.state !== 'shutting-down' &&
    (!sameZone || anyZone || (x.resource.zone ?? '') === (it.resource.zone ?? '')))
}

/**
 * Growing a Google Cloud or Azure disk: its size, and nothing else. Neither
 * changes a disk's type or its performance by resizing it.
 */
function SizeOnlyGrow({ it, env, account, onClose }: { it: CloudItem; env: CloudEnvironment; account?: CloudAccount; onClose: () => void }) {
  const r = it.resource
  const now = volumeOf(r)
  const [gb, setGb] = useState<number>(now.gb)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const shrink = gb < now.gb
  const azure = account?.provider === 'azure'
  const unit = azure ? 'GiB' : 'GB'
  // The disk a VM starts from carries the system it holds.
  const boots = azure && Boolean(r.details?.platform) && r.state === 'in-use'
  return (
    <ChangeModal title={`Grow ${r.name}`} env={env} level="change" reason={reason} setReason={setReason} onClose={onClose}
      busy={busy} submit="Grow disk" disabled={gb <= now.gb || roleLacks(account, 'volume-grow')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'volume-grow', resourceId: r.id,
        spec: { sizeGb: gb }, reason }, 'Disk growing')}>
      <RoleNeedsUpdate account={account} action="volume-grow" />
      <Text size={FS.body} lh={1.55}>
        {azure
          ? `Azure grows a data disk in a few seconds while it stays in use. A disk can only grow, never shrink. Its type and performance stay as they are.${boots ? ' This is the disk its VM starts from, and Azure grows that one only while the VM is stopped.' : ''}`
          : 'Google grows the disk in a few seconds while it stays in use. A disk can only grow, never shrink. Its type and performance stay as they are.'}
      </Text>
      <NumberInput label={`Size, ${unit}`} value={gb} onChange={(v) => setGb(Number(v) || 0)} min={now.gb} max={azure ? AZURE_DISK_MAX_GB : 65536} w={200}
        error={shrink ? `A disk cannot shrink below ${now.gb} ${unit}` : undefined} />
      {gb > now.gb && (
        <Text size={FS.meta} c="dimmed">
          The file system on it keeps its old size until it is grown too, from the server's Disks tab when it has the Argus agent.
        </Text>
      )}
    </ChangeModal>
  )
}

function VolumeGrow({ it, env, account, onClose }: { it: CloudItem; env: CloudEnvironment; account?: CloudAccount; onClose: () => void }) {
  const r = it.resource
  const now = volumeOf(r)
  const [gb, setGb] = useState<number>(now.gb)
  const [type, setType] = useState<string>(now.type || 'gp3')
  const [iops, setIops] = useState<number | ''>('')
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const changed = gb !== now.gb || type !== now.type || iops !== ''
  const shrink = gb < now.gb
  return (
    <ChangeModal title={`Grow or change ${r.name}`} env={env} level="change" reason={reason} setReason={setReason} onClose={onClose}
      busy={busy} submit="Change disk" disabled={!changed || shrink || roleLacks(account, 'volume-grow')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'volume-grow', resourceId: r.id,
        spec: { sizeGb: gb !== now.gb ? gb : 0, type: type !== now.type ? type : '', iops: iops || 0 }, reason }, 'Disk changing')}>
      <RoleNeedsUpdate account={account} action="volume-grow" />
      <Text size={FS.body} lh={1.55}>
        AWS changes the disk while it stays in use. A disk can only grow, and AWS allows one change to it every six hours.
      </Text>
      <Group grow align="flex-start">
        <NumberInput label="Size, GiB" value={gb} onChange={(v) => setGb(Number(v) || 0)} min={now.gb} max={16384}
          error={shrink ? `AWS cannot shrink it below ${now.gb} GiB` : undefined} />
        <Select label="Type" value={type} onChange={(v) => v && setType(v)} data={VOLUME_TYPES.map((t) => ({ value: t.value, label: t.label }))} allowDeselect={false} />
        {(type === 'gp3' || type === 'io1' || type === 'io2') && (
          <NumberInput label="IOPS" placeholder={type === 'gp3' ? '3000 included' : 'Required'} value={iops}
            onChange={(v) => setIops(v === '' ? '' : Number(v))} min={type === 'gp3' ? 3000 : 100} max={type === 'gp3' ? 16000 : 64000} />
        )}
      </Group>
      <Text size={FS.meta} c="dimmed">
        About {money(volumeMonthly(now.type, now.gb))} a month now, {money(volumeMonthly(type, gb))} after, at us-east-1 list prices.
        {gb > now.gb && ' The file system on it keeps its old size until it is grown too, from the server\'s Disks tab when it has the Argus agent.'}
      </Text>
    </ChangeModal>
  )
}

function AttachTo({ it, env, account, action, onClose }: {
  it: CloudItem; env: CloudEnvironment; account?: CloudAccount; action: 'volume-attach' | 'address-associate'; onClose: () => void
}) {
  const r = it.resource
  const disk = action === 'volume-attach'
  const targets = useTargets(it, disk)
  const [instance, setInstance] = useState<string | null>(null)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const target = targets.find((x) => x.resource.id === instance)
  const gcp = account?.provider === 'gcp'
  const azure = account?.provider === 'azure'
  const device = !target || azure ? '' : gcp ? gcpDevicePath(r.name) : freeDevice(target.resource)
  const stricter = target ? envOf(target.resource.tags, target.environment) : env
  const shown = stricter === 'production' || (stricter === 'staging' && env !== 'production') ? stricter : env
  return (
    <ChangeModal title={disk ? `Attach ${r.name}` : `Attach ${r.publicIp}`} env={shown} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Attach" disabled={!instance || roleLacks(account, action)}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action, resourceId: r.id, spec: { instance }, reason }, 'Attached')}>
      <RoleNeedsUpdate account={account} action={action} />
      <Select label="Instance" placeholder={targets.length ? 'Pick an instance' : disk ? (r.zone ? `No instances in ${r.zone}` : 'No instances without a zone here') : 'No instances here'}
        searchable value={instance} onChange={setInstance}
        data={targets.map((x) => ({ value: x.resource.id, label: `${x.resource.name} · ${x.resource.state}` }))} />
      <Text size={FS.meta} c="dimmed" lh={1.5}>
        {disk && azure
          ? `${zoneRedundant(r.details?.type) ? 'This disk is kept in every zone, so it attaches to any VM in its region.' : r.zone ? `A disk attaches only to a VM in its own zone, ${r.zone}.` : 'A disk without a zone attaches only to a VM without one.'} Azure picks the LUN it is attached at; mount it on the server to use it.`
          : disk
          ? `A disk attaches only to an instance in its own zone, ${r.zone}.${device ? ` It appears as ${device}; mount it on the server to use it.` : ''}`
          : `The instance answers on ${r.publicIp} from then on${target?.resource.publicIp ? `, instead of ${target.resource.publicIp}` : ''}.`}
      </Text>
    </ChangeModal>
  )
}

function Detach({ it, env, account, action, onClose }: {
  it: CloudItem; env: CloudEnvironment; account?: CloudAccount; action: 'volume-detach' | 'address-disassociate'; onClose: () => void
}) {
  const r = it.resource
  const at = attachment(r)
  const instances = useQuery(cloudItemsQuery('instance'))
  const name = instances.data?.find((x) => x.accountId === it.accountId && x.resource.id === at?.id)?.resource.name ?? at?.id ?? 'its instance'
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const disk = action === 'volume-detach'
  const gcp = account?.provider === 'gcp'
  const azure = account?.provider === 'azure'
  return (
    <ChangeModal title={disk ? `Detach ${r.name} from ${name}?` : `Take ${r.publicIp} off ${name}?`} env={env} level="change"
      reason={reason} setReason={setReason} onClose={onClose} busy={busy} submit={disk ? 'Detach' : 'Take it off'} color="amber"
      disabled={roleLacks(account, action)}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action, resourceId: r.id, reason }, disk ? 'Detaching' : 'Taken off')}>
      <RoleNeedsUpdate account={account} action={action} />
      <Text size={FS.body} lh={1.55}>
        {disk
          ? azure
            ? 'Unmount it on the server first. Azure detaches the disk at once, whether or not the server is still using it, and files being written can be lost.'
            : gcp
            ? 'Unmount it on the server first. Google detaches the disk at once, whether or not the server is still using it, and files being written can be lost.'
            : 'Unmount it on the server first. Argus never forces a detach, so AWS waits for the server to let go of it; a disk still in use stays attached.'
          : gcp
            ? `${name} loses this address and has no public address until it is given one. The static address stays yours and is billed while nothing uses it.`
            : `${name} loses this address. A running instance gets a new public address from Amazon only when it next starts; the Elastic IP stays yours and is billed while it is not attached.`}
      </Text>
    </ChangeModal>
  )
}

function SnapshotCreate({ it, env, account, onClose }: { it: CloudItem; env: CloudEnvironment; account?: CloudAccount; onClose: () => void }) {
  const r = it.resource
  const [name, setName] = useState('')
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const gb = volumeOf(r).gb
  const gcp = account?.provider === 'gcp'
  const azure = account?.provider === 'azure'
  const badName = name.trim() !== '' && ((gcp && !GCP_NAME.test(name.trim())) || (azure && !AZURE_DISK_NAME.test(name.trim())))
  const stamp = new Date().toISOString().slice(0, 16).replace(/[-:]/g, '').replace('T', '-')
  return (
    <ChangeModal title={`Snapshot ${r.name}`} env={env} level={ACTION_LEVEL['snapshot-create']} reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Take snapshot" disabled={badName || roleLacks(account, 'snapshot-create')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'snapshot-create', resourceId: r.id,
        spec: { name: name.trim() }, reason }, 'Snapshot started')}>
      <RoleNeedsUpdate account={account} action="snapshot-create" />
      <Text size={FS.body} lh={1.55}>
        {azure ? 'Azure' : gcp ? 'Google' : 'AWS'} copies the disk as it is now, while it stays in use, and finishes in the background. For a consistent copy of
        a busy database, pause its writes first.
      </Text>
      {azure ? (
        <TextInput label="Name" placeholder={`${r.name}-${stamp}`} value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80}
          error={badName ? 'Letters, digits, . _ and -, starting with a letter or digit, up to 80' : undefined} />
      ) : gcp ? (
        <TextInput label="Name" placeholder={`${r.name}-${stamp}`}
          value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={63}
          error={badName ? 'Lowercase letters, digits and -, starting with a letter, up to 63' : undefined} />
      ) : (
        <TextInput label="Name" placeholder={`${r.name} ${new Date().toISOString().slice(0, 16).replace('T', ' ')}`} value={name}
          onChange={(e) => setName(e.currentTarget.value)} maxLength={128} />
      )}
      <Text size={FS.meta} c="dimmed">
        {azure
          ? `Azure keeps it in the disk's resource group${r.details?.resourceGroup ? `, ${r.details.resourceGroup},` : ''} and bills it while it is kept; later snapshots of the same disk are billed only for what changed.`
          : gcp
          ? 'Google keeps it for the whole project and bills it while it is kept; later snapshots of the same disk store only what changed.'
          : `Up to about ${money(gb * PRICE.snapshotPerGb)} a month while it is kept; later snapshots of the same disk store only what changed.`}
      </Text>
    </ChangeModal>
  )
}

/** Deleting a disk, snapshot or image, or releasing an address: the name typed, and what goes. */
function Delete({ it, env, account, action, onClose }: { it: CloudItem; env: CloudEnvironment; account?: CloudAccount; action: CloudAction; onClose: () => void }) {
  const r = it.resource
  const images = useQuery(cloudItemsQuery('image'))
  const [typed, setTyped] = useState('')
  const [reason, setReason] = useState('')
  const gcp = account?.provider === 'gcp'
  const azure = account?.provider === 'azure'
  // A final snapshot, and an image's own snapshots, are AWS's; elsewhere each is its own change.
  const aws = !gcp && !azure
  const maker = azure ? 'Azure' : gcp ? 'Google' : 'AWS'
  const [extra, setExtra] = useState<boolean>(action === 'volume-delete' ? env === 'production' && aws : action === 'image-delete' && aws)
  const { submit, busy } = useSubmit(onClose)
  const word = r.kind === 'address' ? r.publicIp || r.name : r.name
  const usedBy = r.kind === 'snapshot'
    ? images.data?.find((im) => im.accountId === it.accountId && im.resource.links?.some((l) => l.kind === 'snapshot' && l.id === r.id))
    : undefined
  const refusal = action === 'volume-delete' && r.state !== 'available' ? 'it is attached to an instance; detach it first'
    : action === 'address-release' && r.state === 'attached' ? 'it is attached; take it off first'
      : usedBy ? `the image ${usedBy.resource.name} is made from it; delete the image, with its snapshots, instead`
        : r.tags?.['aws:cloudformation:stack-name'] && action === 'volume-delete' ? `it belongs to CloudFormation stack ${r.tags['aws:cloudformation:stack-name']}` : ''
  const verb = action === 'address-release' ? 'Release' : 'Delete'
  return (
    <ChangeModal title={`${verb} ${word}?`} env={env} level="remove" reason={reason} setReason={setReason} onClose={onClose} busy={busy}
      submit={`${verb} ${gcp && r.kind === 'address' ? 'static address' : KIND_WORD[r.kind]}`} color="rose" disabled={Boolean(refusal) || (typed.trim() !== word && typed.trim() !== r.id) || roleLacks(account, action)}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action, resourceId: r.id, confirm: typed.trim(), reason,
        spec: action === 'volume-delete' ? { finalSnapshot: extra } : action === 'image-delete' ? { withSnapshots: extra } : undefined }, verb === 'Release' ? 'Released' : 'Deleting')}>
      <RoleNeedsUpdate account={account} action={action} />
      {refusal ? (
        <Alert color="rose" variant="light" title="Argus will not do this"><Text size={FS.meta}>It cannot be done because {refusal}.</Text></Alert>
      ) : (
        <>
          <Text size={FS.body} lh={1.55}>
            {action === 'volume-delete' && `${maker} deletes the disk and everything on it (${r.size}). This cannot be undone.`}
            {action === 'volume-delete' && !aws && ' To keep a copy, take a snapshot of it first and delete it once that is ready.'}
            {action === 'snapshot-delete' && `${maker} deletes the snapshot (${r.size}). Disks already made from it are not touched. This cannot be undone.`}
            {action === 'image-delete' && `Nobody can launch from this image afterwards. ${gcp ? 'VMs' : 'Instances'} already launched from it keep running.`}
            {action === 'address-release' && `${r.publicIp ?? r.addresses?.[0] ?? r.name} goes back to ${gcp ? 'Google' : 'Amazon'} and is gone for good; anything that points at it (DNS, allow lists) stops reaching you.`}
          </Text>
          {action === 'volume-delete' && aws && (
            <Checkbox checked={extra} onChange={(e) => setExtra(e.currentTarget.checked)} label="Take a final snapshot first"
              description="It is kept and billed until someone deletes it. On by default in production." />
          )}
          {action === 'image-delete' && aws && (
            <Checkbox checked={extra} onChange={(e) => setExtra(e.currentTarget.checked)} label="Delete the snapshots it was made of too"
              description="Otherwise they stay, and are billed, after the image is gone." />
          )}
          <TextInput label={<>Type <Text span ff="monospace" fw={700}>{word}</Text> to confirm</>} value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)} data-autofocus />
        </>
      )}
    </ChangeModal>
  )
}

/** A Google image name from a VM's and a day: lowercase, at most 63. */
function gcpImageName(vm: string, day: string) {
  const tail = `-${day}`
  let head = vm.toLowerCase().replace(/[^-a-z0-9]/g, '-').slice(0, 63 - tail.length).replace(/-+$/, '')
  if (!/^[a-z]/.test(head)) head = `vm-${head}`.slice(0, 63 - tail.length)
  return head + tail
}

/** Making an image of an instance, from its panel. */
export function ImageCreate({ it, env, onClose }: { it: CloudItem; env: CloudEnvironment; onClose: () => void }) {
  const r = it.resource
  const accounts = useQuery(cloudAccountsQuery())
  const account = accounts.data?.accounts.find((a) => a.id === it.accountId)
  const gcp = account?.provider === 'gcp' || it.provider === 'gcp'
  const day = new Date().toISOString().slice(0, 10)
  const [name, setName] = useState(gcp ? gcpImageName(r.name, day) : `${r.name} ${day}`)
  const [reboot, setReboot] = useState(true)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const valid = gcp ? /^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$/.test(name.trim()) : /^[A-Za-z0-9()[\] ./'@_-]{3,128}$/.test(name.trim())
  return (
    <ChangeModal title={`Make an image of ${r.name}`} env={env} level="change" reason={reason} setReason={setReason} onClose={onClose}
      busy={busy} submit="Make image" disabled={!valid || roleLacks(account, 'image-create')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'image-create', resourceId: r.id,
        spec: { name: name.trim(), reboot: !gcp && reboot && r.state === 'running' }, reason }, 'Image started')}>
      <RoleNeedsUpdate account={account} action="image-create" />
      {gcp ? (
        <Text size={FS.body} lh={1.55}>
          An image is a copy of the VM's boot disk; other disks are not in it. Launch new VMs from it in any region of the project,
          from Launch an instance.{r.state === 'running' && ' Google copies the disk while the VM runs, so files being written may be caught half-written; stop the VM first for a consistent copy.'}
        </Text>
      ) : (
        <Text size={FS.body} lh={1.55}>
          An image is a snapshot of every disk plus what AWS needs to launch new instances from it. Launch it from AWS, or use it
          to bring this server back as it is today.
        </Text>
      )}
      <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={gcp ? 63 : 128}
        error={name && !valid ? (gcp ? 'Lowercase letters, digits and -, up to 63, starting with a letter' : 'Letters, digits, spaces and ( ) [ ] . / \' @ _ -, 3 to 128') : undefined} />
      {r.state === 'running' && !gcp && (
        <Checkbox checked={reboot} onChange={(e) => setReboot(e.currentTarget.checked)} label="Reboot it for a consistent image"
          description="The instance restarts, and anyone signed in is disconnected. Without it, files being written may be caught half-written." />
      )}
    </ChangeModal>
  )
}

/** A new disk: empty, or from a snapshot. Opened from Disks, or from a snapshot's panel. */
export function NewDisk({ accountId, region, snapshot, onClose }: { accountId?: string; region?: string; snapshot?: CloudItem; onClose: () => void }) {
  const accounts = useQuery(cloudAccountsQuery())
  const subnets = useQuery(cloudItemsQuery('subnet'))
  const instances = useQuery(cloudItemsQuery('instance'))
  const volumes = useQuery(cloudItemsQuery('volume'))
  const manageable = (accounts.data?.accounts ?? []).filter((a) => a.access === 'manage' && allows(a, 'volume-create'))
  const [acct, setAcct] = useState<string | null>(accountId ?? (manageable.length === 1 ? manageable[0]!.id : null))
  const account = manageable.find((a) => a.id === acct)
  const gcp = account?.provider === 'gcp'
  const azure = account?.provider === 'azure'
  // A Google Cloud snapshot belongs to the whole project, so its disk can be made in any region.
  const regions = (account?.regions ?? []).filter((x) => x !== 'global')
  const [reg, setReg] = useState<string | null>(region && region !== 'global' ? region : regions.length === 1 ? regions[0]! : null)
  const [zone, setZone] = useState<string | null>(null)
  const snapGb = snapshot ? volumeOf(snapshot.resource).gb : 0
  const [gb, setGb] = useState<number>(snapGb || 20)
  const [type, setType] = useState<string | null>(null)
  const [name, setName] = useState('')
  const [attachTo, setAttachTo] = useState<string | null>(null)
  const [group, setGroup] = useState<string | null>(snapshot?.resource.details?.resourceGroup ?? null)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const kind = type ?? (azure ? AZURE_DISK_TYPES[0]!.value : gcp ? 'pd-balanced' : 'gp3')
  // Google subnets span a region; its zones are read off the VMs and disks already there.
  const zoned = gcp ? [...(instances.data ?? []), ...(volumes.data ?? [])] : subnets.data ?? []
  // Azure numbers a region's zones 1 to 3, and a disk may have none; a zone-redundant disk never has one.
  const zrs = azure && zoneRedundant(kind)
  const zones = azure ? (reg && !zrs ? [1, 2, 3].map((n) => `${reg}-${n}`) : [])
    : [...new Set(zoned.filter((s) => s.accountId === acct && s.resource.region === reg && s.resource.zone)
      .map((s) => s.resource.zone!))].sort()
  const targets = (instances.data ?? []).filter((x) => x.accountId === acct &&
    (azure ? x.resource.region === reg && (zrs || (x.resource.zone ?? '') === (zone ?? '')) : x.resource.zone === zone) &&
    x.resource.state !== 'terminated' && x.resource.state !== 'shutting-down')
  // A new Azure disk goes into a resource group Argus has read something in.
  const groups = azure ? [...new Set([...(instances.data ?? []), ...(volumes.data ?? [])]
    .filter((x) => x.accountId === acct && x.resource.details?.resourceGroup).map((x) => x.resource.details!.resourceGroup!))].sort() : []
  const target = targets.find((x) => x.resource.id === attachTo)
  const env: CloudEnvironment = target ? envOf(target.resource.tags, target.environment) : account?.environment ?? 'production'
  const hdd = kind === 'st1' || kind === 'sc1'
  const least = gcp ? Math.max(10, snapGb) : hdd ? 125 : Math.max(1, snapGb)
  const tooSmall = gb < least
  // The VM's resource group, unless another is picked.
  const inGroup = group ?? target?.resource.details?.resourceGroup ?? null
  const taken = gcp ? zone !== null && (volumes.data ?? []).some((v) => v.accountId === acct && v.resource.zone === zone && v.resource.name === name.trim())
    : azure && inGroup !== null && (volumes.data ?? []).some((v) => v.accountId === acct && v.resource.name.toLowerCase() === name.trim().toLowerCase() &&
      v.resource.details?.resourceGroup?.toLowerCase() === inGroup.toLowerCase())
  const nameProblem = !(gcp || azure) || name.trim() === '' ? undefined
    : azure ? (!AZURE_DISK_NAME.test(name.trim()) ? 'Letters, digits, . _ and -, starting with a letter or digit, up to 80'
      : taken ? `There is already a disk ${name.trim()} in ${inGroup}` : undefined)
      : !GCP_NAME.test(name.trim()) ? 'Lowercase letters, digits and -, starting with a letter, up to 63'
        : taken ? `There is already a disk ${name.trim()} in ${zone}` : undefined
  const unit = gcp ? 'GB' : 'GiB'
  return (
    <ChangeModal title={snapshot ? `New disk from ${snapshot.resource.name}` : 'New disk'} env={env} level="change" reason={reason}
      setReason={setReason} onClose={onClose} busy={busy} submit="Make disk"
      disabled={!acct || !reg || (!zone && !azure) || (azure && !inGroup) || tooSmall || ((gcp || azure) && (name.trim() === '' || Boolean(nameProblem))) || roleLacks(account, 'volume-create')}
      onSubmit={() => void submit({ accountId: acct!, region: reg!, action: 'volume-create', spec: {
        name: name.trim(), zone: zone ?? undefined, group: azure ? inGroup ?? undefined : undefined, sizeGb: gb, type: kind,
        snapshot: snapshot?.resource.id, attachTo: attachTo ?? undefined }, reason }, 'Making the disk')}>
      <RoleNeedsUpdate account={account} action="volume-create" />
      {!snapshot && manageable.length > 1 && (
        <Select label="Account" placeholder="Pick an account" value={acct} onChange={(v) => { setAcct(v); setReg(null); setZone(null); setType(null) }}
          data={manageable.map((a) => ({ value: a.id, label: a.name }))} allowDeselect={false} />
      )}
      {(!snapshot || (gcp && region === 'global')) && account && regions.length > 1 && (
        <Select label="Region" placeholder="Pick a region" value={reg} onChange={(v) => { setReg(v); setZone(null); setAttachTo(null) }}
          data={regions} allowDeselect={false} />
      )}
      <Group grow align="flex-start">
        {azure ? (
          <Select label="Zone" placeholder={zrs ? 'Kept in every zone' : 'No zone'} value={zrs ? null : zone} clearable disabled={zrs || !reg}
            onChange={(v) => { setZone(v); setAttachTo(null) }} data={zones} />
        ) : (
          <Select label="Zone" placeholder={zones.length ? 'Pick a zone' : 'No zones read yet'} value={zone}
            onChange={(v) => { setZone(v); setAttachTo(null) }} data={zones} allowDeselect={false} />
        )}
        {azure ? (
          <TextInput label="Name" placeholder="Required, like logs-1" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80}
            error={nameProblem} />
        ) : gcp ? (
          <TextInput label="Name" placeholder="Required, like logs-1" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={63}
            error={nameProblem} />
        ) : (
          <TextInput label="Name" placeholder="Optional" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={128} />
        )}
      </Group>
      <Group grow align="flex-start">
        <NumberInput label={`Size, ${unit}`} value={gb} onChange={(v) => setGb(Number(v) || 0)} min={least} max={azure ? AZURE_DISK_MAX_GB : gcp ? 65536 : 16384}
          error={tooSmall ? (snapGb > 0 && least === snapGb ? `At least ${snapGb} ${unit}, the snapshot's size` : `${hdd ? 'HDD disks are' : 'Disks are'} ${least} ${unit} and up`) : undefined} />
        <Select label="Type" value={kind} onChange={(v) => { if (v) { setType(v); setAttachTo(null); if (azure && zoneRedundant(v)) setZone(null) } }}
          data={(azure ? AZURE_DISK_TYPES : gcp ? GCP_DISK_TYPES : VOLUME_TYPES).map((t) => ({ value: t.value, label: t.label }))} allowDeselect={false} />
      </Group>
      {azure && (
        <Select label="Resource group" searchable clearable value={group} onChange={setGroup} data={groups}
          placeholder={target?.resource.details?.resourceGroup ? `${target.resource.details.resourceGroup}, the VM's` : groups.length ? 'Pick a resource group' : 'No resource groups read yet'}
          description="Where the disk is kept and billed. Argus lists the groups it has read a VM or a disk in." />
      )}
      <Select label="Attach to"
        placeholder={azure ? (targets.length ? 'Leave unattached' : zone ? `No VMs in ${zone}` : zrs ? 'No VMs here' : 'No VMs without a zone here')
          : zone ? (targets.length ? 'Leave unattached' : `No instances in ${zone}`) : 'Pick a zone first'}
        clearable value={attachTo} onChange={setAttachTo} disabled={(!zone && !azure) || targets.length === 0}
        data={targets.map((x) => ({ value: x.resource.id, label: azure ? `${x.resource.name} · ${x.resource.state}` : gcp
          ? `${x.resource.name} · as ${name.trim() ? gcpDevicePath(name.trim()) : 'its name'}`
          : `${x.resource.name} · as ${freeDevice(x.resource) || 'no free device'}` }))} />
      <Field label="Cost">
        <Text size={FS.meta} c="dimmed">
          {azure
            ? 'Azure bills it by size and type from when it is made until it is deleted, attached or not. Encrypted with a key Azure manages.'
            : gcp
            ? 'Google bills it by size and type from when it is made until it is deleted, attached or not. Encrypted with a key Google manages.'
            : <>About {money(volumeMonthly(kind, gb))} a month at us-east-1 list prices, from when it is made until it is deleted,
              attached or not. Encrypted with the account's default key.</>}
        </Text>
      </Field>
    </ChangeModal>
  )
}

/** A new Elastic IP, from Network › Elastic IPs. */
export function NewAddress({ onClose }: { onClose: () => void }) {
  const accounts = useQuery(cloudAccountsQuery())
  const addresses = useQuery(cloudItemsQuery('address'))
  const manageable = (accounts.data?.accounts ?? []).filter((a) => a.access === 'manage' && allows(a, 'address-allocate'))
  const [acct, setAcct] = useState<string | null>(manageable.length === 1 ? manageable[0]!.id : null)
  const account = manageable.find((a) => a.id === acct)
  const gcp = account?.provider === 'gcp'
  const regions = (account?.regions ?? []).filter((x) => x !== 'global')
  const [reg, setReg] = useState<string | null>(regions.length === 1 ? regions[0]! : null)
  const [name, setName] = useState('')
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const n = name.trim()
  const taken = gcp && reg !== null && (addresses.data ?? []).some((x) => x.accountId === acct && x.resource.region === reg && x.resource.name === n)
  const nameProblem = !gcp || n === '' ? undefined
    : !GCP_NAME.test(n) ? 'Lowercase letters, digits and -, starting with a letter, up to 63'
      : taken ? `There is already an address ${n} in ${reg}` : undefined
  return (
    <ChangeModal title={gcp ? 'Reserve a static address' : 'Allocate an Elastic IP'} env={account?.environment ?? 'production'} level="change"
      reason={reason} setReason={setReason} onClose={onClose} busy={busy} submit={gcp ? 'Reserve' : 'Allocate'}
      disabled={!acct || !reg || (gcp && (n === '' || Boolean(nameProblem))) || roleLacks(account, 'address-allocate')}
      onSubmit={() => void submit({ accountId: acct!, region: reg!, action: 'address-allocate', spec: { name: n }, reason }, gcp ? 'Reserved' : 'Allocated')}>
      <RoleNeedsUpdate account={account} action="address-allocate" />
      <Text size={FS.body} lh={1.55}>
        {gcp
          ? 'An external address that stays yours until it is released, to give a VM in the same region so it keeps the same address across stops and starts.'
          : 'A public address that stays yours until it is released, to attach to an instance so it keeps the same address across stops and starts.'}
      </Text>
      {manageable.length > 1 && (
        <Select label="Account" placeholder="Pick an account" value={acct} onChange={(v) => { setAcct(v); setReg(null) }} data={manageable.map((a) => ({ value: a.id, label: a.name }))} allowDeselect={false} />
      )}
      {account && regions.length > 1 && (
        <Select label="Region" placeholder="Pick a region" value={reg} onChange={setReg} data={regions} allowDeselect={false} />
      )}
      {gcp ? (
        <TextInput label="Name" placeholder="Required, like web-ip" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={63} error={nameProblem} />
      ) : (
        <TextInput label="Name" placeholder="Optional" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={128} />
      )}
      <Text size={FS.meta} c="dimmed">
        {gcp
          ? 'Google bills a static address while it is reserved, and more while no VM uses it.'
          : `About ${money(PRICE.elasticIp)} a month, attached or not, as AWS bills every public IPv4 address.`}
      </Text>
    </ChangeModal>
  )
}
