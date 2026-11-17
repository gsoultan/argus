import { useMemo, useState } from 'react'
import { Alert, Badge, Box, Button, Group, Loader, Modal, Select, Stack, Table, Text, Textarea, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconLock } from '@tabler/icons-react'
import { ButtonLink } from '~/components/links'
import { AloneOr } from '~/components/ApproveOwn'
import { relTime } from '~/components/primitives'
import { notifyError, notifyOk } from '~/lib/notify'
import { assetsQuery, databaseReplicasQuery, useAskReplica } from '~/lib/queries'
import {
  REPLICA_STATE_COLOR, isIP, replicaNameFor, replicaRefusal, replicaStage, serverAddresses, targetRefusal, validReplicaName,
} from '~/lib/managedReplicas'
import { assetName } from '~/lib/assetType'
import { FS } from '~/theme'
import type { Database, DatabaseReplica } from '~/types/domain'

const MONO = { input: { fontFamily: 'var(--mantine-font-family-monospace)' } }

/*
Streaming replicas of this database's Postgres on other servers Argus
manages (mockup 08, the managed-server half; db_replica.go). A replica copies
the whole server and takes reads; writes stay on the primary, and Argus never
makes a replica the primary. Asking is DBA level and waits for an approver;
the run then goes across both servers' agents and adds the replica to Argus.
*/

export function StreamingReplicas({ database, narrow }: { database: Database; narrow: boolean }) {
  const refusal = replicaRefusal(database)
  const { data, isLoading, error } = useQuery(databaseReplicasQuery(database.id, database.engine === 'postgres'))
  const [asking, setAsking] = useState(false)
  const rows = data ?? []
  return (
    <Box p="md">
      <Group justify="space-between" align="flex-start" mb="sm" wrap="nowrap" gap="md">
        <Text size={FS.meta} c="dimmed" maw={640}>
          A copy of this whole Postgres server on another server, streaming every change and taking reads. Writes stay on {database.hostname}.
          Argus checks {database.hostname}’s settings, adds one pg_hba.conf line and reloads if it must, and never restarts it.
        </Text>
        <Button size="xs" disabled={Boolean(refusal)} title={refusal ?? undefined} onClick={() => setAsking(true)} style={{ flexShrink: 0 }}>
          Add a read replica
        </Button>
      </Group>
      {refusal && <Alert color="slate" variant="light" p="xs" mb="sm"><Text size={FS.meta}>{refusal}.</Text></Alert>}
      {error && <Alert color="rose" variant="light" p="xs" mb="sm"><Text size={FS.meta}>{String((error as Error).message ?? error)}</Text></Alert>}
      {isLoading ? <Loader size={16} /> : rows.length === 0 ? (
        <Text size={FS.meta} c="dimmed">No streaming replicas of {database.hostname} were made from Argus.</Text>
      ) : (
        <Table verticalSpacing={6} fz={12} striped>
          <Table.Thead>
            <Table.Tr><Table.Th>Replica</Table.Th><Table.Th>Where it is</Table.Th>{!narrow && <Table.Th>Asked</Table.Th>}<Table.Th></Table.Th></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rows.map((r) => <ReplicaRow key={r.id} r={r} narrow={narrow} />)}
          </Table.Tbody>
        </Table>
      )}
      {asking && <AskReplica database={database} replicas={rows} onClose={() => setAsking(false)} />}
    </Box>
  )
}

