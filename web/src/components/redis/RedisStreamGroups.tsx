import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Box, Button, Card, Checkbox, Group, Menu, Modal, SegmentedControl, Select, SimpleGrid, Stack, Table, Text, TextInput, ActionIcon,
  NumberInput,
} from '@mantine/core'
import { IconDots, IconPlus } from '@tabler/icons-react'
import { FS } from '~/theme'
import type { StoreAnswer, StoreRequest } from '~/components/useStoreSession'
import {
  ackCmd, claimable, claimCmd, deleteGroupCmd, groupNameProblem, idleWords, moveGroupCmd, newGroupCmd, placeId, removeConsumerCmd, stuckWork,
  type GroupFrom, type StreamGroups, type StreamPending,
} from '~/lib/redisStreams'

/*
A stream's consumer groups: where each reads, how far behind it is, its
consumers and the entries they hold. Changes are staged as XGROUP, XACK and
XCLAIM lines in the editor's bar and run on Apply, with an undo copy of the
key, like any edit. Without stage (Read), the groups are shown only.
*/

const mono = { fontFamily: 'var(--font-mono)' }

export function RedisStreamGroups({ streamKey, keyQ, ask, stage, version }: {
  streamKey: string
  keyQ?: boolean
  ask: (req: StoreRequest) => Promise<StoreAnswer>
  /** Stages command lines in the editor; absent where the session cannot edit. */
  stage?: (...cmds: string[][]) => void
  /** Changes after the key is read again, to read the groups again too. */
  version?: unknown
}) {
  const [data, setData] = useState<StreamGroups | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  const [picked, setPicked] = useState<string[]>([])
  const [dialog, setDialog] = useState<null | { kind: 'new' } | { kind: 'move'; group: string } | { kind: 'claim'; from: string }>(null)

  const load = useCallback(async (group: string | null) => {
    const a = await ask({ op: 'stream_groups', key: streamKey, keyQ: keyQ ?? false, ...(group ? { name: group } : {}) })
    if (a.type !== 'result') { setErr(a.error ?? 'could not read the groups'); return }
    const d = a.data as StreamGroups
    setErr(null)
    setData(d)
    if (!group && d.groups.length > 0) {
      const first = [...d.groups].sort((x, y) => y.pending - x.pending)[0]!.name
      setOpen(first)
    }
  }, [ask, streamKey, keyQ])
  useEffect(() => { setOpen(null); setPicked([]); void load(null) }, [load, version])
  useEffect(() => { if (open) { setPicked([]); void load(open) } }, [open, load])

  if (err) return <Alert color="rose" variant="light">{err}</Alert>
  if (!data) return <Text size={FS.meta} c="dimmed">Reading the groups…</Text>
  const shown = data.group === open ? data : null
  const consumers = shown?.consumers ?? []
  const pending = shown?.pending ?? []
  const stuck = stuckWork(consumers)
  const k = streamKey

  return (
    <Stack gap="sm">
      <Group gap="xs" wrap="wrap">
        <Text size={FS.micro} c="dimmed" style={{ flex: 1, minWidth: 200 }}>
          Groups read this stream each from its own place. Behind is entries after the group's last delivered ID.
        </Text>
        {stage && <Button size="xs" leftSection={<IconPlus size={14} />} onClick={() => setDialog({ kind: 'new' })}>New group</Button>}
      </Group>
      {data.groups.length === 0 ? (
        <Text size={FS.meta} c="dimmed">No consumer groups read this stream.</Text>
      ) : (
        <Card withBorder radius="md" padding={0}>
          <Table.ScrollContainer minWidth={520}>
            <Table verticalSpacing={6} fz="sm" highlightOnHover>
              <Table.Thead><Table.Tr><Table.Th>Group</Table.Th><Table.Th ta="right">Consumers</Table.Th><Table.Th ta="right">Pending</Table.Th><Table.Th ta="right">Behind</Table.Th><Table.Th>Last delivered</Table.Th>{stage && <Table.Th w={40} />}</Table.Tr></Table.Thead>
              <Table.Tbody>
                {data.groups.map((g) => (
                  <Table.Tr key={g.name} onClick={() => setOpen(g.name)} style={{ cursor: 'pointer', background: g.name === open ? 'var(--mantine-color-azure-light)' : undefined }}>
                    <Table.Td style={mono} fw={600}>{g.name}</Table.Td>
                    <Table.Td ta="right">{g.consumers}</Table.Td>
                    <Table.Td ta="right" c={g.pending > 0 ? 'amber.8' : undefined}>{g.pending.toLocaleString()}</Table.Td>
                    <Table.Td ta="right" fw={g.behind ? 650 : undefined} c={g.behind ? 'amber.8' : undefined}>{g.behind === undefined ? 'not known' : g.behind.toLocaleString()}</Table.Td>
                    <Table.Td style={mono} fz={FS.micro}>{g.lastDelivered}</Table.Td>
                    {stage && (
                      <Table.Td onClick={(e) => e.stopPropagation()}>
                        <Menu position="bottom-end" withinPortal>
                          <Menu.Target><ActionIcon variant="subtle" size="sm" aria-label={`Change ${g.name}`}><IconDots size={14} /></ActionIcon></Menu.Target>
                          <Menu.Dropdown>
                            <Menu.Item onClick={() => setDialog({ kind: 'move', group: g.name })}>Move {g.name} to…</Menu.Item>
                            <Menu.Item color="rose" onClick={() => stage(deleteGroupCmd(k, g.name))}>Delete {g.name}</Menu.Item>
                          </Menu.Dropdown>
                        </Menu>
                      </Table.Td>
                    )}
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Card>
      )}

      {stuck && (
        <Alert color="amber" variant="light" title="Stuck work">
          {stuck.pending.toLocaleString()} entries have sat with {stuck.name} for {idleWords(stuck.idleMs)}.
          {stage ? ' Claim gives them to another consumer.' : ''}
        </Alert>
      )}

      {shown && open && (
        <SimpleGrid cols={{ base: 1, md: 2 }} spacing="sm">
          <Card withBorder radius="md" padding={0}>
            <Text fw={600} size={FS.body} p="sm">Consumers in {open}</Text>
            {consumers.length === 0 ? <Text size={FS.meta} c="dimmed" px="sm" pb="sm">No consumers.</Text> : (
              <Table verticalSpacing={6} fz="sm">
                <Table.Thead><Table.Tr><Table.Th>Consumer</Table.Th><Table.Th ta="right">Pending</Table.Th><Table.Th ta="right">Seen</Table.Th>{stage && <Table.Th />}</Table.Tr></Table.Thead>
                <Table.Tbody>
                  {consumers.map((c) => (
                    <Table.Tr key={c.name}>
                      <Table.Td style={mono} fz={FS.micro}>{c.name}</Table.Td>
                      <Table.Td ta="right">{c.pending.toLocaleString()}</Table.Td>
                      <Table.Td ta="right" fz={FS.micro}>{idleWords(c.idleMs)} idle</Table.Td>
                      {stage && (
                        <Table.Td ta="right">
                          <Group gap={2} justify="flex-end" wrap="nowrap">
                            {c.pending > 0 && <Button size="compact-xs" variant="subtle" onClick={() => setDialog({ kind: 'claim', from: c.name })}>Claim…</Button>}
                            <Button size="compact-xs" variant="subtle" color="rose" title={c.pending > 0 ? `Its ${c.pending} pending entries are dropped from the group` : undefined}
                              onClick={() => stage(removeConsumerCmd(k, open, c.name))}>Remove</Button>
                          </Group>
                        </Table.Td>
                      )}
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )}
          </Card>
          <Card withBorder radius="md" padding={0}>
            <Group p="sm" gap="xs">
              <Text fw={600} size={FS.body}>Pending in {open}</Text>
              {stage && (
                <Button size="compact-xs" variant="subtle" ml="auto" disabled={picked.length === 0}
                  onClick={() => { stage(ackCmd(k, open, picked)); setPicked([]) }}>
                  Acknowledge{picked.length ? ` ${picked.length}` : '…'}
                </Button>
              )}
            </Group>
            {pending.length === 0 ? <Text size={FS.meta} c="dimmed" px="sm" pb="sm">Nothing is pending.</Text> : (
              <Table verticalSpacing={6} fz="sm">
                <Table.Thead><Table.Tr>{stage && <Table.Th w={30} />}<Table.Th>Entry</Table.Th><Table.Th>With</Table.Th><Table.Th ta="right">For</Table.Th><Table.Th ta="right">Tries</Table.Th></Table.Tr></Table.Thead>
                <Table.Tbody>
                  {pending.map((p) => (
                    <Table.Tr key={p.id}>
                      {stage && (
                        <Table.Td><Checkbox size="xs" aria-label={`Pick ${p.id}`} checked={picked.includes(p.id)}
                          onChange={(e) => { const on = e.currentTarget.checked; setPicked((x) => (on ? [...x, p.id] : x.filter((y) => y !== p.id))) }} /></Table.Td>
                      )}
                      <Table.Td style={mono} fz={FS.micro}>{p.id}</Table.Td>
                      <Table.Td style={mono} fz={FS.micro}>{p.consumer}</Table.Td>
                      <Table.Td ta="right" fz={FS.micro}>{idleWords(p.idleMs)}</Table.Td>
                      <Table.Td ta="right">{p.deliveries}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )}
            {shown.pendingMore && <Text size={FS.micro} c="dimmed" p="sm">Showing the first {pending.length}.</Text>}
          </Card>
        </SimpleGrid>
      )}
      {stage && open && shown && (
        <Group gap="xs" wrap="wrap">
          <Button size="xs" variant="default" onClick={() => setDialog({ kind: 'move', group: open })}>Move {open} to…</Button>
          <Button size="xs" variant="default" onClick={() => stage(deleteGroupCmd(k, open))}>Delete {open}</Button>
          <Text size={FS.micro} c="dimmed" ml="auto">Each change is staged as XGROUP / XACK / XCLAIM lines, with an undo copy, like any edit.</Text>
        </Group>
      )}

      {stage && (
        <>
          <NewGroup opened={dialog?.kind === 'new'} onClose={() => setDialog(null)} groups={data}
            onStage={(group, id) => { stage(newGroupCmd(k, group, id)); setDialog(null) }} />
          <MoveGroup group={dialog?.kind === 'move' ? dialog.group : null} onClose={() => setDialog(null)}
            onStage={(group, id) => { stage(moveGroupCmd(k, group, id)); setDialog(null) }} />
          <Claim from={dialog?.kind === 'claim' ? dialog.from : null} group={open ?? ''} consumers={consumers.map((c) => c.name)} pending={pending}
            onClose={() => setDialog(null)} onStage={(to, minIdle, ids) => { stage(claimCmd(k, open ?? '', to, minIdle, ids)); setDialog(null) }} />
        </>
      )}
    </Stack>
  )
}

function PlacePick({ from, setFrom, id, setId, verb }: { from: GroupFrom; setFrom: (f: GroupFrom) => void; id: string; setId: (s: string) => void; verb: string }) {
  return (
    <>
      <SegmentedControl size="xs" value={from} onChange={(v) => setFrom(v as GroupFrom)} aria-label={verb}
        data={[{ label: 'From the start', value: 'start' }, { label: 'From now', value: 'now' }, { label: 'From an ID', value: 'id' }]} />
      {from === 'id' && <TextInput size="xs" label="Entry ID" description="The group reads the entries after this one." value={id} onChange={(e) => setId(e.currentTarget.value)} placeholder="1700000000000-0" styles={{ input: mono }} />}
    </>
  )
}

function NewGroup({ opened, onClose, groups, onStage }: { opened: boolean; onClose: () => void; groups: StreamGroups; onStage: (g: string, id: string) => void }) {
  const [name, setName] = useState('')
  const [from, setFrom] = useState<GroupFrom>('now')
  const [id, setId] = useState('')
  const problem = groupNameProblem(name.trim(), groups.groups)
  const place = placeId(from, id)
  return (
    <Modal opened={opened} onClose={onClose} title="New consumer group" centered>
      <Stack gap="sm">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} styles={{ input: mono }} data-autofocus />
        <Text size={FS.meta} fw={500}>Reads</Text>
        <PlacePick from={from} setFrom={setFrom} id={id} setId={setId} verb="Where the group starts" />
        {name.trim() && problem && <Text size={FS.micro} c="rose">{problem}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={problem !== null || place === null} onClick={() => place && onStage(name.trim(), place)}>Stage</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function MoveGroup({ group, onClose, onStage }: { group: string | null; onClose: () => void; onStage: (g: string, id: string) => void }) {
  const [from, setFrom] = useState<GroupFrom>('now')
  const [id, setId] = useState('')
  const place = placeId(from, id)
  return (
    <Modal opened={group !== null} onClose={onClose} title={`Move ${group ?? ''} to…`} centered>
      <Stack gap="sm">
        <Text size={FS.micro} c="dimmed">The group's place moves; its pending entries stay with their consumers.</Text>
        <PlacePick from={from} setFrom={setFrom} id={id} setId={setId} verb="Where the group reads from" />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={place === null} onClick={() => group && place && onStage(group, place)}>Stage</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function Claim({ from, group, consumers, pending, onClose, onStage }: {
  from: string | null; group: string; consumers: string[]; pending: StreamPending[]
  onClose: () => void; onStage: (to: string, minIdleMs: number, ids: string[]) => void
}) {
  const [to, setTo] = useState<string | null>(null)
  const [minutes, setMinutes] = useState<number | string>(5)
  const minIdle = Math.max(0, Number(minutes) || 0) * 60_000
  const ids = from ? claimable(pending, from, minIdle) : []
  const others = consumers.filter((c) => c !== from)
  useEffect(() => { setTo(others[0] ?? null) }, [from]) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <Modal opened={from !== null} onClose={onClose} title={`Claim ${from ?? ''}'s entries`} centered>
      <Stack gap="sm">
        <Select label="Give them to" searchable value={to} onChange={setTo} data={others}
          description="Another consumer of the group. A name not in the list becomes a new consumer." />
        <TextInput size="xs" aria-label="Or a new consumer" placeholder="Or a new consumer's name" styles={{ input: mono }}
          onChange={(e) => { const v = e.currentTarget.value.trim(); if (v) setTo(v) }} />
        <NumberInput label="Idle for at least" min={0} value={minutes} onChange={setMinutes} rightSection={<Text size={FS.micro} c="dimmed">min</Text>} rightSectionWidth={36} />
        <Box>
          <Text size={FS.micro} c="dimmed">
            {ids.length === 0 ? 'None of the entries shown has been idle that long.' : `${ids.length} of the entries shown in ${group}.`}
          </Text>
        </Box>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!to || ids.length === 0 || /\s/.test(to)} onClick={() => to && onStage(to, minIdle, ids)}>Stage claim of {ids.length}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
