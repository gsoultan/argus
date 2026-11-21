import { ActionIcon, Alert, Badge, Box, Button, Checkbox, Code, Group, Menu, Modal, ScrollArea, SegmentedControl, Select, Skeleton, Stack, Switch, Table, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconCopy, IconDots, IconFileText, IconLock, IconLockOpen, IconMoon, IconPhoto, IconSettings, IconShield, IconPlayerPlay, IconPlayerStop, IconRefresh, IconResize, IconTag, IconTrash, IconUserCheck, IconStack3 } from '@tabler/icons-react'
import { useState } from 'react'
import { Field, absTime, relTime } from '~/components/primitives'
import {
  ACTION_LEVEL, allows, cloudAccountsQuery, cloudChangesQuery, cloudConsoleQuery, cloudInstanceSettingsQuery, cloudInstanceSizesQuery, cloudLaunchOptionsQuery, cloudRemovePlanQuery, envOf,
  type CloudAction, type CloudEnvironment, type CloudInstanceConfig, type CloudItem,
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
  const navigate = useNavigate()
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
  const can = (a: CloudAction) => allows(account, a)
  const gcp = account.provider === 'gcp'
  const azure = account.provider === 'azure'
  // A VM shut down from inside it: Azure keeps it allocated and bills it
  // in full until it is deallocated, which is what Argus's stop does.
  const allocated = azure && stopped && r.details?.allocated === 'yes'
  const more = !gone && (['launch', 'scale-group-create', 'settings', 'protect', 'unprotect', 'image-create', 'instance-firewalls', 'remove'] as CloudAction[]).some(can)
  // Azure's is a resource lock on the VM, which Argus puts on and takes off.
  const protectWord = gcp ? 'deletion protection' : azure ? 'the delete lock' : 'termination protection'
  // Google's inventory says whether it is on, so only the way it can go is offered.
  // Azure's inventory says which lock is on the VM: Argus's own, somebody
  // else's, both or none. Argus takes off only its own, so that alone
  // decides which way is offered; when the locks could not be read, both are.
  const lock = azure ? r.details?.lock : undefined
  const protectedNow = gcp ? r.details?.deletionProtection === 'on'
    : lock ? lock === 'argus' || lock === 'both' : undefined
  const lockNote = azure ? lockSays(r.details) : ''

  return (
    <Stack gap="xs">
      {waiting && (
        <Alert color="amber" variant="light" p="xs" icon={<IconUserCheck size={16} />}>
          <Text size={FS.meta}><b>{waiting.summary}</b> is waiting for approval, asked by {waiting.requestedBy} {relTime(waiting.requestedAt)}.</Text>
        </Alert>
      )}
      {allocated && (
        <Alert color="amber" variant="light" p="xs" icon={<IconPlayerStop size={16} />}>
          <Text size={FS.meta}>{r.name} was shut down from inside the VM, so Azure keeps it allocated and still bills it in full.{can('stop') ? ' Deallocate it to stop the bill for the VM.' : ''}</Text>
        </Alert>
      )}
      {lockNote && (
        <Group gap={6} wrap="nowrap" align="flex-start">
          <IconLock size={14} color="var(--mantine-color-dimmed)" style={{ flexShrink: 0, marginTop: 2 }} />
          <Text size={FS.meta} c="dimmed">{lockNote}</Text>
        </Group>
      )}
      <Group gap="xs">
        {(running || allocated) && can('stop') && <Button size="xs" variant="default" leftSection={<IconPlayerStop size={14} />} disabled={!!waiting} onClick={() => setOpen('stop')}>{allocated ? 'Deallocate' : 'Stop'}</Button>}
        {stopped && can('start') && <Button size="xs" variant="default" leftSection={<IconPlayerPlay size={14} />} disabled={!!waiting} onClick={() => setOpen('start')}>Start</Button>}
        {running && can('hibernate') && it.resource.details?.hibernation === 'yes' && (
          <Button size="xs" variant="default" leftSection={<IconMoon size={14} />} disabled={!!waiting} onClick={() => setOpen('hibernate')}>Hibernate</Button>
        )}
        {running && can('reboot') && <Button size="xs" variant="default" leftSection={<IconRefresh size={14} />} disabled={!!waiting} onClick={() => setOpen('reboot')}>{gcp ? 'Reset' : azure ? 'Restart' : 'Reboot'}</Button>}
        {!gone && can('resize') && <Button size="xs" variant="default" leftSection={<IconResize size={14} />} disabled={!!waiting} onClick={() => setOpen('resize')}>{gcp ? 'Change machine type' : 'Change size'}</Button>}
        {!gone && can('tags') && <Button size="xs" variant="default" leftSection={<IconTag size={14} />} disabled={!!waiting} onClick={() => setOpen('tags')}>{gcp ? 'Labels' : 'Tags'}</Button>}
        {more && (
          <Menu position="bottom-end" withinPortal>
            <Menu.Target>
              <ActionIcon variant="default" size={30} aria-label="More actions" disabled={!!waiting}><IconDots size={15} /></ActionIcon>
            </Menu.Target>
            <Menu.Dropdown>
              {can('launch') && (
                <Menu.Item leftSection={<IconCopy size={14} />}
                  onClick={() => void navigate({ to: '/cloud/launch', search: { like: it.resource.id, account: it.accountId } })}>Launch another like this…</Menu.Item>
              )}
              {running && can('scale-group-create') && (
                <Menu.Item leftSection={<IconStack3 size={14} />}
                  onClick={() => void navigate({ to: '/cloud/scale/new', search: { account: it.accountId, region: it.resource.region, from: it.resource.id } })}>
                  Put in a scale group…
                </Menu.Item>
              )}
              {can('settings') && <Menu.Item leftSection={<IconSettings size={14} />} onClick={() => setOpen('settings')}>Instance settings…</Menu.Item>}
              {can('protect') && protectedNow !== true && <Menu.Item leftSection={<IconLock size={14} />} onClick={() => setOpen('protect')}>Turn on {protectWord}</Menu.Item>}
              {can('unprotect') && protectedNow !== false && <Menu.Item leftSection={<IconLockOpen size={14} />} onClick={() => setOpen('unprotect')}>Turn off {protectWord}</Menu.Item>}
              {can('image-create') && <Menu.Item leftSection={<IconPhoto size={14} />} onClick={() => setOpen('image-create')}>Make an image…</Menu.Item>}
              {can('instance-firewalls') && <Menu.Item leftSection={<IconShield size={14} />} onClick={() => setOpen('instance-firewalls')}>{gcp ? 'Change firewall rules…' : azure ? 'Change network security group…' : 'Change security groups…'}</Menu.Item>}
              {can('remove') && (
                <>
                  <Menu.Divider />
                  <Menu.Item color="rose" leftSection={<IconTrash size={14} />} onClick={() => setOpen('remove')}>{gcp || azure ? 'Remove VM…' : 'Remove instance…'}</Menu.Item>
                </>
              )}
            </Menu.Dropdown>
          </Menu>
        )}
      </Group>
      {(open === 'start' || open === 'stop' || open === 'hibernate' || open === 'reboot' || open === 'protect' || open === 'unprotect') && (
        <SimpleChange it={it} env={env} action={open} gcp={gcp} azure={azure} onClose={close} />
      )}
      {open === 'resize' && <ResizeChange it={it} env={env} gcp={gcp} azure={azure} onClose={close} />}
      {open === 'settings' && <SettingsChange it={it} env={env} onClose={close} />}
      {open === 'tags' && <TagsChange it={it} env={env} gcp={gcp} onClose={close} />}
      {open === 'remove' && <RemoveChange it={it} gcp={gcp} azure={azure} onClose={close} />}
      {open === 'image-create' && <ImageCreate it={it} env={env} onClose={close} />}
      {open === 'instance-firewalls' && <InstanceFirewalls it={it} env={env} onClose={close} />}
    </Stack>
  )
}

