import { Alert, Button, Group, MultiSelect, NumberInput, SegmentedControl, Select, Stack, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconHeartbeat, IconStack2, IconUserCheck } from '@tabler/icons-react'
import { useEffect, useState } from 'react'
import { Field, relTime } from '~/components/primitives'
import {
  BALANCER_MONTHLY, LB_NAME, cloudAccountsQuery, cloudItemsQuery, defaultHealthCheck, envOf, groupSizeOf, healthCheckOf, healthProblem, healthWords,
  money, roleLacks, type CloudAccount, type CloudHealthCheck, type CloudItem,
} from '~/lib/cloud'
import { FS, SP } from '~/theme'
import { ChangeModal, useSubmit } from './ChangeModal'
import { Gate, RoleNeedsUpdate, useChangeable } from './StorageActions'

/*
 * Putting more than one server behind one address: making target groups
 * with their health checks, load balancers with one listener, changing a
 * health check, and sizing an Auto Scaling group. Each is a Change level
 * cloud change, so production waits for someone else. A load balancer bills
 * from the moment it exists, and its dialog says how much.
 */

/** Pick a manage account and one of its regions; one of each is picked for you. */
function useWhere() {
  const accounts = useQuery(cloudAccountsQuery())
  const manageable = (accounts.data?.accounts ?? []).filter((a) => a.access === 'manage' && a.provider === 'aws')
  const [acct, setAcct] = useState<string | null>(null)
  const id = acct ?? (manageable.length === 1 ? manageable[0]!.id : null)
  const account = manageable.find((a) => a.id === id)
  const [reg, setReg] = useState<string | null>(null)
  const region = reg ?? (account?.regions.length === 1 ? account.regions[0]! : null)
  const fields = (onChange: () => void) => (
    <>
      {manageable.length > 1 && (
        <Select label="Account" placeholder="Pick an account" value={id} onChange={(v) => { setAcct(v); setReg(null); onChange() }}
          data={manageable.map((a) => ({ value: a.id, label: a.name }))} allowDeselect={false} />
      )}
      {account && account.regions.length > 1 && (
        <Select label="Region" value={region} onChange={(v) => { setReg(v); onChange() }} data={account.regions} allowDeselect={false} />
      )}
    </>
  )
  return { id, account, region, fields }
}

/** The health check's fields. A TCP target group may check over TCP or HTTP(S); an HTTP one over HTTP(S). */
function HealthFields({ h, set, tcpGroup }: { h: CloudHealthCheck; set: (h: CloudHealthCheck) => void; tcpGroup: boolean }) {
  const web = h.protocol !== 'TCP'
  const problem = healthProblem(h)
  return (
    <Stack gap={SP.tight}>
      <Group grow align="flex-start">
        <Select label="Checked over" value={h.protocol} allowDeselect={false}
          data={tcpGroup ? ['TCP', 'HTTP', 'HTTPS'] : ['HTTP', 'HTTPS']}
          onChange={(v) => {
            const p = (v ?? 'HTTP') as CloudHealthCheck['protocol']
            set(p === 'TCP' ? { ...h, protocol: p, path: undefined, matcher: undefined } : { ...h, protocol: p, path: h.path ?? '/', matcher: h.matcher ?? '200' })
          }} />
        {web && <TextInput label="Path" value={h.path ?? ''} onChange={(e) => set({ ...h, path: e.currentTarget.value })} styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />}
        <TextInput label="Check port" description="Blank for the target's own" value={h.port === 'traffic-port' ? '' : h.port ?? ''} placeholder="Its own"
          onChange={(e) => set({ ...h, port: e.currentTarget.value.trim() || 'traffic-port' })} />
      </Group>
      <Group grow align="flex-start">
        <NumberInput label="Every (seconds)" value={h.intervalSeconds} min={5} max={300} onChange={(v) => set({ ...h, intervalSeconds: Number(v) || 0 })} />
        <NumberInput label="Healthy after" value={h.healthy} min={2} max={10} onChange={(v) => set({ ...h, healthy: Number(v) || 0 })} />
        <NumberInput label="Unhealthy after" value={h.unhealthy} min={2} max={10} onChange={(v) => set({ ...h, unhealthy: Number(v) || 0 })} />
        {web && <TextInput label="Healthy codes" value={h.matcher ?? ''} placeholder="200" onChange={(e) => set({ ...h, matcher: e.currentTarget.value.trim() })} />}
      </Group>
      <Text size={FS.meta} c={problem ? 'rose' : 'dimmed'}>{problem ?? `Checks ${healthWords(h)}.`}</Text>
    </Stack>
  )
}

