import { Button, Code, Group, Modal, NumberInput, SegmentedControl, Stack, Text, TextInput, Textarea } from '@mantine/core'
import { useEffect, useState } from 'react'
import { FS } from '~/theme'

/*
New key, as a form that writes the Redis commands it runs: one per line,
through the command line like typed ones, so each is shown and recorded and
the person's ACL user decides. A key that is there is never written over.
*/

export type KeyType = 'string' | 'hash' | 'list' | 'set' | 'zset' | 'stream'

/** One argument, quoted the way SplitRedisCommand (tunnel) reads it back. */
export function redisArg(s: string): string {
  if (s !== '' && /^[A-Za-z0-9:_.\-*@/{}]+$/.test(s)) return s
  return `"${s.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\n/g, '\\n').replace(/\r/g, '\\r').replace(/\t/g, '\\t')}"`
}

/** Pairs from "field value" lines; the first space splits them. */
function pairs(text: string): [string, string][] {
  return text.split('\n').map((l) => l.trim()).filter(Boolean).map((l) => {
    const i = l.search(/\s/)
    return i < 0 ? [l, ''] : [l.slice(0, i), l.slice(i).trim()]
  })
}

/** The commands for the fields; an error when they are not complete. */
export function newKeyCommands(f: {
  key: string; type: KeyType; value: string; ttlS: number | string; group: string
}): { lines: string[]; error: string } {
  const k = f.key
  if (!k) return { lines: [], error: '' }
  const K = redisArg(k)
  const items = f.value.split('\n').map((l) => l.trim()).filter(Boolean)
  const ttl = Number(f.ttlS) > 0 ? Math.round(Number(f.ttlS)) : 0
  const lines: string[] = []
  switch (f.type) {
    case 'string':
      lines.push(`SET ${K} ${redisArg(f.value)} NX${ttl ? ` EX ${ttl}` : ''}`)
      return { lines, error: '' }
    case 'hash': {
      const p = pairs(f.value)
      if (!p.length) return { lines: [], error: 'A hash needs a field: one "field value" a line.' }
      lines.push(`HSET ${K} ${p.map(([a, b]) => `${redisArg(a)} ${redisArg(b)}`).join(' ')}`)
      break
    }
    case 'list':
      if (!items.length) return { lines: [], error: 'A list needs an item: one a line.' }
      lines.push(`RPUSH ${K} ${items.map(redisArg).join(' ')}`)
      break
    case 'set':
      if (!items.length) return { lines: [], error: 'A set needs a member: one a line.' }
      lines.push(`SADD ${K} ${items.map(redisArg).join(' ')}`)
      break
    case 'zset': {
      const p = pairs(f.value)
      if (!p.length) return { lines: [], error: 'A sorted set needs a member: one "score member" a line.' }
      if (p.some(([s]) => !Number.isFinite(Number(s)))) return { lines: [], error: 'Each line starts with a score, a number.' }
      lines.push(`ZADD ${K} ${p.map(([s, m]) => `${s} ${redisArg(m)}`).join(' ')}`)
      break
    }
    case 'stream': {
      const p = pairs(f.value)
      const g = f.group.trim()
      if (!p.length && !g) return { lines: [], error: 'A stream needs a first entry or a consumer group.' }
      if (p.length) lines.push(`XADD ${K} * ${p.map(([a, b]) => `${redisArg(a)} ${redisArg(b)}`).join(' ')}`)
      if (g) lines.push(`XGROUP CREATE ${K} ${redisArg(g)} ${p.length ? '0' : '$'} MKSTREAM`)
      break
    }
  }
  if (ttl) lines.push(`EXPIRE ${K} ${ttl}`)
  return { lines, error: '' }
}

const HINT: Record<KeyType, string> = {
  string: 'The value',
  hash: 'One "field value" a line',
  list: 'One item a line, in order',
  set: 'One member a line',
  zset: 'One "score member" a line',
  stream: 'The first entry: one "field value" a line',
}

export function NewKey({ opened, onClose, exists, onRun }: {
  opened: boolean; onClose: () => void
  /** Whether the key is there now, asked of Redis just before writing. */
  exists: (key: string) => Promise<boolean>
  /** Runs one line; false if it failed, which stops the rest. */
  onRun: (line: string) => Promise<boolean>
}) {
  const [key, setKey] = useState('')
  const [type, setType] = useState<KeyType>('string')
  const [value, setValue] = useState('')
  const [ttlS, setTtlS] = useState<number | string>('')
  const [group, setGroup] = useState('')
  const [busy, setBusy] = useState(false)
  const [taken, setTaken] = useState(false)
  useEffect(() => { if (opened) { setKey(''); setValue(''); setGroup(''); setTaken(false) } }, [opened])
  const { lines, error } = newKeyCommands({ key, type, value, ttlS, group })
  const go = async () => {
    setBusy(true)
    try {
      if (await exists(key)) { setTaken(true); return }
      for (const l of lines) if (!(await onRun(l))) return
      onClose()
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title="New key" centered size="lg">
      <Stack gap="sm">
        <TextInput label="Key" value={key} onChange={(e) => { setKey(e.currentTarget.value); setTaken(false) }} data-autofocus
          styles={{ input: { fontFamily: 'var(--font-mono)' } }} placeholder="user:42"
          error={taken ? 'This key is there already; nothing was written. Open it from the list, or pick another name.' : undefined} />
        <SegmentedControl value={type} onChange={(v) => setType(v as KeyType)} fullWidth
          data={[{ label: 'String', value: 'string' }, { label: 'Hash', value: 'hash' }, { label: 'List', value: 'list' },
            { label: 'Set', value: 'set' }, { label: 'Sorted set', value: 'zset' }, { label: 'Stream', value: 'stream' }]} />
        <Textarea label={HINT[type]} value={value} onChange={(e) => setValue(e.currentTarget.value)} autosize minRows={type === 'string' ? 2 : 4} maxRows={12}
          styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12 } }} error={error || undefined}
          placeholder={type === 'hash' ? 'name Ana\nplan pro' : type === 'zset' ? '10 ana\n25 ben' : type === 'stream' ? 'event signup\nuser 42' : undefined} />
        {type === 'stream' && (
          <TextInput label="Consumer group" value={group} onChange={(e) => setGroup(e.currentTarget.value)} placeholder="Optional, e.g. workers"
            description="Made with XGROUP CREATE … MKSTREAM, so the stream exists even with no entry yet." />
        )}
        <NumberInput label="Expire after (s)" value={ttlS} onChange={setTtlS} min={1} placeholder="Never" w={{ base: '100%', sm: 220 }} />
        {lines.length > 0 && <Code block style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{lines.join('\n')}</Code>}
        <Text size={FS.micro} c="dimmed">Runs on the command line below as your Redis user, each command recorded like a typed one.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!lines.length} loading={busy} onClick={() => void go()}>Create</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
