import {
  ActionIcon, Alert, Autocomplete, Badge, Button, Code, Group, Menu, MultiSelect, NumberInput, Select, Stack, Text, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconArrowBackUp, IconDots, IconPlugConnected, IconPlugConnectedX, IconPlus, IconShield, IconTag, IconTrash, IconUserCheck, IconWorld, IconX,
} from '@tabler/icons-react'
import { useState } from 'react'
import { Field, relTime } from '~/components/primitives'
import {
  cloudAccountsQuery, cloudItemsQuery, envOf, exposures, firewallEnv, firewallUsers, normalSource, portText, roleLacks, sameRule,
  stricterEnv, targetsOf, validSource, type CloudAccount, type CloudEnvironment, type CloudItem, type CloudRule,
} from '~/lib/cloud'
import { FS, INK, SP } from '~/theme'
import { ChangeModal, TagsChange, useSubmit } from './ChangeModal'
import { Gate, RoleNeedsUpdate, useChangeable } from './StorageActions'

/*
 * Security groups and load balancer targets, from their panels: editing a
 * group's rules as a diff, making and deleting groups, changing an
 * instance's groups, and putting instances behind or taking them out of a
 * target group. Anything that opens more than a web port to the whole
 * internet is said in red, and the control plane makes it need someone
 * else's approval on staging as well as production.
 */

/** Common services, so most rules are one pick. */
const SERVICES: { value: string; label: string; protocol: string; port?: number }[] = [
  { value: 'https', label: 'HTTPS · tcp 443', protocol: 'tcp', port: 443 },
  { value: 'http', label: 'HTTP · tcp 80', protocol: 'tcp', port: 80 },
  { value: 'ssh', label: 'SSH · tcp 22', protocol: 'tcp', port: 22 },
  { value: 'rdp', label: 'Remote Desktop · tcp 3389', protocol: 'tcp', port: 3389 },
  { value: 'postgres', label: 'PostgreSQL · tcp 5432', protocol: 'tcp', port: 5432 },
  { value: 'mysql', label: 'MySQL · tcp 3306', protocol: 'tcp', port: 3306 },
  { value: 'redis', label: 'Redis · tcp 6379', protocol: 'tcp', port: 6379 },
  { value: 'tcp', label: 'Custom TCP', protocol: 'tcp' },
  { value: 'udp', label: 'Custom UDP', protocol: 'udp' },
  { value: 'icmp', label: 'Ping (ICMP)', protocol: 'icmp' },
  { value: 'all', label: 'All traffic', protocol: 'all' },
]

interface Draft { inbound: boolean; service: string; from: number | ''; to: number | ''; source: string; description: string }

const newDraft = (inbound: boolean): Draft => ({ inbound, service: 'https', from: '', to: '', source: '', description: '' })

/** A draft as a rule, or null while it is not one yet. */
function ruleOf(d: Draft): CloudRule | null {
  const svc = SERVICES.find((s) => s.value === d.service)
  if (!svc) return null
  const source = normalSource(d.source.split(' · ')[0] ?? '')
  if (!validSource(source)) return null
  let from = 0
  let to = 65535
  if (svc.protocol === 'icmp') { from = -1; to = -1 }
  if (svc.protocol === 'tcp' || svc.protocol === 'udp') {
    from = svc.port ?? (d.from === '' ? NaN : d.from)
    to = svc.port ?? (d.to === '' ? from : d.to)
    if (!Number.isInteger(from) || !Number.isInteger(to) || from < 0 || to > 65535 || from > to) return null
  }
  return { inbound: d.inbound, protocol: svc.protocol, fromPort: from, toPort: to, source, description: d.description.trim() || undefined }
}

function ruleText(r: CloudRule, names: Map<string, string>): string {
  const anywhere = r.source === '0.0.0.0/0' || r.source === '::/0'
  const src = anywhere ? 'anywhere' : names.get(r.source) ? `${names.get(r.source)} (${r.source})` : r.source
  return `${portText(r)} ${r.inbound ? 'from' : 'to'} ${src}`
}