/** A new target group of instances, with its health check. */
export function NewTargetGroup({ onClose }: { onClose: () => void }) {
  const networks = useQuery(cloudItemsQuery('network'))
  const groups = useQuery(cloudItemsQuery('target-group'))
  const { id, account, region, fields } = useWhere()
  const here = (networks.data ?? []).filter((n) => n.accountId === id && n.resource.region === region)
  const [net, setNet] = useState<string | null>(null)
  const network = net ?? (here.length === 1 ? here[0]!.resource.id : null)
  const [name, setName] = useState('')
  const [protocol, setProtocol] = useState<CloudHealthCheck['protocol']>('HTTP')
  const [port, setPort] = useState<number | ''>(80)
  const [health, setHealth] = useState<CloudHealthCheck>(defaultHealthCheck('HTTP'))
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const n = name.trim()
  const taken = (groups.data ?? []).some((g) => g.accountId === id && g.resource.region === region && g.resource.name.toLowerCase() === n.toLowerCase())
  const nameError = n && !LB_NAME.test(n) ? 'Letters, digits and hyphens, up to 32, not starting or ending with a hyphen' : taken ? `There is already a target group called ${n} here` : undefined
  return (
    <ChangeModal title="New target group" env={account?.environment ?? 'production'} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Make target group"
      disabled={!id || !region || !network || !n || Boolean(nameError) || !port || Boolean(healthProblem(health)) || roleLacks(account, 'target-group-create')}
      onSubmit={() => void submit({ accountId: id!, region: region!, action: 'target-group-create',
        spec: { network, name: n, protocol, port, health }, reason }, 'Made the target group')}>
      <RoleNeedsUpdate account={account} action="target-group-create" />
      {fields(() => setNet(null))}
      <Select label="Network" description="Its servers must be in this network" placeholder={here.length ? 'Pick a network' : 'No networks read here yet'}
        value={network} onChange={setNet} allowDeselect={false}
        data={here.map((x) => ({ value: x.resource.id, label: `${x.resource.name} · ${x.resource.size}` }))} />
      <Group grow align="flex-start">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={32} error={nameError} />
        <Field label="Traffic">
          <SegmentedControl mt={4} fullWidth value={protocol} data={['HTTP', 'HTTPS', 'TCP']} onChange={(v) => {
            const p = v as CloudHealthCheck['protocol']
            setProtocol(p)
            setPort(p === 'HTTPS' ? 443 : p === 'HTTP' ? 80 : port)
            setHealth(defaultHealthCheck(p === 'TCP' ? 'TCP' : p === 'HTTPS' ? 'HTTPS' : 'HTTP'))
          }} />
        </Field>
        <NumberInput label="Port" value={port} min={1} max={65535} hideControls onChange={(v) => setPort(v === '' ? '' : Number(v))} />
      </Group>
      <Field label="Health check">
        <Stack mt={4}><HealthFields h={health} set={setHealth} tcpGroup={protocol === 'TCP'} /></Stack>
      </Field>
      <Text size={FS.meta} c="dimmed">It starts empty and costs nothing. Put instances behind it from its panel, and a load balancer in front of it from Load balancers.</Text>
    </ChangeModal>
  )
}

