import {
  Alert, Anchor, Badge, Box, Button, Checkbox, Group, Loader, Modal, NumberInput, Radio, ScrollArea, SegmentedControl,
  Skeleton, Stack, Table, Text, TextInput, UnstyledButton,
} from '@mantine/core'
import { useDebouncedValue, useDisclosure, useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconDatabase, IconPlus, IconRefresh, IconSearch, IconTable } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { ButtonLink } from '~/components/links'
import { EmptyState, SectionCard } from '~/components/page'
import { discoverDatabases, isConfigured } from '~/lib/live'
import { bytes } from '~/components/primitives'
import { notifyError, notifyOk } from '~/lib/notify'
import { databasesOnAssetQuery, useCreateDatabase, usersQuery } from '~/lib/queries'
import { assetDBAccessQuery } from '~/lib/dbAccessQueries'
import { CreateOnServer } from '~/components/CreateOnServer'
import { GiveAccessMany, RemoveAccessMany, SelectionBar, TickAll, WhoHasWhat, whoHasIt } from '~/components/BulkDBAccess'
import { targetName } from '~/lib/dbTarget'
import { FS } from '~/theme'
import { ENGINES, engineOf, isBroker } from '~/lib/engines'
import { kafkaCovered } from '~/lib/kafka'
import type { Asset, DBAssignment, DBEngine, DBLabel } from '~/types/domain'

/**
 * The databases Argus opens on one server, on that server's page: adding one
 * is an engine, a name and a label, because the agent already on the host
 * does the rest as the engine's admin there -- Postgres's peer
 * authentication, Redis's own redis.conf, rabbitmqctl. No password is ever
 * typed in.
 */
