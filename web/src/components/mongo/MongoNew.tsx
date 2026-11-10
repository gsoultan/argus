import {
  Button, Checkbox, Code, Group, Modal, NumberInput, SegmentedControl, Select, SimpleGrid, Stack, Text, TextInput, Textarea,
} from '@mantine/core'
import { useEffect, useState } from 'react'
import { FS } from '~/theme'

/*
New collection and New index, as forms that write the one shell line they
run. The line goes through the command line like a typed one, so it is shown,
recorded and refused by MongoDB's own roles in the same way.
*/

const q = (s: string) => JSON.stringify(s)
const coll = (name: string) => /^[A-Za-z_][A-Za-z0-9_]*$/.test(name) ? `db.${name}` : `db.getCollection(${q(name)})`

/** Builds db.createCollection(...) for the fields; '' while they are not complete. */
export function collectionLine(f: {
  name: string; kind: 'plain' | 'timeseries' | 'capped'; timeField: string; metaField: string
  granularity: string; keepDays: number | string; cappedMB: number | string; cappedDocs: number | string; validator: string
}): { line: string; error: string } {
  const n = f.name.trim()
  if (!n) return { line: '', error: '' }
  if (n.startsWith('system.') || n.includes('$') || n.length > 120) return { line: '', error: 'Not system., no $, at most 120 characters.' }
  const opts: string[] = []
  if (f.kind === 'timeseries') {
    if (!f.timeField.trim()) return { line: '', error: 'A time series needs its time field.' }
    const ts = [`timeField: ${q(f.timeField.trim())}`]
    if (f.metaField.trim()) ts.push(`metaField: ${q(f.metaField.trim())}`)
    if (f.granularity) ts.push(`granularity: ${q(f.granularity)}`)
    opts.push(`timeseries: { ${ts.join(', ')} }`)
    if (Number(f.keepDays) > 0) opts.push(`expireAfterSeconds: ${Math.round(Number(f.keepDays) * 86400)}`)
  }
  if (f.kind === 'capped') {
    if (!(Number(f.cappedMB) > 0)) return { line: '', error: 'A capped collection needs its size.' }
    opts.push('capped: true', `size: ${Math.round(Number(f.cappedMB) * 1024 * 1024)}`)
    if (Number(f.cappedDocs) > 0) opts.push(`max: ${Math.round(Number(f.cappedDocs))}`)
  }
  if (f.validator.trim()) {
    try {
      const v = JSON.parse(f.validator)
      if (typeof v !== 'object' || v === null || Array.isArray(v)) throw new Error('not an object')
      opts.push(`validator: ${JSON.stringify(v)}`)
    } catch {
      return { line: '', error: 'The validator is a JSON object, such as {"$jsonSchema": {...}}.' }
    }
  }
  return { line: `db.createCollection(${q(n)}${opts.length ? `, { ${opts.join(', ')} }` : ''})`, error: '' }
}

export function NewCollection({ opened, onClose, onRun }: {
  opened: boolean; onClose: () => void; onRun: (line: string) => Promise<boolean>
}) {
  const [name, setName] = useState('')
  const [kind, setKind] = useState<'plain' | 'timeseries' | 'capped'>('plain')
  const [timeField, setTimeField] = useState('ts')
  const [metaField, setMetaField] = useState('')
  const [granularity, setGranularity] = useState<string | null>(null)
  const [keepDays, setKeepDays] = useState<number | string>('')
  const [cappedMB, setCappedMB] = useState<number | string>(100)
  const [cappedDocs, setCappedDocs] = useState<number | string>('')
  const [validator, setValidator] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => { if (opened) { setName(''); setValidator('') } }, [opened])
  const { line, error } = collectionLine({ name, kind, timeField, metaField, granularity: granularity ?? '', keepDays, cappedMB, cappedDocs, validator })
  const go = async () => { setBusy(true); try { if (await onRun(line)) onClose() } finally { setBusy(false) } }
  return (
    <Modal opened={opened} onClose={onClose} title="New collection" centered size="lg">
      <Stack gap="sm">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} data-autofocus
          styles={{ input: { fontFamily: 'var(--font-mono)' } }} error={error || undefined} />
        <SegmentedControl value={kind} onChange={(v) => setKind(v as typeof kind)} fullWidth
          data={[{ label: 'Plain', value: 'plain' }, { label: 'Time series', value: 'timeseries' }, { label: 'Capped', value: 'capped' }]} />
        {kind === 'timeseries' && (
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <TextInput label="Time field" value={timeField} onChange={(e) => setTimeField(e.currentTarget.value)} />
            <TextInput label="Meta field" value={metaField} onChange={(e) => setMetaField(e.currentTarget.value)} placeholder="Optional, e.g. sensor" />
            <Select label="Granularity" value={granularity} onChange={setGranularity} clearable placeholder="seconds (default)"
              data={['seconds', 'minutes', 'hours']} />
            <NumberInput label="Keep for (days)" value={keepDays} onChange={setKeepDays} min={1} placeholder="For ever" />
          </SimpleGrid>
        )}
        {kind === 'capped' && (
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <NumberInput label="Size (MB)" value={cappedMB} onChange={setCappedMB} min={1} description="The oldest documents go once it is full." />
            <NumberInput label="Most documents" value={cappedDocs} onChange={setCappedDocs} min={1} placeholder="No limit" />
          </SimpleGrid>
        )}
        <Textarea label="Validator" value={validator} onChange={(e) => setValidator(e.currentTarget.value)} autosize minRows={2} maxRows={10}
          placeholder='Optional: {"$jsonSchema": {"required": ["email"]}}' styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12 } }} />
        {line && <Code block style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{line}</Code>}
        <Text size={FS.micro} c="dimmed">Runs on the command line below as your MongoDB user, and is recorded like any command.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!line} loading={busy} onClick={() => void go()}>Create</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

