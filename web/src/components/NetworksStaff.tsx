import { Alert, Badge, Button, Checkbox, Group, Modal, NumberInput, SegmentedControl, Select, Stack, Switch, Table, Text, TextInput } from '@mantine/core'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { IconPlus, IconTrash, IconUsersGroup } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { DataTable, EmptyState, SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import {
  askWGNetwork, removeStaffDevice, setStaffGrant, setStaffMode, setStaffPolicy, STAFF_MODE, staffVPNsQuery, wgNetworksQuery,
  type StaffGrant, type StaffMode, type StaffPolicy, type StaffVPN,
} from '~/lib/networks'
import { assetsQuery, teamsQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'
import { ServerNetwork } from './NetworksWireGuard'

/*
 * The staff VPN, as an admin sets it up (internal/control/wg_staff.go): the
 * hub, a server network of its own approved like the others; what each team
 * reaches; the default traffic and each person's own; and every device.
 * People make their own devices on My VPN.
 */

const mono = { input: { fontFamily: 'var(--mantine-font-family-monospace)' } }
const ranges = (s: string) => s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean)

export const LOGINS_NOT_RECORDED =
  'SSH and RDP straight to a server are not recorded: Argus records the logins that go through it. Leave this off and people log in through Argus.'

export function StaffVPNCard({ canManage, me }: { canManage: boolean; me?: string }) {
  const { data, isLoading } = useQuery({ ...staffVPNsQuery(), enabled: isConfigured() })
  const [setting, setSetting] = useState(false)
  const vpns = data ?? []
  return (
    <SectionCard title="Staff VPN" icon={IconUsersGroup} iconColor="grape"
      description="People's laptops and phones on WireGuard, through one of your servers. Each person reaches what their teams are given; profiles expire and are made again with an authenticator code."
      action={canManage && vpns.length === 0 && (
        <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} onClick={() => setSetting(true)}>Set up the staff VPN</Button>
      )}>
      {isLoading ? null : vpns.length === 0 ? (
        <EmptyState icon={IconUsersGroup} title="No staff VPN yet."
          description="Pick a server people's devices can reach from the internet, and a private range for the devices. Then give teams what they may reach." />
      ) : (
        <Stack gap="xl">{vpns.map((v) => <StaffVPNBlock key={v.network.id} v={v} canManage={canManage} me={me} />)}</Stack>
      )}
      {setting && <SetUpStaff onClose={() => setSetting(false)} />}
    </SectionCard>
  )
}

function StaffVPNBlock({ v, canManage, me }: { v: StaffVPN; canManage: boolean; me?: string }) {
  return (
    <Stack gap="lg" data-testid="staff-vpn">
      <ServerNetwork n={v.network} canManage={canManage} me={me} onEdit={() => {}} />
      {v.network.state === 'active' && (
        <>
          {v.network.staff && <PolicyRow vpn={v.network.id} policy={v.network.staff} canManage={canManage} />}
          <Grants v={v} canManage={canManage} />
          <Modes v={v} canManage={canManage} />
          <Devices v={v} canManage={canManage} />
        </>
      )}
    </Stack>
  )
}

function useDone() {
  const qc = useQueryClient()
  return () => qc.invalidateQueries({ queryKey: ['networks'] })
}

