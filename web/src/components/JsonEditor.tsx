import { useMemo, useState } from 'react'
import {
  ActionIcon, Box, Button, Group, Modal, SegmentedControl, Select, Stack, Text, TextInput, Textarea, Tooltip,
} from '@mantine/core'
import { IconArrowDown, IconArrowUp, IconChevronDown, IconChevronRight, IconPlus, IconTrash } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { formatArray, parseArray } from '~/lib/rowEdit'

/*
Editors for the values a text box can't hold well: a JSON document (json
and jsonb) and a Postgres array. Both edit a copy and hand back text only
on Apply, so Cancel leaves the cell as it was.

JSON has two views of the same value. Tree: open objects and arrays, change
a value in place, change its type, add, remove and move items; a new array
item copies the shape of the one before it. Text: type the JSON; a mistake
is marked with its line and Apply stays off until it parses.
*/

type J = null | boolean | number | string | J[] | { [k: string]: J }
type JType = 'string' | 'number' | 'boolean' | 'null' | 'object' | 'array'

const typeOf = (v: J): JType => (v === null ? 'null' : Array.isArray(v) ? 'array' : typeof v as JType)

function convert(v: J, to: JType): J {
  switch (to) {
    case 'null': return null
    case 'string': return v === null ? '' : typeof v === 'object' ? JSON.stringify(v) : String(v)
    case 'number': { const n = Number(v); return Number.isFinite(n) ? n : 0 }
    case 'boolean': return v === true || v === 'true'
    case 'object': return v && typeof v === 'object' && !Array.isArray(v) ? v : {}
    case 'array': return Array.isArray(v) ? v : v === null ? [] : [v]
  }
}

/** An empty value with the same shape: a new array item copies its sibling. */
function blankLike(v: J | undefined): J {
  if (v === undefined || v === null) return null
  if (Array.isArray(v)) return []
  if (typeof v === 'object') return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, blankLike(x)]))
  if (typeof v === 'number') return 0
  if (typeof v === 'boolean') return false
  return ''
}

/** Where JSON.parse failed, as a line number, when the engine says. */
function parseError(text: string): string | null {
  try { JSON.parse(text); return null } catch (e) {
    const msg = e instanceof Error ? e.message : String(e)
    const m = /position (\d+)/.exec(msg)
    if (m) {
      const line = text.slice(0, Number(m[1])).split('\n').length
      return `Line ${line}: ${msg.replace(/^JSON\.parse: /, '')}`
    }
    return msg
  }
}

