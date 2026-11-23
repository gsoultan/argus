import {
  Badge, Box, Button, Card, Group, Modal, NumberInput, SimpleGrid, Stack, Table, Text, Textarea, TextInput, UnstyledButton,
} from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useCallback, useEffect, useState } from 'react'
import { fmtCount } from '~/lib/rabbit'
import {
  MAX_SUPER_PARTITIONS, parseKeys, superStreamKeys, superStreamPlan, superStreamProblem, type SuperStream,
} from '~/lib/rabbitStreams'
import { FS } from '~/theme'
import { MONO, useBroker } from './BrokerWorkspace'

/*
Super streams: one stream split into partitions, each a stream of its own.
RabbitMQ keeps one as an exchange with x-super-stream and a stream per
partition bound to it in order, as rabbitmq-streams add_super_stream makes
it. Each partition opens as a stream you can read.
*/

export function SuperStreamsCard() {
  const { ask, can, openQueue, confirm } = useBroker()
  const [list, setList] = useState<SuperStream[] | null>(null)
  const [creating, setCreating] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const load = useCallback(async () => {
    const a = await ask({ op: 'super_streams' })
    if (a.type === 'result') setList(Array.isArray(a.data) ? (a.data as SuperStream[]) : [])
  }, [ask])
  useEffect(() => { void load() }, [load])

  if (!list || (list.length === 0 && !can('declare_super_stream'))) return null
  const shown = list.find((s) => s.name === open)
  return (
    <Card withBorder radius="md" padding={0}>
      <Group gap="xs" p="md" pb="xs" wrap="wrap">
        <Box>
          <Text fw={600} size={FS.body}>Super streams</Text>
          <Text size={FS.micro} c="dimmed">One stream split into partitions, each a stream of its own.</Text>
        </Box>
        {can('declare_super_stream') && (
          <Button size="xs" ml="auto" leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New super stream</Button>
        )}
      </Group>
      {list.length === 0 ? (
        <Text size={FS.meta} c="dimmed" px="md" pb="md">None on this vhost.</Text>
      ) : (
        <Table verticalSpacing={6} fz="sm">
          <Table.Thead><Table.Tr><Table.Th>Super stream</Table.Th><Table.Th ta="right">Partitions</Table.Th><Table.Th ta="right">Messages</Table.Th><Table.Th /></Table.Tr></Table.Thead>
          <Table.Tbody>
            {list.map((s) => (
              <Table.Tr key={s.name}>
                <Table.Td ff="monospace" fw={600}>{s.name}</Table.Td>
                <Table.Td ta="right">{s.partitions.length}</Table.Td>
                <Table.Td ta="right">{fmtCount(s.messages)}</Table.Td>
                <Table.Td ta="right"><Button size="compact-xs" variant="subtle" onClick={() => setOpen(s.name)}>Open</Button></Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      <Modal opened={shown !== undefined} onClose={() => setOpen(null)} title={shown ? `Super stream ${shown.name}` : ''} centered size="lg">
        {shown && (
          <Stack gap="sm">
            <Text size={FS.micro} c="dimmed">Each partition is a stream. Open one to read it.</Text>
            <Table verticalSpacing={6} fz="sm">
              <Table.Thead><Table.Tr><Table.Th>#</Table.Th><Table.Th>Stream</Table.Th><Table.Th>Routing key</Table.Th><Table.Th ta="right">Messages</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {shown.partitions.map((p) => (
                  <Table.Tr key={p.stream}>
                    <Table.Td>{p.order}</Table.Td>
                    <Table.Td>
                      {p.missing ? (
                        <Group gap={4}><Text ff="monospace" size={FS.meta}>{p.stream}</Text><Badge size="xs" color="rose" variant="light">missing</Badge></Group>
                      ) : (
                        <UnstyledButton onClick={() => { setOpen(null); openQueue(p.stream) }}>
                          <Text ff="monospace" size={FS.meta} c="azure">{p.stream}</Text>
                        </UnstyledButton>
                      )}
                    </Table.Td>
                    <Table.Td ff="monospace" fz={FS.micro}>{p.routingKey}</Table.Td>
                    <Table.Td ta="right">{fmtCount(p.messages)}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
            {can('delete_super_stream') && (
              <Group justify="flex-end">
                <Button size="xs" variant="subtle" color="rose" onClick={() => {
                  const s = shown
                  setOpen(null)
                  confirm({ op: 'delete_super_stream', req: { name: s.name },
                    what: `Delete super stream ${s.name}: its exchange and its ${s.partitions.length} partition streams, with their ${fmtCount(s.messages)} messages.`,
                    then: () => void load() })
                }}>Delete super stream</Button>
              </Group>
            )}
          </Stack>
        )}
      </Modal>
      <NewSuperStream opened={creating} onClose={() => setCreating(false)} onCreated={() => void load()} />
    </Card>
  )
}

function NewSuperStream({ opened, onClose, onCreated }: { opened: boolean; onClose: () => void; onCreated: () => void }) {
  const { act, database, exchanges, queues, reload } = useBroker()
  const [name, setName] = useState('')
  const [by, setBy] = useState<'hash' | 'keys'>('hash')
  const [partitions, setPartitions] = useState<number | string>(3)
  const [keysText, setKeysText] = useState('')
  const [days, setDays] = useState<number | string>('')
  const [gb, setGB] = useState<number | string>('')
  const [busy, setBusy] = useState(false)
  const n = name.trim()
  const keys = superStreamKeys(by, Number(partitions) || 0, parseKeys(keysText))
  const problem = superStreamProblem(n, keys, [...exchanges.map((x) => x.name), ...queues.map((q) => q.name)])
  const ageDays = Math.max(0, Number(days) || 0)
  const maxGB = Math.max(0, Number(gb) || 0)
  const plan = superStreamPlan(n || 'name', keys, ageDays, maxGB)

  const create = async () => {
    setBusy(true)
    const a = await act({
      op: 'declare_super_stream', name: n,
      superStream: {
        ...(by === 'keys' ? { keys } : { partitions: keys.length }),
        maxAgeS: ageDays > 0 ? Math.round(ageDays * 86400) : undefined,
        maxLengthBytes: maxGB > 0 ? Math.round(maxGB * 1e9) : undefined,
      },
    }, `Created super stream ${n} with ${keys.length} partitions.`)
    setBusy(false)
    if (a.type === 'result') {
      onClose(); setName(''); setKeysText('')
      onCreated(); void reload(true)
    }
  }
  const choice = (v: 'hash' | 'keys', title: string, sub: string) => (
    <UnstyledButton onClick={() => setBy(v)} aria-pressed={by === v}
      style={{ border: `1px solid ${by === v ? 'var(--mantine-color-azure-6)' : 'var(--color-line)'}`, borderRadius: 8, padding: '8px 10px',
        background: by === v ? 'var(--mantine-color-azure-light)' : undefined }}>
      <Text size={FS.meta} fw={600}>{title}</Text>
      <Text size={FS.micro} c="dimmed">{sub}</Text>
    </UnstyledButton>
  )
  return (
    <Modal opened={opened} onClose={onClose} centered size="xl"
      title={<Group gap="xs"><Text fw={650}>New super stream</Text>{database.label && <Badge size="sm" variant="outline" color="slate" style={{ textTransform: 'none' }}>{database.label}</Badge>}</Group>}>
      <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="lg">
        <Stack gap="sm">
          <SimpleGrid cols={2} spacing="sm">
            <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} styles={MONO} data-autofocus placeholder="payments" />
            {by === 'hash' && (
              <NumberInput label="Partitions" min={1} max={MAX_SUPER_PARTITIONS} value={partitions} onChange={setPartitions} />
            )}
          </SimpleGrid>
          <Text size={FS.micro} c="dimmed">
            {by === 'hash'
              ? <>Makes an exchange <Text span ff="monospace" size={FS.micro}>{n || 'name'}</Text> and {keys.length} streams, <Text span ff="monospace" size={FS.micro}>{n || 'name'}-0</Text>{keys.length > 1 && <> to <Text span ff="monospace" size={FS.micro}>{n || 'name'}-{keys.length - 1}</Text></>}. Producers pick a partition by hashing the routing key.</>
              : <>One stream per routing key, named <Text span ff="monospace" size={FS.micro}>{n || 'name'}-</Text> and the key. Producers pick a partition by its key.</>}
          </Text>
          <Text size={FS.meta} fw={500}>Partition by</Text>
          <SimpleGrid cols={2} spacing="sm">
            {choice('hash', 'Routing key hash', `Partitions named 0 to ${Math.max(0, keys.length - 1)}`)}
            {choice('keys', 'Routing keys I name', 'One per partition')}
          </SimpleGrid>
          {by === 'keys' && (
            <Textarea label="Routing keys" description="One per line, or separated by commas." autosize minRows={2} styles={MONO}
              value={keysText} onChange={(e) => setKeysText(e.currentTarget.value)} placeholder={'eu\nus\napac'} />
          )}
          <Text size={FS.meta} fw={500}>Each partition keeps</Text>
          <SimpleGrid cols={2} spacing="sm">
            <NumberInput aria-label="Keep for, days" min={0} value={days} onChange={setDays} placeholder="No limit" rightSection={<Text size={FS.micro} c="dimmed">days</Text>} rightSectionWidth={44} />
            <NumberInput aria-label="Keep at most, GB" min={0} decimalScale={2} value={gb} onChange={setGB} placeholder="No limit" rightSection={<Text size={FS.micro} c="dimmed">GB</Text>} rightSectionWidth={36} />
          </SimpleGrid>
          {n && problem && <Text size={FS.micro} c="rose">{problem}</Text>}
        </Stack>
        <Box>
          <Text size={FS.meta} fw={500} mb={6}>Runs on {database.hostname}</Text>
          <pre className="argus-broker-pre" style={{ margin: 0 }} aria-label="What runs">{plan.join('\n')}</pre>
        </Box>
      </SimpleGrid>
      <Group mt="lg" justify="space-between">
        <Text size={FS.micro} c="dimmed">Recorded in the session.</Text>
        <Group gap="xs">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={problem !== null} loading={busy} onClick={() => void create()}>Create {n || 'super stream'}</Button>
        </Group>
      </Group>
    </Modal>
  )
}
