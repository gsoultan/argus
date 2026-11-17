import { Alert, Badge, Box, Button, Group, Select, Stack, Table, Text, TextInput, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconDatabase } from '@tabler/icons-react'
import { Fragment, useState } from 'react'
import { DataTable, EmptyState, SectionCard } from '~/components/page'
import { cloudAccountsQuery, cloudItemsQuery, roleLacks, type CloudItem } from '~/lib/cloud'
import { engineWords, nextReplicaName, replicaFamilies, replicaProblem, validDBName } from '~/lib/cloudReplicas'
import { FS } from '~/theme'
import { ChangeModal, useSubmit } from './ChangeModal'
import { RoleNeedsUpdate } from './StorageActions'

/*
 * Read replicas of RDS databases on Cloud › Scaling (mockup 08, the RDS
 * half). A replica takes reads off a busy primary; writes stay where they
 * are, and Argus never promotes one. Adding is Change level and removing is
 * Remove level, each with the environment's approval, and only a replica
 * Argus made can be removed from here.
 */

const STATE_COLOR: Record<string, string> = { available: 'teal', creating: 'azure', deleting: 'amber', failed: 'rose' }

function StateBadge({ state }: { state?: string }) {
  return <Badge variant="light" radius="sm" color={STATE_COLOR[state ?? ''] ?? 'slate'}>{state ?? 'unknown'}</Badge>
}

export function ReadReplicasCard({ items, loading, canChange }: { items?: CloudItem[]; loading?: boolean; canChange: boolean }) {
  const accounts = useQuery(cloudAccountsQuery())
  const manage = new Set((accounts.data?.accounts ?? []).filter((a) => a.access === 'manage').map((a) => a.id))
  const families = replicaFamilies(items)
  const [adding, setAdding] = useState<CloudItem | null>(null)
  const [removing, setRemoving] = useState<CloudItem | null>(null)
  return (
    <SectionCard title="Read replicas" icon={IconDatabase} flush
      description="RDS databases and the copies that take their reads. A replica streams from its primary; writes stay on the primary, and Argus never makes a replica the primary.">
      <DataTable minWidth={760} loading={loading} isEmpty={families.length === 0}
        empty={<EmptyState icon={IconDatabase} title="No RDS databases read"
          description="Databases appear here once the account's read role has read RDS in its regions." />}
        columns={['Database', 'Account', 'Class', 'State', '']}>
        {families.map(({ primary, replicas }) => {
          const problem = replicaProblem(primary)
          const can = canChange && manage.has(primary.accountId)
          return (
            <Fragment key={primary.accountId + primary.resource.id}>
              <Table.Tr>
                <Table.Td>
                  <Text fw={600} size={FS.body}>{primary.resource.name}</Text>
                  <Text size={FS.micro} c="dimmed">
                    {engineWords(primary)}{replicas.length > 0 ? ` · ${replicas.length} replica${replicas.length === 1 ? '' : 's'}` : ''}
                  </Text>
                </Table.Td>
                <Table.Td><Text size={FS.meta}>{primary.accountName}</Text><Text size={FS.micro} c="dimmed">{primary.resource.region}</Text></Table.Td>
                <Table.Td><Text size={FS.meta}>{primary.resource.size}</Text></Table.Td>
                <Table.Td><StateBadge state={primary.resource.state} /></Table.Td>
                <Table.Td align="right">
                  {can && (
                    <Tooltip label={problem} disabled={!problem} multiline w={280}>
                      <Box component="span">
                        <Button size="xs" variant="light" disabled={!!problem} onClick={() => setAdding(primary)}>Add a read replica</Button>
                      </Box>
                    </Tooltip>
                  )}
                </Table.Td>
              </Table.Tr>
              {replicas.map((r) => (
                <Table.Tr key={r.accountId + r.resource.id} data-testid="replica-row">
                  <Table.Td pl={36}>
                    <Text size={FS.body}>↳ {r.resource.name}</Text>
                    <Text size={FS.micro} c="dimmed">Replica of {primary.resource.name}{r.madeByArgus ? ' · made by Argus' : ''}</Text>
                  </Table.Td>
                  <Table.Td><Text size={FS.micro} c="dimmed">{r.resource.zone ?? r.resource.region}</Text></Table.Td>
                  <Table.Td><Text size={FS.meta}>{r.resource.size}</Text></Table.Td>
                  <Table.Td><StateBadge state={r.resource.state} /></Table.Td>
                  <Table.Td align="right">
                    {canChange && manage.has(r.accountId) && r.madeByArgus && r.resource.state !== 'deleting' && (
                      <Button size="xs" variant="subtle" color="rose" onClick={() => setRemoving(r)}>Remove</Button>
                    )}
                  </Table.Td>
                </Table.Tr>
              ))}
            </Fragment>
          )
        })}
      </DataTable>
      {adding && <NewReplica primary={adding} items={items} onClose={() => setAdding(null)} />}
      {removing && <RemoveReplica it={removing} onClose={() => setRemoving(null)} />}
    </SectionCard>
  )
}