function PolicyRow({ vpn, policy, canManage }: { vpn: string; policy: StaffPolicy; canManage: boolean }) {
  const done = useDone()
  const [p, setP] = useState(policy)
  const save = useMutation({ mutationFn: () => setStaffPolicy(vpn, p), onSettled: done })
  const changed = p.default !== policy.default || p.days !== policy.days || p.mfa !== policy.mfa
  return (
    <Group align="flex-end" gap="lg" wrap="wrap" data-testid="staff-policy">
      <Stack gap={4}>
        <Text size={FS.body} fw={500}>Traffic by default</Text>
        <SegmentedControl disabled={!canManage} value={p.default} onChange={(x) => setP({ ...p, default: x as StaffMode })}
          data={(['company', 'full'] as const).map((m) => ({ value: m, label: STAFF_MODE[m].label }))} />
      </Stack>
      <NumberInput label="Profiles last" suffix=" days" min={1} max={365} w={130} disabled={!canManage} value={p.days}
        onChange={(x) => setP({ ...p, days: Number(x) || 1 })} />
      <Switch label="Authenticator code to make or renew one" disabled={!canManage} checked={p.mfa}
        onChange={(e) => setP({ ...p, mfa: e.currentTarget.checked })} />
      {canManage && changed && (
        <Button size="xs" loading={save.isPending}
          onClick={() => void run(() => save.mutateAsync(), { failure: 'Not saved', success: ['Saved', 'Profiles already made keep at most the new length.'] })}>Save</Button>
      )}
      <Text size={FS.meta} c="dimmed" w="100%">{STAFF_MODE[p.default].hint}</Text>
    </Group>
  )
}

function Grants({ v, canManage }: { v: StaffVPN; canManage: boolean }) {
  const [editing, setEditing] = useState<StaffGrant | 'new' | null>(null)
  const grants = v.grants ?? []
  return (
    <Stack gap={SP.cozy}>
      <Group justify="space-between">
        <Text size={FS.body} fw={600}>What each team reaches</Text>
        {canManage && <Button size="xs" variant="subtle" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Give a team access</Button>}
      </Group>
      <DataTable columns={['Team', 'Reaches', 'SSH and RDP straight to it', '']} minWidth={640} isEmpty={grants.length === 0}
        empty={<EmptyState icon={IconUsersGroup} title="No team reaches anything yet." description="Until a team is given ranges, nobody can make a profile." />}>
        {grants.map((g) => (
          <Table.Tr key={g.teamId} data-testid="staff-grant">
            <Table.Td><Text size={FS.body} fw={500}>{g.teamName}</Text></Table.Td>
            <Table.Td><Text size={FS.body} ff="monospace">{g.ranges.join(', ')}</Text></Table.Td>
            <Table.Td>
              {g.logins
                ? <Badge color="amber" variant="light" title={LOGINS_NOT_RECORDED}>Allowed, not recorded</Badge>
                : <Badge color="teal" variant="light">Through Argus only</Badge>}
            </Table.Td>
            <Table.Td align="right">{canManage && <Button size="compact-xs" variant="subtle" onClick={() => setEditing(g)}>Change</Button>}</Table.Td>
          </Table.Tr>
        ))}
      </DataTable>
      {editing && <GrantEditor vpn={v.network.id} g={editing === 'new' ? null : editing} taken={grants} onClose={() => setEditing(null)} />}
    </Stack>
  )
}

function GrantEditor({ vpn, g, taken, onClose }: { vpn: string; g: StaffGrant | null; taken: StaffGrant[]; onClose: () => void }) {
  const done = useDone()
  const teams = useQuery(teamsQuery())
  const [team, setTeam] = useState<string | null>(g?.teamId ?? null)
  const [text, setText] = useState((g?.ranges ?? []).join(', '))
  const [logins, setLogins] = useState(g?.logins ?? false)
  const save = useMutation({ mutationFn: (rs: string[]) => setStaffGrant(vpn, team ?? '', rs, logins), onSettled: done })
  const choices = (teams.data ?? []).filter((t) => t.id === g?.teamId || !taken.some((x) => x.teamId === t.id))
  const submit = (rs: string[]) =>
    void run(() => save.mutateAsync(rs), { failure: 'Not saved', success: ['Saved', 'The VPN server applies it within a minute.'] }).then((ok) => { if (ok) onClose() })
  return (
    <Modal opened onClose={onClose} title={g ? `What ${g.teamName} reaches` : 'Give a team access'} size="lg">
      <Stack gap="md">
        <Select label="Team" searchable disabled={!!g} value={team} onChange={setTeam} data={choices.map((t) => ({ value: t.id, label: t.name }))} />
        <TextInput label="Ranges it reaches" description="Private ranges, comma separated: server networks, offices, cloud networks"
          placeholder="10.88.0.0/24, 192.168.10.0/24" styles={mono} value={text} onChange={(e) => setText(e.currentTarget.value)} />
        <Checkbox label="Allow SSH and RDP straight to these servers" checked={logins} onChange={(e) => setLogins(e.currentTarget.checked)} />
        <Alert color={logins ? 'amber' : 'sky'} p="xs" data-testid="staff-logins-note">
          <Text size={FS.meta}>{LOGINS_NOT_RECORDED}</Text>
        </Alert>
        <Group justify="space-between">
          {g ? <Button variant="subtle" color="rose" leftSection={<IconTrash size={14} />} loading={save.isPending} onClick={() => submit([])}>Take it away</Button> : <span />}
          <Group gap="xs">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button disabled={!team || ranges(text).length === 0} loading={save.isPending} onClick={() => submit(ranges(text))}>Save</Button>
          </Group>
        </Group>
      </Stack>
    </Modal>
  )
}

