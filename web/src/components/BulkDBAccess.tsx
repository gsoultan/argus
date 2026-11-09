import {
  ActionIcon, Alert, Anchor, Badge, Box, Button, Checkbox, Group, Menu, Modal, MultiSelect, Paper, SegmentedControl, Stack,
  Table, Text, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useDebouncedValue, useMediaQuery } from '@mantine/hooks'
import { useQueries, useQuery } from '@tanstack/react-query'
import { IconCheck, IconClock, IconDotsVertical, IconUser, IconUsersGroup, IconX } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { EngineMark } from '~/components/EngineMark'
import { LABEL_COLOR } from '~/components/StoreChrome'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  assetDBAccessQuery, dbAssignmentsQuery, planDBAccessQuery, teamsQuery, useGiveDBAccess, useRemoveDBAccess, useRemoveDBAssignment,
  useRemoveDBRule, useSetDBAssignment, usersQuery,
} from '~/lib/queries'
import { engineOf, isBroker, levelName } from '~/lib/engines'
import { FS } from '~/theme'
import type { BulkAccessLine, BulkApproval, DBAssignment, DBLevel, Database, DatabaseRule } from '~/types/domain'

/*
 * Giving teams and people access to many databases and brokers at once.
 *
 * Every grant here writes the same rows giving it one database at a time
 * would, so each database's own Who can connect page shows them as usual.
 * The dialog asks the control plane what would change before anything is
 * written, and shows it; nobody is lowered unless that is ticked.
 */

const LEVELS: DBLevel[] = ['read', 'write', 'owner', 'dba']

/** A database or broker's name the way its own page titles it. */
export function targetName(d: Pick<Database, 'engine' | 'dbname'>): string {
  if (d.engine === 'redis') return `Redis · db ${d.dbname}`
  if (d.engine === 'rabbitmq') return `RabbitMQ · ${d.dbname}`
  if (d.engine === 'kafka') return `Kafka · ${d.dbname === '*' ? 'every topic' : d.dbname}`
  if (d.engine === 'mysql') return `MySQL · ${d.dbname}`
  if (d.engine === 'mongodb') return `MongoDB · ${d.dbname}`
  return d.dbname
}

/** "3 databases, 2 brokers", "1 broker". */
export function countKinds(list: Pick<Database, 'engine'>[]): string {
  const brokers = list.filter((d) => isBroker(d.engine)).length
  const dbs = list.length - brokers
  return [dbs ? `${dbs} database${dbs === 1 ? '' : 's'}` : '', brokers ? `${brokers} broker${brokers === 1 ? '' : 's'}` : '']
    .filter(Boolean).join(', ')
}

/** Who, as the multi-pick holds it: "t:<team id>" or "u:<email>". */
export type WhoKey = string
const splitWho = (keys: WhoKey[]) => ({
  teams: keys.filter((k) => k.startsWith('t:')).map((k) => k.slice(2)),
  people: keys.filter((k) => k.startsWith('u:')).map((k) => k.slice(2)),
})
export const whoKey = (g: { teamId?: string; userEmail?: string }): WhoKey => g.teamId ? `t:${g.teamId}` : `u:${g.userEmail ?? ''}`

/** Teams and people as one grouped pick list, admins left out: they always can. */
function useWhoOptions() {
  const { data: teams } = useQuery(teamsQuery())
  const { data: users } = useQuery(usersQuery())
  const data = useMemo(() => [
    { group: 'Teams', items: (teams ?? []).map((t) => ({ value: `t:${t.id}`, label: `${t.name} (${t.members.length})` })) },
    {
      group: 'People', items: (users ?? []).filter((u) => u.role !== 'admin' && u.role !== 'owner')
        .map((u) => ({ value: `u:${u.email}`, label: `${u.displayName} · ${u.email}` })),
    },
  ], [teams, users])
  const name = (k: WhoKey) => {
    if (k.startsWith('t:')) return teams?.find((t) => t.id === k.slice(2))?.name ?? 'a team'
    const e = k.slice(2)
    return users?.find((u) => u.email === e)?.displayName ?? e
  }
  return { data, name }
}

