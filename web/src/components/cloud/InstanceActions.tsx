import { ActionIcon, Alert, Badge, Box, Button, Checkbox, Group, Menu, Select, Stack, Table, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconDots, IconLock, IconLockOpen, IconPhoto, IconShield, IconPlayerPlay, IconPlayerStop, IconRefresh, IconResize, IconTag, IconTrash, IconUserCheck,
} from '@tabler/icons-react'
import { useState } from 'react'
import { Field, relTime } from '~/components/primitives'
import {
  ACTION_LEVEL, cloudAccountsQuery, cloudChangesQuery, cloudLaunchOptionsQuery, cloudRemovePlanQuery, envOf,
  type CloudAction, type CloudEnvironment, type CloudItem,
} from '~/lib/cloud'
import { meQuery } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import { FS, INK } from '~/theme'
import { ChangeModal, reportChange, TagsChange, useSubmit } from './ChangeModal'
import { InstanceFirewalls } from './NetworkActions'
import { ImageCreate } from './StorageActions'

export { reportChange }

/*
 * What can be done to one instance, from its panel. Shown only to admins and
 * owners, and only on an account Argus may change. Every button asks the
 * control plane for a change; the control plane decides whether someone else
 * has to approve it first and says so in its answer.
 */


export function InstanceActions({ it }: { it: CloudItem }) {
  const { data: me } = useQuery(meQuery())
  const accounts = useQuery(cloudAccountsQuery())
  const pending = useQuery(cloudChangesQuery({ pending: true }))
  const [open, setOpen] = useState<CloudAction | null>(null)
  const account = accounts.data?.accounts.find((a) => a.id === it.accountId)
  if (!canSee(me?.role, 'admin') || !account) return null
  const r = it.resource
  const env = envOf(r.tags, it.environment)
  const waiting = pending.data?.find((c) => c.accountId === it.accountId && c.resourceId === r.id)

  if (account.access !== 'manage') {
    return (
      <Text size={FS.meta} c="dimmed">
        {account.name} is read only. Allow changes on the Cloud overview to start, stop, resize or remove instances from here.
      </Text>
    )
  }
  const gone = r.state === 'terminated' || r.state === 'shutting-down'
  const running = r.state === 'running'
  const stopped = r.state === 'stopped'
  const close = () => setOpen(null)

  return (
    <Stack gap="xs">
      {waiting && (
        <Alert color="amber" variant="light" p="xs" icon={<IconUserCheck size={16} />}>
          <Text size={FS.meta}><b>{waiting.summary}</b> is waiting for approval, asked by {waiting.requestedBy} {relTime(waiting.requestedAt)}.</Text>
        </Alert>
      )}
      <Group gap="xs">
        {running && <Button size="xs" variant="default" leftSection={<IconPlayerStop size={14} />} disabled={!!waiting} onClick={() => setOpen('stop')}>Stop</Button>}
        {stopped && <Button size="xs" variant="default" leftSection={<IconPlayerPlay size={14} />} disabled={!!waiting} onClick={() => setOpen('start')}>Start</Button>}
        {running && <Button size="xs" variant="default" leftSection={<IconRefresh size={14} />} disabled={!!waiting} onClick={() => setOpen('reboot')}>Reboot</Button>}
        {!gone && <Button size="xs" variant="default" leftSection={<IconResize size={14} />} disabled={!!waiting} onClick={() => setOpen('resize')}>Change size</Button>}
        {!gone && <Button size="xs" variant="default" leftSection={<IconTag size={14} />} disabled={!!waiting} onClick={() => setOpen('tags')}>Tags</Button>}
        {!gone && (
          <Menu position="bottom-end" withinPortal>
            <Menu.Target>
              <ActionIcon variant="default" size={30} aria-label="More actions" disabled={!!waiting}><IconDots size={15} /></ActionIcon>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Item leftSection={<IconLock size={14} />} onClick={() => setOpen('protect')}>Turn on termination protection</Menu.Item>
              <Menu.Item leftSection={<IconLockOpen size={14} />} onClick={() => setOpen('unprotect')}>Turn off termination protection</Menu.Item>
              <Menu.Item leftSection={<IconPhoto size={14} />} onClick={() => setOpen('image-create')}>Make an image…</Menu.Item>
              <Menu.Item leftSection={<IconShield size={14} />} onClick={() => setOpen('instance-firewalls')}>Change security groups…</Menu.Item>
              <Menu.Divider />
              <Menu.Item color="rose" leftSection={<IconTrash size={14} />} onClick={() => setOpen('remove')}>Remove instance…</Menu.Item>
            </Menu.Dropdown>
          </Menu>
        )}
      </Group>
      {(open === 'start' || open === 'stop' || open === 'reboot' || open === 'protect' || open === 'unprotect') && (
        <SimpleChange it={it} env={env} action={open} onClose={close} />
      )}
      {open === 'resize' && <ResizeChange it={it} env={env} onClose={close} />}
      {open === 'tags' && <TagsChange it={it} env={env} onClose={close} />}
      {open === 'remove' && <RemoveChange it={it} onClose={close} />}
      {open === 'image-create' && <ImageCreate it={it} env={env} onClose={close} />}
      {open === 'instance-firewalls' && <InstanceFirewalls it={it} env={env} onClose={close} />}
    </Stack>
  )
}

