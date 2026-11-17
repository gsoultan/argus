import { useMemo, useState } from 'react'
import { Alert, Badge, Box, Button, Checkbox, Group, Loader, Modal, Paper, Select, Stack, Table, Text, Textarea, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconLock } from '@tabler/icons-react'
import { ButtonLink } from '~/components/links'
import { AloneOr } from '~/components/ApproveOwn'
import { bytes, relTime } from '~/components/primitives'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  assetsQuery, databaseReplicasQuery, meQuery, readEndpointQuery, useAskReplica, useRemoveReadEndpoint, useSetReadEndpoint,
} from '~/lib/queries'
import {
  REPLICA_STATE_COLOR, endpointStatus, isIP, replicaHealth, replicaNameFor, replicaRefusal, replicaStage, serverAddresses, targetRefusal, validReplicaName,
} from '~/lib/managedReplicas'
import { assetName } from '~/lib/assetType'
import { FS } from '~/theme'
import { canSee } from '~/lib/roles'
import type { Database, DatabaseReplica, ReadEndpoint, ReadEndpointChoice } from '~/types/domain'

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
  const endpoint = useQuery(readEndpointQuery(database.id, database.engine === 'postgres'))
  const [asking, setAsking] = useState(false)
  const rows = data ?? []
  const ep = endpoint.data?.endpoint ?? null
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
      {(ep || rows.some((r) => r.state === 'done')) && (
        <ReadEndpointSection database={database} endpoint={ep} choices={endpoint.data?.choices ?? []} />
      )}
      {isLoading ? <Loader size={16} /> : rows.length === 0 ? (
        <Text size={FS.meta} c="dimmed">No streaming replicas of {database.hostname} were made from Argus.</Text>
      ) : (
        <Table verticalSpacing={6} fz={12} striped>
          <Table.Thead>
            <Table.Tr><Table.Th>Replica</Table.Th><Table.Th>Where it is</Table.Th>{!narrow && <Table.Th>Asked</Table.Th>}<Table.Th></Table.Th></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rows.map((r) => <ReplicaRow key={r.id} r={r} narrow={narrow} endpoint={ep} />)}
          </Table.Tbody>
        </Table>
      )}
      {asking && <AskReplica database={database} replicas={rows} onClose={() => setAsking(false)} />}
    </Box>
  )
}

