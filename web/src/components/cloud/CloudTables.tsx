import { Badge, Group, SegmentedControl, Stack, Table, Text, Tooltip } from '@mantine/core'
import { IconArrowsSplit, IconCamera, IconDeviceFloppy, IconNetwork, IconPhoto, IconServer2 } from '@tabler/icons-react'
import { useState } from 'react'
import { DataTable, EmptyState } from '~/components/page'
import { relTime, rowNav } from '~/components/primitives'
import { openToInternet, scheduleOf, type CloudItem } from '~/lib/cloud'
import { FS, INK } from '~/theme'
import { NameCell, ResourceDrawer, ServerCell, StateBadge, WhereCell } from './common'

interface TableProps {
  items?: CloudItem[]
  loading: boolean
  names: Map<string, string>
  filtered: boolean
}

function useDrawer(names: Map<string, string>) {
  const [open, setOpen] = useState<CloudItem | null>(null)
  return { show: setOpen, drawer: <ResourceDrawer it={open} onClose={() => setOpen(null)} names={names} /> }
}

function emptyText(filtered: boolean, what: string) {
  return filtered ? `No ${what} match the account or search.` : `Argus has not found any ${what} in the connected accounts.`
}

export function InstancesTable({ items, loading, names, filtered }: TableProps) {
  const { show, drawer } = useDrawer(names)
  return (
    <>
      <DataTable minWidth={980} loading={loading} isEmpty={(items?.length ?? 0) === 0}
        empty={<EmptyState icon={IconServer2} title="No instances" description={emptyText(filtered, 'instances')} />}
        columns={['Instance', 'Account and zone', 'Type', 'State', 'Schedule', 'Addresses', 'Argus server', 'Launched']}>
        {items?.map((it) => {
          const r = it.resource
          return (
            <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
              <Table.Td maw={260}><NameCell r={r} /></Table.Td>
              <Table.Td><WhereCell it={it} /></Table.Td>
              <Table.Td><Text size={FS.meta} ff="monospace">{r.size}</Text></Table.Td>
              <Table.Td><StateBadge state={r.state} /></Table.Td>
              <Table.Td>
                {scheduleOf(r.tags) ? <Text size={FS.meta} ff="monospace">{scheduleOf(r.tags)}</Text> : <Text size={FS.meta} c="dimmed">—</Text>}
              </Table.Td>
              <Table.Td>
                <Text size={FS.meta} ff="monospace">{r.addresses?.[0] ?? '—'}</Text>
                {r.publicIp && <Text size={FS.micro} c="dimmed" ff="monospace">{r.publicIp} public</Text>}
              </Table.Td>
              <Table.Td><ServerCell it={it} /></Table.Td>
              <Table.Td><Text size={FS.meta} c="dimmed">{relTime(r.createdAt ?? null)}</Text></Table.Td>
            </Table.Tr>
          )
        })}
      </DataTable>
      {drawer}
    </>
  )
}

function DisksTable({ items, loading, names, filtered, show }: TableProps & { show: (it: CloudItem) => void }) {
  return (
    <>
      <DataTable minWidth={820} loading={loading} isEmpty={(items?.length ?? 0) === 0}
        empty={<EmptyState icon={IconDeviceFloppy} title="No disks" description={emptyText(filtered, 'disks')} />}
        columns={['Disk', 'Account and zone', 'Size', 'State', 'Attached to', 'Encrypted']}>
        {items?.map((it) => {
          const r = it.resource
          const att = r.links?.find((l) => l.kind === 'instance')
          return (
            <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
              <Table.Td maw={260}><NameCell r={r} /></Table.Td>
              <Table.Td><WhereCell it={it} /></Table.Td>
              <Table.Td><Text size={FS.meta} ff="monospace">{r.size}</Text></Table.Td>
              <Table.Td>
                {r.state === 'available'
                  ? <Tooltip label="Not attached to any instance, and still billed"><Badge variant="light" color="amber" radius="sm">Not attached</Badge></Tooltip>
                  : <StateBadge state={r.state} />}
              </Table.Td>
              <Table.Td>
                {att ? <><Text size={FS.meta}>{names.get(att.id) ?? att.id}</Text><Text size={FS.micro} c="dimmed" ff="monospace">{att.name}</Text></> : <Text size={FS.meta} c="dimmed">—</Text>}
              </Table.Td>
              <Table.Td>
                <Text size={FS.meta} c={r.details?.encrypted === 'yes' ? undefined : INK.pending}>{r.details?.encrypted === 'yes' ? 'Yes' : 'No'}</Text>
              </Table.Td>
            </Table.Tr>
          )
        })}
      </DataTable>
    </>
  )
}