function Modes({ v, canManage }: { v: StaffVPN; canManage: boolean }) {
  const done = useDone()
  const [email, setEmail] = useState('')
  const [mode, setMode] = useState<StaffMode>(v.network.staff?.default === 'full' ? 'company' : 'full')
  const save = useMutation({ mutationFn: ({ e, m }: { e: string; m: StaffMode | '' }) => setStaffMode(v.network.id, e, m), onSettled: done })
  const own = Object.entries(v.modes ?? {}).sort(([a], [b]) => a.localeCompare(b))
  return (
    <Stack gap={SP.cozy}>
      <Text size={FS.body} fw={600}>People with their own setting</Text>
      {own.length === 0 && <Text size={FS.meta} c="dimmed">Everyone has the default: {STAFF_MODE[v.network.staff?.default ?? 'company'].label.toLowerCase()}.</Text>}
      {own.map(([e, m]) => (
        <Group key={e} gap="xs" data-testid="staff-mode">
          <Text size={FS.body}>{e}</Text><Badge variant="light">{STAFF_MODE[m].label}</Badge>
          {canManage && <Button size="compact-xs" variant="subtle" onClick={() => void run(() => save.mutateAsync({ e, m: '' }), { failure: 'Not saved' })}>Back to the default</Button>}
        </Group>
      ))}
      {canManage && (
        <Group align="flex-end" gap="xs">
          <TextInput label="Person" placeholder="rina@northwind.id" value={email} onChange={(e) => setEmail(e.currentTarget.value)} w={260} />
          <Select w={170} value={mode} onChange={(x) => setMode((x as StaffMode) ?? 'full')} allowDeselect={false}
            data={(['company', 'full'] as const).map((m) => ({ value: m, label: STAFF_MODE[m].label }))} />
          <Button size="sm" variant="default" disabled={!email.includes('@')} loading={save.isPending}
            onClick={() => void run(() => save.mutateAsync({ e: email.trim(), m: mode }), { failure: 'Not saved', success: ['Saved', 'Their new profiles route it; the VPN server enforces it within a minute.'] }).then((ok) => { if (ok) setEmail('') })}>Set</Button>
        </Group>
      )}
    </Stack>
  )
}