function ReplicaRow({ r, narrow }: { r: DatabaseReplica; narrow: boolean }) {
  const words = r.state === 'running' && r.detail ? `${replicaStage(r)} · ${r.detail}` : replicaStage(r)
  return (
    <Table.Tr data-testid="streaming-replica">
      <Table.Td>
        <Text size={FS.meta} fw={600}>{r.hostname}</Text>
        <Text size={FS.micro} c="dimmed" ff="monospace" style={{ whiteSpace: 'nowrap' }}>{r.name}</Text>
        <Text size={FS.micro} c="dimmed" ff="monospace">{r.replicaAddress}</Text>
      </Table.Td>
      <Table.Td>
        <Group gap={6} wrap="nowrap" align="flex-start">
          <Badge size="sm" variant="light" radius="sm" color={REPLICA_STATE_COLOR[r.state]}
            leftSection={r.state === 'running' ? <Loader size={8} color="azure" /> : undefined}>{r.state}</Badge>
          <Box>
            <Text size={FS.meta} lineClamp={2}>{words}</Text>
            {r.state === 'done' && r.detail && <Text size={FS.micro} c="dimmed">{r.detail}</Text>}
            {r.error && <Text size={FS.micro} c={r.state === 'failed' ? 'rose' : 'dimmed'}>{r.error}</Text>}
          </Box>
        </Group>
      </Table.Td>
      {!narrow && (
        <Table.Td>
          <Text size={FS.micro}>{r.requestedBy} · {relTime(r.createdAt)}</Text>
          <Text size={FS.micro} c="dimmed" lineClamp={1}>{r.reason}</Text>
        </Table.Td>
      )}
      <Table.Td ta="right">
        {r.replicaDatabaseId && (
          <ButtonLink to="/databases/$dbId" params={{ dbId: r.replicaDatabaseId }} size="compact-xs" variant="light">Open</ButtonLink>
        )}
      </Table.Td>
    </Table.Tr>
  )
}