/** The groups a source can name, in the same account and region. */
function useSources(accountId: string, region: string) {
  const groups = useQuery(cloudItemsQuery('firewall'))
  const here = (groups.data ?? []).filter((g) => g.accountId === accountId && g.resource.region === region)
  const names = new Map(here.map((g) => [g.resource.id, g.resource.name]))
  return { options: ['0.0.0.0/0', '::/0', ...here.map((g) => `${g.resource.id} · ${g.resource.name}`)], names }
}

function DraftRow({ d, set, drop, options }: { d: Draft; set: (d: Draft) => void; drop: () => void; options: string[] }) {
  const svc = SERVICES.find((s) => s.value === d.service)
  const custom = svc && !svc.port && (svc.protocol === 'tcp' || svc.protocol === 'udp')
  const r = ruleOf(d)
  const bad = d.source.trim() !== '' && !validSource(normalSource(d.source.split(' · ')[0] ?? ''))
  return (
    <Stack gap={4}>
      <Group gap="xs" wrap="nowrap" align="flex-start">
        <Select aria-label="Service" value={d.service} onChange={(v) => v && set({ ...d, service: v })} data={SERVICES} allowDeselect={false} w={190} />
        {custom && (
          <>
            <NumberInput aria-label="From port" placeholder="Port" value={d.from} onChange={(v) => set({ ...d, from: v === '' ? '' : Number(v) })} min={0} max={65535} w={86} hideControls />
            <NumberInput aria-label="To port" placeholder="to" value={d.to} onChange={(v) => set({ ...d, to: v === '' ? '' : Number(v) })} min={0} max={65535} w={78} hideControls />
          </>
        )}
        <Autocomplete aria-label={d.inbound ? 'From' : 'To'} placeholder={d.inbound ? 'From: 10.0.0.0/16, an address or a group' : 'To: a range or a group'}
          value={d.source} onChange={(v) => set({ ...d, source: v })} data={options} style={{ flex: 1 }} error={bad ? 'Not a range, address or group' : undefined} />
        <ActionIcon variant="subtle" color="gray" aria-label="Drop this rule" onClick={drop} mt={6}><IconX size={14} /></ActionIcon>
      </Group>
      <TextInput aria-label="Description" placeholder="Description, optional" size="xs" value={d.description}
        onChange={(e) => set({ ...d, description: e.currentTarget.value })} maxLength={255} />
      {r && r.inbound && exposures([r]).length > 0 && (
        <Text size={FS.micro} c={INK.denied}>Lets the whole internet reach {exposures([r])[0]}.</Text>
      )}
    </Stack>
  )
}

/** What a change opens to the internet, said plainly. */
function ExposureAlert({ exposes, level }: { exposes: string[]; level?: boolean }) {
  if (exposes.length === 0) return null
  return (
    <Alert color="rose" variant="light" icon={<IconWorld size={16} />} p="sm" title="Open to the whole internet">
      <Text size={FS.meta} lh={1.5}>
        This lets anyone on the internet reach {exposes.join(', ')}. Bots find open ports within minutes. Prefer a range you
        trust, or a load balancer in front.{level && ' Because of this, someone else approves it on staging as well as production.'}
      </Text>
    </Alert>
  )
}