const CHANGE: Record<BulkAccessLine['change'], { word: string; color: string }> = {
  new: { word: 'new', color: 'teal' },
  raised: { word: 'raised', color: 'azure' },
  approval: { word: 'approval changes', color: 'azure' },
  same: { word: 'already has it', color: 'slate' },
  kept: { word: 'has more, kept', color: 'amber' },
  lowered: { word: 'lowered', color: 'rose' },
  removed: { word: 'removed', color: 'rose' },
}

function LevelPicker({ label, value, onChange, broker }: { label: string; value: DBLevel; onChange: (l: DBLevel) => void; broker: boolean }) {
  const engine = broker ? 'rabbitmq' : 'postgres'
  return (
    <Box>
      <Text size={FS.meta} fw={600} mb={4}>{label}</Text>
      <SegmentedControl fullWidth value={value} onChange={(v) => onChange(v as DBLevel)} aria-label={label}
        data={LEVELS.map((l) => ({ value: l, label: levelName(l, engine) }))} />
      <Text size={FS.micro} c="dimmed" mt={4}>{engineOf(engine).levels[value]}</Text>
    </Box>
  )
}

/**
 * Give teams and people a level on many databases and brokers. `targets`
 * are what was ticked; they can still be unticked here. With every target on
 * one server, the dialog also offers to give the ones added there later.
 */