function ReplicaRow({ r, narrow, endpoint }: { r: DatabaseReplica; narrow: boolean; endpoint: ReadEndpoint | null }) {
  const words = r.state === 'running' && r.detail ? `${replicaStage(r)} · ${r.detail}` : replicaStage(r)
  const health = replicaHealth(r)
  const inEndpoint = endpointStatus(r, endpoint)
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
            {!health && <Text size={FS.meta} lineClamp={2}>{words}</Text>}
            {health && (
              <Text size={FS.meta} c={health.color === 'teal' ? undefined : health.color} fw={health.color === 'teal' ? undefined : 600}>
                {health.words}{r.watchedAt ? ` · as of ${relTime(r.watchedAt)}` : ''}
              </Text>
            )}
            {health && !r.streaming && r.retainedBytes !== undefined && r.retainedBytes > 0 && (
              <Text size={FS.micro} c="amber">Its slot keeps {bytes(r.retainedBytes)} of WAL on {r.primaryHostname}, growing until the replica is back.</Text>
            )}
            {inEndpoint && <Text size={FS.micro} c={inEndpoint.color} data-testid="replica-endpoint">{inEndpoint.words}</Text>}
            {r.state === 'done' && r.watchError && <Text size={FS.micro} c="dimmed">Last check failed: {r.watchError}</Text>}
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

/*
The read endpoint (cloud scaling 8c-2; db_read_endpoint.go): a target group
of a network load balancer that Argus keeps to the primary, if asked, and
the replicas that are not behind. Setting it is an admin's, on top of DBA
access to the database; the server says no to anyone else as well.
*/
function ReadEndpointSection({ database, endpoint, choices }: {
  database: Database; endpoint: ReadEndpoint | null; choices: ReadEndpointChoice[]
}) {
  const { data: me } = useQuery(meQuery())
  const admin = canSee(me?.role, 'admin')
  const remove = useRemoveReadEndpoint()
  const [editing, setEditing] = useState(false)
  const usable = choices.filter((c) => !c.problem)
  const why = !admin ? 'Setting a read endpoint needs the admin or owner role'
    : usable.length === 0 ? `No target group in a connected account takes TCP on port ${database.port} to instances` : undefined
  const drop = async () => {
    try {
      await remove.mutateAsync({ id: database.id })
      notifyOk('Read endpoint removed', `What Argus put in ${endpoint!.targetName} came out; anything put in by hand stays.`)
    } catch (err) {
      notifyError('Not removed', err)
    }
  }
  const primaryHealth = endpoint?.primaryInstance ? endpoint.health[endpoint.primaryInstance] : undefined
  return (
    <Paper withBorder p="sm" mb="sm" radius="md" data-testid="read-endpoint">
      <Group justify="space-between" align="flex-start" wrap="nowrap" gap="md">
        <Box>
          <Text size={FS.body} fw={600}>Read endpoint</Text>
          {endpoint ? (
            <>
              <Text size={FS.meta}>
                Target group <Text span fw={600} size={FS.meta}>{endpoint.targetName}</Text> in {endpoint.accountName} {endpoint.region}, kept by Argus.
                A replica more than a minute behind for five minutes is taken out, and put back when it catches up.
              </Text>
              <Text size={FS.micro} c={endpoint.note ? 'rose' : 'dimmed'}>
                {endpoint.includePrimary
                  ? endpoint.primaryInstance
                    ? `${database.hostname} (the primary) is in${primaryHealth ? ` · ${primaryHealth}` : ''}.`
                    : `${database.hostname} (the primary) is not in${endpoint.note ? `: ${endpoint.note}` : ' yet'}.`
                  : `${database.hostname} (the primary) is left out; writes never go through this endpoint either way.`}
              </Text>
              {!endpoint.includePrimary && endpoint.note && <Text size={FS.micro} c="rose">{endpoint.note}</Text>}
              <Text size={FS.micro} c="dimmed">Set by {endpoint.setBy} · {relTime(endpoint.setAt)}. A DNS name for it is not made by Argus.</Text>
            </>
          ) : (
            <Text size={FS.meta} c="dimmed" maw={640}>
              Send reads to one address: pick a network load balancer’s target group on port {database.port}, and Argus keeps the replicas
              that are not behind in it. Make the target group under Cloud first if there is none.
            </Text>
          )}
        </Box>
        <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
          {endpoint && admin && (
            <Button size="xs" variant="subtle" color="rose" loading={remove.isPending} onClick={() => void drop()}>Remove</Button>
          )}
          <Button size="xs" variant={endpoint ? 'default' : 'light'} disabled={Boolean(why)} title={why} onClick={() => setEditing(true)}>
            {endpoint ? 'Change' : 'Set a read endpoint'}
          </Button>
        </Group>
      </Group>
      {editing && <SetReadEndpoint database={database} endpoint={endpoint} choices={choices} onClose={() => setEditing(false)} />}
    </Paper>
  )
}

const choiceKey = (c: Pick<ReadEndpointChoice, 'accountId' | 'region' | 'targetGroup'>) => `${c.accountId}|${c.region}|${c.targetGroup}`

function SetReadEndpoint({ database, endpoint, choices, onClose }: {
  database: Database; endpoint: ReadEndpoint | null; choices: ReadEndpointChoice[]; onClose: () => void
}) {
  const set = useSetReadEndpoint()
  const [key, setKey] = useState<string | null>(endpoint ? choiceKey(endpoint) : null)
  const [primary, setPrimary] = useState(endpoint?.includePrimary ?? true)
  const picked = choices.find((c) => choiceKey(c) === key)
  const moving = endpoint && picked && choiceKey(endpoint) !== key
  const submit = async () => {
    if (!picked) return
    try {
      const ep = await set.mutateAsync({ id: database.id, accountId: picked.accountId, region: picked.region,
        targetGroup: picked.targetGroup, includePrimary: primary })
      notifyOk('Read endpoint set', ep.note ? ep.note : `Argus keeps ${ep.targetName} from now on.`)
      onClose()
    } catch (err) {
      notifyError('Not set', err)
    }
  }
  return (
    <Modal opened onClose={onClose} title={`Read endpoint for ${database.dbname}`} centered size="lg">
      <Stack gap="sm">
        <Select label="Target group" placeholder="Pick a target group" value={key} onChange={setKey} searchable
          description={`A network load balancer’s target group: TCP, instance targets, port ${database.port}.`}
          nothingFoundMessage={`No target group on port ${database.port}`}
          data={choices.map((c) => ({
            value: choiceKey(c),
            label: `${c.name}${c.balancer ? ` (${c.balancer})` : ''} · ${c.accountName} ${c.region}${c.problem ? ` · ${c.problem}` : ''}`,
            disabled: Boolean(c.problem),
          }))} />
        <Checkbox label={`Include ${database.hostname}, the primary`} checked={primary} onChange={(e) => setPrimary(e.currentTarget.checked)}
          description="It takes reads too, and is never taken out for lag. Leave it out to keep reads off it entirely." />
        <Box>
          <Text size={FS.body} fw={600} mb={4}>What Argus does</Text>
          <Stack gap={2}>
            <Text size={FS.meta}>1 · Puts in each replica of {database.dbname} whose server is an instance in the same account and region{primary ? `, and ${database.hostname}` : ''}.</Text>
            <Text size={FS.meta}>2 · Every minute, after the lag check: takes out a replica that has been behind for five minutes, and puts it back once it catches up.</Text>
            <Text size={FS.meta}>3 · Only ever takes out what it put in. Targets added by hand stay. In the cloud it runs as argus-replicas.</Text>
          </Stack>
        </Box>
        {moving && (
          <Alert color="amber" variant="light" p="xs">
            <Text size={FS.meta}>What Argus put in {endpoint!.targetName} comes out first.</Text>
          </Alert>
        )}
        <Group gap="xs">
          <Button onClick={() => void submit()} loading={set.isPending} disabled={!picked || Boolean(picked.problem)}>
            {endpoint ? 'Save' : 'Set read endpoint'}
          </Button>
          <Button variant="default" onClick={onClose}>Cancel</Button>
        </Group>
        <Text size={FS.micro} c="dimmed">Writes still go to {database.hostname} directly. Argus does not make a DNS name for the endpoint.</Text>
      </Stack>
    </Modal>
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