export function FirewallActions({ it }: { it: CloudItem }) {
  const { admin, account, waiting } = useChangeable(it)
  const instances = useQuery(cloudItemsQuery('instance'))
  const balancers = useQuery(cloudItemsQuery('load-balancer'))
  const [open, setOpen] = useState<'rules' | 'tags' | 'delete' | null>(null)
  if (!admin) return null
  const users = firewallUsers(it, instances.data, balancers.data)
  const env = firewallEnv(it, users)
  const close = () => setOpen(null)
  return (
    <Gate it={it} account={account}>
      <Stack gap="xs">
        {waiting && (
          <Alert color="amber" variant="light" p="xs" icon={<IconUserCheck size={16} />}>
            <Text size={FS.meta}><b>{waiting.summary}</b> is waiting for approval, asked by {waiting.requestedBy} {relTime(waiting.requestedAt)}.</Text>
          </Alert>
        )}
        <Group gap="xs">
          <Button size="xs" variant="default" leftSection={<IconShield size={14} />} disabled={Boolean(waiting)} onClick={() => setOpen('rules')}>Edit rules</Button>
          <Button size="xs" variant="default" leftSection={<IconTag size={14} />} disabled={Boolean(waiting)} onClick={() => setOpen('tags')}>Tags</Button>
          <Menu position="bottom-end" withinPortal>
            <Menu.Target>
              <ActionIcon variant="default" size={30} aria-label="More actions" disabled={Boolean(waiting)}><IconDots size={15} /></ActionIcon>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Item color="rose" leftSection={<IconTrash size={14} />} onClick={() => setOpen('delete')}>Delete security group…</Menu.Item>
            </Menu.Dropdown>
          </Menu>
        </Group>
        <Text size={FS.meta} c="dimmed">
          {users.length === 0 ? 'Nothing uses this group.' : `Used by ${users.map((u) => u.resource.name).join(', ')}.`}
          {users.length > 0 && env !== envOf(it.resource.tags, it.environment) && ` Changes to it are ${env} changes, as the strictest of them.`}
        </Text>
        {open === 'rules' && <RulesEditor it={it} env={env} users={users} account={account} onClose={close} />}
        {open === 'tags' && <TagsChange it={it} env={env} onClose={close} />}
        {open === 'delete' && <FirewallDelete it={it} env={env} users={users} account={account} onClose={close} />}
      </Stack>
    </Gate>
  )
}

/** A group's rules, edited as a diff: what is taken away and what is added. */
function RulesEditor({ it, env, users, account, onClose }: {
  it: CloudItem; env: CloudEnvironment; users: CloudItem[]; account?: CloudAccount; onClose: () => void
}) {
  const r = it.resource
  const rules = r.rules ?? []
  const { options, names } = useSources(it.accountId, r.region)
  const [removed, setRemoved] = useState<Set<number>>(new Set())
  const [drafts, setDrafts] = useState<Draft[]>([])
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const add = drafts.map(ruleOf)
  const ready = add.every(Boolean)
  const adding = add.filter((x): x is CloudRule => x !== null)
  const remove = rules.filter((_, i) => removed.has(i))
  const dup = adding.some((x, i) => adding.slice(0, i).some((y) => sameRule(x, y)) ||
    rules.some((y, j) => !removed.has(j) && sameRule(x, y)))
  const exposes = exposures(adding)
  const changes = adding.length + remove.length
  const toggle = (i: number) => {
    const next = new Set(removed)
    if (next.has(i)) next.delete(i)
    else next.add(i)
    setRemoved(next)
  }
  const section = (inbound: boolean) => (
    <Field label={inbound ? 'Inbound rules' : 'Outbound rules'}>
      <Stack gap={SP.tight} mt={4}>
        {rules.map((x, i) => x.inbound !== inbound ? null : (
          <Group key={i} gap="xs" wrap="nowrap" justify="space-between">
            <Text size={FS.meta} ff="monospace" td={removed.has(i) ? 'line-through' : undefined}
              c={removed.has(i) ? 'dimmed' : x.inbound && exposures([x]).length ? INK.denied : undefined} style={{ overflowWrap: 'anywhere' }}>
              {ruleText(x, names)}{x.description ? ` · ${x.description}` : ''}
            </Text>
            <ActionIcon variant="subtle" color={removed.has(i) ? 'gray' : 'rose'} aria-label={removed.has(i) ? 'Keep this rule' : 'Take this rule away'} onClick={() => toggle(i)}>
              {removed.has(i) ? <IconArrowBackUp size={14} /> : <IconTrash size={14} />}
            </ActionIcon>
          </Group>
        ))}
        {drafts.map((d, i) => d.inbound !== inbound ? null : (
          <DraftRow key={`d${i}`} d={d} options={options} set={(n) => setDrafts(drafts.map((x, j) => (j === i ? n : x)))}
            drop={() => setDrafts(drafts.filter((_, j) => j !== i))} />
        ))}
        <Button variant="subtle" size="xs" w="fit-content" leftSection={<IconPlus size={13} />} onClick={() => setDrafts([...drafts, newDraft(inbound)])}>
          Add {inbound ? 'an inbound' : 'an outbound'} rule
        </Button>
      </Stack>
    </Field>
  )
  return (
    <ChangeModal title={`Rules of ${r.name}`} env={env} level={exposes.length ? 'remove' : 'change'} reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit={changes ? `Save ${changes} change${changes === 1 ? '' : 's'}` : 'Save'}
      disabled={changes === 0 || !ready || dup || roleLacks(account, 'firewall-rules')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'firewall-rules', resourceId: r.id,
        spec: { add: adding, remove }, reason }, 'Rules changed')}>
      <RoleNeedsUpdate account={account} action="firewall-rules" />
      {section(true)}
      {section(false)}
      {changes > 0 && (
        <Field label="What changes">
          <Stack gap={2} mt={4}>
            {remove.map((x, i) => <Code key={`r${i}`} fz={FS.meta} c="dimmed">− {ruleText(x, names)}</Code>)}
            {adding.map((x, i) => <Code key={`a${i}`} fz={FS.meta} c={exposures([x]).length ? INK.denied : undefined}>+ {ruleText(x, names)}</Code>)}
          </Stack>
        </Field>
      )}
      {dup && <Text size={FS.meta} c={INK.denied}>A rule being added is already in the group, or added twice.</Text>}
      <ExposureAlert exposes={exposes} level />
      {users.length > 0 && changes > 0 && (
        <Text size={FS.meta} c="dimmed">
          Applies at once to {users.length} instance{users.length === 1 ? '' : 's'} or load balancer{users.length === 1 ? '' : 's'} using it.
          Rules are taken away before new ones are added, so the group is never briefly open to both.
        </Text>
      )}
    </ChangeModal>
  )
}