const SIMPLE: Record<string, { title: string; verb: string; done: string; text: string; color?: string }> = {
  start: { title: 'Start', verb: 'Start', done: 'Starting', text: 'AWS starts the instance. It keeps its disks; a public address that is not an Elastic IP changes.' },
  stop: { title: 'Stop', verb: 'Stop', done: 'Stopping', color: 'amber', text: 'AWS shuts the instance down. Its disks stay and are still billed; the instance is not. Anything only in memory is lost.' },
  reboot: { title: 'Reboot', verb: 'Reboot', done: 'Rebooting', color: 'amber', text: 'AWS restarts the instance. Anyone signed in to it is disconnected.' },
  protect: { title: 'Turn on termination protection for', verb: 'Turn on', done: 'Protected', text: 'Nobody can remove the instance, from Argus or AWS, until protection is turned off again.' },
  unprotect: { title: 'Turn off termination protection for', verb: 'Turn off', done: 'Protection off', color: 'amber', text: 'The instance can be removed again. Removing it from Argus still needs its name typed, and approval where its environment asks for it.' },
}

function SimpleChange({ it, env, action, onClose }: { it: CloudItem; env: CloudEnvironment; action: CloudAction; onClose: () => void }) {
  const s = SIMPLE[action]!
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  return (
    <ChangeModal title={`${s.title} ${it.resource.name}?`} env={env} level={ACTION_LEVEL[action]} reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit={s.verb} color={s.color}
      onSubmit={() => void submit({ accountId: it.accountId, region: it.resource.region, action, resourceId: it.resource.id, reason }, s.done)}>
      <Text size={FS.body} lh={1.55}>{s.text}</Text>
    </ChangeModal>
  )
}

function ResizeChange({ it, env, onClose }: { it: CloudItem; env: CloudEnvironment; onClose: () => void }) {
  const r = it.resource
  const opts = useQuery(cloudLaunchOptionsQuery(it.accountId, r.region))
  const [size, setSize] = useState<string | null>(null)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const stopped = r.state === 'stopped'
  return (
    <ChangeModal title={`Change the size of ${r.name}`} env={env} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Change size" disabled={!stopped || !size || size === r.size}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'resize', resourceId: r.id,
        spec: { size, from: r.size }, reason }, 'Size changed')}>
      {!stopped && (
        <Alert color="sky" variant="light" p="sm">
          <Text size={FS.meta}>AWS changes the size of a stopped instance only. Stop it first, then come back here.</Text>
        </Alert>
      )}
      <Group grow align="flex-end">
        <Field label="Now"><Text ff="monospace" size={FS.body} mt={6}>{r.size}</Text></Field>
        <Select label="New size" searchable placeholder={opts.isPending ? 'Loading…' : 'Pick a size'} value={size} onChange={setSize}
          data={(opts.data?.sizes ?? []).filter((x) => x !== r.size)} disabled={!stopped} nothingFoundMessage="No such size"
          styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
      </Group>
    </ChangeModal>
  )
}

