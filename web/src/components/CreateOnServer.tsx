import {
  Alert, Anchor, Badge, Box, Button, Checkbox, Code, Collapse, Group, Loader, MultiSelect, NumberInput, SegmentedControl,
  Select, SimpleGrid, Stack, Text, TextInput,
} from '@mantine/core'
import { useDebouncedValue, useMediaQuery } from '@mantine/hooks'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { IconAlertTriangle, IconCheck, IconCircleDashed, IconX } from '@tabler/icons-react'
import { useEffect, useMemo, useState } from 'react'
import { GiveAccessMany } from '~/components/BulkDBAccess'
import { ButtonLink } from '~/components/links'
import {
  createOnServer, createOptions, isConfigured,
  type CreateAnswer, type CreateFields, type CreateRequest, type CreateStep,
} from '~/lib/live'
import { notifyError, notifyOk } from '~/lib/notify'
import { databasesOnAssetQuery, dbqk } from '~/lib/queries'
import { ENGINES, isBroker } from '~/lib/engines'
import { FS } from '~/theme'
import { pgLevelRole } from '~/lib/pgRoles'
import type { Asset, Database, DBEngine, DBLabel } from '~/types/domain'

/**
 * Create new, in the add dialog: make a database, vhost or topic on the
 * server and add it to Argus in one step. The agent builds the statements
 * from these fields only, and the right-hand preview is the control plane's
 * dry run of the same request, so what is shown is what runs. Nothing is
 * ever run over a name that is there: a taken name offers to add the
 * existing one instead. See plans/create-databases/design.md.
 */