function FirewallDelete({ it, env, users, account, onClose }: {
  it: CloudItem; env: CloudEnvironment; users: CloudItem[]; account?: CloudAccount; onClose: () => void
}) {
  const r = it.resource
  const groups = useQuery(cloudItemsQuery('firewall'))
  const [typed, setTyped] = useState('')
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const naming = (groups.data ?? []).find((g) => g.accountId === it.accountId && g.resource.id !== r.id &&
    (g.resource.rules ?? []).some((x) => x.source === r.id))
  const stack = r.tags?.['aws:cloudformation:stack-name']
  const refusal = r.name === 'default' ? 'every network keeps its default group'
    : users.length ? `${users.map((u) => u.resource.name).join(', ')} use${users.length === 1 ? 's' : ''} it; take it off them first`
      : naming ? `a rule in ${naming.resource.name} names it; remove that rule first`
        : stack ? `it belongs to CloudFormation stack ${stack}` : ''
  return (
    <ChangeModal title={`Delete ${r.name}?`} env={env} level="remove" reason={reason} setReason={setReason} onClose={onClose} busy={busy}
      submit="Delete security group" color="rose" disabled={Boolean(refusal) || (typed.trim() !== r.name && typed.trim() !== r.id) || roleLacks(account, 'firewall-delete')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'firewall-delete', resourceId: r.id, confirm: typed.trim(), reason }, 'Deleted')}>
      <RoleNeedsUpdate account={account} action="firewall-delete" />
      {refusal ? (
        <Alert color="rose" variant="light" title="Argus will not do this"><Text size={FS.meta}>It cannot be deleted because {refusal}.</Text></Alert>
      ) : (
        <>
          <Text size={FS.body} lh={1.55}>AWS deletes the group and its rules. Nothing uses it now. This cannot be undone.</Text>
          <TextInput label={<>Type <Text span ff="monospace" fw={700}>{r.name}</Text> to confirm</>} value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)} data-autofocus />
        </>
      )}
    </ChangeModal>
  )
}