function Devices({ v, canManage }: { v: StaffVPN; canManage: boolean }) {
  const done = useDone()
  const remove = useMutation({ mutationFn: (id: string) => removeStaffDevice(id), onSettled: done })
  return (
    <Stack gap={SP.cozy}>
      <Text size={FS.body} fw={600}>Devices</Text>
      <DataTable columns={['Person', 'Device', 'Address', 'Lasts until', '']} minWidth={640} isEmpty={v.devices.length === 0}
        empty={<EmptyState icon={IconUsersGroup} title="No devices yet." description="People make their own on My VPN." />}>
        {v.devices.map((d) => (
          <Table.Tr key={d.id} data-testid="staff-device">
            <Table.Td><Text size={FS.body}>{d.userEmail}</Text></Table.Td>
            <Table.Td><Text size={FS.body}>{d.name}</Text></Table.Td>
            <Table.Td><Text size={FS.body} ff="monospace">{d.address}</Text></Table.Td>
            <Table.Td>{d.expired ? <Badge color="rose" variant="light">Expired</Badge> : <Text size={FS.body}>{relTime(d.expiresAt)}</Text>}</Table.Td>
            <Table.Td align="right">
              {canManage && <Button size="compact-xs" variant="subtle" color="rose"
                onClick={() => void run(() => remove.mutateAsync(d.id), { failure: 'Not removed', success: ['Removed', 'The VPN server drops it within a minute.'] })}>Remove</Button>}
            </Table.Td>
          </Table.Tr>
        ))}
      </DataTable>
    </Stack>
  )
}

function SetUpStaff({ onClose }: { onClose: () => void }) {
  const done = useDone()
  const { data: assets } = useQuery(assetsQuery())
  const nets = useQuery(wgNetworksQuery())
  const servers = useMemo(() => (assets ?? []).filter((a) => !a.protocol || a.protocol === 'ssh'), [assets])
  const suggested = useMemo(() => {
    const used = new Set((nets.data ?? []).map((n) => n.cidr))
    for (let b = 66; b < 80; b++) if (!used.has(`10.${b}.0.0/22`)) return `10.${b}.0.0/22`
    return ''
  }, [nets.data])
  const [name, setName] = useState('Staff VPN')
  const [assetId, setAssetId] = useState<string | null>(null)
  const [endpoint, setEndpoint] = useState('')
  const [cidr, setCidr] = useState('')
  const [port, setPort] = useState(51900)
  const [understood, setUnderstood] = useState(false)
  const save = useMutation({
    mutationFn: () => askWGNetwork({ name: name.trim(), shape: 'staff', cidr: (cidr || suggested).trim(), port, members: [{ assetId: assetId ?? '', endpoint: endpoint.trim() }] }),
    onSettled: done,
  })
  return (
    <Modal opened onClose={onClose} title="Set up the staff VPN" size="lg">
      <Stack gap="md">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} />
        <Group grow align="flex-start">
          <Select label="Server" description="Devices connect to it; Argus sets up WireGuard and its firewall" searchable value={assetId}
            onChange={setAssetId} data={servers.map((s) => ({ value: s.id, label: s.hostname }))} />
          <TextInput label="Its public address" description="A hostname or IPv4 address devices reach" placeholder="vpn.northwind.id"
            styles={mono} value={endpoint} onChange={(e) => setEndpoint(e.currentTarget.value)} />
        </Group>
        <Group grow align="flex-start">
          <TextInput label="Range for devices" description="A /22 gives about a thousand devices" placeholder={suggested} styles={mono}
            value={cidr} onChange={(e) => setCidr(e.currentTarget.value)} />
          <NumberInput label="UDP port" min={1024} max={65535} value={port} onChange={(x) => setPort(Number(x) || 51900)} />
        </Group>
        <Alert color="sky" p="xs">
          <Text size={FS.meta}>
            The server passes devices' traffic on as itself, so servers behind it need no route back, and refuses anything a device's
            teams are not given. Each change to the devices restarts the VPN on that server for a moment; connected devices pick up again by themselves.
          </Text>
        </Alert>
        <Checkbox checked={understood} onChange={(e) => setUnderstood(e.currentTarget.checked)}
          label="I understand the server applies this on a 2-minute trial and undoes it if it can no longer reach Argus" />
        <Text size={FS.meta} c="dimmed">Another admin approves it before the server changes.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!assetId || !endpoint.trim() || !understood} loading={save.isPending}
            onClick={() => void run(() => save.mutateAsync(), { failure: 'Not asked', success: ['Asked', 'It waits for another admin to approve.'] }).then((ok) => { if (ok) onClose() })}>
            Ask for approval
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