export function CreateOnServer({ asset, engine, onClose, onAddExisting }: {
  asset: Asset
  engine: DBEngine
  onClose: () => void
  /** Switch the dialog to Already on this server with this name picked. */
  onAddExisting: (name: string) => void
}) {
  const narrow = useMediaQuery('(max-width: 48em)') ?? false
  const qc = useQueryClient()
  const host = asset.hostname.split('.')[0]!
  const info = ENGINES[engine]
  const { data: onAsset } = useQuery(databasesOnAssetQuery(asset.id))
  const entries = useMemo(() => onAsset?.databases ?? [], [onAsset])
  // Where it listens: like the entries of this engine already here, so a new
  // Postgres database goes through the same PgBouncer as its neighbours.
  const like = entries.find((d) => d.engine === engine)
  const [where, setWhere] = useState(false)
  const [addr, setAddr] = useState('')
  const [port, setPort] = useState<number | string>('')
  const [configPath, setConfigPath] = useState('')
  const [poolerPort, setPoolerPort] = useState<number | string>('')
  useEffect(() => {
    setAddr(like?.host ?? ''); setPort(like?.port ?? ''); setConfigPath(like?.configPath ?? '')
    setPoolerPort(like?.poolerPort || '')
  }, [like?.id, like?.host, like?.port, like?.configPath, like?.poolerPort])

  const [name, setName] = useState('')
  const [label, setLabel] = useState<DBLabel>(like?.label ?? 'production')
  // Postgres
  const [owner, setOwner] = useState('')
  const [extensions, setExtensions] = useState<string[]>([])
  const [encoding, setEncoding] = useState('')
  const [locale, setLocale] = useState('')
  const [moreOpen, setMoreOpen] = useState(false)
  // MySQL
  const [charset, setCharset] = useState<string | null>(null)
  const [collation, setCollation] = useState<string | null>(null)
  // MongoDB
  const [collection, setCollection] = useState('')
  const [kind, setKind] = useState<'plain' | 'timeseries' | 'capped'>('plain')
  const [timeField, setTimeField] = useState('ts')
  const [keepDays, setKeepDays] = useState<number | string>('')
  const [cappedMB, setCappedMB] = useState<number | string>(100)
  // RabbitMQ
  const [description, setDescription] = useState('')
  const [queueType, setQueueType] = useState<'' | 'classic' | 'quorum' | 'stream'>('')
  const [maxConnections, setMaxConnections] = useState<number | string>('')
  const [maxQueues, setMaxQueues] = useState<number | string>('')
  // Kafka
  const [partitions, setPartitions] = useState<number | string>(3)
  const [replicas, setReplicas] = useState<number | string>('')
  const [retentionDays, setRetentionDays] = useState<number | string>('')
  const [cleanup, setCleanup] = useState<'' | 'delete' | 'compact'>('')
  const [addEntry, setAddEntry] = useState(false)

  const { data: ownerRole } = useQuery({
    queryKey: ['pg-level-role', 'owner', name.trim()], queryFn: () => pgLevelRole('owner', name.trim()),
    enabled: engine === 'postgres' && name.trim() !== '', staleTime: Infinity,
  })
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<CreateAnswer | null>(null)
  const [giving, setGiving] = useState<Database | null>(null)

  const whereQ = { engine, port: Number(port) || 0, host: addr.trim(), configPath: configPath.trim() }
  const [whereD] = useDebouncedValue(whereQ, 400)
  const opts = useQuery({
    queryKey: ['create-options', asset.id, whereD] as const,
    queryFn: () => createOptions(asset.id, whereD),
    enabled: isConfigured() && engine !== 'redis',
    staleTime: 30_000,
    retry: false,
  })
  const o = opts.data?.state === 'ok' ? opts.data.options : undefined
  const charsets = o?.charsets ?? {}
  const cs = charset ?? o?.defaultCharset ?? null
  const collations = cs ? charsets[cs] ?? [] : []
  const co = collation && collations.includes(collation) ? collation
    : cs === o?.defaultCharset && o?.defaultCollation ? o.defaultCollation : null

  const fields = (): CreateFields => {
    switch (engine) {
      case 'postgres':
        return { postgres: { owner, extensions, encoding: encoding.trim() || undefined, locale: locale.trim() || undefined } }
      case 'mysql':
        return { mysql: { charset: cs ?? undefined, collation: co ?? undefined } }
      case 'mongodb':
        return {
          mongo: {
            collection: collection.trim(), kind,
            ...(kind === 'timeseries' ? { timeField: timeField.trim(), expireAfterS: Number(keepDays) > 0 ? Math.round(Number(keepDays) * 86400) : undefined } : {}),
            ...(kind === 'capped' ? { cappedBytes: Math.round(Number(cappedMB) * 1024 * 1024) } : {}),
          },
        }
      case 'rabbitmq':
        return { rabbit: { description: description.trim() || undefined, queueType, maxConnections: Number(maxConnections) || undefined, maxQueues: Number(maxQueues) || undefined } }
      case 'kafka':
        return {
          kafka: {
            partitions: Number(partitions) || 1, replicas: Number(replicas) || undefined,
            retentionMs: retentionDays === '' ? undefined : Number(retentionDays) < 0 ? -1 : Math.round(Number(retentionDays) * 86_400_000),
            cleanup,
          },
        }
    }
    return {}
  }
  const req: CreateRequest = {
    engine, name: name.trim(), label, addEntry,
    ...(addr.trim() ? { host: addr.trim() } : {}),
    ...(Number(port) ? { port: Number(port) } : {}),
    ...(configPath.trim() ? { configPath: configPath.trim() } : {}),
    ...(engine === 'postgres' && Number(poolerPort) ? { poolerPort: Number(poolerPort) } : {}),
    ...fields(),
  }
  const [reqD] = useDebouncedValue(req, 350)
  const preview = useQuery({
    queryKey: ['create-preview', asset.id, reqD] as const,
    queryFn: () => createOnServer(asset.id, { ...reqD, dryRun: true }),
    enabled: isConfigured() && engine !== 'redis' && reqD.name !== '',
    retry: false,
    staleTime: 0,
  })
  const previewError = preview.error ? (preview.error as Error).message : ''
  const n = name.trim()
  const takenHere = n !== '' && (o?.taken ?? []).includes(n)
  const inArgus = takenHere && entries.some((d) => d.engine === engine && d.dbname === n)
  const covered = engine === 'kafka' ? preview.data?.coveredBy : undefined
  const agentDown = opts.data && (opts.data.state === 'offline' || opts.data.state === 'unsupported')
  const ready = n !== '' && !takenHere && !previewError && preview.data?.state === 'ok' && !agentDown && !busy
    && (engine !== 'mongodb' || collection.trim() !== '')

  const submit = async () => {
    setBusy(true)
    try {
      const a = await createOnServer(asset.id, req)
      setResult(a)
      if (a.state === 'ok' || a.state === 'partial') {
        void qc.invalidateQueries({ queryKey: dbqk.databases })
        void qc.invalidateQueries({ queryKey: ['create-options', asset.id] })
        notifyOk(a.state === 'ok' ? 'Created' : 'Created, with a problem',
          `${info.label} ${n} on ${host}.${a.database ? ' It is in Argus now; the server’s rules have been applied.' : ''}`)
      }
    } catch (err) {
      notifyError('Not created', err)
    } finally {
      setBusy(false)
    }
  }

  if (engine === 'redis') {
    return (
      <Stack gap="sm">
        <Alert variant="light" color="slate" title="Nothing to create in Redis">
          <Text size={FS.meta}>
            A Redis has a fixed set of numbered databases, set by <Code>databases</Code> in its redis.conf (16 unless changed).
            They all exist already, and one entry in Argus covers every one: <Code>SELECT 3</Code> in a session moves to db 3.
          </Text>
          <Text size={FS.meta} mt={6}>
            For more numbers, raise <Code>databases</Code> and restart Redis. For a Redis of its own, install one from the Apps tab and add it here.
          </Text>
        </Alert>
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Close</Button></Group>
      </Stack>
    )
  }

  if (result && result.state !== 'exists') {
    const made = result.state === 'ok' || result.state === 'partial'
    return (
      <Stack gap="sm">
        <Text size={FS.body} fw={600}>{made ? `Created ${n} on ${host}` : `${n} was not created`}</Text>
        <Steps steps={result.steps} />
        {result.error && (
          <Alert variant="light" color={made ? 'amber' : 'rose'} p="xs" icon={<IconAlertTriangle size={16} />}>
            <Text size={FS.meta}>{result.error}</Text>
          </Alert>
        )}
        {result.statements.length > 0 && made && <Statements title={`Ran on ${host}`} lines={result.statements} />}
        <Group justify="flex-end" gap="xs">
          {!made && <Button variant="default" onClick={() => setResult(null)}>Back</Button>}
          {result.database && (
            <>
              <Button variant="default" onClick={() => setGiving(result.database!)}>Give access…</Button>
              <ButtonLink {...(isBroker(engine) ? { to: '/brokers/$brokerId', params: { brokerId: result.database.id } } as const
                : { to: '/databases/$dbId', params: { dbId: result.database.id } } as const)} variant="default">Open</ButtonLink>
            </>
          )}
          <Button onClick={onClose}>Done</Button>
        </Group>
        {giving && <GiveAccessMany opened targets={[giving]} onClose={() => setGiving(null)} />}
      </Stack>
    )
  }

  const nounName = engine === 'rabbitmq' ? 'Vhost' : engine === 'kafka' ? 'Topic' : 'Name'
  const form = (
    <Stack gap="sm">
      {agentDown && (
        <Alert variant="light" color="rose" p="xs"><Text size={FS.meta}>{opts.data?.error} You can still add one that is there by name.</Text></Alert>
      )}
      {opts.data?.state === 'failed' && (
        <Alert variant="light" color="amber" p="xs"><Text size={FS.meta}>{opts.data.error}. Check where it listens below.</Text></Alert>
      )}
      <TextInput label={nounName} value={name} onChange={(e) => { setName(e.currentTarget.value); setResult(null) }} data-autofocus
        styles={{ input: { fontFamily: 'var(--font-mono)' } }} placeholder={engine === 'rabbitmq' ? '/payments' : engine === 'kafka' ? 'orders.refunds' : 'invoices'}
        error={takenHere ? undefined : previewError || undefined}
        description={{
          postgres: 'Up to 63 characters. Lowercase letters, digits and _ need no quoting in SQL.',
          mysql: 'Up to 63 characters, no / \\ or . (MySQL makes a folder of it).',
          mongodb: 'Up to 63 bytes, none of / \\ . " $ * < > : | ? or spaces; not admin, local or config.',
          rabbitmq: 'Up to 63 characters, no quotes.',
          kafka: 'Up to 249 letters, digits, . _ or -.',
          redis: '',
        }[engine]} />
      {(takenHere || result?.state === 'exists') && (
        <Alert variant="light" color="amber" p="xs">
          <Text size={FS.meta} fw={600}>{host} already has {engine === 'rabbitmq' ? 'a vhost' : engine === 'kafka' ? 'a topic' : 'a database'} called {n}.</Text>
          {inArgus ? <Text size={FS.meta}>It is in Argus already. Pick another name.</Text> : (
            <Group gap="xs" mt={6}>
              <Button size="compact-xs" variant="default" onClick={() => onAddExisting(n)}>Add {n} instead</Button>
              <Text size={FS.micro} c="dimmed">or pick another name</Text>
            </Group>
          )}
        </Alert>
      )}

      {engine === 'postgres' && (
        <>
          <Select label="Owner" value={owner} onChange={(v) => setOwner(v ?? '')} allowDeselect={false} searchable
            data={[{ value: '', label: `Argus Owner level${n && ownerRole ? ` (${ownerRole})` : ''}` },
              ...(o?.roles ?? []).map((r) => ({ value: r, label: r }))]}
            description="Whoever is given Owner here can create and change its tables. Pick an application's own role instead if the app will run its migrations." />
          <MultiSelect label="Extensions" placeholder={o?.extensions ? `Add from ${o.extensions.length} installed…` : 'Optional'}
            data={o?.extensions ?? []} value={extensions} onChange={setExtensions} searchable clearable maxValues={20} />
          <Box>
            <Anchor component="button" type="button" size={FS.micro} onClick={() => setMoreOpen((x) => !x)}>
              {encoding || locale ? `${encoding || o?.encoding || 'server default'} · ${locale || o?.locale || 'server default'}` : `${o?.encoding ?? 'UTF8'} · ${o?.locale ?? 'the server’s locale'} · from template0`}
              {' '}· {moreOpen ? 'Hide' : 'Change'}
            </Anchor>
            <Collapse expanded={moreOpen}>
              <SimpleGrid cols={2} mt={6}>
                <TextInput label="Encoding" placeholder={o?.encoding ?? 'UTF8'} value={encoding} onChange={(e) => setEncoding(e.currentTarget.value)} />
                <TextInput label="Locale" placeholder={o?.locale ?? 'en_US.UTF-8'} value={locale} onChange={(e) => setLocale(e.currentTarget.value)} />
              </SimpleGrid>
            </Collapse>
          </Box>
        </>
      )}

      {engine === 'mysql' && (
        <>
          <SimpleGrid cols={2}>
            <Select label="Character set" value={cs} onChange={(v) => { setCharset(v); setCollation(null) }} searchable
              data={Object.keys(charsets)} placeholder={o?.defaultCharset ?? 'server default'} />
            <Select label="Collation" value={co} onChange={setCollation} searchable data={collations}
              placeholder={o?.defaultCollation ?? 'server default'} />
          </SimpleGrid>
          <Text size={FS.micro} c="dimmed">The server's defaults, from the agent. On MariaDB the lists show MariaDB's.</Text>
        </>
      )}

      {engine === 'mongodb' && (
        <>
          <TextInput label="First collection" value={collection} onChange={(e) => setCollection(e.currentTarget.value)}
            styles={{ input: { fontFamily: 'var(--font-mono)' } }} placeholder="events"
            description="MongoDB keeps a database only while it has a collection, so Argus makes one, empty." />
          <Stack gap={4}>
            <Text size={FS.meta} fw={500}>Kind</Text>
            <SegmentedControl value={kind} onChange={(v) => setKind(v as typeof kind)} fullWidth
              data={[{ label: 'Plain', value: 'plain' }, { label: 'Time series', value: 'timeseries' }, { label: 'Capped', value: 'capped' }]} />
          </Stack>
          {kind === 'timeseries' && (
            <SimpleGrid cols={2}>
              <TextInput label="Time field" value={timeField} onChange={(e) => setTimeField(e.currentTarget.value)} styles={{ input: { fontFamily: 'var(--font-mono)' } }} />
              <NumberInput label="Keep for (days)" value={keepDays} onChange={setKeepDays} min={1} placeholder="for ever" />
            </SimpleGrid>
          )}
          {kind === 'capped' && (
            <NumberInput label="Size (MB)" value={cappedMB} onChange={setCappedMB} min={1}
              description="The oldest documents go once it is full." />
          )}
        </>
      )}

      {engine === 'rabbitmq' && (
        <>
          <TextInput label="Description" value={description} onChange={(e) => setDescription(e.currentTarget.value)} placeholder="Optional" maxLength={200} />
          <Stack gap={4}>
            <Text size={FS.meta} fw={500}>Default queue type</Text>
            <SegmentedControl value={queueType} onChange={(v) => setQueueType(v as typeof queueType)} fullWidth
              data={[{ label: 'Server default', value: '' }, { label: 'Classic', value: 'classic' }, { label: 'Quorum', value: 'quorum' }, { label: 'Stream', value: 'stream' }]} />
            <Text size={FS.micro} c="dimmed">What a queue declared without a type becomes. Quorum keeps messages on a majority of nodes.</Text>
          </Stack>
          <SimpleGrid cols={2}>
            <NumberInput label="Most connections" value={maxConnections} onChange={setMaxConnections} min={1} placeholder="No limit" />
            <NumberInput label="Most queues" value={maxQueues} onChange={setMaxQueues} min={1} placeholder="No limit" />
          </SimpleGrid>
        </>
      )}

      {engine === 'kafka' && (
        <>
          <SimpleGrid cols={2}>
            <NumberInput label="Partitions" value={partitions} onChange={setPartitions} min={1} max={1000} />
            <NumberInput label="Replicas" value={replicas} onChange={setReplicas} min={1} max={o?.brokers || 16}
              placeholder="cluster default" description={o?.brokers ? `of ${o.brokers} broker${o.brokers === 1 ? '' : 's'}` : undefined} />
          </SimpleGrid>
          <SimpleGrid cols={2}>
            <NumberInput label="Keep messages for (days)" value={retentionDays} onChange={setRetentionDays} min={-1} decimalScale={3}
              placeholder="broker default" description="-1 keeps them for ever." />
            <Stack gap={4}>
              <Text size={FS.meta} fw={500}>Clean-up</Text>
              <SegmentedControl value={cleanup} onChange={(v) => setCleanup(v as typeof cleanup)} fullWidth
                data={[{ label: 'Default', value: '' }, { label: 'Delete old', value: 'delete' }, { label: 'Compact', value: 'compact' }]} />
            </Stack>
          </SimpleGrid>
          {covered && (
            <Box p="xs" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
              <Group gap={6}><Badge size="xs" color="teal" variant="light">Covered</Badge>
                <Text size={FS.meta}>Already in Argus through the entry <Code>{covered.dbname}</Code>; whoever can connect to it can use the topic at once.</Text></Group>
              <Checkbox mt={6} size="xs" checked={addEntry} onChange={(e) => setAddEntry(e.currentTarget.checked)}
                label={`Also add ${n} as its own entry, to give it to someone else`} />
            </Box>
          )}
        </>
      )}

      <Stack gap={4}>
        <Text size={FS.meta} fw={500}>Label</Text>
        <SegmentedControl value={label} onChange={(v) => setLabel(v as DBLabel)} fullWidth
          data={[{ label: 'Production', value: 'production' }, { label: 'Staging', value: 'staging' }, { label: narrow ? 'Dev' : 'Development', value: 'development' }]} />
      </Stack>

      <Box>
        <Anchor component="button" type="button" size={FS.micro} onClick={() => setWhere((x) => !x)}>
          Where: {addr || info.defaults.host}:{port || info.defaults.port}{configPath ? ` · ${configPath}` : ''}{engine === 'postgres' && Number(poolerPort) ? ` · PgBouncer ${poolerPort}` : ''}
          {like ? ', like the others here' : ''} · {where ? 'Hide' : 'Change'}
        </Anchor>
        <Collapse expanded={where}>
          <SimpleGrid cols={2} mt={6}>
            <TextInput label="Address" placeholder={info.defaults.host} value={addr} onChange={(e) => setAddr(e.currentTarget.value)} />
            <NumberInput label={engine === 'rabbitmq' ? 'Management port' : 'Port'} placeholder={String(info.defaults.port)} value={port} onChange={setPort} min={1} max={65535} />
            {engine === 'postgres' && (
              <NumberInput label="PgBouncer port" placeholder="none" value={poolerPort} onChange={setPoolerPort} min={1} max={65535} />
            )}
            {(engine === 'mysql' || engine === 'mongodb' || engine === 'kafka') && (
              <TextInput label={engine === 'mysql' ? 'Option file' : 'Admin file'} placeholder={info.defaults.configPath ?? 'none'}
                value={configPath} onChange={(e) => setConfigPath(e.currentTarget.value)} />
            )}
          </SimpleGrid>
        </Collapse>
      </Box>
    </Stack>
  )

  const side = (
    <Stack gap="xs">
      <Statements title={`Runs on ${host}`} lines={preview.data?.statements ?? []}
        loading={preview.isFetching} empty={n ? (previewError ? 'Fix the fields to see what runs.' : '') : 'Type a name to see what runs.'} />
      <Text size={FS.micro} c="dimmed">
        Then it is added to Argus as {label}, the server’s rules for databases added later apply, and it is backed up like the others here.
        {covered && !addEntry ? ' A covered topic adds nothing to Argus.' : ''} Recorded in Activity with what ran.
      </Text>
    </Stack>
  )

  return (
    <Stack gap="sm">
      {narrow ? <>{form}{side}</> : (
        <Box style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) 300px', gap: 16 }}>{form}{side}</Box>
      )}
      <Group justify="flex-end">
        <Button variant="default" onClick={onClose}>Cancel</Button>
        <Button onClick={() => void submit()} loading={busy} disabled={!ready} fullWidth={narrow}>
          {n ? `Create ${n}` : 'Create'}
        </Button>
      </Group>
    </Stack>
  )
}