/** A new load balancer with one listener, forwarding to a target group. */
export function NewBalancer({ onClose }: { onClose: () => void }) {
  const groups = useQuery(cloudItemsQuery('target-group'))
  const subnets = useQuery(cloudItemsQuery('subnet'))
  const firewalls = useQuery(cloudItemsQuery('firewall'))
  const balancers = useQuery(cloudItemsQuery('load-balancer'))
  const { id, account, region, fields } = useWhere()
  const [type, setType] = useState<'application' | 'network'>('application')
  const [internal, setInternal] = useState(false)
  const [name, setName] = useState('')
  const [tg, setTg] = useState<string | null>(null)
  const [subs, setSubs] = useState<string[]>([])
  const [fws, setFws] = useState<string[]>([])
  const [port, setPort] = useState<number | ''>(80)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const fits = (g: CloudItem) => g.accountId === id && g.resource.region === region &&
    (type === 'network' ? g.resource.details?.protocol === 'TCP' : g.resource.details?.protocol !== 'TCP') &&
    !(g.resource.links ?? []).some((l) => l.kind === 'load-balancer')
  const choices = (groups.data ?? []).filter(fits)
  const group = choices.find((g) => g.resource.id === tg)
  const net = group?.resource.network
  const subnetsHere = (subnets.data ?? []).filter((s) => s.accountId === id && s.resource.region === region && s.resource.network === net)
  const fwsHere = (firewalls.data ?? []).filter((f) => f.accountId === id && f.resource.region === region && f.resource.network === net)
  // A pick that no longer fits, after the type, account or region changed, is cleared.
  useEffect(() => { if (tg && !group) setTg(null) }, [tg, group])
  useEffect(() => { setSubs([]); setFws([]) }, [net])
  const zones = subs.map((s) => subnetsHere.find((x) => x.resource.id === s)?.resource.zone ?? '')
  const twice = zones.some((z, i) => zones.indexOf(z) !== i)
  const n = name.trim()
  const taken = (balancers.data ?? []).some((b) => b.accountId === id && b.resource.region === region && b.resource.name.toLowerCase() === n.toLowerCase())
  const nameError = n && (!LB_NAME.test(n) || n.toLowerCase().startsWith('internal-')) ? 'Letters, digits and hyphens, up to 32, not starting with internal-'
    : taken ? `There is already a load balancer called ${n} here` : undefined
  const app = type === 'application'
  return (
    <ChangeModal title="New load balancer" env={account?.environment ?? 'production'} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Make load balancer"
      disabled={!id || !region || !n || Boolean(nameError) || !group || subs.length < 2 || twice || (app && (fws.length === 0 || fws.length > 5)) || !port || roleLacks(account, 'balancer-create')}
      onSubmit={() => void submit({ accountId: id!, region: region!, action: 'balancer-create', spec: {
        name: n, type, internal, subnets: subs, firewalls: app ? fws : undefined, port, targetGroup: tg }, reason }, 'Made the load balancer')}>
      <RoleNeedsUpdate account={account} action="balancer-create" />
      {fields(() => setTg(null))}
      <Group grow align="flex-start">
        <Field label="Kind">
          <SegmentedControl mt={4} fullWidth value={type} onChange={(v) => { setType(v as 'application' | 'network'); setPort(80) }}
            data={[{ value: 'application', label: 'Application · HTTP' }, { value: 'network', label: 'Network · TCP' }]} />
        </Field>
        <Field label="Reached from">
          <SegmentedControl mt={4} fullWidth value={internal ? 'internal' : 'internet'} onChange={(v) => setInternal(v === 'internal')}
            data={[{ value: 'internet', label: 'The internet' }, { value: 'internal', label: 'Inside the network' }]} />
        </Field>
      </Group>
      <Group grow align="flex-start">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={32} error={nameError} />
        <NumberInput label={`Listens on ${app ? 'HTTP' : 'TCP'} port`} value={port} min={1} max={65535} hideControls onChange={(v) => setPort(v === '' ? '' : Number(v))} />
      </Group>
      <Select label="Forwards to" description={app ? 'An HTTP or HTTPS target group not behind another load balancer' : 'A TCP target group not behind another load balancer'}
        placeholder={choices.length ? 'Pick a target group' : 'No target group fits; make one first'} value={tg} onChange={setTg} allowDeselect={false}
        data={choices.map((g) => ({ value: g.resource.id, label: `${g.resource.name} · ${g.resource.size ?? ''}` }))} />
      <MultiSelect label="Subnets" description="One in each of two or more zones, in the target group's network" disabled={!group}
        value={subs} onChange={setSubs} error={twice ? 'Pick one subnet in each zone' : undefined}
        data={subnetsHere.map((s) => ({ value: s.resource.id, label: `${s.resource.name} · ${s.resource.zone} · ${s.resource.size}` }))} />
      {app && (
        <MultiSelect label="Security groups" description="One to five; they must let clients in on the listener's port" disabled={!group}
          value={fws} onChange={setFws} maxValues={5}
          data={fwsHere.map((f) => ({ value: f.resource.id, label: `${f.resource.name} · ${f.resource.id}` }))} />
      )}
      <Alert color="amber" variant="light" p="sm">
        <Text size={FS.meta}>
          A load balancer bills from the moment it exists: about {money(BALANCER_MONTHLY)} a month before traffic, more with it.
          {app ? ' Its listener is HTTP; add HTTPS with a certificate in AWS.' : ''}
        </Text>
      </Alert>
    </ChangeModal>
  )
}

/** A target group's health check, as read, and changing it. */
export function HealthCheckPanel({ it }: { it: CloudItem }) {
  const { admin, account, waiting } = useChangeable(it)
  const [open, setOpen] = useState(false)
  const h = healthCheckOf(it.resource)
  return (
    <Field label="Health check">
      <Group gap="xs" justify="space-between" wrap="nowrap" mt={4}>
        <Text size={FS.meta}>{healthWords(h)}</Text>
        {admin && account?.access === 'manage' && (
          <Button size="compact-xs" variant="subtle" leftSection={<IconHeartbeat size={13} />} disabled={Boolean(waiting)} onClick={() => setOpen(true)}>Change</Button>
        )}
      </Group>
      {open && <HealthCheckChange it={it} account={account} onClose={() => setOpen(false)} />}
    </Field>
  )
}