/** What an Azure VM's details say of the locks that stop it being deleted. */
function lockSays(d?: Record<string, string>): string {
  const names = d?.lockNames
  const parts: string[] = []
  if (d?.lock === 'argus') parts.push('The delete lock is on: nobody can delete this VM until it is off.')
  if (d?.lock === 'both') parts.push(`The delete lock is on, and so is ${names} from Azure, which comes off in the Azure portal.`)
  if (d?.lock === 'other') parts.push(`Locked in Azure by ${names}, which Argus did not put on; it comes off in the Azure portal.`)
  if (d?.lockAbove) parts.push(`A lock on its ${d.lockAbove} also stops it being deleted.`)
  return parts.join(' ')
}

const CHECK_WORD: Record<string, { label: string; color: string }> = {
  ok: { label: 'Passing', color: 'teal' }, impaired: { label: 'Failing', color: 'rose' },
  initializing: { label: 'Starting', color: 'slate' }, 'insufficient-data': { label: 'Not enough data', color: 'slate' },
}

/**
 * How AWS sees the instance: its two status checks (AWS's hardware and
 * network, and the instance itself), maintenance AWS has planned, and the
 * boot log for one that does not answer. Read with the last inventory, so
 * it is as old as "Last read" says.
 */
export function InstanceHealth({ it }: { it: CloudItem }) {
  const { data: me } = useQuery(meQuery())
  const accounts = useQuery(cloudAccountsQuery())
  const [log, setLog] = useState(false)
  const d = it.resource.details ?? {}
  const account = accounts.data?.accounts.find((a) => a.id === it.accountId)
  // Google's serial console is not read through Argus yet.
  const canRead = canSee(me?.role, 'admin') && account?.access === 'manage' && account.provider !== 'gcp' && it.resource.state !== 'terminated'
  const sys = d.systemStatus ? CHECK_WORD[d.systemStatus] : undefined
  const own = d.instanceStatus ? CHECK_WORD[d.instanceStatus] : undefined
  if (!sys && !own && !d.eventCode && !canRead) return null
  return (
    <Stack gap="xs" data-testid="instance-health">
      {(sys || own) && (
        <Group gap="lg">
          {sys && <Field label="AWS hardware and network"><Badge variant="light" color={sys.color} radius="sm">{sys.label}</Badge></Field>}
          {own && <Field label="The instance itself"><Badge variant="light" color={own.color} radius="sm">{own.label}</Badge></Field>}
        </Group>
      )}
      {d.eventCode && (
        <Alert color="amber" variant="light" p="xs" icon={<IconAlertTriangle size={16} />}>
          <Text size={FS.meta}>
            <b>AWS has planned maintenance{d.eventNotBefore ? ` from ${absTime(d.eventNotBefore)}` : ''}.</b> {d.eventDescription}.
            Stopping and starting it yourself at a quiet time before then usually clears it.
          </Text>
        </Alert>
      )}
      {canRead && (
        <Button size="xs" variant="default" w="fit-content" leftSection={<IconFileText size={14} />} onClick={() => setLog(true)}>Boot log</Button>
      )}
      {log && <BootLog it={it} onClose={() => setLog(false)} />}
    </Stack>
  )
}