/** A new security group, from Network › Firewalls. */
export function NewFirewall({ onClose }: { onClose: () => void }) {
  const accounts = useQuery(cloudAccountsQuery())
  const networks = useQuery(cloudItemsQuery('network'))
  const manageable = (accounts.data?.accounts ?? []).filter((a) => a.access === 'manage')
  const [acct, setAcct] = useState<string | null>(manageable.length === 1 ? manageable[0]!.id : null)
  const account = manageable.find((a) => a.id === acct)
  const [reg, setReg] = useState<string | null>(account?.regions.length === 1 ? account.regions[0]! : null)
  const here = (networks.data ?? []).filter((n) => n.accountId === acct && n.resource.region === reg)
  const [net, setNet] = useState<string | null>(null)
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  const [drafts, setDrafts] = useState<Draft[]>([newDraft(true)])
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const { options } = useSources(acct ?? '', reg ?? '')
  const rules = drafts.map(ruleOf)
  const ready = rules.every(Boolean)
  const exposes = exposures(rules.filter((x): x is CloudRule => x !== null))
  const network = net ?? (here.length === 1 ? here[0]!.resource.id : null)
  const validName = /^[A-Za-z0-9 ._\-:/()#,@[\]+=&;{}!$*]{1,255}$/.test(name.trim()) && !name.trim().toLowerCase().startsWith('sg-')
  return (
    <ChangeModal title="New security group" env={account?.environment ?? 'production'} level={exposes.length ? 'remove' : 'change'}
      reason={reason} setReason={setReason} onClose={onClose} busy={busy} submit="Make group"
      disabled={!acct || !reg || !network || !validName || !ready || roleLacks(account, 'firewall-create')}
      onSubmit={() => void submit({ accountId: acct!, region: reg!, action: 'firewall-create', spec: {
        network, name: name.trim(), description: desc.trim() || name.trim(), rules }, reason }, 'Made the group')}>
      <RoleNeedsUpdate account={account} action="firewall-create" />
      {manageable.length > 1 && (
        <Select label="Account" placeholder="Pick an account" value={acct} onChange={(v) => { setAcct(v); setReg(null); setNet(null) }}
          data={manageable.map((a) => ({ value: a.id, label: a.name }))} allowDeselect={false} />
      )}
      {account && account.regions.length > 1 && (
        <Select label="Region" value={reg} onChange={(v) => { setReg(v); setNet(null) }} data={account.regions} allowDeselect={false} />
      )}
      <Select label="Network" placeholder={here.length ? 'Pick a network' : 'No networks read here yet'} value={network} onChange={setNet}
        data={here.map((n) => ({ value: n.resource.id, label: `${n.resource.name} · ${n.resource.size}` }))} allowDeselect={false} />
      <Group grow align="flex-start">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={255}
          error={name && !validName ? 'Letters, digits, spaces and ._-:/()#,@[]+=&;{}!$*, not starting with sg-' : undefined} />
        <TextInput label="Description" placeholder="Same as the name" value={desc} onChange={(e) => setDesc(e.currentTarget.value)} maxLength={255} />
      </Group>
      <Field label="Inbound rules">
        <Stack gap={SP.tight} mt={4}>
          {drafts.map((d, i) => (
            <DraftRow key={i} d={d} options={options} set={(n) => setDrafts(drafts.map((x, j) => (j === i ? n : x)))}
              drop={() => setDrafts(drafts.filter((_, j) => j !== i))} />
          ))}
          <Button variant="subtle" size="xs" w="fit-content" leftSection={<IconPlus size={13} />} onClick={() => setDrafts([...drafts, newDraft(true)])}>Add a rule</Button>
        </Stack>
      </Field>
      <Text size={FS.meta} c="dimmed">AWS gives every new group one outbound rule, all traffic anywhere. Edit it afterwards if the group should be stricter.</Text>
      <ExposureAlert exposes={exposes} level />
    </ChangeModal>
  )
}

/** The security groups on an instance, from its panel. */
export function InstanceFirewalls({ it, env, onClose }: { it: CloudItem; env: CloudEnvironment; onClose: () => void }) {
  const r = it.resource
  const accounts = useQuery(cloudAccountsQuery())
  const groups = useQuery(cloudItemsQuery('firewall'))
  const account = accounts.data?.accounts.find((a) => a.id === it.accountId)
  const now = (r.links ?? []).filter((l) => l.kind === 'firewall').map((l) => l.id)
  const [ids, setIds] = useState<string[]>(now)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const here = (groups.data ?? []).filter((g) => g.accountId === it.accountId && g.resource.region === r.region && g.resource.network === r.network)
  const added = here.filter((g) => ids.includes(g.resource.id) && !now.includes(g.resource.id))
  const exposes = exposures(added.flatMap((g) => g.resource.rules ?? []))
  const shown = added.reduce((e, g) => stricterEnv(e, envOf(g.resource.tags, g.environment)), env)
  const same = ids.length === now.length && ids.every((x) => now.includes(x))
  return (
    <ChangeModal title={`Security groups of ${r.name}`} env={shown} level={exposes.length ? 'remove' : 'change'} reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Save groups" disabled={same || ids.length === 0 || ids.length > 5 || roleLacks(account, 'instance-firewalls')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'instance-firewalls', resourceId: r.id,
        spec: { firewalls: ids }, reason }, 'Groups changed')}>
      <RoleNeedsUpdate account={account} action="instance-firewalls" />
      <MultiSelect label="Security groups" description="One to five, from the instance's own network. The instance gets exactly these." searchable
        value={ids} onChange={setIds} maxValues={5}
        data={here.map((g) => ({ value: g.resource.id, label: `${g.resource.name} · ${g.resource.id}` }))} />
      {added.length > 0 && (
        <Field label="Adds">
          <Stack gap={2} mt={4}>
            {added.flatMap((g) => (g.resource.rules ?? []).filter((x) => x.inbound).map((x, i) => (
              <Code key={g.resource.id + i} fz={FS.meta} c={exposures([x]).length ? INK.denied : undefined}>{g.resource.name}: {portText(x)} from {x.source === '0.0.0.0/0' || x.source === '::/0' ? 'anywhere' : x.source}</Code>
            )))}
          </Stack>
        </Field>
      )}
      <ExposureAlert exposes={exposes} level />
      <Text size={FS.meta} c="dimmed">AWS applies the change at once, while the instance runs. Connections the old groups allowed and the new ones do not are cut.</Text>
    </ChangeModal>
  )
}