function HealthCheckChange({ it, account, onClose }: { it: CloudItem; account?: CloudAccount; onClose: () => void }) {
  const r = it.resource
  const now = healthCheckOf(r)
  const [h, setH] = useState<CloudHealthCheck>(now)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const same = healthWords(h) === healthWords(now)
  const stricter = h.unhealthy < now.unhealthy || h.intervalSeconds < now.intervalSeconds || h.path !== now.path || h.matcher !== now.matcher
  return (
    <ChangeModal title={`Health check of ${r.name}`} env={envOf(r.tags, it.environment)} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Change check" disabled={same || Boolean(healthProblem(h)) || roleLacks(account, 'health-check')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'health-check', resourceId: r.id, spec: { ...h }, reason }, 'Changed the check')}>
      <RoleNeedsUpdate account={account} action="health-check" />
      <Text size={FS.meta} c="dimmed">Now: {healthWords(now)}.</Text>
      <HealthFields h={h} set={setH} tcpGroup={r.details?.protocol === 'TCP'} />
      {stricter && Number(r.details?.targets ?? 0) > 0 && (
        <Text size={FS.meta} c="dimmed">The new check applies at once. Targets that fail it stop getting traffic once they fail it {h.unhealthy} times in a row.</Text>
      )}
    </ChangeModal>
  )
}

/** An Auto Scaling group's size and changing it, from its panel. */
export function ScalingGroupActions({ it }: { it: CloudItem }) {
  const { admin, account, waiting } = useChangeable(it)
  const [open, setOpen] = useState(false)
  const s = groupSizeOf(it.resource)
  return (
    <Stack gap="xs">
      <Field label="Size">
        <Text size={FS.body} mt={4}>
          Wants <b>{s.desired}</b>, between {s.min} and {s.max}; {s.running} running now.
        </Text>
      </Field>
      {admin && (
        <Gate it={it} account={account}>
          {waiting && (
            <Alert color="amber" variant="light" p="xs" icon={<IconUserCheck size={16} />}>
              <Text size={FS.meta}><b>{waiting.summary}</b> is waiting for approval, asked by {waiting.requestedBy} {relTime(waiting.requestedAt)}.</Text>
            </Alert>
          )}
          <Group gap="xs">
            <Button size="xs" variant="default" leftSection={<IconStack2 size={14} />} disabled={Boolean(waiting)} onClick={() => setOpen(true)}>Change size</Button>
          </Group>
        </Gate>
      )}
      {open && <GroupSizeChange it={it} account={account} onClose={() => setOpen(false)} />}
    </Stack>
  )
}

function GroupSizeChange({ it, account, onClose }: { it: CloudItem; account?: CloudAccount; onClose: () => void }) {
  const r = it.resource
  const now = groupSizeOf(r)
  const [min, setMin] = useState<number | ''>(now.min)
  const [max, setMax] = useState<number | ''>(now.max)
  const [desired, setDesired] = useState<number | ''>(now.desired)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const ok = min !== '' && max !== '' && desired !== '' && min >= 0 && max >= 1 && max <= 100 && min <= max && desired >= min && desired <= max
  const same = min === now.min && max === now.max && desired === now.desired
  const stack = r.tags?.['aws:cloudformation:stack-name']
  const delta = desired === '' ? 0 : desired - now.desired
  return (
    <ChangeModal title={`Size of ${r.name}`} env={envOf(r.tags, it.environment)} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Change size" color={delta < 0 ? 'amber' : undefined}
      disabled={!ok || same || roleLacks(account, 'group-size')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'group-size', resourceId: r.id,
        spec: { min, max, desired }, reason }, 'Changed the size')}>
      <RoleNeedsUpdate account={account} action="group-size" />
      <Group grow align="flex-start">
        <NumberInput label="Minimum" value={min} min={0} max={100} onChange={(v) => setMin(v === '' ? '' : Number(v))} />
        <NumberInput label="Wants now" value={desired} min={0} max={100} onChange={(v) => setDesired(v === '' ? '' : Number(v))}
          error={ok || desired === '' || min === '' || max === '' ? undefined : 'Between the minimum and the maximum'} />
        <NumberInput label="Maximum" value={max} min={1} max={100} onChange={(v) => setMax(v === '' ? '' : Number(v))} />
      </Group>
      <Text size={FS.meta} c="dimmed">
        {delta > 0 ? `AWS starts ${delta} more from the group's launch template.`
          : delta < 0 ? `AWS stops ${-delta} by the group's termination policy, after draining them from its target groups.`
            : 'AWS keeps the instances it has.'}
        {' '}Its own scaling policies, if any, may move how many it wants within the new limits.
      </Text>
      {stack && (
        <Alert color="amber" variant="light" p="sm">
          <Text size={FS.meta}>It belongs to CloudFormation stack {stack}, which may set it back at its next update.</Text>
        </Alert>
      )}
    </ChangeModal>
  )
}