interface IndexKey { field: string; dir: '1' | '-1' | 'text' | '2dsphere' | 'hashed' }

/** Builds db.c.createIndex(...) for the fields; '' while they are not complete. */
export function indexLine(collection: string, keys: IndexKey[], o: { unique: boolean; sparse: boolean; ttlS: number | string; name: string }): string {
  const ks = keys.filter((k) => k.field.trim())
  if (!collection || ks.length === 0) return ''
  const key = ks.map((k) => `${q(k.field.trim())}: ${/^-?1$/.test(k.dir) ? k.dir : q(k.dir)}`).join(', ')
  const opts: string[] = []
  if (o.name.trim()) opts.push(`name: ${q(o.name.trim())}`)
  if (o.unique) opts.push('unique: true')
  if (o.sparse) opts.push('sparse: true')
  if (Number(o.ttlS) > 0) opts.push(`expireAfterSeconds: ${Math.round(Number(o.ttlS))}`)
  return `${coll(collection)}.createIndex({ ${key} }${opts.length ? `, { ${opts.join(', ')} }` : ''})`
}

export function NewIndex({ opened, collections, initial, onClose, onRun }: {
  opened: boolean; collections: string[]; initial?: string; onClose: () => void; onRun: (line: string) => Promise<boolean>
}) {
  const [collection, setCollection] = useState<string | null>(initial ?? null)
  const [keys, setKeys] = useState<IndexKey[]>([{ field: '', dir: '1' }])
  const [unique, setUnique] = useState(false)
  const [sparse, setSparse] = useState(false)
  const [ttlS, setTtlS] = useState<number | string>('')
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => { if (opened) { setCollection(initial ?? null); setKeys([{ field: '', dir: '1' }]); setName('') } }, [opened, initial])
  const line = indexLine(collection ?? '', keys, { unique, sparse, ttlS: keys.length === 1 ? ttlS : '', name })
  const go = async () => { setBusy(true); try { if (await onRun(line)) onClose() } finally { setBusy(false) } }
  return (
    <Modal opened={opened} onClose={onClose} title="New index" centered size="lg">
      <Stack gap="sm">
        <Select label="Collection" data={collections} value={collection} onChange={setCollection} searchable data-autofocus />
        <Stack gap={6}>
          <Text size={FS.meta} fw={500}>Keys</Text>
          {keys.map((k, i) => (
            <Group key={i} gap="xs" wrap="nowrap">
              <TextInput placeholder="field" value={k.field} style={{ flex: 1 }} aria-label={`Key ${i + 1}`}
                styles={{ input: { fontFamily: 'var(--font-mono)' } }}
                onChange={(e) => { const v = e.currentTarget.value; setKeys((ks) => ks.map((x, j) => j === i ? { ...x, field: v } : x)) }} />
              <Select w={150} value={k.dir} allowDeselect={false} aria-label={`Order of key ${i + 1}`}
                onChange={(v) => setKeys((ks) => ks.map((x, j) => j === i ? { ...x, dir: (v ?? '1') as IndexKey['dir'] } : x))}
                data={[{ value: '1', label: 'Ascending' }, { value: '-1', label: 'Descending' }, { value: 'text', label: 'Text' },
                  { value: '2dsphere', label: '2dsphere' }, { value: 'hashed', label: 'Hashed' }]} />
              {keys.length > 1 && <Button size="compact-xs" variant="subtle" onClick={() => setKeys((ks) => ks.filter((_, j) => j !== i))}>Remove</Button>}
            </Group>
          ))}
          {keys.length < 8 && <Button size="compact-xs" variant="subtle" w="fit-content" onClick={() => setKeys((ks) => [...ks, { field: '', dir: '1' }])}>Add a key</Button>}
        </Stack>
        <Group gap="lg">
          <Checkbox label="Unique" checked={unique} onChange={(e) => setUnique(e.currentTarget.checked)} />
          <Checkbox label="Sparse" checked={sparse} onChange={(e) => setSparse(e.currentTarget.checked)} />
        </Group>
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <NumberInput label="Expire documents after (s)" value={ttlS} onChange={setTtlS} min={1} placeholder="Never"
            disabled={keys.length > 1} description="A TTL index: one date field only." />
          <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} placeholder="From the keys" />
        </SimpleGrid>
        {line && <Code block style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{line}</Code>}
        <Text size={FS.micro} c="dimmed">Building an index on a large collection takes a while and uses the server’s memory; MongoDB builds it without blocking reads and writes.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!line} loading={busy} onClick={() => void go()}>Create</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