type StorageView = 'disks' | 'snapshots' | 'images'

/** Disks, and the snapshots and images made from them. */
export function StorageTables({ volumes, snapshots, images, loading, names, filtered }: {
  volumes?: CloudItem[]
  snapshots?: CloudItem[]
  images?: CloudItem[]
  loading: boolean
  names: Map<string, string>
  filtered: boolean
}) {
  const [view, setView] = useState<StorageView>('disks')
  const { show, drawer } = useDrawer(names)
  const imageOf = new Map<string, string>()
  for (const im of images ?? []) for (const l of im.resource.links ?? []) if (l.kind === 'snapshot') imageOf.set(l.id, im.resource.name)
  return (
    <Stack gap="md">
      <SegmentedControl value={view} onChange={(v) => setView(v as StorageView)} w="fit-content"
        data={[
          { value: 'disks', label: `Disks${volumes ? ` · ${volumes.length}` : ''}` },
          { value: 'snapshots', label: `Snapshots${snapshots ? ` · ${snapshots.length}` : ''}` },
          { value: 'images', label: `Images${images ? ` · ${images.length}` : ''}` },
        ]} />
      {view === 'disks' && <DisksTable items={volumes} loading={loading} names={names} filtered={filtered} show={show} />}
      {view === 'snapshots' && (
        <DataTable minWidth={820} loading={loading} isEmpty={(snapshots?.length ?? 0) === 0}
          empty={<EmptyState icon={IconCamera} title="No snapshots" description={emptyText(filtered, 'snapshots owned by these accounts')} />}
          columns={['Snapshot', 'Account and region', 'Size', 'State', 'Of disk', 'Used by', 'Taken']}>
          {snapshots?.map((it) => {
            const r = it.resource
            const of = r.links?.find((l) => l.kind === 'volume')
            const used = imageOf.get(r.id)
            return (
              <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
                <Table.Td maw={260}><NameCell r={r} /></Table.Td>
                <Table.Td><WhereCell it={it} /></Table.Td>
                <Table.Td><Text size={FS.meta} ff="monospace">{r.size}</Text></Table.Td>
                <Table.Td>
                  {r.state === 'pending'
                    ? <Badge variant="light" color="sky" radius="sm">Taking {r.details?.progress ?? ''}</Badge>
                    : <StateBadge state={r.state} />}
                </Table.Td>
                <Table.Td><Text size={FS.meta} c={of ? undefined : 'dimmed'}>{of ? names.get(of.id) ?? of.id : '—'}</Text></Table.Td>
                <Table.Td>{used ? <Text size={FS.meta}>Image {used}</Text> : <Text size={FS.meta} c="dimmed">—</Text>}</Table.Td>
                <Table.Td><Text size={FS.meta} c="dimmed">{relTime(r.createdAt ?? null)}</Text></Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      )}
      {view === 'images' && (
        <DataTable minWidth={760} loading={loading} isEmpty={(images?.length ?? 0) === 0}
          empty={<EmptyState icon={IconPhoto} title="No images" description={emptyText(filtered, 'images owned by these accounts')} />}
          columns={['Image', 'Account and region', 'Size', 'State', 'Shared', 'Made']}>
          {images?.map((it) => {
            const r = it.resource
            return (
              <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
                <Table.Td maw={260}><NameCell r={r} /></Table.Td>
                <Table.Td><WhereCell it={it} /></Table.Td>
                <Table.Td><Text size={FS.meta} ff="monospace">{r.size ?? '—'}</Text></Table.Td>
                <Table.Td><StateBadge state={r.state} /></Table.Td>
                <Table.Td>
                  {r.details?.public === 'yes'
                    ? <Tooltip label="Anyone with an AWS account can launch it, and read what is on its disks"><Badge variant="light" color="rose" radius="sm">Public</Badge></Tooltip>
                    : <Text size={FS.meta} c="dimmed">This account</Text>}
                </Table.Td>
                <Table.Td><Text size={FS.meta} c="dimmed">{relTime(r.createdAt ?? null)}</Text></Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      )}
      {drawer}
    </Stack>
  )
}

type NetView = 'firewalls' | 'networks' | 'addresses' | 'balancers' | 'targets'

const TG_COLOR: Record<string, string> = { healthy: 'teal', degraded: 'rose', empty: 'amber', unused: 'slate' }

export function NetworkTables({ firewalls, networks, subnets, addresses, balancers, targetGroups, loading, names, filtered }: {
  firewalls?: CloudItem[]
  networks?: CloudItem[]
  subnets?: CloudItem[]
  addresses?: CloudItem[]
  balancers?: CloudItem[]
  targetGroups?: CloudItem[]
  loading: boolean
  names: Map<string, string>
  filtered: boolean
}) {
  const [view, setView] = useState<NetView>('firewalls')
  const { show, drawer } = useDrawer(names)
  return (
    <Stack gap="md">
      <SegmentedControl value={view} onChange={(v) => setView(v as NetView)} w="fit-content"
        data={[
          { value: 'firewalls', label: `Firewalls${firewalls ? ` · ${firewalls.length}` : ''}` },
          { value: 'networks', label: `Networks${networks ? ` · ${networks.length}` : ''}` },
          { value: 'addresses', label: `Elastic IPs${addresses ? ` · ${addresses.length}` : ''}` },
          { value: 'balancers', label: `Load balancers${balancers ? ` · ${balancers.length}` : ''}` },
          { value: 'targets', label: `Target groups${targetGroups ? ` · ${targetGroups.length}` : ''}` },
        ]} />
      {view === 'firewalls' && (
        <DataTable minWidth={760} loading={loading} isEmpty={(firewalls?.length ?? 0) === 0}
          empty={<EmptyState icon={IconNetwork} title="No firewalls" description={emptyText(filtered, 'security groups')} />}
          columns={['Security group', 'Account and region', 'Network', 'Rules', 'Exposure']}>
          {firewalls?.map((it) => {
            const r = it.resource
            const open = openToInternet(r)
            const inbound = r.rules?.filter((x) => x.inbound).length ?? 0
            return (
              <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
                <Table.Td maw={260}><NameCell r={r} /></Table.Td>
                <Table.Td><WhereCell it={it} /></Table.Td>
                <Table.Td><Text size={FS.meta} ff="monospace">{names.get(r.network ?? '') ?? r.network ?? '—'}</Text></Table.Td>
                <Table.Td><Text size={FS.meta}>{inbound} in · {(r.rules?.length ?? 0) - inbound} out</Text></Table.Td>
                <Table.Td>
                  {open.length > 0
                    ? <Badge variant="light" color="rose" radius="sm">{open.join(', ')} from anywhere</Badge>
                    : <Text size={FS.meta} c="dimmed">Nothing risky from the internet</Text>}
                </Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      )}
      {view === 'networks' && (
        <DataTable minWidth={720} loading={loading} isEmpty={(networks?.length ?? 0) === 0}
          empty={<EmptyState icon={IconNetwork} title="No networks" description={emptyText(filtered, 'VPCs')} />}
          columns={['Network', 'Account and region', 'Address range', 'Subnets']}>
          {networks?.map((it) => {
            const r = it.resource
            const subs = subnets?.filter((s) => s.accountId === it.accountId && s.resource.network === r.id) ?? []
            return (
              <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
                <Table.Td maw={260}>
                  <Group gap={6} wrap="nowrap"><NameCell r={r} />{r.details?.default === 'yes' && <Badge variant="default" radius="sm" tt="none" fw={500}>Default</Badge>}</Group>
                </Table.Td>
                <Table.Td><WhereCell it={it} /></Table.Td>
                <Table.Td><Text size={FS.meta} ff="monospace">{r.size}</Text></Table.Td>
                <Table.Td>
                  <Text size={FS.meta}>{subs.length}</Text>
                  <Text size={FS.micro} c="dimmed" truncate maw={320}>{subs.map((s) => `${s.resource.name} ${s.resource.size}`).join(' · ')}</Text>
                </Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      )}
      {view === 'addresses' && (
        <DataTable minWidth={640} loading={loading} isEmpty={(addresses?.length ?? 0) === 0}
          empty={<EmptyState icon={IconNetwork} title="No Elastic IPs" description={emptyText(filtered, 'Elastic IPs')} />}
          columns={['Address', 'Account and region', 'Attached to']}>
          {addresses?.map((it) => {
            const r = it.resource
            const to = r.links?.find((l) => l.kind === 'instance')
            return (
              <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
                <Table.Td><Text fw={600} size={FS.body} ff="monospace">{r.publicIp}</Text><Text size={FS.micro} c="dimmed">{r.name !== r.publicIp ? r.name : r.id}</Text></Table.Td>
                <Table.Td><WhereCell it={it} /></Table.Td>
                <Table.Td>
                  {to ? <Text size={FS.meta}>{names.get(to.id) ?? to.id}</Text>
                    : r.state === 'attached' ? <Text size={FS.meta} c="dimmed">A network interface</Text>
                      : <Tooltip label="Billed while it is not attached"><Badge variant="light" color="amber" radius="sm">Not attached</Badge></Tooltip>}
                </Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      )}
      {view === 'balancers' && (
        <DataTable minWidth={820} loading={loading} isEmpty={(balancers?.length ?? 0) === 0}
          empty={<EmptyState icon={IconArrowsSplit} title="No load balancers" description={emptyText(filtered, 'application or network load balancers')} />}
          columns={['Load balancer', 'Account and region', 'Kind', 'Reached from', 'Target groups', 'State']}>
          {balancers?.map((it) => {
            const r = it.resource
            const tgs = (targetGroups ?? []).filter((t) => t.accountId === it.accountId && r.links?.some((l) => l.kind === 'target-group' && l.id === t.resource.id))
            return (
              <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
                <Table.Td maw={260}><Text fw={600} size={FS.body} truncate>{r.name}</Text><Text size={FS.micro} c="dimmed" ff="monospace" truncate>{r.details?.dnsName}</Text></Table.Td>
                <Table.Td><WhereCell it={it} /></Table.Td>
                <Table.Td><Text size={FS.meta} tt="capitalize">{r.size}</Text></Table.Td>
                <Table.Td>
                  {r.details?.scheme === 'internet-facing'
                    ? <Badge variant="light" color="sky" radius="sm">The internet</Badge>
                    : <Text size={FS.meta} c="dimmed">Inside the network</Text>}
                </Table.Td>
                <Table.Td>
                  {tgs.length === 0 ? <Text size={FS.meta} c="dimmed">—</Text> : (
                    <Group gap={4}>
                      {tgs.map((t) => (
                        <Badge key={t.resource.id} variant="light" color={TG_COLOR[t.resource.state ?? ''] ?? 'slate'} radius="sm" tt="none" fw={500}>
                          {t.resource.name} · {t.resource.details?.healthy ?? 0}/{t.resource.details?.targets ?? 0}
                        </Badge>
                      ))}
                    </Group>
                  )}
                </Table.Td>
                <Table.Td><StateBadge state={r.state} /></Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      )}
      {view === 'targets' && (
        <DataTable minWidth={760} loading={loading} isEmpty={(targetGroups?.length ?? 0) === 0}
          empty={<EmptyState icon={IconArrowsSplit} title="No target groups" description={emptyText(filtered, 'target groups')} />}
          columns={['Target group', 'Account and region', 'Protocol and port', 'Load balancer', 'Healthy']}>
          {targetGroups?.map((it) => {
            const r = it.resource
            const lbs = (r.links ?? []).filter((l) => l.kind === 'load-balancer')
            return (
              <Table.Tr key={it.accountId + r.id} {...rowNav(() => show(it))}>
                <Table.Td maw={260}><Text fw={600} size={FS.body} truncate>{r.name}</Text><Text size={FS.micro} c="dimmed">{r.details?.targetType ?? ''} targets</Text></Table.Td>
                <Table.Td><WhereCell it={it} /></Table.Td>
                <Table.Td><Text size={FS.meta} ff="monospace">{r.size}</Text></Table.Td>
                <Table.Td><Text size={FS.meta} c={lbs.length ? undefined : 'dimmed'}>{lbs.length ? lbs.map((l) => l.name ?? l.id).join(', ') : 'None'}</Text></Table.Td>
                <Table.Td>
                  <Badge variant="light" color={TG_COLOR[r.state ?? ''] ?? 'slate'} radius="sm">
                    {r.state === 'unused' ? 'Not in use' : `${r.details?.healthy ?? 0} of ${r.details?.targets ?? 0}`}
                  </Badge>
                </Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      )}
      {drawer}
    </Stack>
  )
}