function BootLog({ it, onClose }: { it: CloudItem; onClose: () => void }) {
  const q = useQuery(cloudConsoleQuery(it, true))
  return (
    <Modal opened onClose={onClose} centered size="xl" title={<Text fw={700}>Boot log of {it.resource.name}</Text>}>
      <Stack gap="sm">
        <Text size={FS.meta} c="dimmed">
          What the instance last printed to its console, as AWS keeps it{q.data?.at ? `, taken ${relTime(q.data.at)}` : ''}.
          AWS updates it a few minutes after the instance writes. Reading it is recorded in the audit log.
        </Text>
        {q.isPending && <Skeleton h={240} />}
        {q.error && <Alert color="rose" variant="light">{(q.error as Error).message}</Alert>}
        {q.data && (q.data.text.trim() === ''
          ? <Text size={FS.meta} c="dimmed">AWS has no console output for it yet. A Windows instance, or one started moments ago, often has none.</Text>
          : (
            <ScrollArea.Autosize mah={420} type="auto" data-testid="boot-log">
              <Code block fz={FS.micro} style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{q.data.text}</Code>
            </ScrollArea.Autosize>
          ))}
      </Stack>
    </Modal>
  )
}

const SIMPLE: Record<string, { title: string; verb: string; done: string; text: string; color?: string }> = {
  start: { title: 'Start', verb: 'Start', done: 'Starting', text: 'AWS starts the instance. It keeps its disks; a public address that is not an Elastic IP changes.' },
  stop: { title: 'Stop', verb: 'Stop', done: 'Stopping', color: 'amber', text: 'AWS shuts the instance down. Its disks stay and are still billed; the instance is not. Anything only in memory is lost.' },
  hibernate: { title: 'Hibernate', verb: 'Hibernate', done: 'Hibernating', color: 'amber', text: 'AWS saves what is in memory to the root disk and stops the instance. Starting it again picks up where it left off. The instance is not billed while hibernated; its disks are.' },
  reboot: { title: 'Reboot', verb: 'Reboot', done: 'Rebooting', color: 'amber', text: 'AWS restarts the instance. Anyone signed in to it is disconnected.' },
  protect: { title: 'Turn on termination protection for', verb: 'Turn on', done: 'Protected', text: 'Nobody can remove the instance, from Argus or AWS, until protection is turned off again.' },
  unprotect: { title: 'Turn off termination protection for', verb: 'Turn off', done: 'Protection off', color: 'amber', text: 'The instance can be removed again. Removing it from Argus still needs its name typed, and approval where its environment asks for it.' },
}

