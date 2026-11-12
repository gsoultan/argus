import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Badge, Button, Checkbox, Divider, Drawer, Group, NumberInput, SegmentedControl, Select, Stack, Table, Text, TextInput,
} from '@mantine/core'
import { IconAlertTriangle, IconArrowBackUp } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import type { StoreRequest, StoreSession } from '~/components/useStoreSession'
import {
  bytesText, ttlText, unitSeconds, type RedisBulkResult, type RedisCountData, type RedisUndo,
} from '~/lib/redisManage'
import { dataOf, type ChangeAnswer } from '~/components/redis/useRedisChange'

/*
Changing many keys at once: say which keys (a pattern, and optionally a type,
how long nobody touched them, only keys with no TTL), count them, then pick
what to do. The count is what is approved and what is enforced: the agent
stops before it changes more keys than were counted. Over 100 keys it waits
for someone else's approval; the work goes in steps of 500 with a pause, so
Redis keeps answering others meanwhile.
*/

const APPROVAL_OVER = 100

type Action = 'delete' | 'expire' | 'persist'

export function RedisBulk({ opened, onClose, s, change, initialPattern, onDone }: {
  opened: boolean
  onClose: () => void
  s: StoreSession
  change: (req: StoreRequest) => Promise<ChangeAnswer>
  initialPattern: string
  onDone: () => void
}) {
  const [pattern, setPattern] = useState(initialPattern)
  const [type, setType] = useState<string | null>(null)
  const [idle, setIdle] = useState<number | string>('')
  const [noExpiry, setNoExpiry] = useState(false)
  const [counted, setCounted] = useState<RedisCountData | null>(null)
  const [counting, setCounting] = useState(false)
  const [action, setAction] = useState<Action>('delete')
  const [n, setN] = useState<number | string>(1)
  const [unit, setUnit] = useState<'seconds' | 'minutes' | 'hours' | 'days'>('days')
  const [keep, setKeep] = useState(true)
  const [typed, setTyped] = useState('')
  const [running, setRunning] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)

  useEffect(() => { if (opened) { setPattern(initialPattern); setCounted(null); setMsg(null); setTyped('') } }, [opened, initialPattern])

  const filter = { pattern: pattern || '*', type: type ?? '', idleDays: Number(idle) || 0, noExpiry }
  const count = async () => {
    setCounting(true)
    setMsg(null)
    setTyped('')
    const a = await s.ask({ op: 'count', filter })
    setCounting(false)
    const d = dataOf<RedisCountData>(a)
    if (!d) { setCounted(null); setMsg({ ok: false, text: a.error ?? 'the count failed' }); return }
    setCounted(d)
  }

  const go = async () => {
    if (!counted) return
    setRunning(true)
    setMsg(null)
    const a = await change({
      op: 'bulk', filter, action, max: counted.count, undo: keep,
      ...(action === 'expire' ? { seconds: unitSeconds(Number(n), unit) } : {}),
    })
    setRunning(false)
    if (a.notApproved) { setMsg({ ok: false, text: 'Nothing changed: it was not approved.' }); return }
    const r = (a.data ?? {}) as RedisBulkResult
    if (a.type === 'result') {
      setMsg({ ok: true, text: `${r.changed.toLocaleString()} of ${r.matched.toLocaleString()} keys changed.${r.undoId ? ' An undo copy is kept for a day, below.' : ''}` })
      setCounted(null)
      setTyped('')
      onDone()
    } else {
      setMsg({ ok: false, text: `${a.error ?? r.stopped ?? 'failed'}${r.changed ? ` (${r.changed.toLocaleString()} changed before it stopped)` : ''}` })
      if (r.changed) onDone()
    }
  }

  const words = action === 'delete' ? 'Delete' : action === 'expire' ? 'Set an expiry on' : 'Remove the expiry from'
  const confirmOK = counted !== null && typed.trim() === String(counted.count)
  const badExpiry = action === 'expire' && !(Number(n) > 0)

  return (
    <Drawer opened={opened} onClose={onClose} position="right" size="lg" title="Change many keys">
      <Stack gap="md">
        <Stack gap={SP.cozy}>
          <Text size={FS.meta} fw={600}>1. Which keys</Text>
          <TextInput label="Pattern" value={pattern} onChange={(e) => { setPattern(e.currentTarget.value); setCounted(null) }}
            styles={{ input: { fontFamily: 'var(--font-mono)' } }} placeholder="session:*" />
          <Group grow>
            <Select label="Type" placeholder="Any" clearable value={type} onChange={(t) => { setType(t); setCounted(null) }} data={['string', 'hash', 'list', 'set', 'zset', 'stream']} />
            <NumberInput label="Untouched for at least" suffix=" days" placeholder="any" min={0} value={idle} onChange={(v) => { setIdle(v); setCounted(null) }} />
          </Group>
          <Checkbox label="Only keys with no expiry" checked={noExpiry} onChange={(e) => { setNoExpiry(e.currentTarget.checked); setCounted(null) }} />
          <Group><Button variant="default" size="xs" onClick={() => void count()} loading={counting}>Count</Button></Group>
        </Stack>

        {counted && (
          <Stack gap={SP.cozy}>
            <Group gap="xs">
              <Text size={FS.figure} fw={650}>{counted.count.toLocaleString()}</Text>
              <Text size={FS.meta} c="dimmed">keys{counted.more ? ' or more' : ''} · about {bytesText(counted.bytes)}</Text>
              {Object.entries(counted.types).map(([t, c]) => <Badge key={t} size="sm" variant="outline" color="slate">{t} {c.toLocaleString()}</Badge>)}
            </Group>
            {counted.more && <Alert color="amber" icon={<IconAlertTriangle size={16} />}>More keys match than one change may touch. Narrow the pattern.</Alert>}
            <Table verticalSpacing={2} fz="xs" withTableBorder>
              <Table.Tbody>
                {counted.sample.slice(0, 8).map((k) => (
                  <Table.Tr key={k.name}><Table.Td ff="monospace">{k.name}</Table.Td><Table.Td w={70}>{k.type}</Table.Td><Table.Td w={70} c="dimmed">{k.ttl >= 0 ? ttlText(k.ttl) : 'no TTL'}</Table.Td></Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Stack>
        )}

        {counted && counted.count > 0 && !counted.more && (
          <Stack gap={SP.cozy}>
            <Divider />
            <Text size={FS.meta} fw={600}>2. What to do</Text>
            <SegmentedControl value={action} onChange={(v) => setAction(v as Action)} data={[
              { value: 'delete', label: 'Delete' }, { value: 'expire', label: 'Set expiry' }, { value: 'persist', label: 'Remove expiry' },
            ]} />
            {action === 'expire' && (
              <Group gap={6} grow>
                <NumberInput label="Expire after" min={1} value={n} onChange={setN} />
                <Select label="Unit" value={unit} onChange={(u) => u && setUnit(u as typeof unit)} data={['seconds', 'minutes', 'hours', 'days']} />
              </Group>
            )}
            <Checkbox checked={keep} onChange={(e) => setKeep(e.currentTarget.checked)}
              label="Keep a copy first so it can be undone for a day"
              description={`About ${bytesText(counted.bytes)} on the agent's disk; copies over 2 GB in all are refused.`} />
            {counted.count > APPROVAL_OVER && s.ready?.changesNeedApproval && (
              <Alert color="azure" icon={<IconAlertTriangle size={16} />}>
                Over {APPROVAL_OVER} keys on this database waits for someone else to approve it. Once approved, it changes at most {counted.count.toLocaleString()} keys.
              </Alert>
            )}
            <TextInput label={`Type ${counted.count} to confirm`} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} />
            <Group justify="flex-end">
              <Button color={action === 'delete' ? 'rose' : undefined} disabled={!confirmOK || badExpiry} loading={running} onClick={() => void go()}>
                {words} {counted.count.toLocaleString()} keys
              </Button>
            </Group>
          </Stack>
        )}
        {msg && <Alert color={msg.ok ? 'teal' : 'rose'}>{msg.text}</Alert>}

        <Divider />
        <UndoCopies s={s} change={change} refreshKey={msg} onDone={onDone} />
      </Stack>
    </Drawer>
  )
}

/** The undo copies this person made on this database, newest first. */
export function UndoCopies({ s, change, refreshKey, onDone }: {
  s: StoreSession
  change: (req: StoreRequest) => Promise<ChangeAnswer>
  refreshKey?: unknown
  onDone: () => void
}) {
  const [list, setList] = useState<RedisUndo[] | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [note, setNote] = useState<string | null>(null)
  const load = useCallback(async () => {
    setList(dataOf<RedisUndo[]>(await s.ask({ op: 'undo_list' })) ?? [])
  }, [s])
  useEffect(() => { void load() }, [load, refreshKey])
  const restore = async (u: RedisUndo) => {
    setBusy(u.id)
    const a = await change({ op: 'undo', undoId: u.id })
    setBusy(null)
    const r = (a.data ?? {}) as { restored?: number; expired?: number }
    setNote(a.type === 'result' ? `${r.restored ?? 0} keys put back${r.expired ? `; ${r.expired} had expired since and were left out` : ''}.` : (a.error ?? 'failed'))
    onDone()
    void load()
  }
  return (
    <Stack gap={SP.cozy}>
      <Text size={FS.meta} fw={600}>Undo copies</Text>
      <Text size={FS.micro} c="dimmed">What your changes replaced or deleted, kept a day. Putting one back overwrites the keys as they are now.</Text>
      {note && <Text size={FS.meta}>{note}</Text>}
      {list === null ? <Text size={FS.meta} c="dimmed">Loading…</Text> : list.length === 0 ? <Text size={FS.meta} c="dimmed">None.</Text> : (
        <Table verticalSpacing={4} fz="xs">
          <Table.Tbody>
            {list.map((u) => (
              <Table.Tr key={u.id}>
                <Table.Td ff="monospace" style={{ wordBreak: 'break-all' }}>{u.what}</Table.Td>
                <Table.Td w={90} c="dimmed">{u.keys.toLocaleString()} key{u.keys === 1 ? '' : 's'}</Table.Td>
                <Table.Td w={110} c="dimmed">{ttlText(Math.max(0, Math.round(Date.now() / 1000 - u.at)))} ago</Table.Td>
                <Table.Td w={90}>
                  <Button size="compact-xs" variant="default" leftSection={<IconArrowBackUp size={12} />} loading={busy === u.id} onClick={() => void restore(u)}>Put back</Button>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
    </Stack>
  )
}