/**
 * Removing an instance: what goes with it, read from AWS now, and the name
 * typed. The plan shown is the plan approved; if AWS has changed by the time
 * someone approves, the control plane sends it back for another look.
 */
function RemoveChange({ it, onClose }: { it: CloudItem; onClose: () => void }) {
  const r = it.resource
  const plan = useQuery(cloudRemovePlanQuery(it))
  const [typed, setTyped] = useState('')
  const [reason, setReason] = useState('')
  const [snaps, setSnaps] = useState<boolean | null>(null)
  const { submit, busy } = useSubmit(onClose)
  const v = plan.data
  const env = v?.environment ?? envOf(r.tags, it.environment)
  const finalSnaps = snaps ?? env === 'production'
  const goes = v?.plan.volumes.filter((x) => x.deleted) ?? []
  const stays = v?.plan.volumes.filter((x) => !x.deleted) ?? []
  return (
    <ChangeModal title={`Remove ${r.name}?`} env={env} level="remove" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Remove instance" color="rose" disabled={!v || Boolean(v.refusal) || typed.trim() !== r.name}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'remove', resourceId: r.id,
        spec: { finalSnapshots: finalSnaps }, reason, confirm: typed.trim() }, 'Removing')}>
      {plan.isPending && <Text size={FS.meta} c="dimmed">Asking AWS what goes with it…</Text>}
      {plan.error && <Alert color="rose" variant="light">{(plan.error as Error).message}</Alert>}
      {v?.refusal && (
        <Alert color="rose" variant="light" title="Argus will not remove it">
          <Text size={FS.meta}>It cannot be removed because {v.refusal}.</Text>
        </Alert>
      )}
      {v && !v.refusal && (
        <>
          <Text size={FS.body} lh={1.55}>
            AWS terminates the instance. This cannot be undone: the instance and the disks below that go with it are gone for good.
          </Text>
          <Table withTableBorder verticalSpacing={6}>
            <Table.Thead>
              <Table.Tr><Table.Th>Disk</Table.Th><Table.Th>Size</Table.Th><Table.Th>Last snapshot</Table.Th><Table.Th>After</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {[...goes, ...stays].map((x) => (
                <Table.Tr key={x.id}>
                  <Table.Td><Text size={FS.meta} ff="monospace">{x.id}</Text><Text size={FS.micro} c="dimmed">{x.device}</Text></Table.Td>
                  <Table.Td><Text size={FS.meta}>{x.sizeGb} GB</Text></Table.Td>
                  <Table.Td><Text size={FS.meta} c={x.lastSnapshotAt ? undefined : INK.pending}>{x.lastSnapshotAt ? relTime(x.lastSnapshotAt) : 'None'}</Text></Table.Td>
                  <Table.Td>{x.deleted
                    ? <Badge color="rose" variant="light" radius="sm">Deleted</Badge>
                    : <Badge color="slate" variant="light" radius="sm">Kept, still billed</Badge>}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
          {(v.plan.publicIp || v.plan.elasticIp) && (
            <Text size={FS.meta} c="dimmed">
              {v.plan.elasticIp ? `Elastic IP ${v.plan.elasticIp} is released from it and kept, still billed.` : `Public address ${v.plan.publicIp} goes with it.`}
            </Text>
          )}
          {v.server && (
            <Text size={FS.meta} c="dimmed">Argus launched it as <b>{v.server}</b>. That server is archived; its sessions and recordings stay.</Text>
          )}
          {goes.length > 0 && (
            <Checkbox checked={finalSnaps} onChange={(e) => setSnaps(e.currentTarget.checked)}
              label="Take a final snapshot of each disk first"
              description="Snapshots are kept and billed until someone deletes them in AWS. On by default in production." />
          )}
          <TextInput label={<>Type <Text span ff="monospace" fw={700}>{r.name}</Text> to confirm</>} value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)} data-autofocus />
        </>
      )}
      <Box />
    </ChangeModal>
  )
}