/** What Google Cloud does instead, where it differs from AWS. */
const GCP_SIMPLE: Record<string, { title: string; verb: string; done: string; text: string; color?: string }> = {
  start: { title: 'Start', verb: 'Start', done: 'Starting', text: 'Google starts the VM. It keeps its disks; an ephemeral external address changes, a reserved one does not.' },
  stop: { title: 'Stop', verb: 'Stop', done: 'Stopping', color: 'amber', text: 'Google shuts the VM down. Its disks stay and are still billed; the VM is not. Anything only in memory is lost.' },
  reboot: { title: 'Reset', verb: 'Reset', done: 'Resetting', color: 'amber', text: 'Google Cloud has no gentle reboot: a reset is like pressing the power button. The operating system is not asked to shut down first, so unsaved work in memory is lost and anyone signed in is disconnected.' },
  protect: { title: 'Turn on deletion protection for', verb: 'Turn on', done: 'Protected', text: 'Nobody can delete the VM, from Argus or Google Cloud, until protection is turned off again. It can still be stopped and reset.' },
  unprotect: { title: 'Turn off deletion protection for', verb: 'Turn off', done: 'Protection off', color: 'amber', text: 'The VM can be deleted again by anyone allowed to in the project.' },
}

/** What Azure does instead, where it differs from AWS. */
const AZURE_SIMPLE: Record<string, { title: string; verb: string; done: string; text: string; color?: string }> = {
  start: { title: 'Start', verb: 'Start', done: 'Starting', text: 'Azure starts the VM. It keeps its disks; a public address that is not static changes.' },
  stop: { title: 'Stop', verb: 'Stop', done: 'Stopping', color: 'amber', text: 'Azure shuts the VM down and deallocates it, as Stop in the portal does, so the VM is no longer billed. Its disks stay and are still billed. Anything only in memory is lost, and a public address that is not static is given up.' },
  reboot: { title: 'Restart', verb: 'Restart', done: 'Restarting', color: 'amber', text: 'Azure restarts the VM. Anyone signed in to it is disconnected.' },
  protect: { title: 'Put a delete lock on', verb: 'Lock', done: 'Locked', text: 'Argus puts a resource lock on the VM in Azure. Nobody can delete the VM, from Argus or Azure, until the lock is off again. It can still be stopped, restarted, resized and tagged.' },
  unprotect: { title: 'Take the delete lock off', verb: 'Unlock', done: 'Lock off', color: 'amber', text: 'Argus takes off the lock it put on, and the VM can be deleted again. A lock somebody put on in the Azure portal stays, and comes off there.' },
}