function Node({ value, onChange, onRemove, onMove, name, depth }: {
  value: J
  onChange: (v: J) => void
  onRemove?: () => void
  onMove?: (d: -1 | 1) => void
  name: string
  depth: number
}) {
  const [open, setOpen] = useState(depth < 2)
  const t = typeOf(value)
  const container = t === 'object' || t === 'array'
  const head = (
    <Group gap={4} wrap="nowrap" align="center">
      {container
        ? <ActionIcon size="xs" variant="subtle" onClick={() => setOpen(!open)} aria-label={open ? `Close ${name}` : `Open ${name}`}>
            {open ? <IconChevronDown size={12} /> : <IconChevronRight size={12} />}
          </ActionIcon>
        : <Box w={22} />}
      <Text size={FS.meta} ff="var(--font-mono)" fw={600} style={{ whiteSpace: 'nowrap' }}>{name}</Text>
      <Select size="xs" w={96} data={['string', 'number', 'boolean', 'null', 'object', 'array']} value={t} allowDeselect={false}
        aria-label={`Type of ${name}`} onChange={(x) => x && onChange(convert(value, x as JType))} comboboxProps={{ withinPortal: true }} />
      {t === 'string' && <TextInput size="xs" style={{ flex: 1 }} value={value as string} aria-label={`Value of ${name}`}
        onChange={(e) => onChange(e.currentTarget.value)} styles={{ input: { fontFamily: 'var(--font-mono)' } }} />}
      {t === 'number' && <TextInput size="xs" style={{ flex: 1 }} defaultValue={String(value)} aria-label={`Value of ${name}`}
        onChange={(e) => { const n = Number(e.currentTarget.value); if (e.currentTarget.value.trim() !== '' && Number.isFinite(n)) onChange(n) }}
        styles={{ input: { fontFamily: 'var(--font-mono)' } }} />}
      {t === 'boolean' && <SegmentedControl size="xs" data={['true', 'false']} value={String(value)} onChange={(x) => onChange(x === 'true')} />}
      {t === 'null' && <Text size={FS.micro} c="dimmed" fs="italic">null</Text>}
      {container && <Text size={FS.micro} c="dimmed">{t === 'array' ? `${(value as J[]).length} ${(value as J[]).length === 1 ? 'item' : 'items'}` : `${Object.keys(value as object).length} keys`}</Text>}
      <Group gap={0} wrap="nowrap" ml="auto">
        {onMove && <>
          <ActionIcon size="xs" variant="subtle" aria-label={`Move ${name} up`} onClick={() => onMove(-1)}><IconArrowUp size={12} /></ActionIcon>
          <ActionIcon size="xs" variant="subtle" aria-label={`Move ${name} down`} onClick={() => onMove(1)}><IconArrowDown size={12} /></ActionIcon>
        </>}
        {onRemove && <ActionIcon size="xs" variant="subtle" color="rose" aria-label={`Remove ${name}`} onClick={onRemove}><IconTrash size={12} /></ActionIcon>}
      </Group>
    </Group>
  )
  if (!container || !open) return head
  return (
    <Box>
      {head}
      <Box pl={18} style={{ borderLeft: '1px solid var(--color-line)', marginLeft: 10 }}>
        {t === 'array'
          ? (value as J[]).map((x, i, arr) => (
              <Node key={i} name={`[${i}]`} value={x} depth={depth + 1}
                onChange={(v) => onChange(arr.map((y, j) => (j === i ? v : y)))}
                onRemove={() => onChange(arr.filter((_, j) => j !== i))}
                onMove={(d) => { const k = i + d; if (k < 0 || k >= arr.length) return; const n = [...arr]; [n[i], n[k]] = [n[k]!, n[i]!]; onChange(n) }} />
            ))
          : Object.entries(value as Record<string, J>).map(([k, x]) => (
              <Node key={k} name={k} value={x} depth={depth + 1}
                onChange={(v) => onChange({ ...(value as Record<string, J>), [k]: v })}
                onRemove={() => { const n = { ...(value as Record<string, J>) }; delete n[k]; onChange(n) }} />
            ))}
        <AddItem kind={t} onAdd={(key) => {
          if (t === 'array') { const arr = value as J[]; onChange([...arr, blankLike(arr[arr.length - 1])]) }
          else if (key && !(key in (value as object))) onChange({ ...(value as Record<string, J>), [key]: '' })
        }} />
      </Box>
    </Box>
  )
}

function AddItem({ kind, onAdd }: { kind: JType; onAdd: (key?: string) => void }) {
  const [key, setKey] = useState('')
  if (kind === 'array') {
    return <Button size="compact-xs" variant="subtle" leftSection={<IconPlus size={12} />} onClick={() => onAdd()} my={2}>Add an item</Button>
  }
  return (
    <Group gap={4} my={2} wrap="nowrap">
      <TextInput size="xs" placeholder="new key" value={key} onChange={(e) => setKey(e.currentTarget.value)} aria-label="New key"
        onKeyDown={(e) => { if (e.key === 'Enter' && key) { onAdd(key); setKey('') } }} styles={{ input: { fontFamily: 'var(--font-mono)' } }} />
      <Button size="compact-xs" variant="subtle" leftSection={<IconPlus size={12} />} disabled={!key} onClick={() => { onAdd(key); setKey('') }}>Add a key</Button>
    </Group>
  )
}