export function NewReplica({ primary, items, onClose }: { primary: CloudItem; items?: CloudItem[]; onClose: () => void }) {
  const accounts = useQuery(cloudAccountsQuery())
  const subnets = useQuery(cloudItemsQuery('subnet'))
  const account = accounts.data?.accounts.find((a) => a.id === primary.accountId)
  const p = primary.resource
  const zones = [...new Set((subnets.data ?? []).filter((s) => s.accountId === primary.accountId && s.resource.region === p.region && s.resource.zone)
    .map((s) => s.resource.zone!))].sort()
  const [name, setName] = useState(() => nextReplicaName(primary, items))
  const [size, setSize] = useState(p.size ?? '')
  const [zone, setZone] = useState<string>('')
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const taken = (items ?? []).some((it) => it.accountId === primary.accountId && it.resource.region === p.region && it.resource.name.toLowerCase() === name.trim().toLowerCase())
  const nameError = !validDBName(name.trim()) ? 'A letter, then letters, digits and single hyphens, up to 63'
    : taken ? `${p.region} already has a database named ${name.trim()}` : undefined
  const sizeError = /^db\.[a-z0-9-]+\.[a-z0-9]+$/.test(size.trim()) ? undefined : 'A database class, like db.r6i.large'
  const storage = p.details?.storage_gb
  return (
    <ChangeModal title={`Add a read replica of ${p.name}`} env={primary.environment} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Add replica" disabled={!!nameError || !!sizeError || roleLacks(account, 'replica-create')}
      onSubmit={() => void submit({ accountId: primary.accountId, region: p.region, action: 'replica-create', reason,
        spec: { source: p.name, name: name.trim(), size: size.trim(), zone: zone || undefined } }, 'Read replica asked for')}>
      <RoleNeedsUpdate account={account} action="replica-create" />
      <Text size={FS.meta} c="dimmed">{engineWords(primary)} in {primary.accountName}, {p.region}. Writes stay on {p.name}.</Text>
      <Group grow align="flex-start">
        <TextInput label="Replica name" value={name} onChange={(e) => setName(e.currentTarget.value)} error={nameError} maxLength={63} />
        <TextInput label="Class" description={size.trim() === p.size ? 'Same as the primary' : `The primary is ${p.size}`}
          value={size} onChange={(e) => setSize(e.currentTarget.value)} error={sizeError} />
      </Group>
      <Select label="Zone" value={zone} onChange={(v) => setZone(v ?? '')} allowDeselect={false}
        description={p.zone ? `The primary is in ${p.zone}; another zone keeps reads going if that one has trouble.` : undefined}
        data={[{ value: '', label: 'Let AWS pick' }, ...zones.map((z) => ({ value: z, label: z === p.zone ? `${z} (the primary's)` : z }))]} />
      <Alert color="azure" variant="light" p="sm">
        <Text size={FS.meta}>
          AWS copies {p.name}'s latest backup{storage ? ` (${storage} GiB)` : ''} and then streams every change to the replica, which takes some
          minutes, longer for a large database. The replica gets its own endpoint and bills like a database of its own: the class above,
          plus its storage. Deletion protection is on, so only a removal asked here, or one in the AWS console, deletes it.
        </Text>
      </Alert>
      <Text size={FS.meta} c="dimmed">
        To open the replica from Argus, add it as a database from a server whose PgBouncer sends to its endpoint. It then takes {p.name}'s
        access in Argus, with write and owner made read.
      </Text>
    </ChangeModal>
  )
}

export function RemoveReplica({ it, onClose }: { it: CloudItem; onClose: () => void }) {
  const accounts = useQuery(cloudAccountsQuery())
  const account = accounts.data?.accounts.find((a) => a.id === it.accountId)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const r = it.resource
  const of = r.details?.replica_of ?? 'its primary'
  return (
    <ChangeModal title={`Remove ${r.name}`} env={it.environment} level="remove" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Remove replica" color="rose" disabled={roleLacks(account, 'replica-remove')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'replica-remove', resourceId: r.id, reason }, 'Replica removed')}>
      <RoleNeedsUpdate account={account} action="replica-remove" />
      <Stack gap={6}>
        <Text size={FS.body}>Turns off deletion protection on {r.name} and deletes it. {of} and its data are not touched.</Text>
        <Text size={FS.meta} c="dimmed">
          Anything reading from {r.name}'s endpoint stops working. A replica holds nothing the primary does not, so no final snapshot is taken.
        </Text>
      </Stack>
    </ChangeModal>
  )
}