function SimpleChange({ it, env, action, gcp, azure, onClose }: { it: CloudItem; env: CloudEnvironment; action: CloudAction; gcp?: boolean; azure?: boolean; onClose: () => void }) {
  const s = (gcp && GCP_SIMPLE[action]) || (azure && AZURE_SIMPLE[action]) || SIMPLE[action]!
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

type SettingsDraft = { stopProtect?: boolean; requireTokens?: boolean; creditMode?: CloudInstanceConfig['creditMode'] }

/**
 * Stop protection, the metadata token setting and a burstable instance's
 * credit mode, read from AWS when the dialog opens. Only what differs from
 * now is sent, so the change (and its approver) names exactly that.
 */
export function SettingsChange({ it, env, onClose, preset }: {
  it: CloudItem; env: CloudEnvironment; onClose: () => void; preset?: SettingsDraft
}) {
  const r = it.resource
  const cur = useQuery(cloudInstanceSettingsQuery(it))
  const [draft, setDraft] = useState<SettingsDraft>(preset ?? {})
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const c = cur.data
  const val = <K extends keyof SettingsDraft>(k: K): NonNullable<SettingsDraft[K]> | undefined =>
    (draft[k] ?? c?.[k]) as NonNullable<SettingsDraft[K]> | undefined
  const spec: SettingsDraft = {}
  if (c) {
    if (draft.stopProtect !== undefined && draft.stopProtect !== c.stopProtect) spec.stopProtect = draft.stopProtect
    if (draft.requireTokens !== undefined && draft.requireTokens !== c.requireTokens) spec.requireTokens = draft.requireTokens
    if (draft.creditMode && draft.creditMode !== c.creditMode) spec.creditMode = draft.creditMode
  }
  const changes = Object.keys(spec).length
  return (
    <ChangeModal title={`Settings of ${r.name}`} env={env} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit={changes ? `Save ${changes} change${changes === 1 ? '' : 's'}` : 'Save'} disabled={!c || changes === 0}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'settings', resourceId: r.id, spec, reason }, 'Settings saved')}>
      {cur.isPending && <Skeleton h={120} />}
      {cur.error && <Alert color="rose" variant="light">{(cur.error as Error).message}</Alert>}
      {c && (
        <Stack gap="md" data-testid="instance-settings">
          <Switch checked={!!val('stopProtect')} onChange={(e) => setDraft({ ...draft, stopProtect: e.currentTarget.checked })}
            label="Stop protection"
            description="Nobody can stop or hibernate it, from Argus, its schedules or AWS, until this is turned off. Rebooting still works." />
          <Switch checked={!!val('requireTokens')} onChange={(e) => setDraft({ ...draft, requireTokens: e.currentTarget.checked })}
            label="Require tokens for instance metadata (IMDSv2)"
            description="A request forged through software on the server cannot read its role's keys. Software older than about 2020 that reads metadata without a token stops getting answers; check before turning it on." />
          {c.creditMode ? (
            <Stack gap={4}>
              <Text size={FS.body} fw={500}>Processor credits</Text>
              <SegmentedControl size="xs" w="fit-content" value={val('creditMode') ?? 'standard'}
                onChange={(v) => setDraft({ ...draft, creditMode: v as CloudInstanceConfig['creditMode'] })}
                data={[{ value: 'standard', label: 'Standard' }, { value: 'unlimited', label: 'Unlimited' }]} />
              <Text size={FS.meta} c="dimmed">
                {r.size} is burstable. Standard slows it to its baseline when its credits run out. Unlimited keeps it fast and bills the extra processor time.
              </Text>
            </Stack>
          ) : null}
          <Text size={FS.meta} c="dimmed">
            Termination protection is {c.terminationProtect ? 'on' : 'off'}; change it from the menu.
            {c.hibernation ? ' It was launched able to hibernate.' : ' It was not launched able to hibernate, which only a new launch can change.'}
          </Text>
        </Stack>
      )}
    </ChangeModal>
  )
}