/** The statements, as the dark code block the design shows. */
export function Statements({ title, lines, loading, empty }: { title: string; lines: string[]; loading?: boolean; empty?: string }) {
  return (
    <Stack gap={4}>
      <Group gap={6}><Text size={FS.meta} fw={600}>{title}</Text>{loading && <Loader size={10} />}</Group>
      {lines.length > 0 ? (
        <Code block style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 11.5 }}>{lines.map((l) => l.endsWith(';') || l.startsWith('use ') || l.startsWith('db.') || l.startsWith('rabbitmqctl') || l.startsWith('CreateTopics') ? l : `${l};`).join('\n')}</Code>
      ) : empty ? <Text size={FS.micro} c="dimmed">{empty}</Text> : null}
    </Stack>
  )
}

/** Each step the agent took, as it reported it. */
export function Steps({ steps }: { steps: CreateStep[] }) {
  if (!steps.length) return null
  return (
    <Stack gap={4}>
      {steps.map((s, i) => (
        <Group key={i} gap={8} wrap="nowrap" align="flex-start">
          {s.ok ? <IconCheck size={16} color="var(--mantine-color-teal-6)" /> : s.error ? <IconX size={16} color="var(--mantine-color-rose-6)" /> : <IconCircleDashed size={16} />}
          <Box>
            <Text size={FS.meta}>{s.text}</Text>
            {s.error && <Text size={FS.micro} c="rose">{s.error}</Text>}
          </Box>
        </Group>
      ))}
    </Stack>
  )
}