export function GiveAccessMany({ opened, onClose, targets, initialWho = [] }: {
  opened: boolean
  onClose: () => void
  targets: Database[]
  initialWho?: WhoKey[]
}) {
  const narrow = useMediaQuery('(max-width: 48em)')
  const who = useWhoOptions()
  const give = useGiveDBAccess()
  const [picked, setPicked] = useState<WhoKey[]>(initialWho)
  const [off, setOff] = useState<string[]>([])
  const [level, setLevel] = useState<DBLevel>('read')
  const [brokerLevel, setBrokerLevel] = useState<DBLevel>('read')
  const [approval, setApproval] = useState<BulkApproval>('label')
  const [lower, setLower] = useState(false)
  const [later, setLater] = useState(false)
  const [all, setAll] = useState(false)

  const on = targets.filter((d) => !off.includes(d.id))
  const brokers = on.filter((d) => isBroker(d.engine))
  const dbs = on.filter((d) => !isBroker(d.engine))
  const hosts = [...new Set(targets.map((d) => d.assetId))]
  const host = hosts.length === 1 ? targets[0]?.hostname : undefined
  const { teams, people } = splitWho(picked)
  const input = {
    databaseIds: on.map((d) => d.id), teams, people, level, brokerLevel, approval, lower,
    later: later && !!host,
  }
  const [settled] = useDebouncedValue(input, 300)
  const ready = settled.databaseIds.length > 0 && settled.teams.length + settled.people.length > 0
  const plan = useQuery(planDBAccessQuery(settled, opened && ready))
  const lines = plan.data?.lines ?? []
  const counts = lines.reduce<Record<string, number>>((m, l) => ({ ...m, [l.change]: (m[l.change] ?? 0) + 1 }), {})
  const kept = lines.filter((l) => l.change === 'kept' || l.change === 'lowered')
  const writes = lines.filter((l) => !['same', 'kept'].includes(l.change)).length

  const close = () => {
    setPicked(initialWho); setOff([]); setLower(false); setLater(false); setAll(false)
    onClose()
  }
  const submit = async () => {
    try {
      const out = await give.mutateAsync(input)
      const n = out.lines.filter((l) => !['same', 'kept'].includes(l.change)).length
      const rules = out.rules?.length ?? 0
      notifyOk('Access given', `${n} grant${n === 1 ? '' : 's'} written on ${countKinds(on)}${rules ? `, and ${host} will give the ones added later` : ''}.`)
      close()
    } catch (err) {
      notifyError('Access not given', err)
    }
  }

  const title = host ? `Give access on ${host.split('.')[0]}` : `Give access on ${countKinds(targets)}`
  const kindsWord = dbs.length && brokers.length ? 'databases and brokers' : brokers.length ? 'brokers' : 'databases'

  return (
    <Modal opened={opened} onClose={close} size="xl" fullScreen={narrow} centered={!narrow}
      title={<Group gap="xs"><Text fw={650}>{title}</Text><Text size={FS.meta} c="dimmed">{countKinds(on)}</Text></Group>}>
      <Stack gap="md">
        <MultiSelect label="Who" placeholder={picked.length ? 'Add a team or person' : 'Pick teams and people'} searchable
          data={who.data} value={picked} onChange={setPicked} nothingFoundMessage="Nobody by that name"
          description="Any number of teams and people. Each gets its own grant on every one picked, the same as giving it one by one." />
        <Group grow align="flex-start" wrap={narrow ? 'wrap' : 'nowrap'}>
          {dbs.length > 0 && <LevelPicker label={brokers.length ? 'Databases · what they can do' : 'What they can do'} value={level} onChange={setLevel} broker={false} />}
          {brokers.length > 0 && <LevelPicker label={dbs.length ? 'Brokers · what they can do' : 'What they can do'} value={brokerLevel} onChange={setBrokerLevel} broker />}
        </Group>
        <Box>
          <Text size={FS.meta} fw={600} mb={4}>Ask an approver before each session</Text>
          <SegmentedControl value={approval} onChange={(v) => setApproval(v as BulkApproval)} aria-label="Ask an approver before each session"
            data={[{ value: 'label', label: 'By label' }, { value: 'always', label: 'Always' }, { value: 'never', label: 'Never' }]} />
          <Text size={FS.micro} c="dimmed" mt={4}>
            {approval === 'label' ? 'As each one’s label says: on for Write and above on production, off elsewhere.'
              : approval === 'always' ? 'Every session waits for an approver, on each one picked.' : 'Sessions open straight away, even on production.'}
          </Text>
        </Box>
        <Box>
          <Text size={FS.meta} fw={600} mb={4}>On</Text>
          <Paper withBorder radius="md" p="xs" mah={180} style={{ overflowY: 'auto' }}>
            <Stack gap={4}>
              {targets.map((d) => (
                <Checkbox key={d.id} checked={!off.includes(d.id)} size="xs"
                  onChange={(e) => { const on = e.currentTarget.checked; setOff((o) => on ? o.filter((x) => x !== d.id) : [...o, d.id]) }}
                  label={<Group gap={6} wrap="nowrap"><EngineMark engine={d.engine} size={16} />
                    <Text size={FS.meta}>{targetName(d)}</Text>
                    <Text size={FS.micro} c="dimmed" ff="monospace">{d.hostname}</Text>
                    <Badge size="xs" variant="light" radius="sm" color={LABEL_COLOR[d.label]}>{d.label}</Badge></Group>} />
              ))}
            </Stack>
          </Paper>
        </Box>
        {host && (
          <Checkbox checked={later} onChange={(e) => setLater(e.currentTarget.checked)}
            label={`Also give it on ${kindsWord} added to ${host.split('.')[0]} later`}
            description={`Each one added to ${host} from now on goes to them at the same level, straight away. You can stop it from the server’s Who has what.`} />
        )}

        <Paper radius="md" p="sm" bg="var(--mantine-color-default-hover)" aria-label="What changes">
          <Group justify="space-between" mb={lines.length ? 6 : 0}>
            <Text size={FS.meta} fw={650}>What changes</Text>
            {lines.length > 0 && <Anchor component="button" type="button" size={FS.micro} onClick={() => setAll((v) => !v)}>
              {all ? 'Hide' : `See all ${lines.length}`}</Anchor>}
          </Group>
          {!ready ? <Text size={FS.micro} c="dimmed">Pick who, and at least one database or broker.</Text>
            : plan.isError ? <Text size={FS.micro} c="rose">{(plan.error as Error).message}</Text>
            : !plan.data ? <Text size={FS.micro} c="dimmed">Working it out…</Text>
            : (
              <Stack gap={6}>
                <Group gap={6}>
                  {(Object.keys(CHANGE) as BulkAccessLine['change'][]).filter((c) => counts[c]).map((c) => (
                    <Badge key={c} variant="light" radius="sm" color={CHANGE[c].color}>{counts[c]} {CHANGE[c].word}</Badge>
                  ))}
                </Group>
                {kept.length > 0 && (
                  <Checkbox size="xs" checked={lower} onChange={(e) => setLower(e.currentTarget.checked)}
                    label="Also lower anyone who has more"
                    description={`${kept.length} ${kept.length === 1 ? 'grant has' : 'grants have'} more than this. Left alone unless this is ticked; an open session keeps its level until it ends.`} />
                )}
                {all && <PlanTable lines={lines} who={who.name} />}
              </Stack>
            )}
        </Paper>

        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button onClick={() => void submit()} loading={give.isPending} disabled={!ready || on.length === 0 || plan.isError || (writes === 0 && !later)}>
            {writes === 0 && plan.data && !later ? 'Nothing to change' : `Give access on ${on.length}`}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function PlanTable({ lines, who }: { lines: BulkAccessLine[]; who: (k: WhoKey) => string }) {
  return (
    <Box mah={260} style={{ overflowY: 'auto' }}>
      <Table verticalSpacing={4} fz={FS.micro}>
        <Table.Thead><Table.Tr><Table.Th>Who</Table.Th><Table.Th>On</Table.Th><Table.Th>Now</Table.Th><Table.Th>After</Table.Th><Table.Th /></Table.Tr></Table.Thead>
        <Table.Tbody>
          {lines.map((l) => (
            <Table.Tr key={`${whoKey(l)}|${l.databaseId}`} style={l.change === 'same' ? { opacity: 0.6 } : undefined}>
              <Table.Td>{l.teamId ? l.teamName : who(whoKey(l))}</Table.Td>
              <Table.Td>{targetName({ engine: l.engine as Database['engine'], dbname: l.databaseName })}</Table.Td>
              <Table.Td>{l.before ? levelName(l.before, l.engine) : '—'}{l.beforeApproval ? ' ⏱' : ''}</Table.Td>
              <Table.Td fw={600}>{l.after ? levelName(l.after, l.engine) : '—'}{l.afterApproval ? ' ⏱' : ''}</Table.Td>
              <Table.Td><Badge size="xs" variant="light" radius="sm" color={CHANGE[l.change].color}>{CHANGE[l.change].word}</Badge></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      <Text size={FS.micro} c="dimmed" mt={4}>⏱ asks an approver before each session.</Text>
    </Box>
  )
}

/**
 * Take teams and people off many databases at once. Lists only who has
 * something among them, with how much, and names what goes.
 */
export function RemoveAccessMany({ opened, onClose, targets }: { opened: boolean; onClose: () => void; targets: Database[] }) {
  const who = useWhoOptions()
  const remove = useRemoveDBAccess()
  const results = useQueries({ queries: targets.map((d) => ({ ...dbAssignmentsQuery(d.id), enabled: opened })) })
  const loading = results.some((r) => r.isPending)
  const holders = useMemo(() => {
    const m = new Map<WhoKey, { key: WhoKey; label: string; team: boolean; on: string[] }>()
    results.forEach((r, i) => {
      for (const g of r.data ?? []) {
        const k = whoKey(g)
        const e = m.get(k) ?? { key: k, label: g.teamId ? g.teamName ?? 'a team' : who.name(k), team: !!g.teamId, on: [] }
        e.on.push(targetName(targets[i]!))
        m.set(k, e)
      }
    })
    return [...m.values()].sort((a, b) => Number(b.team) - Number(a.team) || a.label.localeCompare(b.label))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [results.map((r) => r.dataUpdatedAt).join(), targets])
  const [picked, setPicked] = useState<WhoKey[]>([])
  const chosen = holders.filter((h) => picked.includes(h.key))
  const close = () => { setPicked([]); onClose() }
  const submit = async () => {
    try {
      const out = await remove.mutateAsync({ databaseIds: targets.map((d) => d.id), ...splitWho(picked) })
      const n = out.lines?.length ?? 0
      notifyOk('Access removed', `${n} grant${n === 1 ? '' : 's'} taken away. Open sessions keep their user until they end.`)
      close()
    } catch (err) {
      notifyError('Not removed', err)
    }
  }
  return (
    <Modal opened={opened} onClose={close} centered size="lg"
      title={<Group gap="xs"><Text fw={650}>Remove access</Text><Text size={FS.meta} c="dimmed">{countKinds(targets)}</Text></Group>}>
      <Stack gap="sm">
        <Text size={FS.meta} fw={600}>Who loses access</Text>
        {loading ? <Text size={FS.meta} c="dimmed">Loading who has them…</Text>
          : holders.length === 0 ? <Text size={FS.meta} c="dimmed">Nobody but admins can open these.</Text>
          : (
            <Paper withBorder radius="md" p="xs">
              <Stack gap={6}>
                {holders.map((h) => (
                  <Checkbox key={h.key} checked={picked.includes(h.key)}
                    onChange={(e) => { const on = e.currentTarget.checked; setPicked((p) => on ? [...p, h.key] : p.filter((x) => x !== h.key)) }}
                    label={<Group gap={6} wrap="nowrap">{h.team ? <IconUsersGroup size={14} /> : <IconUser size={14} />}
                      <Text size={FS.meta} fw={500}>{h.label}</Text>
                      <Text size={FS.micro} c="dimmed">has {h.on.length} of the {targets.length}</Text></Group>} />
                ))}
              </Stack>
            </Paper>
          )}
        {chosen.length > 0 && (
          <Alert color="amber" variant="light" p="sm">
            {chosen.map((h) => (
              <Text key={h.key} size={FS.meta} fw={600}>{h.label} loses {h.on.join(', ')}.</Text>
            ))}
            <Text size={FS.micro} mt={4}>Open sessions keep their user until they end. People in a team who also have access by name keep that.</Text>
          </Alert>
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button color="rose" disabled={chosen.length === 0} loading={remove.isPending} onClick={() => void submit()}>
            {chosen.length === 1 ? `Remove ${chosen[0]!.label} from ${chosen[0]!.on.length}` : `Remove ${chosen.length || ''} from these`}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/** The bar that appears once something is ticked. */
export function SelectionBar({ selected, total, onAll, onClear, onGive, onRemove, where }: {
  selected: Database[]
  total: number
  onAll?: () => void
  onClear: () => void
  onGive: () => void
  onRemove: () => void
  where?: string
}) {
  if (selected.length === 0) return null
  const hosts = new Set(selected.map((d) => d.assetId)).size
  return (
    <Paper radius="md" p="xs" px="sm" bg="dark.8" c="white" role="region" aria-label="Selected"
      style={{ position: 'sticky', bottom: 12, zIndex: 3, boxShadow: '0 8px 24px rgba(16,24,40,.24)' }}>
      <Group gap="sm" wrap="wrap">
        <Text size={FS.body} fw={650}>{selected.length} selected</Text>
        <Text size={FS.micro} c="gray.4">{where ?? (hosts > 1 ? `on ${hosts} servers` : countKinds(selected))}</Text>
        {onAll && selected.length < total && <Anchor component="button" type="button" size={FS.micro} c="blue.2" onClick={onAll}>Select all {total}</Anchor>}
        <Anchor component="button" type="button" size={FS.micro} c="blue.2" onClick={onClear}>Clear</Anchor>
        <Group gap="xs" ml="auto">
          <Button size="xs" variant="white" color="dark" onClick={onRemove}>Remove access…</Button>
          <Button size="xs" onClick={onGive}>Give access…</Button>
        </Group>
      </Group>
    </Paper>
  )
}

/** A tick box for a header: all, none or some. */
export function TickAll({ ids, selected, onChange, label }: { ids: string[]; selected: string[]; onChange: (ids: string[]) => void; label: string }) {
  const n = ids.filter((id) => selected.includes(id)).length
  return (
    <Checkbox size="xs" aria-label={label} checked={ids.length > 0 && n === ids.length} indeterminate={n > 0 && n < ids.length}
      onChange={() => onChange(n === ids.length ? selected.filter((s) => !ids.includes(s)) : [...new Set([...selected, ...ids])])} />
  )
}

/** One sentence per database for the server's list: who may open it. */
export function whoHasIt(rows: DBAssignment[] | undefined, engine: string, name: (k: WhoKey) => string): string {
  if (!rows || rows.length === 0) return 'Only admins'
  return rows.map((g) => `${g.teamId ? g.teamName : name(whoKey(g))} · ${levelName(g.level, engine)}`).join(', ')
}

/**
 * Who has what on one server: teams and people down, its databases and
 * brokers across. A cell is one grant, changed or taken away in place;
 * Change all opens the many-at-once dialog for that row. The server's
 * rules for databases added later are listed under it.
 */
export function WhoHasWhat({ hostname, assetId, databases, onChangeAll }: {
  hostname: string
  assetId: string
  databases: Database[]
  onChangeAll: (who: WhoKey) => void
}) {
  const { data } = useQuery(assetDBAccessQuery(assetId))
  const who = useWhoOptions()
  const [kind, setKind] = useState<'all' | 'db' | 'broker'>('all')
  const cols = databases.filter((d) => kind === 'all' || (kind === 'broker') === isBroker(d.engine))
  const rows = useMemo(() => {
    const m = new Map<WhoKey, { key: WhoKey; label: string; team: boolean; by: Map<string, DBAssignment> }>()
    for (const g of data?.assignments ?? []) {
      const k = whoKey(g)
      const e = m.get(k) ?? { key: k, label: g.teamId ? g.teamName ?? '' : who.name(k), team: !!g.teamId, by: new Map() }
      e.by.set(g.databaseId, g)
      m.set(k, e)
    }
    return [...m.values()].sort((a, b) => Number(b.team) - Number(a.team) || a.label.localeCompare(b.label))
  }, [data, who])
  const brokersHere = databases.some((d) => isBroker(d.engine))
  const dbsHere = databases.some((d) => !isBroker(d.engine))

  return (
    <Stack gap="sm">
      {brokersHere && dbsHere && (
        <SegmentedControl size="xs" w="fit-content" value={kind} onChange={(v) => setKind(v as typeof kind)} aria-label="Show"
          data={[{ value: 'all', label: 'All' }, { value: 'db', label: 'Databases' }, { value: 'broker', label: 'Brokers' }]} />
      )}
      {!data ? <Text size={FS.meta} c="dimmed">Loading…</Text> : rows.length === 0 ? (
        <Text size={FS.meta} c="dimmed">Only admins can open these so far.</Text>
      ) : (
        <Table.ScrollContainer minWidth={Math.max(520, 200 + cols.length * 96)} type="native">
          <Table verticalSpacing={6} aria-label={`Who has what on ${hostname}`}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Team or person</Table.Th>
                {cols.map((d) => (
                  <Table.Th key={d.id} ta="center" style={{ verticalAlign: 'bottom' }}>
                    <Stack gap={2} align="center">
                      <EngineMark engine={d.engine} size={16} />
                      <Text size={FS.micro} fw={600} ff="monospace" lh={1.2}>{d.engine === 'redis' ? `db ${d.dbname}` : d.dbname}</Text>
                      <Text size="10px" c="dimmed" lh={1}>{isBroker(d.engine) ? 'broker' : engineOf(d.engine).label}</Text>
                    </Stack>
                  </Table.Th>
                ))}
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((r) => (
                <Table.Tr key={r.key}>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">{r.team ? <IconUsersGroup size={14} /> : <IconUser size={14} />}
                      <Text size={FS.meta} fw={600} truncate>{r.label}</Text></Group>
                  </Table.Td>
                  {cols.map((d) => <Cell key={d.id} d={d} g={r.by.get(d.id)} rowKey={r.key} label={r.label} />)}
                  <Table.Td ta="right">
                    <Anchor component="button" type="button" size={FS.micro} onClick={() => onChangeAll(r.key)}>Change all…</Anchor>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
      <Text size={FS.micro} c="dimmed">Click a cell to change or take away one grant. ⏱ asks an approver before each session.</Text>
      <Rules assetId={assetId} hostname={hostname} rules={data?.rules ?? []} name={who.name} />
    </Stack>
  )
}

function Cell({ d, g, rowKey, label }: { d: Database; g?: DBAssignment; rowKey: WhoKey; label: string }) {
  const set = useSetDBAssignment()
  const remove = useRemoveDBAssignment()
  const target = rowKey.startsWith('t:') ? { teamId: rowKey.slice(2) } : { userEmail: rowKey.slice(2) }
  const put = async (level: DBLevel, needsApproval: boolean) => {
    try {
      await set.mutateAsync({ id: d.id, level, needsApproval, ...target })
      notifyOk('Access changed', `${label}: ${levelName(level, d.engine)} on ${targetName(d)}${needsApproval ? ', with approval before each session' : ''}.`)
    } catch (err) {
      notifyError('Not changed', err)
    }
  }
  const takeAway = async () => {
    if (!g) return
    try {
      await remove.mutateAsync({ id: d.id, assignmentId: g.id })
      notifyOk('Removed', `${label} can no longer open ${targetName(d)}.`)
    } catch (err) {
      notifyError('Not removed', err)
    }
  }
  const byLabel = (l: DBLevel) => d.label === 'production' && l !== 'read'
  return (
    <Table.Td ta="center">
      <Menu position="bottom" withinPortal shadow="md" width={210}>
        <Menu.Target>
          <UnstyledButton aria-label={`${label} on ${targetName(d)}: ${g ? levelName(g.level, d.engine) : 'none'}`}
            style={{ borderRadius: 6, padding: '2px 6px' }}>
            {g ? (
              <Badge variant="light" radius="sm" color={g.level === 'read' ? 'slate' : 'azure'}
                rightSection={g.needsApproval ? <IconClock size={10} /> : undefined}>{levelName(g.level, d.engine)}</Badge>
            ) : <Text size={FS.meta} c="dimmed">—</Text>}
          </UnstyledButton>
        </Menu.Target>
        <Menu.Dropdown>
          <Menu.Label>{label} on {targetName(d)}</Menu.Label>
          {LEVELS.map((l) => (
            <Menu.Item key={l} rightSection={g?.level === l ? <IconCheck size={14} /> : undefined}
              onClick={() => void put(l, g ? g.needsApproval : byLabel(l))}>{levelName(l, d.engine)}</Menu.Item>
          ))}
          {g && (
            <>
              <Menu.Divider />
              <Menu.Item onClick={() => void put(g.level, !g.needsApproval)}>
                {g.needsApproval ? 'Open without asking' : 'Ask before each session'}
              </Menu.Item>
              <Menu.Item color="rose" leftSection={<IconX size={14} />} onClick={() => void takeAway()}>Take away</Menu.Item>
            </>
          )}
        </Menu.Dropdown>
      </Menu>
    </Table.Td>
  )
}

function Rules({ assetId, hostname, rules, name }: { assetId: string; hostname: string; rules: DatabaseRule[]; name: (k: WhoKey) => string }) {
  const remove = useRemoveDBRule()
  if (rules.length === 0) return null
  const stop = async (r: DatabaseRule, takeAway: boolean) => {
    try {
      const out = await remove.mutateAsync({ assetId, ruleId: r.id, takeAway })
      const n = out.lines?.length ?? 0
      notifyOk('Stopped', takeAway ? `New ${r.kind}s on ${hostname} are no longer given, and ${n} grant${n === 1 ? '' : 's'} it gave ${n === 1 ? 'was' : 'were'} taken back.`
        : `New ${r.kind}s on ${hostname} are no longer given. What it already gave stays.`)
    } catch (err) {
      notifyError('Not stopped', err)
    }
  }
  return (
    <Box mt="xs">
      <Text size={FS.meta} fw={650}>Given when added later</Text>
      <Text size={FS.micro} c="dimmed" mb={6}>Each database or broker added to {hostname} from now on goes to these at once.</Text>
      <Stack gap={4}>
        {rules.map((r) => (
          <Group key={r.id} gap="sm" wrap="nowrap" justify="space-between">
            <Group gap={6} wrap="nowrap" style={{ minWidth: 0 }}>
              {r.teamId ? <IconUsersGroup size={14} /> : <IconUser size={14} />}
              <Text size={FS.meta} truncate>
                <b>{r.teamId ? r.teamName : name(`u:${r.userEmail}`)}</b> gets {levelName(r.level, r.kind === 'broker' ? 'rabbitmq' : 'postgres')} on every new {r.kind}
                {r.approval === 'always' ? ', always with approval' : r.approval === 'never' ? ', never with approval' : ''}
              </Text>
              <Text size={FS.micro} c="dimmed">gave {r.gave}</Text>
            </Group>
            <Menu withinPortal position="bottom-end">
              <Menu.Target>
                <Tooltip label="Stop"><ActionIcon variant="subtle" color="slate" aria-label="Stop this rule"><IconDotsVertical size={16} /></ActionIcon></Tooltip>
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Item onClick={() => void stop(r, false)}>Stop, keep what it gave</Menu.Item>
                <Menu.Item color="rose" disabled={r.gave === 0} onClick={() => void stop(r, true)}>Stop and take back {r.gave}</Menu.Item>
              </Menu.Dropdown>
            </Menu>
          </Group>
        ))}
      </Stack>
    </Box>
  )
}
