import { useEffect, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Code, Group, Loader, Modal, NumberInput, Popover, Select, Stack, Table, Text, Textarea,
  TextInput, Tooltip,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconArrowBackUp, IconClockHour4, IconPencil, IconPlus, IconTrash, IconX } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { formatRedis, plain, type RedisKey, type RedisValue, type StoreAnswer, type StoreRequest } from '~/components/useStoreSession'
import { editText, ttlText, unitSeconds, type RedisEditResult } from '~/lib/redisManage'
import type { ChangeAnswer } from '~/components/redis/useRedisChange'

/*
Changing one key without typing commands: each change becomes a command,
waits in the bar at the bottom until Apply, and then the agent keeps a copy of
the key as it was (an undo copy, a day) and runs them in order, each recorded.
Only the commands the agent's editor whitelist takes are made here; anything
else is the console's.
*/

const typeBadge = { variant: 'outline', color: 'slate', radius: 'sm' } as const
const mono = { fontFamily: 'var(--font-mono)', fontSize: 12.5, whiteSpace: 'pre-wrap' as const, wordBreak: 'break-all' as const }

export function RedisKeyEditor({ selected, value, change, onChanged, onReload }: {
  selected: RedisKey
  value: StoreAnswer | null
  change: (req: StoreRequest) => Promise<ChangeAnswer>
  /** After a change ran: the new name when renamed, null when the key is gone. */
  onChanged: (name: string | null) => void
  onReload: () => void
}) {
  const key = selected.name
  const [pending, setPending] = useState<string[][]>([])
  const [applying, setApplying] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  useEffect(() => { setPending([]); setErr(null) }, [key])

  const stage = (...cmds: string[][]) => setPending((p) => [...p, ...cmds])

  const undo = async (id: string) => {
    const a = await change({ op: 'undo', undoId: id })
    notifications.show(a.type === 'result'
      ? { color: 'teal', title: 'Put back', message: `${key} is as it was before the change.` }
      : { color: 'rose', title: 'Could not undo', message: a.error ?? 'failed' })
    onChanged(key)
  }

  const apply = async (cmds = pending) => {
    if (cmds.length === 0) return
    setApplying(true)
    setErr(null)
    const a = await change({ op: 'edit', key, edits: cmds, undo: true })
    setApplying(false)
    const res = (a.data ?? {}) as RedisEditResult
    if (a.type !== 'result') {
      setErr(`${a.error ?? 'failed'}${res.ran ? ` (${res.ran} of ${cmds.length} ran)` : ''}`)
      if (res.ran) { setPending(cmds.slice(res.ran)); onReload() }
      return
    }
    setPending([])
    const last = cmds[cmds.length - 1]!
    const verb = last[0]!.toUpperCase()
    const gone = verb === 'DEL' || verb === 'UNLINK'
    const renamed = verb === 'RENAME' || verb === 'RENAMENX' ? last[2]! : undefined
    const nid = `redis-edit-${Date.now()}`
    notifications.show({
      id: nid,
      color: 'teal',
      title: `${res.ran} change${res.ran === 1 ? '' : 's'} applied to ${key}`,
      message: res.undoId ? (
        <Group gap={6} justify="space-between">
          <Text size={FS.meta}>A copy of the key as it was is kept for a day.</Text>
          <Button size="compact-xs" variant="default" leftSection={<IconArrowBackUp size={12} />}
            onClick={() => { notifications.hide(nid); void undo(res.undoId!) }}>Undo</Button>
        </Group>
      ) : (res.noUndo ? `No undo copy: ${res.noUndo}` : undefined),
      autoClose: 15000,
    })
    onChanged(gone ? null : renamed ?? key)
  }

  if (!value) return <Loader size="sm" />
  const v = value.value
  const items = v?.a ?? []
  const kt = value.keyType ?? selected.type
  const editable = value.type === 'result' && !value.error && !selected.q

  const head = (
    <Group gap="xs" mb="sm" wrap="wrap">
      <Text fw={650} ff="monospace" size={FS.body} style={{ wordBreak: 'break-all' }}>{key}</Text>
      {kt && <Badge size="sm" {...typeBadge}>{kt}</Badge>}
      <Text size={FS.micro} c="dimmed">
        {value.ttl !== undefined && value.ttl >= 0 ? `expires in ${ttlText(value.ttl)}` : 'no expiry'}
        {value.total !== undefined && kt !== 'string' ? ` · ${value.total.toLocaleString()} items` : ''}
        {value.truncated ? ' · showing the first ones' : ''}
      </Text>
      <Group gap={4} ml="auto">
        {editable && <TtlButton ttl={value.ttl ?? -1} onSet={(s) => stage(s === null ? ['PERSIST', key] : ['EXPIRE', key, String(s)])} />}
        {editable && <RenameButton name={key} onRename={(n) => stage(['RENAME', key, n])} />}
        {editable && (
          <Tooltip label="Delete the key (kept for a day, undoable)">
            <ActionIcon variant="subtle" color="rose" aria-label="Delete key" onClick={() => stage(['UNLINK', key])}><IconTrash size={14} /></ActionIcon>
          </Tooltip>
        )}
        <Button size="compact-xs" variant="subtle" onClick={onReload}>Reload</Button>
      </Group>
    </Group>
  )

  let body: React.ReactNode
  if (value.type !== 'result') body = <Text size={FS.meta} c="rose">{value.error}</Text>
  else if (value.error) body = <Text size={FS.meta} c="dimmed">{value.error}</Text>
  else if (kt === 'string') body = <StringEdit v={v} editable={editable} onSave={(t) => stage(['SET', key, t, 'KEEPTTL'])} />
  else if (kt === 'hash' || kt === 'zset') {
    const pairs: [RedisValue, RedisValue][] = []
    for (let i = 0; i + 1 < items.length; i += 2) pairs.push([items[i]!, items[i + 1]!])
    const hash = kt === 'hash'
    body = (
      <PairTable pairs={pairs} editable={editable} a={hash ? 'Field' : 'Member'} b={hash ? 'Value' : 'Score'}
        onSet={(a, b) => stage(hash ? ['HSET', key, a, b] : ['ZADD', key, b, a])}
        onRemove={(a) => stage(hash ? ['HDEL', key, a] : ['ZREM', key, a])} numeric={!hash} />
    )
  } else if (kt === 'list' || kt === 'set') {
    const list = kt === 'list'
    body = (
      <ItemTable items={items} editable={editable} indexed={list}
        onSet={list ? (i, t) => stage(['LSET', key, String(i), t]) : undefined}
        onRemove={(t) => stage(list ? ['LREM', key, '1', t] : ['SREM', key, t])}
        onAdd={(t) => stage(list ? ['RPUSH', key, t] : ['SADD', key, t])} />
    )
  } else {
    body = (
      <Stack gap={SP.cozy}>
        <Box style={mono}>{formatRedis(v)}</Box>
        {editable && kt === 'stream' && <AddLine label="Delete entry by id" placeholder="1700000000000-0" onAdd={(id) => stage(['XDEL', key, id])} />}
      </Stack>
    )
  }

  return (
    <Box>
      {head}
      {selected.q && <Text size={FS.micro} c="dimmed" mb="xs">This key's name is not UTF-8, so it is changed from the console only.</Text>}
      {body}
      {(pending.length > 0 || err) && (
        <Box mt="md" p="sm" style={{ position: 'sticky', bottom: 0, border: '1px solid var(--color-line)', borderRadius: 8, background: 'var(--mantine-color-body)' }}>
          {err && <Text size={FS.meta} c="rose" mb={4}>{err}</Text>}
          {pending.length > 0 && (
            <>
              <Text size={FS.micro} c="dimmed" mb={4}>{pending.length} change{pending.length === 1 ? '' : 's'} waiting. Apply keeps a copy of the key first, for a day.</Text>
              <Code block style={{ maxHeight: 120, overflow: 'auto' }}>{editText(pending)}</Code>
              <Group gap={6} mt={6} justify="flex-end">
                <Button size="xs" variant="default" onClick={() => { setPending([]); setErr(null) }} disabled={applying}>Discard</Button>
                <Button size="xs" onClick={() => void apply()} loading={applying}>Apply {pending.length}</Button>
              </Group>
            </>
          )}
        </Box>
      )}
    </Box>
  )
}

function StringEdit({ v, editable, onSave }: { v: RedisValue | undefined; editable: boolean; onSave: (t: string) => void }) {
  const text = plain(v)
  const [draft, setDraft] = useState(text)
  useEffect(() => setDraft(text), [text])
  const canEdit = editable && !v?.cut && !v?.q
  if (!canEdit) {
    return <Box p="sm" style={{ ...mono, border: '1px solid var(--color-line)', borderRadius: 8 }}>{text}{v?.cut ? ' …(cut)' : ''}</Box>
  }
  return (
    <Stack gap={6}>
      <Textarea value={draft} onChange={(e) => setDraft(e.currentTarget.value)} autosize minRows={3} maxRows={16}
        styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12.5 } }} aria-label="String value" />
      <Group justify="flex-end">
        <Button size="xs" variant="default" disabled={draft === text} onClick={() => onSave(draft)}>Stage change</Button>
      </Group>
    </Stack>
  )
}