export function DatabasesOnServer({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data } = useQuery(databasesOnAssetQuery(asset.id))
  const { data: access } = useQuery({ ...assetDBAccessQuery(asset.id), enabled: canEdit && isConfigured() })
  const { data: users } = useQuery({ ...usersQuery(), enabled: canEdit })
  const [adding, add] = useDisclosure(false)
  const [selected, setSelected] = useState<string[]>([])
  const [dialog, setDialog] = useState<null | { kind: 'give'; who: string[]; ids: string[] } | { kind: 'remove' } | { kind: 'grid' }>(null)
  if (!isConfigured()) return null
  const list = data?.databases ?? []
  const picked = list.filter((d) => selected.includes(d.id))
  const ids = list.map((d) => d.id)
  const byDB = new Map<string, DBAssignment[]>()
  for (const g of access?.assignments ?? []) byDB.set(g.databaseId, [...(byDB.get(g.databaseId) ?? []), g])
  const name = (k: string) => k.startsWith('u:') ? users?.find((u) => u.email === k.slice(2))?.displayName ?? k.slice(2) : k
  const toggle = (id: string) => setSelected((s) => s.includes(id) ? s.filter((x) => x !== id) : [...s, id])
  return (
    <SectionCard
      title="Databases and brokers"
      icon={IconDatabase}
      description="Postgres databases, Redis and RabbitMQ on this server that people can open in the browser. Tick several to give a team access to all of them at once. Every session is recorded command by command."
    >
      {canEdit && (
        <Group justify="flex-end" gap="xs" mb="xs">
          {list.length > 0 && (
            <Button size="compact-xs" variant="default" leftSection={<IconTable size={12} />} onClick={() => setDialog({ kind: 'grid' })}>
              Who has what
            </Button>
          )}
          <Button size="compact-xs" variant="default" leftSection={<IconPlus size={12} />} onClick={add.open}>
            Add
          </Button>
        </Group>
      )}
      {list.length === 0 ? (
        <EmptyState compact icon={IconDatabase} title="No databases on this server yet."
          description={canEdit
            ? 'Add a Postgres database, a Redis or a RabbitMQ vhost. The agent on this host signs in as its admin locally; nothing to install, no password to store.'
            : 'An admin can add one from here.'} />
      ) : (
        <Table verticalSpacing="xs">
          {canEdit && (
            <Table.Thead><Table.Tr>
              <Table.Th w={32}><TickAll ids={ids} selected={selected} onChange={setSelected} label="Select every database and broker" /></Table.Th>
              <Table.Th>Name</Table.Th><Table.Th>Label</Table.Th><Table.Th visibleFrom="sm">Who can connect</Table.Th><Table.Th />
            </Table.Tr></Table.Thead>
          )}
          <Table.Tbody>
            {list.map((d) => (
              <Table.Tr key={d.id} bg={selected.includes(d.id) ? 'var(--mantine-primary-color-light)' : undefined}>
                {canEdit && (
                  <Table.Td w={32}>
                    <Checkbox size="xs" checked={selected.includes(d.id)} onChange={() => toggle(d.id)} aria-label={`Select ${targetName(d)}`} />
                  </Table.Td>
                )}
                <Table.Td>
                  <Text size={FS.body} fw={600}>{targetName(d)}</Text>
                  <Text size={FS.micro} c="dimmed" ff="monospace">{engineOf(d.engine).label} · {d.host}:{d.port}</Text>
                </Table.Td>
                <Table.Td>
                  <Badge size="xs" variant="light" radius="sm"
                    color={{ production: 'rose', staging: 'amber', development: 'teal' }[d.label]}>{d.label}</Badge>
                </Table.Td>
                {canEdit && (
                  <Table.Td visibleFrom="sm" maw={320}>
                    <Text size={FS.micro} c={byDB.get(d.id)?.length ? undefined : 'dimmed'} lineClamp={2}>
                      {access ? whoHasIt(byDB.get(d.id), d.engine, name) : ''}
                    </Text>
                  </Table.Td>
                )}
                <Table.Td ta="right">
                  <Group gap="xs" justify="flex-end" wrap="nowrap">
                    {canEdit && (
                      <ButtonLink {...(isBroker(d.engine) ? { to: '/brokers/$brokerId/settings', params: { brokerId: d.id } } as const : { to: '/databases/$dbId/settings', params: { dbId: d.id } } as const)}
                        size="compact-xs" variant="subtle" visibleFrom="sm">
                        Who can connect
                      </ButtonLink>
                    )}
                    <ButtonLink {...(isBroker(d.engine) ? { to: '/brokers/$brokerId', params: { brokerId: d.id } } as const : { to: '/databases/$dbId', params: { dbId: d.id } } as const)}
                      size="compact-xs" variant="default">Open</ButtonLink>
                  </Group>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {canEdit && (
        <Box mt="xs">
          <SelectionBar selected={picked} total={list.length} onAll={() => setSelected(ids)} onClear={() => setSelected([])}
            onGive={() => setDialog({ kind: 'give', who: [], ids: selected })} onRemove={() => setDialog({ kind: 'remove' })} />
        </Box>
      )}
      {canEdit && <AddDatabase asset={asset} opened={adding} onClose={add.close} />}
      {canEdit && dialog?.kind === 'give' && (
        <GiveAccessMany opened initialWho={dialog.who} targets={list.filter((d) => dialog.ids.includes(d.id))}
          onClose={() => { setDialog(null); setSelected([]) }} />
      )}
      {canEdit && dialog?.kind === 'remove' && (
        <RemoveAccessMany opened targets={picked} onClose={() => { setDialog(null); setSelected([]) }} />
      )}
      {canEdit && dialog?.kind === 'grid' && (
        <Modal opened onClose={() => setDialog(null)} size="calc(100vw - 4rem)" title={`Who has what on ${asset.hostname.split('.')[0]}`}>
          <WhoHasWhat hostname={asset.hostname} assetId={asset.id} databases={list}
            onChangeAll={(who) => setDialog({ kind: 'give', who: [who], ids })} />
        </Modal>
      )}
    </SectionCard>
  )
}

/**
 * The add-a-database form. The agent on the server lists what is there --
 * Postgres databases, a Redis's logical dbs, RabbitMQ's vhosts -- and the
 * admin ticks what to add; typing the name is the fallback whenever the
 * agent cannot list (offline, too old, wrong port), and the form says why.
 * `initialEngine`, `initialPort` and `initialPoolerPort` come from what the agent found
 * running, when it is opened from there.
 */
export function AddDatabase({ asset, opened, onClose, initialEngine, initialPort, initialPoolerPort, initialConfigPath, initialMode, onlyEngines }: {
  asset: Asset; opened: boolean; onClose: () => void; initialEngine?: DBEngine; initialPort?: number; initialPoolerPort?: number
  initialConfigPath?: string
  /** Open on Create new instead of Already on this server. */
  initialMode?: 'existing' | 'create'
  /** The engines offered: brokers on the Brokers page, for one. */
  onlyEngines?: DBEngine[]
}) {
  const [mode, setMode] = useState<'existing' | 'create'>(initialMode ?? 'existing')
  const create = useCreateDatabase()
  const narrow = useMediaQuery('(max-width: 48em)')
  const [engine, setEngine] = useState<DBEngine>(initialEngine ?? 'postgres')
  const info = ENGINES[engine]
  const [typed, setTyped] = useState('')
  const [manual, setManual] = useState(false)
  const [picked, setPicked] = useState<string[]>([])
  const [filter, setFilter] = useState('')
  const [host, setHost] = useState('')
  const [port, setPort] = useState<number | string>(initialPort ?? '')
  const [configPath, setConfigPath] = useState(initialConfigPath ?? '')
  const [label, setLabel] = useState<DBLabel>('production')
  const [poolerPort, setPoolerPort] = useState<number | string>(initialPoolerPort ?? '')
  const [busy, setBusy] = useState(false)

  // Ask the agent once the address settles, so typing a port is one question.
  const [where] = useDebouncedValue({ engine, port: Number(port) || 0, host: host.trim(), configPath: configPath.trim() }, 500)
  const found = useQuery({
    queryKey: ['discover-databases', asset.id, where] as const,
    queryFn: () => discoverDatabases(asset.id, where),
    enabled: opened && !manual && isConfigured(),
    staleTime: 0,
    gcTime: 0,
    retry: false,
  })
  const ans = found.data
  const listing = !manual && ans?.state === 'ok'
  const failed = !manual && (found.isError || (ans !== undefined && ans.state !== 'ok'))
  const multi = engine !== 'redis'
  // A Kafka entry is a scope of topics, typed, not one picked from a list;
  // the agent's list of topics shows what the scope would cover.
  const scoped = engine === 'kafka'

  const rows = useMemo(() => {
    const list = (ans?.databases ?? []).filter((d) => !(engine === 'postgres' && d.name === 'postgres'))
    const needle = filter.trim().toLowerCase()
    return needle ? list.filter((d) => d.name.toLowerCase().includes(needle)) : list
  }, [ans, engine, filter])
  // A Redis is one entry, whichever db it opens in; once added, it is added.
  const redisAdded = engine === 'redis' && (ans?.databases ?? []).some((d) => d.added)

  const names = listing && !scoped ? picked : [typed.trim() || info.defaults.dbname].filter(Boolean)
  const nameOk = (n: string) => engine === 'postgres' ? n.length > 0 && n.length <= 63 && !/["'\\\x00]/.test(n)
    : engine === 'mysql' ? n.length > 0 && n.length <= 63 && !/[/\\.\x00]/.test(n) && !/ $/.test(n)
      && !['mysql', 'sys', 'information_schema', 'performance_schema'].includes(n.toLowerCase())
    : engine === 'redis' ? /^\d{1,4}$/.test(n)
    : engine === 'mongodb' ? n.length > 0 && new TextEncoder().encode(n).length < 64 && !/[/\\. "$*<>:|?\x00]/.test(n) && !['admin', 'local', 'config'].includes(n)
    : engine === 'kafka' ? /^(\*|[A-Za-z0-9._-]{1,63})$/.test(n) && n !== '.' && n !== '..'
    : n.length > 0 && n.length <= 255 && !/[\x00\n\r]/.test(n)
  const valid = names.length > 0 && names.every(nameOk) && !redisAdded

  const reset = () => { setPicked([]); setFilter(''); setTyped('') }
  const pick = (e: DBEngine) => { setEngine(e); reset(); setManual(false); setHost(''); setPort(''); setConfigPath('') }
  const toggle = (n: string) => setPicked((p) => !multi ? [n] : p.includes(n) ? p.filter((x) => x !== n) : [...p, n])

  const submit = async () => {
    setBusy(true)
    const added: string[] = []
    try {
      for (const n of names) {
        await create.mutateAsync({
          assetId: asset.id, engine, dbname: n, label,
          port: Number(port) || info.defaults.port,
          ...(host.trim() ? { host: host.trim() } : {}),
          ...(engine === 'postgres' ? { poolerPort: Number(poolerPort) || 0 } : {}),
          ...(engine === 'redis' ? { configPath: configPath.trim() || info.defaults.configPath } : {}),
          ...((engine === 'mysql' || engine === 'mongodb' || engine === 'kafka') && configPath.trim() ? { configPath: configPath.trim() } : {}),
        })
        added.push(n)
      }
      notifyOk('Added', `${added.length === 1 ? `${info.label} ${added[0]}` : `${added.length} ${info.noun}s`} on ${asset.hostname}. Give a team access from its page; nobody can open it until you do.`)
      onClose()
      reset()
    } catch (err) {
      if (added.length) notifyOk('Partly added', `${added.join(', ')} added; the rest were not.`)
      notifyError('Not added', err)
    } finally {
      setBusy(false)
    }
  }

  const why = found.isError ? (found.error as Error).message
    : ans?.state === 'offline' ? `The agent on ${asset.hostname} is offline, so it can't look. You can still add by name; it will connect once the agent is back.`
    : ans?.state === 'unsupported' ? `This server's agent is too old to list databases. Update the agent from the server's page, or type the name below.`
    : ans?.error && scoped ? `${ans.error}. Check the port and the admin file; you can still add the topics.`
    : ans?.error ? `${ans.error}. Check the ${engine === 'rabbitmq' ? 'management port' : 'port'}, or type the name.`
    : ''

  return (
    <Modal opened={opened} onClose={onClose} title={`Add to ${asset.hostname.split('.')[0]}`} centered={!narrow}
      fullScreen={narrow} size={mode === 'create' ? 'xl' : 'lg'}>
      <Stack gap="sm">
        <SegmentedControl value={engine} onChange={(v) => pick(v as DBEngine)} fullWidth
          data={(onlyEngines ?? (Object.keys(ENGINES) as DBEngine[])).map((e) => ({ label: ENGINES[e].label, value: e }))} />
        <SegmentedControl value={mode} onChange={(v) => setMode(v as typeof mode)} size="xs"
          data={[{ label: `Already on ${asset.hostname.split('.')[0]}`, value: 'existing' }, { label: 'Create new', value: 'create' }]} />

        {mode === 'create' ? (
          <CreateOnServer key={engine} asset={asset} engine={engine} onClose={onClose}
            onAddExisting={(n) => { setMode('existing'); setManual(true); setTyped(n) }} />
        ) : (<>

        {!manual && !scoped && (
          <Stack gap={6}>
            <Group gap="xs" wrap="nowrap">
              <Text size={FS.meta} fw={500}>{engine === 'rabbitmq' ? 'Vhosts' : engine === 'redis' ? 'Databases in this Redis' : 'Databases on this server'}</Text>
              <Group gap={6} ml="auto" wrap="nowrap">
                {found.isFetching ? (
                  <><Loader size={12} /><Text size={FS.micro} c="dimmed">Asking the agent…</Text></>
                ) : listing ? (
                  <Text size={FS.micro} c="dimmed" visibleFrom="xs">Found by the agent{ans?.version ? ` · ${ans.server === 'valkey' ? 'Valkey' : info.label} ${ans.version.split(' ')[0]}` : ''}</Text>
                ) : null}
                <Button size="compact-xs" variant="subtle" leftSection={<IconRefresh size={12} />}
                  onClick={() => void found.refetch()} disabled={found.isFetching}>Look again</Button>
              </Group>
            </Group>
            {failed && !found.isFetching && (
              <Alert color={ans?.state === 'offline' ? 'rose' : 'amber'} variant="light" p="xs">
                <Text size={FS.meta}>{why}</Text>
              </Alert>
            )}
            {listing && (ans?.databases.length ?? 0) > 8 && (
              <TextInput size="xs" placeholder={`Filter ${ans?.databases.length} ${engine === 'rabbitmq' ? 'vhosts' : 'databases'}`}
                leftSection={<IconSearch size={12} />} value={filter} onChange={(e) => setFilter(e.currentTarget.value)}
                aria-label="Filter the list" />
            )}
            {found.isFetching && !ans && (
              <Stack gap={6}>{[0, 1, 2].map((i) => <Skeleton key={i} h={34} radius="sm" />)}</Stack>
            )}
            {listing && (
              <ScrollArea.Autosize mah={narrow ? 320 : 280} type="auto">
                <Stack gap={0} style={{ border: '1px solid var(--color-line)', borderRadius: 8, overflow: 'hidden' }}>
                  {rows.length === 0 && (
                    <Text size={FS.meta} c="dimmed" p="sm">
                      {filter ? 'Nothing matches.' : `No ${engine === 'rabbitmq' ? 'vhosts' : 'databases'} found. Type a name instead.`}
                    </Text>
                  )}
                  {rows.map((d) => {
                    const on = picked.includes(d.name)
                    const off = Boolean(d.added) || redisAdded
                    const meta = d.added ? null
                      : engine === 'postgres' ? [d.owner && `owner ${d.owner}`, d.bytes ? bytes(d.bytes) : ''].filter(Boolean).join(' · ')
                      : engine === 'mysql' ? (d.bytes ? bytes(d.bytes) : '')
                      : engine === 'redis' ? `${(d.keys ?? 0).toLocaleString()} keys`
                      : engine === 'mongodb' && d.bytes ? bytes(d.bytes)
                      : ''
                    const Ctl = multi ? Checkbox : Radio
                    return (
                      <UnstyledButton key={d.name} onClick={() => !off && toggle(d.name)} disabled={off}
                        px="sm" py={narrow ? 10 : 8}
                        style={{ display: 'flex', alignItems: 'center', gap: 10, borderBottom: '1px solid var(--color-line)',
                          background: on ? 'var(--mantine-color-azure-light)' : undefined, opacity: off ? 0.55 : 1,
                          cursor: off ? 'default' : 'pointer' }}>
                        <Ctl checked={on} disabled={off} readOnly tabIndex={-1} size="xs" aria-label={d.name}
                          style={{ pointerEvents: 'none' }} />
                        <Text size={FS.body} fw={600} ff="monospace" style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                          {engine === 'redis' ? `db ${d.name}` : d.name}
                        </Text>
                        <Box ml="auto" style={{ flexShrink: 0 }}>
                          {d.added ? <Badge size="xs" variant="light" color="slate" radius="sm">In Argus</Badge>
                            : <Text size={FS.micro} c="dimmed">{meta}</Text>}
                        </Box>
                      </UnstyledButton>
                    )
                  })}
                </Stack>
              </ScrollArea.Autosize>
            )}
            {listing && (
              <Text size={FS.micro} c="dimmed">
                {engine === 'postgres' && 'Template databases and the postgres maintenance database are left out. '}
                {engine === 'mysql' && 'MySQL’s own schemas (mysql, sys, information_schema, performance_schema) are left out. '}
                {engine === 'redis' && (redisAdded ? 'This Redis is already in Argus; one entry covers every db. ' : 'Pick the db sessions open in; SELECT moves between them. ')}
                {ans?.truncated && 'Only the first 500 are listed. '}
                Not listed? <Anchor component="button" type="button" size={FS.micro} onClick={() => setManual(true)}>Type a name instead</Anchor>
              </Text>
            )}
          </Stack>
        )}

        {scoped && (
          <Stack gap={6}>
            <TextInput label={info.nameLabel} placeholder="*" value={typed} onChange={(e) => setTyped(e.currentTarget.value)}
              styles={{ input: { fontFamily: 'var(--font-mono)' } }} description={info.nameHint} />
            {found.isFetching ? (
              <Group gap={6}><Loader size={12} /><Text size={FS.micro} c="dimmed">Asking the agent for the cluster's topics…</Text></Group>
            ) : listing ? (
              <KafkaCoverage scope={typed.trim() || '*'} topics={(ans?.databases ?? []).map((d) => d.name)} truncated={ans?.truncated} />
            ) : failed ? (
              <Alert color={ans?.state === 'offline' ? 'rose' : 'amber'} variant="light" p="xs"><Text size={FS.meta}>{why}</Text></Alert>
            ) : null}
          </Stack>
        )}

        {(manual || failed) && !scoped && (
          <TextInput label={info.nameLabel} placeholder={info.defaults.dbname || 'orders'} value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)}
            description={<>{info.nameHint}{manual && <> <Anchor component="button" type="button" size={FS.micro} onClick={() => { setManual(false); setTyped('') }}>Pick from the server instead</Anchor></>}</>} />
        )}

        {engine !== 'postgres' && (
          <TextInput label={engine === 'rabbitmq' ? 'Management API address' : engine === 'kafka' ? 'Listener address' : 'Address'} placeholder={info.defaults.host}
            value={host} onChange={(e) => setHost(e.currentTarget.value)}
            description={engine === 'kafka' ? 'On this server: a SASL listener with SCRAM, and an authorizer on the cluster.'
              : engine !== 'rabbitmq' ? 'On this server: 127.0.0.1, or the path of its unix socket.'
              : 'On this server, with the management plugin on (rabbitmq-plugins enable rabbitmq_management).'} />
        )}
        <NumberInput label={engine === 'rabbitmq' ? 'Management port' : 'Port'} value={port} onChange={setPort} min={1} max={65535}
          placeholder={String(info.defaults.port)}
          description={`Only if it is not ${info.defaults.port}. Changing it asks the agent again.`} />
        {engine === 'redis' && (
          <TextInput label="Config file" placeholder={info.defaults.configPath} value={configPath}
            onChange={(e) => setConfigPath(e.currentTarget.value)}
            description="The redis.conf or valkey.conf the agent reads requirepass from to sign in as admin. With the default and no redis.conf, it reads /etc/valkey/valkey.conf. The password never leaves the server and is never stored in Argus." />
        )}
        {engine === 'mysql' && (
          <TextInput label="Option file" placeholder="none: root over the socket" value={configPath}
            onChange={(e) => setConfigPath(e.currentTarget.value)}
            description="Only if root cannot sign in over the socket: a .cnf on the server, private to root, with a [client] user and password that may create users and grant. The agent reads it there; it is never sent or stored in Argus." />
        )}
        {engine === 'mongodb' && (
          <TextInput label="Admin file" placeholder={info.defaults.configPath} value={configPath}
            onChange={(e) => setConfigPath(e.currentTarget.value)}
            description="A file on the server, readable by root only, with user = and password = for an account that may create users in this database. The agent reads it there; the password never leaves the server and is never stored in Argus." />
        )}
        {engine === 'kafka' && (
          <TextInput label="Admin file" placeholder={info.defaults.configPath} value={configPath}
            onChange={(e) => setConfigPath(e.currentTarget.value)} styles={{ input: { fontFamily: 'var(--font-mono)' } }}
            description="On the server, readable by root only: user = and password = for a SCRAM account that may manage users and ACLs. The agent gives each person their own user with ACLs on these topics, for their session only. The password never leaves the server." />
        )}
        {engine === 'rabbitmq' && (
          <Text size={FS.micro} c="dimmed">
            The agent uses rabbitmqctl on the server to give each person a user for their session only. No password is typed in or stored.
          </Text>
        )}
        {engine === 'postgres' && (
          <NumberInput label="PgBouncer port" value={poolerPort} onChange={setPoolerPort} min={1} max={65535} placeholder="none"
            description="If applications here connect through PgBouncer, sessions can too. Its address and one-time setup are on the database's Connection tab." />
        )}
        <Stack gap={4}>
          <Text size={FS.meta} fw={500}>Label</Text>
          <SegmentedControl value={label} onChange={(v) => setLabel(v as DBLabel)} fullWidth={narrow}
            data={[{ label: 'Production', value: 'production' }, { label: 'Staging', value: 'staging' }, { label: narrow ? 'Dev' : 'Development', value: 'development' }]} />
          <Text size={FS.micro} c="dimmed">
            {names.length > 1 ? 'Applies to all of them. ' : ''}On production, Write and above need approval each time unless you say otherwise.
          </Text>
        </Stack>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void submit()} loading={busy} disabled={!valid} fullWidth={narrow}>
            {listing && names.length > 1 ? `Add ${names.length} ${info.noun}s` : 'Add'}
          </Button>
        </Group>
        </>)}
      </Stack>
    </Modal>
  )
}

/** Which of the cluster's topics a scope covers, so a prefix can be checked before it is saved. */
function KafkaCoverage({ scope, topics, truncated }: { scope: string; topics: string[]; truncated?: boolean }) {
  const covered = kafkaCovered(scope, topics)
  const shown = covered.slice(0, 6)
  return (
    <Text size={FS.micro} c="dimmed">
      {topics.length === 0 ? 'The agent found no topics on this cluster yet.'
        : <>Covers {covered.length} of {truncated ? 'the first ' : ''}{topics.length} topics{shown.length ? ': ' : '.'}
          {shown.length > 0 && <Text span ff="monospace" size={FS.micro}>{shown.join(', ')}{covered.length > shown.length ? ', …' : ''}</Text>}</>}
      {' '}Internal topics such as __consumer_offsets are never shown.
    </Text>
  )
}