/** Changing an instance's size; a suggestion opens it with the size it names. */
export function ResizeChange({ it, env, initial, gcp, azure, onClose }: { it: CloudItem; env: CloudEnvironment; initial?: string; gcp?: boolean; azure?: boolean; onClose: () => void }) {
  const r = it.resource
  // Google's machine types depend on the VM's zone and Azure's sizes on
  // where the VM runs, so they are read for the VM itself.
  const own = Boolean(gcp || azure)
  const opts = useQuery({ ...cloudLaunchOptionsQuery(it.accountId, r.region), enabled: !own })
  const zoneSizes = useQuery({ ...cloudInstanceSizesQuery(it), enabled: own })
  const sizes = own ? zoneSizes.data?.sizes : opts.data?.sizes
  const loading = own ? zoneSizes.isPending : opts.isPending
  const [size, setSize] = useState<string | null>(initial ?? null)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const stopped = r.state === 'stopped'
  const noun = gcp ? 'machine type' : 'size'
  return (
    <ChangeModal title={`Change the ${noun} of ${r.name}`} env={env} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit={gcp ? 'Change machine type' : 'Change size'} disabled={!stopped || !size || size === r.size}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'resize', resourceId: r.id,
        spec: { size, from: r.size }, reason }, gcp ? 'Machine type changed' : 'Size changed')}>
      {!stopped && (
        <Alert color="sky" variant="light" p="sm">
          <Text size={FS.meta}>
            {gcp ? 'Google changes the machine type of a stopped VM only. Stop it first, then come back here.'
              : azure ? 'Argus changes the size of a stopped VM only: Azure would restart a running one to resize it. Stop it first, then come back here.'
              : 'AWS changes the size of a stopped instance only. Stop it first, then come back here.'}
          </Text>
        </Alert>
      )}
      {own && zoneSizes.error && <Alert color="rose" variant="light" p="sm">{(zoneSizes.error as Error).message}</Alert>}
      <Group grow align="flex-end">
        <Field label="Now"><Text ff="monospace" size={FS.body} mt={6}>{r.size}</Text></Field>
        <Select label={gcp ? 'New machine type' : 'New size'} searchable placeholder={loading ? 'Loading…' : gcp ? 'Pick a machine type' : 'Pick a size'} value={size} onChange={setSize}
          data={[...new Set([...(sizes ?? []), ...(initial ? [initial] : [])])].filter((x) => x !== r.size)} disabled={!stopped} nothingFoundMessage={gcp ? `No such machine type in ${r.zone ?? 'its zone'}` : azure ? 'Azure does not offer this VM that size where it is' : 'No such size'}
          limit={own ? 200 : undefined}
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
export function RemoveChange({ it, gcp, azure, onClose }: { it: CloudItem; gcp: boolean; azure?: boolean; onClose: () => void }) {
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
      onClose={onClose} busy={busy} submit={gcp || azure ? 'Remove VM' : 'Remove instance'} color="rose" disabled={!v || Boolean(v.refusal) || typed.trim() !== r.name}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'remove', resourceId: r.id,
        spec: { finalSnapshots: finalSnaps }, reason, confirm: typed.trim() }, 'Removing')}>
      {plan.isPending && <Text size={FS.meta} c="dimmed">Asking {gcp ? 'Google' : azure ? 'Azure' : 'AWS'} what goes with it…</Text>}
      {plan.error && <Alert color="rose" variant="light">{(plan.error as Error).message}</Alert>}
      {v?.refusal && (
        <Alert color="rose" variant="light" title="Argus will not remove it">
          <Text size={FS.meta}>It cannot be removed because {v.refusal}.</Text>
        </Alert>
      )}
      {v && !v.refusal && (
        <>
          <Text size={FS.body} lh={1.55}>
            {gcp
              ? 'Google deletes the VM. This cannot be undone: the VM and the disks below that go with it are gone for good.'
              : azure ? 'Azure deletes the VM. This cannot be undone: the VM and the disks below that go with it are gone for good. Azure deletes with a VM only what was marked to go with it when it was made; the rest stays, as listed.'
              : 'AWS terminates the instance. This cannot be undone: the instance and the disks below that go with it are gone for good.'}
          </Text>
          <Table withTableBorder verticalSpacing={6}>
            <Table.Thead>
              <Table.Tr><Table.Th>Disk</Table.Th><Table.Th>Size</Table.Th><Table.Th>Last snapshot</Table.Th><Table.Th>After</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {[...goes, ...stays].map((x) => (
                <Table.Tr key={x.id}>
                  <Table.Td><Text size={FS.meta} ff="monospace">{x.id}</Text><Text size={FS.micro} c="dimmed">{(gcp || azure) && x.description ? x.description : x.device}</Text></Table.Td>
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
              {v.plan.elasticIp
                ? gcp ? `Static address ${v.plan.elasticIp} is let go and stays reserved, still billed.` : azure ? `Public address ${v.plan.elasticIp} is taken off it and stays in the subscription, still billed.` : `Elastic IP ${v.plan.elasticIp} is released from it and kept, still billed.`
                : `Public address ${v.plan.publicIp} goes with it.`}
            </Text>
          )}
          {(v.plan.stays?.length ?? 0) > 0 && (
            <Text size={FS.meta} c="dimmed">Also left behind, at no charge: {v.plan.stays!.join(', ')}.</Text>
          )}
          {v.server && (
            <Text size={FS.meta} c="dimmed">Argus launched it as <b>{v.server}</b>. That server is archived; its sessions and recordings stay.</Text>
          )}
          {goes.length > 0 && (
            <Checkbox checked={finalSnaps} onChange={(e) => setSnaps(e.currentTarget.checked)}
              label="Take a final snapshot of each disk first"
              description={gcp || azure
                ? 'Argus waits for each snapshot to finish before deleting anything, which can take a few minutes. They are kept and billed until someone deletes them. On by default in production.'
                : 'Snapshots are kept and billed until someone deletes them in AWS. On by default in production.'} />
          )}
          <TextInput label={<>Type <Text span ff="monospace" fw={700}>{r.name}</Text> to confirm</>} value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)} data-autofocus />
        </>
      )}
      <Box />
    </ChangeModal>
  )
}