const HEALTH_COLOR: Record<string, string> = { healthy: 'teal', unhealthy: 'rose', initial: 'sky', draining: 'amber', unused: 'slate' }

/** The instances behind a target group, and putting more behind it or taking them out. */
export function TargetActions({ it }: { it: CloudItem }) {
  const { admin, account, waiting } = useChangeable(it)
  const instances = useQuery(cloudItemsQuery('instance'))
  const [open, setOpen] = useState<{ action: 'target-register' | 'target-deregister'; instance?: string } | null>(null)
  const r = it.resource
  const targets = targetsOf(r)
  const nameOf = (id: string) => instances.data?.find((x) => x.accountId === it.accountId && x.resource.id === id)?.resource.name ?? id
  const instanceTargets = !r.details?.targetType || r.details.targetType === 'instance'
  return (
    <Stack gap="xs">
      <Field label={`Targets · ${r.details?.healthy ?? 0} of ${r.details?.targets ?? targets.length} healthy`}>
        <Stack gap={SP.tight} mt={4}>
          {targets.length === 0 && <Text size={FS.meta} c="dimmed">{instanceTargets ? 'No instances behind it.' : `Its targets are ${r.details?.targetType} addresses, which Argus does not list.`}</Text>}
          {targets.map((t) => (
            <Group key={t.id} gap="xs" justify="space-between" wrap="nowrap">
              <Group gap={6} wrap="nowrap">
                <Badge variant="light" color={HEALTH_COLOR[t.health] ?? 'slate'} radius="sm">{t.health}</Badge>
                <Text size={FS.meta}>{nameOf(t.id)}</Text>
              </Group>
              {admin && account?.access === 'manage' && (
                <Button size="compact-xs" variant="subtle" color="amber" leftSection={<IconPlugConnectedX size={13} />} disabled={Boolean(waiting)}
                  onClick={() => setOpen({ action: 'target-deregister', instance: t.id })}>Take out</Button>
              )}
            </Group>
          ))}
        </Stack>
      </Field>
      {admin && instanceTargets && (
        <Gate it={it} account={account}>
          {waiting && (
            <Alert color="amber" variant="light" p="xs" icon={<IconUserCheck size={16} />}>
              <Text size={FS.meta}><b>{waiting.summary}</b> is waiting for approval, asked by {waiting.requestedBy} {relTime(waiting.requestedAt)}.</Text>
            </Alert>
          )}
          <Group gap="xs">
            <Button size="xs" variant="default" leftSection={<IconPlugConnected size={14} />} disabled={Boolean(waiting)} onClick={() => setOpen({ action: 'target-register' })}>
              Put an instance behind it
            </Button>
          </Group>
        </Gate>
      )}
      {open && <TargetChange it={it} account={account} action={open.action} instance={open.instance} onClose={() => setOpen(null)} />}
    </Stack>
  )
}