export function JsonEditor({ opened, column, type, value, onApply, onClose }: {
  opened: boolean
  column: string
  type: string
  value: string | null
  onApply: (v: string | null) => void
  onClose: () => void
}) {
  const start = useMemo(() => {
    if (value === null) return 'null'
    try { return JSON.stringify(JSON.parse(value), null, 2) } catch { return value }
  }, [value])
  const [text, setText] = useState(start)
  const [mode, setMode] = useState<'tree' | 'text'>('tree')
  const err = parseError(text)
  const parsed: J | undefined = err ? undefined : (JSON.parse(text) as J)
  return (
    <Modal opened={opened} onClose={onClose} size="lg" title={
      <Group gap={8}><Text fw={600}>{column}</Text><Text size={FS.micro} c="dimmed">{type}</Text></Group>
    }>
      <Stack gap="sm">
        <Group justify="space-between">
          <SegmentedControl size="xs" value={mode} onChange={(m) => setMode(m as 'tree' | 'text')}
            data={[{ label: 'Tree', value: 'tree', disabled: Boolean(err) }, { label: 'Text', value: 'text' }]} aria-label="View" />
          <Text size={FS.micro} c="dimmed">
            {type === 'jsonb' ? 'jsonb keeps the last of duplicate keys and orders keys its own way.' : 'json is stored exactly as written.'}
          </Text>
        </Group>
        {mode === 'tree' && parsed !== undefined
          ? <Box style={{ maxHeight: 420, overflow: 'auto' }} p={4}>
              <Node name="value" value={parsed} depth={0} onChange={(v) => setText(JSON.stringify(v, null, 2))} />
            </Box>
          : <Textarea value={text} onChange={(e) => setText(e.currentTarget.value)} autosize minRows={8} maxRows={18} aria-label={`JSON for ${column}`}
              error={err} styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12 } }} spellCheck={false} />}
        <Group justify="space-between">
          <Button size="xs" variant="subtle" color="gray" onClick={() => onApply(null)}>Set to SQL NULL</Button>
          <Group gap="xs">
            <Button size="xs" variant="default" onClick={onClose}>Cancel</Button>
            <Tooltip label={err ?? ''} disabled={!err} withinPortal>
              <Button size="xs" disabled={Boolean(err)} onClick={() => onApply(JSON.stringify(JSON.parse(text)))}>Apply</Button>
            </Tooltip>
          </Group>
        </Group>
      </Stack>
    </Modal>
  )
}

/** A Postgres array (text[], int[], …) as a list of items. */
export function ArrayEditor({ opened, column, type, value, onApply, onClose }: {
  opened: boolean
  column: string
  type: string
  value: string | null
  onApply: (v: string | null) => void
  onClose: () => void
}) {
  const parsed = value === null ? [] : parseArray(value)
  const [items, setItems] = useState<(string | null)[]>(parsed ?? [])
  return (
    <Modal opened={opened} onClose={onClose} size="md" title={
      <Group gap={8}><Text fw={600}>{column}</Text><Text size={FS.micro} c="dimmed">{type}</Text></Group>
    }>
      {parsed === null
        ? <Text size={FS.meta} c={INK.pending}>This array has more than one dimension; edit it as text instead.</Text>
        : (
          <Stack gap={6}>
            {items.map((x, i) => (
              <Group key={i} gap={4} wrap="nowrap">
                <Text size={FS.micro} c="dimmed" w={24} ta="right">{i + 1}</Text>
                <TextInput size="xs" style={{ flex: 1 }} value={x ?? ''} placeholder={x === null ? 'NULL' : ''} aria-label={`Item ${i + 1}`}
                  onChange={(e) => { const v = e.currentTarget.value; setItems((a) => a.map((y, j) => (j === i ? v : y))) }}
                  styles={{ input: { fontFamily: 'var(--font-mono)' } }} />
                <ActionIcon size="sm" variant="subtle" aria-label={`Move item ${i + 1} up`} disabled={i === 0}
                  onClick={() => setItems((a) => { const n = [...a]; [n[i - 1], n[i]] = [n[i]!, n[i - 1]!]; return n })}><IconArrowUp size={12} /></ActionIcon>
                <ActionIcon size="sm" variant="subtle" aria-label={`Move item ${i + 1} down`} disabled={i === items.length - 1}
                  onClick={() => setItems((a) => { const n = [...a]; [n[i + 1], n[i]] = [n[i]!, n[i + 1]!]; return n })}><IconArrowDown size={12} /></ActionIcon>
                <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Remove item ${i + 1}`}
                  onClick={() => setItems((a) => a.filter((_, j) => j !== i))}><IconTrash size={12} /></ActionIcon>
              </Group>
            ))}
            <Group gap="xs">
              <Button size="compact-xs" variant="subtle" leftSection={<IconPlus size={12} />} onClick={() => setItems((a) => [...a, ''])}>Add an item</Button>
            </Group>
            <Text size={FS.micro} c="dimmed" ff="var(--font-mono)">{formatArray(items)}</Text>
            <Group justify="space-between" mt="xs">
              <Button size="xs" variant="subtle" color="gray" onClick={() => onApply(null)}>Set to NULL</Button>
              <Group gap="xs">
                <Button size="xs" variant="default" onClick={onClose}>Cancel</Button>
                <Button size="xs" onClick={() => onApply(formatArray(items))}>Apply</Button>
              </Group>
            </Group>
          </Stack>
        )}
    </Modal>
  )
}