function PairTable({ pairs, editable, a, b, onSet, onRemove, numeric }: {
  pairs: [RedisValue, RedisValue][]; editable: boolean; a: string; b: string; numeric: boolean
  onSet: (a: string, b: string) => void; onRemove: (a: string) => void
}) {
  const [editing, setEditing] = useState<string | null>(null)
  const [draft, setDraft] = useState('')
  const [na, setNa] = useState('')
  const [nb, setNb] = useState('')
  const bad = numeric && nb !== '' && !Number.isFinite(Number(nb))
  return (
    <Table withTableBorder verticalSpacing={4} fz="xs">
      <Table.Thead><Table.Tr><Table.Th>{a}</Table.Th><Table.Th>{b}</Table.Th>{editable && <Table.Th w={64} />}</Table.Tr></Table.Thead>
      <Table.Tbody>
        {pairs.map(([x, y], i) => {
          const name = plain(x)
          const can = editable && !x.q && !y.q
          return (
            <Table.Tr key={i}>
              <Table.Td style={mono}>{name}</Table.Td>
              <Table.Td style={mono}>
                {editing === name ? (
                  <TextInput size="xs" value={draft} autoFocus onChange={(e) => setDraft(e.currentTarget.value)} aria-label={`New ${b.toLowerCase()}`}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') { onSet(name, draft); setEditing(null) }
                      if (e.key === 'Escape') setEditing(null)
                    }} />
                ) : plain(y)}
              </Table.Td>
              {editable && (
                <Table.Td>
                  {can && (
                    <Group gap={2} wrap="nowrap">
                      <ActionIcon size="sm" variant="subtle" aria-label={`Edit ${name}`} onClick={() => { setEditing(name); setDraft(plain(y)) }}><IconPencil size={12} /></ActionIcon>
                      <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Remove ${name}`} onClick={() => onRemove(name)}><IconX size={12} /></ActionIcon>
                    </Group>
                  )}
                </Table.Td>
              )}
            </Table.Tr>
          )
        })}
        {editable && (
          <Table.Tr>
            <Table.Td><TextInput size="xs" placeholder={`New ${a.toLowerCase()}`} value={na} onChange={(e) => setNa(e.currentTarget.value)} aria-label={`New ${a.toLowerCase()}`} /></Table.Td>
            <Table.Td><TextInput size="xs" placeholder={b} value={nb} error={bad} onChange={(e) => setNb(e.currentTarget.value)} aria-label={`${b} for the new ${a.toLowerCase()}`} /></Table.Td>
            <Table.Td>
              <ActionIcon size="sm" variant="default" aria-label={`Add ${a.toLowerCase()}`} disabled={!na || nb === '' || bad}
                onClick={() => { onSet(na, nb); setNa(''); setNb('') }}><IconPlus size={12} /></ActionIcon>
            </Table.Td>
          </Table.Tr>
        )}
      </Table.Tbody>
    </Table>
  )
}

function ItemTable({ items, editable, indexed, onSet, onRemove, onAdd }: {
  items: RedisValue[]; editable: boolean; indexed: boolean
  onSet?: (i: number, t: string) => void; onRemove: (t: string) => void; onAdd: (t: string) => void
}) {
  const [editing, setEditing] = useState<number | null>(null)
  const [draft, setDraft] = useState('')
  return (
    <Stack gap={SP.cozy}>
      <Table withTableBorder verticalSpacing={4} fz="xs">
        <Table.Tbody>
          {items.map((e, i) => (
            <Table.Tr key={i}>
              {indexed && <Table.Td w={60} c="dimmed">{i}</Table.Td>}
              <Table.Td style={mono}>
                {editing === i ? (
                  <TextInput size="xs" value={draft} autoFocus onChange={(ev) => setDraft(ev.currentTarget.value)} aria-label="New item"
                    onKeyDown={(ev) => {
                      if (ev.key === 'Enter') { onSet?.(i, draft); setEditing(null) }
                      if (ev.key === 'Escape') setEditing(null)
                    }} />
                ) : plain(e)}
              </Table.Td>
              {editable && (
                <Table.Td w={64}>
                  {!e.q && (
                    <Group gap={2} wrap="nowrap">
                      {onSet && <ActionIcon size="sm" variant="subtle" aria-label={`Edit item ${i}`} onClick={() => { setEditing(i); setDraft(plain(e)) }}><IconPencil size={12} /></ActionIcon>}
                      <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Remove item ${i}`} onClick={() => onRemove(plain(e))}><IconX size={12} /></ActionIcon>
                    </Group>
                  )}
                </Table.Td>
              )}
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      {editable && <AddLine label={indexed ? 'Push to the end' : 'Add member'} placeholder="value" onAdd={onAdd} />}
    </Stack>
  )
}

function AddLine({ label, placeholder, onAdd }: { label: string; placeholder: string; onAdd: (t: string) => void }) {
  const [t, setT] = useState('')
  return (
    <Group gap={6} wrap="nowrap">
      <TextInput size="xs" style={{ flex: 1 }} placeholder={placeholder} value={t} onChange={(e) => setT(e.currentTarget.value)} aria-label={label}
        onKeyDown={(e) => { if (e.key === 'Enter' && t) { onAdd(t); setT('') } }} />
      <Button size="xs" variant="default" disabled={!t} onClick={() => { onAdd(t); setT('') }}>{label}</Button>
    </Group>
  )
}

function TtlButton({ ttl, onSet }: { ttl: number; onSet: (s: number | null) => void }) {
  const [open, setOpen] = useState(false)
  const [n, setN] = useState<number | string>(1)
  const [unit, setUnit] = useState<'seconds' | 'minutes' | 'hours' | 'days'>('hours')
  return (
    <Popover opened={open} onChange={setOpen} position="bottom-end" withArrow>
      <Popover.Target>
        <Tooltip label="Expiry">
          <ActionIcon variant="subtle" aria-label="Set expiry" onClick={() => setOpen((o) => !o)}><IconClockHour4 size={14} /></ActionIcon>
        </Tooltip>
      </Popover.Target>
      <Popover.Dropdown>
        <Stack gap={6} w={220}>
          <Text size={FS.micro} c="dimmed">{ttl >= 0 ? `Expires in ${ttlText(ttl)} now.` : 'Has no expiry now.'}</Text>
          <Group gap={6} wrap="nowrap">
            <NumberInput size="xs" min={1} value={n} onChange={setN} w={80} aria-label="Expire after" />
            <Select size="xs" value={unit} onChange={(u) => u && setUnit(u as typeof unit)} data={['seconds', 'minutes', 'hours', 'days']} aria-label="Unit" />
          </Group>
          <Group gap={6} justify="space-between">
            {ttl >= 0 ? <Button size="compact-xs" variant="subtle" onClick={() => { onSet(null); setOpen(false) }}>Never expire</Button> : <span />}
            <Button size="compact-xs" disabled={!(Number(n) > 0)} onClick={() => { onSet(unitSeconds(Number(n), unit)); setOpen(false) }}>Stage</Button>
          </Group>
        </Stack>
      </Popover.Dropdown>
    </Popover>
  )
}

function RenameButton({ name, onRename }: { name: string; onRename: (n: string) => void }) {
  const [open, setOpen] = useState(false)
  const [n, setN] = useState(name)
  return (
    <>
      <Tooltip label="Rename">
        <ActionIcon variant="subtle" aria-label="Rename key" onClick={() => { setN(name); setOpen(true) }}><IconPencil size={14} /></ActionIcon>
      </Tooltip>
      <Modal opened={open} onClose={() => setOpen(false)} title="Rename key" size="sm">
        <Stack gap="sm">
          <TextInput label="New name" value={n} onChange={(e) => setN(e.currentTarget.value)} styles={{ input: { fontFamily: 'var(--font-mono)' } }} />
          <Text size={FS.micro} c="dimmed">RENAME replaces a key that already has the new name; its old value is not in the undo copy.</Text>
          <Group justify="flex-end">
            <Button variant="default" size="xs" onClick={() => setOpen(false)}>Cancel</Button>
            <Button size="xs" disabled={!n || n === name} onClick={() => { onRename(n); setOpen(false) }}>Stage</Button>
          </Group>
        </Stack>
      </Modal>
    </>
  )
}
