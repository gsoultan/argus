import { Alert, Badge, Button, Checkbox, Group, Modal, MultiSelect, NumberInput, Radio, SegmentedControl, Stack, Table, Text, TextInput, Tooltip } from '@mantine/core'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { IconCheck, IconPencil, IconPlus, IconShieldLock, IconTrash, IconX } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { AloneOr, ApproveOwn } from '~/components/ApproveOwn'
import { AnchorLink } from '~/components/links'
import { DataTable, EmptyState, SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import {
  approveWG, askWGChange, askWGNetwork, askWGRemove, declineWG, WG_MEMBER_STATE, wgNetworksQuery,
  type WGMemberDraft, type WGNetwork, type WGNetworkDraft, type WGShape,
} from '~/lib/networks'
import { assetsQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'

/*
 * Server networks: WireGuard networks Argus sets up between servers through
 * their agents (internal/control/wg_networks.go). Every change waits for a
 * second admin; on the server, a change that cuts it off from Argus undoes
 * itself within two minutes.
 */

const SHAPE: Record<WGShape, { label: string; hint: string }> = {
  mesh: { label: 'Mesh', hint: 'Every server reaches every other directly.' },
  hub: { label: 'Hub', hint: 'Servers reach each other through one hub. Use it when most servers cannot be reached from outside.' },
}

function draftWords(d: WGNetworkDraft): string {
  return `${SHAPE[d.shape].label} · ${d.cidr} · UDP ${d.port} · ${d.members.map((m) => `${m.hostname ?? m.assetId} ${m.address ?? ''}${m.hub ? ' (hub)' : ''}${m.routes?.length ? ` routing ${m.routes.join(', ')}` : ''}`).join(', ')}`
}

export function ServerNetworksCard({ canManage, me }: { canManage: boolean; me?: string }) {
  const { data, isLoading } = useQuery({ ...wgNetworksQuery(), enabled: isConfigured() })
  const [editing, setEditing] = useState<WGNetwork | 'new' | null>(null)
  const nets = data ?? []
  return (
    <SectionCard title="Server networks" icon={IconShieldLock} iconColor="teal"
      description="WireGuard networks Argus sets up between your servers, on premises and in the cloud. Each server makes its own key, and every change waits for another admin."
      action={canManage && <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>New network</Button>}>
      {isLoading ? null : nets.length === 0 ? (
        <EmptyState icon={IconShieldLock} title="No server networks yet."
          description="Pick the servers and a private range, and Argus sets up WireGuard on each: keys, addresses, peers and the firewall port. A change that would cut a server off from Argus undoes itself." />
      ) : (
        <Stack gap="lg">{nets.map((n) => <ServerNetwork key={n.id} n={n} canManage={canManage} me={me} onEdit={() => setEditing(n)} />)}</Stack>
      )}
      {editing && <NetworkEditor n={editing === 'new' ? null : editing} taken={nets} onClose={() => setEditing(null)} />}
    </SectionCard>
  )
}

/** What a change does to the members, in words: "adds x, takes out y, moves to 10.9.0.0/24". */
function changeWords(n: WGNetwork, d: WGNetworkDraft): string {
  if (d.remove) return 'remove it from every server'
  if (n.state === 'waiting') return 'set it up'
  const before = new Set(n.members.map((m) => m.assetId))
  const after = new Set(d.members.map((m) => m.assetId))
  const parts = [
    ...d.members.filter((m) => !before.has(m.assetId)).map((m) => `add ${m.hostname ?? m.assetId}`),
    ...n.members.filter((m) => !after.has(m.assetId)).map((m) => `take out ${m.hostname}`),
  ]
  if (d.cidr !== n.cidr) parts.push(`move it to ${d.cidr}`)
  if (d.port !== n.port) parts.push(`use UDP ${d.port}`)
  if (d.name !== n.name) parts.push(`rename it ${d.name}`)
  if (d.shape !== n.shape) parts.push(`make it a ${SHAPE[d.shape].label.toLowerCase()}`)
  return parts.length ? parts.join(', ') : 'change server addresses or how they are reached'
}

function ServerNetwork({ n, canManage, me, onEdit }: { n: WGNetwork; canManage: boolean; me?: string; onEdit: () => void }) {
  const qc = useQueryClient()
  const done = () => qc.invalidateQueries({ queryKey: ['networks'] })
  const approve = useMutation({ mutationFn: (confirm?: string) => approveWG(n.id, confirm), onSettled: done })
  const decline = useMutation({ mutationFn: () => declineWG(n.id), onSettled: done })
  const [removing, setRemoving] = useState(false)
  const p = n.pending
  const what = p ? changeWords(n, p) : ''
  const mine = !!me && n.pendingBy?.toLowerCase() === me.toLowerCase()
  return (
    <Stack gap={SP.cozy} data-testid="wg-network">
      <Group justify="space-between" wrap="wrap" gap="xs">
        <Group gap="xs">
          <Text size={FS.body} fw={600}>{n.name}</Text>
          <Tooltip label={SHAPE[n.shape].hint} withArrow><Badge variant="light" color="slate">{SHAPE[n.shape].label}</Badge></Tooltip>
          <Text size={FS.meta} ff="monospace">{n.cidr}</Text>
          <Text size={FS.meta} c="dimmed">UDP {n.port} · {n.iface}</Text>
          {n.state === 'waiting' && <Badge color="amber" variant="light">Waiting for approval</Badge>}
        </Group>
        {canManage && n.state === 'active' && !p && (
          <Group gap="xs">
            <Button size="xs" variant="subtle" leftSection={<IconPencil size={14} />} onClick={onEdit}>Change</Button>
            <Button size="xs" variant="subtle" color="rose" leftSection={<IconTrash size={14} />} onClick={() => setRemoving(true)}>Remove</Button>
          </Group>
        )}
      </Group>
      {p && (
        <Alert color="amber" p="xs" data-testid="wg-pending">
          <Group justify="space-between" wrap="wrap" gap="xs">
            <Text size={FS.meta}>
              {n.pendingBy} asks to {what}{n.pendingAt ? `, ${relTime(n.pendingAt)}` : ''}. Nothing changes on the servers until another admin approves.
              {!p.remove && <><br />{draftWords(p)}</>}
            </Text>
            {canManage && (
              <Group gap="xs">
                {mine && !n.approveOwn
                  ? <Tooltip label="Another admin approves it" withArrow><Button size="compact-xs" disabled leftSection={<IconCheck size={12} />}>Approve</Button></Tooltip>
                  : mine
                  ? <ApproveOwn what="this network change" loading={approve.isPending}
                    onApprove={(c) => run(() => approve.mutateAsync(c), { failure: 'Not approved', success: ['Approved', 'The servers apply it on their next heartbeat.'] })} />
                  : <Button size="compact-xs" color="teal" leftSection={<IconCheck size={12} />} loading={approve.isPending}
                    onClick={() => void run(() => approve.mutateAsync(undefined), { failure: 'Not approved', success: ['Approved', 'The servers apply it on their next heartbeat.'] })}>Approve</Button>}
                <Button size="compact-xs" variant="default" leftSection={<IconX size={12} />} loading={decline.isPending}
                  onClick={() => void run(() => decline.mutateAsync(), { failure: 'Not declined', success: [mine ? 'Taken back' : 'Declined', n.state === 'waiting' ? `${n.name} will not be set up.` : 'The network stays as it is.'] })}>{mine ? 'Take back' : 'Decline'}</Button>
              </Group>
            )}
          </Group>
        </Alert>
      )}
      {n.state === 'active' && (
        <DataTable columns={['Server', 'Address', 'Role', 'Reached at', 'On the server']} minWidth={760} isEmpty={n.members.length === 0}
          empty={<EmptyState icon={IconShieldLock} title="No servers in this network." />}>
          {n.members.map((m) => {
            const st = WG_MEMBER_STATE[m.state] ?? { label: m.state, color: 'slate', hint: '' }
            return (
              <Table.Tr key={m.assetId} data-testid="wg-member">
                <Table.Td><AnchorLink to="/assets/$assetId" params={{ assetId: m.assetId }} size={FS.body}>{m.hostname}</AnchorLink></Table.Td>
                <Table.Td>
                  <Text size={FS.body} ff="monospace">{m.address}</Text>
                  {m.routes && m.routes.length > 0 && <Text size={FS.meta} c="dimmed" ff="monospace">routes {m.routes.join(', ')}</Text>}
                </Table.Td>
                <Table.Td><Text size={FS.body}>{n.shape === 'hub' ? (m.hub ? 'Hub' : 'Spoke') : 'Member'}</Text></Table.Td>
                <Table.Td><Text size={FS.body} ff="monospace" c={m.endpoint ? undefined : 'dimmed'}>{m.endpoint ? `${m.endpoint}:${n.port}` : 'Dials out only'}</Text></Table.Td>
                <Table.Td>
                  <Tooltip multiline w={300} label={m.message || st.hint}>
                    <Badge color={st.color} variant={st.color === 'rose' ? 'filled' : 'light'}>{st.label}</Badge>
                  </Tooltip>
                  {m.at && <Text span size={FS.meta} c="dimmed" ml={6}>{relTime(m.at)}</Text>}
                  {m.message && (m.state === 'failed' || m.state === 'rolled-back') && <Text size={FS.meta} c="rose">{m.message}</Text>}
                </Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      )}
      {removing && (
        <Modal opened onClose={() => setRemoving(false)} title={`Remove ${n.name}?`}>
          <RemoveNetwork n={n} onClose={() => setRemoving(false)} />
        </Modal>
      )}
    </Stack>
  )
}

function RemoveNetwork({ n, onClose }: { n: WGNetwork; onClose: () => void }) {
  const qc = useQueryClient()
  const remove = useMutation({ mutationFn: () => askWGRemove(n.id), onSettled: () => qc.invalidateQueries({ queryKey: ['networks'] }) })
  return (
    <Stack gap="md">
      <Text size={FS.body}>
        Once approved, every server takes down {n.iface}, deletes its file and closes UDP {n.port} where Argus opened it.
        Traffic between them over {n.cidr} stops.
      </Text>
      <Text size={FS.meta} c="dimmed"><AloneOr kind="runs" what="removing it">Another admin approves it first.</AloneOr></Text>
      <Group justify="flex-end">
        <Button variant="default" onClick={onClose}>Cancel</Button>
        <Button color="rose" loading={remove.isPending}
          onClick={() => void run(() => remove.mutateAsync(), { failure: 'Not asked', success: ['Asked', 'It waits for another admin to approve.'] }).then((ok) => { if (ok) onClose() })}>
          Ask to remove
        </Button>
      </Group>
    </Stack>
  )
}

/** The first /24 in 10.88–10.99 no network here uses: a starting point only, the control plane checks clashes. */
function suggestRange(taken: WGNetwork[]): string {
  const used = new Set(taken.flatMap((n) => [n.cidr, n.pending?.cidr]).filter(Boolean))
  for (let b = 88; b < 100; b++) if (!used.has(`10.${b}.0.0/24`)) return `10.${b}.0.0/24`
  return ''
}

/** A member as the editor holds it: the ranges as typed. */
type EditMember = WGMemberDraft & { routesText?: string }

function NetworkEditor({ n, taken, onClose }: { n: WGNetwork | null; taken: WGNetwork[]; onClose: () => void }) {
  const qc = useQueryClient()
  const { data: assets } = useQuery(assetsQuery())
  const servers = useMemo(() => (assets ?? []).filter((a) => !a.protocol || a.protocol === 'ssh'), [assets])
  const [name, setName] = useState(n?.name ?? '')
  const [shape, setShape] = useState<WGShape>(n?.shape ?? 'mesh')
  const [cidr, setCidr] = useState(n?.cidr ?? suggestRange(taken))
  const [port, setPort] = useState<number>(n?.port ?? 51820)
  const [members, setMembers] = useState<EditMember[]>(n?.members.map((m) => ({ assetId: m.assetId, hostname: m.hostname, address: m.address, hub: m.hub, endpoint: m.endpoint, routesText: (m.routes ?? []).join(', ') })) ?? [])
  const [reachable, setReachable] = useState(false)
  const save = useMutation({
    mutationFn: (d: WGNetworkDraft) => (n ? askWGChange(n.id, d) : askWGNetwork(d)),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['networks'] }),
  })
  const pick = (ids: string[]) =>
    setMembers(ids.map((id) => members.find((m) => m.assetId === id) ?? { assetId: id, hostname: servers.find((s) => s.id === id)?.hostname, endpoint: '' }))
  const setM = (id: string, change: Partial<EditMember>) => setMembers(members.map((m) => (m.assetId === id ? { ...m, ...change } : m)))
  const submit = () =>
    void run(() => save.mutateAsync({
      name: name.trim(), shape, cidr: cidr.trim(), port,
      members: members.map((m) => ({ assetId: m.assetId, address: m.address?.trim() || undefined, hub: shape === 'hub' && !!m.hub, endpoint: m.endpoint?.trim() || undefined,
        routes: (m.routesText ?? '').split(/[\s,]+/).filter(Boolean),
      })),
    }), { failure: 'Not asked', success: ['Asked', 'It waits for another admin to approve. Nothing changes on the servers before then.'] }).then((ok) => { if (ok) onClose() })
  return (
    <Modal opened onClose={onClose} title={n ? `Change ${n.name}` : 'New server network'} size="xl">
      <Stack gap="md">
        <Group grow align="flex-start">
          <TextInput label="Name" placeholder="Database mesh" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} />
          <TextInput label="Range" description="Private, /16 to /29, used by nothing else" value={cidr} onChange={(e) => setCidr(e.currentTarget.value)}
            styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
          <NumberInput label="UDP port" value={port} min={1024} max={65535} onChange={(v) => setPort(Number(v) || 51820)} />
        </Group>
        <Stack gap={4}>
          <Text size={FS.body} fw={500}>Shape</Text>
          <SegmentedControl value={shape} onChange={(v) => setShape(v as WGShape)} data={[{ value: 'mesh', label: 'Mesh' }, { value: 'hub', label: 'Hub' }]} w={220} />
          <Text size={FS.meta} c="dimmed">{SHAPE[shape].hint}</Text>
        </Stack>
        <MultiSelect label="Servers" placeholder="Pick two or more" searchable value={members.map((m) => m.assetId)} onChange={pick}
          data={servers.map((s) => ({ value: s.id, label: s.hostname }))} maxValues={64} />
        {members.length > 0 && (
          <Table withTableBorder>
            <Table.Thead><Table.Tr>
              <Table.Th>Server</Table.Th><Table.Th>Address in the range</Table.Th><Table.Th>Reached at</Table.Th><Table.Th>Site behind it</Table.Th>{shape === 'hub' && <Table.Th>Hub</Table.Th>}
            </Table.Tr></Table.Thead>
            <Table.Tbody>
              {members.map((m) => {
                const server = servers.find((s) => s.id === m.assetId)
                return (
                  <Table.Tr key={m.assetId}>
                    <Table.Td><Text size={FS.body}>{m.hostname ?? server?.hostname}</Text></Table.Td>
                    <Table.Td><TextInput size="xs" placeholder="Automatic" value={m.address ?? ''} onChange={(e) => setM(m.assetId, { address: e.currentTarget.value })} aria-label={`Address of ${m.hostname}`} /></Table.Td>
                    <Table.Td>
                      <TextInput size="xs" placeholder={server?.address ? `e.g. ${server.address}` : 'Public IP or hostname'} value={m.endpoint ?? ''}
                        onChange={(e) => setM(m.assetId, { endpoint: e.currentTarget.value })} aria-label={`Address others reach ${m.hostname} on`} />
                    </Table.Td>
                    <Table.Td>
                      <TextInput size="xs" placeholder="None" value={m.routesText ?? ''} onChange={(e) => setM(m.assetId, { routesText: e.currentTarget.value })}
                        aria-label={`Ranges ${m.hostname} routes`} />
                    </Table.Td>
                    {shape === 'hub' && <Table.Td><Radio checked={!!m.hub} onChange={() => setMembers(members.map((x) => ({ ...x, hub: x.assetId === m.assetId })))} aria-label={`${m.hostname} is the hub`} /></Table.Td>}
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        )}
        <Text size={FS.meta} c="dimmed">
          "Reached at" is the address the other servers dial, without the port. Leave it empty for a server behind NAT that only dials out:
          {shape === 'mesh' ? ' in a mesh, at most one server may do that.' : ' every spoke can, but the hub needs one.'} Argus opens the UDP port in ufw or firewalld on servers that are reached; open it in your cloud's security group or firewall too.
        </Text>
        {members.some((m) => (m.routesText ?? '').trim()) && (
          <Alert color="sky" p="xs" data-testid="wg-site-note">
            <Text size={FS.meta}>
              A server with a site behind it routes that range for the others, so it becomes the site's gateway. For the way back, the site needs a
              route to {cidr || 'this range'} through that server: on your office router, or in the cloud's route table. In AWS also turn off the
              instance's source/destination check; in Azure turn on IP forwarding on its network interface; in Google Cloud create it with IP forwarding.
            </Text>
          </Alert>
        )}
        <Checkbox checked={reachable} onChange={(e) => setReachable(e.currentTarget.checked)}
          label="I understand each server applies this on a 2-minute trial and undoes it if it can no longer reach Argus" />
        <Text size={FS.meta} c="dimmed"><AloneOr kind="runs" what="this network">Another admin approves it before any server changes.</AloneOr></Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={submit} loading={save.isPending} disabled={!name.trim() || !cidr.trim() || members.length < 2 || !reachable}>Ask for approval</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