export function AskReplica({ database, replicas, onClose }: { database: Database; replicas: DatabaseReplica[]; onClose: () => void }) {
  const assets = useQuery(assetsQuery())
  const ask = useAskReplica()
  const all = assets.data ?? []
  const primary = all.find((a) => a.id === database.assetId)
  const options = useMemo(() => all
    .filter((a) => (a.kind ?? 'server') === 'server' && a.id !== database.assetId)
    .map((a) => ({ a, why: targetRefusal(a, database, replicas) }))
    .sort((x, y) => Number(Boolean(x.why)) - Number(Boolean(y.why)) || assetName(x.a).localeCompare(assetName(y.a))),
  [all, database, replicas])
  const [targetId, setTargetId] = useState<string | null>(null)
  const target = all.find((a) => a.id === targetId)
  const [name, setName] = useState('')
  const [named, setNamed] = useState(false)
  const [replicaAddr, setReplicaAddr] = useState('')
  const [primaryAddr, setPrimaryAddr] = useState('')
  const [reason, setReason] = useState('')

  const pick = (id: string | null) => {
    setTargetId(id)
    const t = all.find((a) => a.id === id)
    if (!t) return
    if (!named) setName(replicaNameFor(t.hostname))
    setReplicaAddr(serverAddresses(t)[0] ?? '')
  }
  const primaryDefault = primary ? serverAddresses(primary)[0] ?? primary.address : ''
  const primaryAt = primaryAddr.trim() || primaryDefault
  const nameError = name && !validReplicaName(name) ? 'A lowercase letter, then lowercase letters, digits and _, up to 40' : undefined
  const replicaError = replicaAddr.trim() && !isIP(replicaAddr.trim()) ? `An IP address, as ${database.hostname} sees ${target?.hostname ?? 'the replica'}` : undefined
  const ok = Boolean(target) && !targetRefusal(target!, database, replicas) && validReplicaName(name) && isIP(replicaAddr.trim())
    && Boolean(primaryAt) && reason.trim().length > 0

  const submit = async () => {
    try {
      await ask.mutateAsync({ id: database.id, assetId: targetId!, name, replicaAddress: replicaAddr.trim(),
        primaryAddress: primaryAddr.trim() || undefined, reason: reason.trim() })
      notifyOk('Sent to approvers', `Nothing changes on ${database.hostname} or ${target!.hostname} until an approver says yes. Progress shows here.`)
      onClose()
    } catch (err) {
      notifyError('Not asked', err)
    }
  }

  return (
    <Modal opened onClose={onClose} title={`Add a read replica of ${database.dbname}`} centered size="lg">
      <Stack gap="sm">
        <Text size={FS.meta} c="dimmed">Postgres on {database.hostname}. The replica copies the whole server, every database on it; writes stay where they are.</Text>
        <Select label="Replica server" placeholder={assets.isPending ? 'Reading servers…' : 'Pick a server'} searchable value={targetId} onChange={pick}
          description="An Argus server with the same Postgres major version installed and nothing of its own in it yet."
          nothingFoundMessage="No other servers"
          data={options.map(({ a, why }) => ({ value: a.id, label: why ? `${assetName(a)} · ${why}` : assetName(a), disabled: Boolean(why) }))} />
        <Group grow align="flex-start">
          <TextInput label="Replica name" value={name} styles={MONO} error={nameError} maxLength={40}
            description={name && validReplicaName(name) ? `Role argus_repl_${name}, slot argus_replica_${name}` : ' '}
            onChange={(e) => { setName(e.currentTarget.value.toLowerCase()); setNamed(true) }} />
          <AddressInput label={`Replica’s address, as ${database.hostname} sees it`} value={replicaAddr} onChange={setReplicaAddr}
            choices={target ? serverAddresses(target) : []} error={replicaError} />
        </Group>
        <AddressInput label={`${database.hostname}’s address, as the replica reaches it`} value={primaryAddr} onChange={setPrimaryAddr}
          placeholder={primaryDefault} choices={primary ? serverAddresses(primary) : []}
          description={primaryAddr ? undefined : `Leave empty for ${primaryDefault || 'the address Argus has'}`} />
        <Textarea label="Why (approvers see this)" value={reason} onChange={(e) => setReason(e.currentTarget.value)} autosize minRows={2}
          maxLength={500} placeholder="Reports read the whole orders table every hour; take those reads off the primary" />

        <Box>
          <Text size={FS.body} fw={600} mb={4}>What happens, once approved</Text>
          <Stack gap={2}>
            <Text size={FS.meta}>1 · Argus checks {database.hostname}: wal_level, free WAL senders and slots, listen_addresses. One that needs a restart stops it here, with the value to set.</Text>
            <Text size={FS.meta}>2 · The agent on {target?.hostname ?? 'the replica server'} checks its Postgres is the same major version and holds nothing of its own.</Text>
            <Text size={FS.meta}>3 · On {database.hostname}: replication role <Text span ff="monospace" size={FS.meta}>argus_repl_{name || '…'}</Text> and slot <Text span ff="monospace" size={FS.meta}>argus_replica_{name || '…'}</Text>, and one pg_hba.conf line with a reload if no rule lets the replica in.</Text>
            <Text size={FS.meta}>4 · {target?.hostname ?? 'The replica server'} stops its Postgres, keeps its empty data directory aside, copies with pg_basebackup and starts as a replica.</Text>
            <Text size={FS.meta}>5 · It is added to Argus as {database.dbname} on {target?.hostname ?? 'that server'}, with {database.dbname}’s access made read.</Text>
          </Stack>
        </Box>
        <Alert color="teal" variant="light" p="xs" title="No password stored">
          <Text size={FS.meta}>The replication password is made on the replica server and stays there. {database.hostname} only gets its SCRAM verifier, and Argus keeps neither.</Text>
        </Alert>
        <Alert color="amber" variant="light" p="xs" icon={<IconLock size={14} />}>
          <Text size={FS.meta}>Needs an approver: it changes {database.hostname} and replaces {target?.hostname ?? 'the replica server'}’s Postgres data. If it fails, the role, slot and pg_hba.conf line come off {database.hostname} again.</Text>
        </Alert>
        {target && !target.tunnelConnected && <Alert color="rose" variant="light" p="xs">The agent on {target.hostname} is not connected.</Alert>}
        <Group justify="space-between">
          <Group gap="xs">
            <Button onClick={() => void submit()} loading={ask.isPending} disabled={!ok}>Ask to add replica</Button>
            <Button variant="default" onClick={onClose}>Cancel</Button>
          </Group>
          <Text size={FS.micro} c="dimmed"><AloneOr kind="requests" what="the replica">Approvers · 60 min to decide</AloneOr></Text>
        </Group>
        <Text size={FS.micro} c="dimmed">Failover, making a replica the primary, is not part of this.</Text>
      </Stack>
    </Modal>
  )
}

function AddressInput({ label, value, onChange, choices, error, placeholder, description }: {
  label: string; value: string; onChange: (v: string) => void; choices: string[]; error?: string; placeholder?: string; description?: string
}) {
  return (
    <Box>
      <TextInput label={label} value={value} onChange={(e) => onChange(e.currentTarget.value)} styles={MONO} error={error}
        placeholder={placeholder} description={description} />
      {choices.length > 1 && (
        <Group gap={4} mt={4}>
          {choices.slice(0, 4).map((c) => (
            <Button key={c} size="compact-xs" variant={c === value ? 'light' : 'subtle'} ff="monospace" onClick={() => onChange(c)}>{c}</Button>
          ))}
        </Group>
      )}
    </Box>
  )
}