function TargetChange({ it, account, action, instance, onClose }: {
  it: CloudItem; account?: CloudAccount; action: 'target-register' | 'target-deregister'; instance?: string; onClose: () => void
}) {
  const r = it.resource
  const instances = useQuery(cloudItemsQuery('instance'))
  const [pick, setPick] = useState<string | null>(instance ?? null)
  const [port, setPort] = useState<number | ''>('')
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const behind = new Set(targetsOf(r).map((t) => t.id))
  const choices = (instances.data ?? []).filter((x) => x.accountId === it.accountId && x.resource.region === r.region &&
    (!r.network || x.resource.network === r.network) && !behind.has(x.resource.id) &&
    x.resource.state !== 'terminated' && x.resource.state !== 'shutting-down')
  const target = instances.data?.find((x) => x.accountId === it.accountId && x.resource.id === pick)
  const env = stricterEnv(envOf(r.tags, it.environment), target ? envOf(target.resource.tags, target.environment) : envOf(r.tags, it.environment))
  const register = action === 'target-register'
  const lbs = (r.links ?? []).filter((l) => l.kind === 'load-balancer').map((l) => l.name ?? l.id)
  const othersHealthy = targetsOf(r).filter((t) => t.id !== pick && t.health === 'healthy').length
  const last = !register && othersHealthy === 0 && lbs.length > 0
  return (
    <ChangeModal title={register ? `Put an instance behind ${r.name}` : `Take ${target?.resource.name ?? pick} out of ${r.name}?`} env={env} level="change"
      reason={reason} setReason={setReason} onClose={onClose} busy={busy} submit={register ? 'Put it behind' : 'Take it out'}
      color={register ? undefined : 'amber'} disabled={!pick || roleLacks(account, action)}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action, resourceId: r.id,
        spec: { instance: pick, port: port || undefined }, reason }, register ? 'Registered' : 'Taken out')}>
      <RoleNeedsUpdate account={account} action={action} />
      {register ? (
        <Group grow align="flex-start">
          <Select label="Instance" searchable placeholder={choices.length ? 'Pick an instance' : 'No other instances in its network'} value={pick}
            onChange={setPick} data={choices.map((x) => ({ value: x.resource.id, label: `${x.resource.name} · ${x.resource.state}` }))} />
          <NumberInput label="Port" placeholder={r.details?.port ? `${r.details.port}, the group's` : 'The group\'s'} value={port}
            onChange={(v) => setPort(v === '' ? '' : Number(v))} min={1} max={65535} hideControls />
        </Group>
      ) : (
        <Text size={FS.body} lh={1.55}>
          {lbs.length ? `${lbs.join(' and ')} stops sending it new requests` : 'It leaves the group'}, after draining the connections it has open.
        </Text>
      )}
      {last && (
        <Alert color="rose" variant="light" p="sm" title="Nothing healthy is left">
          <Text size={FS.meta}>No other healthy instance is behind {r.name}, so {lbs.join(' and ')} stops answering until one is.</Text>
        </Alert>
      )}
      {register && (
        <Text size={FS.meta} c="dimmed">
          The load balancer sends it traffic once its health check passes{r.details?.healthCheckPath ? ` (${r.details.healthCheckProtocol} ${r.details.healthCheckPath})` : ''}.
          Its security group must let the load balancer in on that port.
        </Text>
      )}
    </ChangeModal>
  )
}
