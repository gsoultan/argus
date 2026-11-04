import { useMemo, useState } from 'react'
import { ActionIcon, Box, Button, Group, Modal, ScrollArea, SegmentedControl, Table, Text, TextInput, Tooltip, UnstyledButton } from '@mantine/core'
import { IconArrowsMaximize, IconChevronDown, IconChevronRight, IconSearch } from '@tabler/icons-react'
import { FS } from '~/theme'
import { itemLabel, summary, type J } from '~/lib/jsonSummary'

/*
A JSON value, read rather than edited: a tree that opens a level at a time,
a table when it is a list of objects (each key a column), or the text laid
out. Each item of a list is named by the key that best tells it apart
(name, title, id…), so a long list of rules reads as their names.
*/

/** Whether a value is a list of objects, which reads best as a table. */
const tabular = (v: J): v is { [k: string]: J }[] => Array.isArray(v) && v.length > 0 && v.every((x) => x && typeof x === 'object' && !Array.isArray(x))

function Scalar({ v }: { v: J }) {
  if (v === null) return <Text span c="dimmed" fs="italic" ff="var(--font-mono)" fz={12}>null</Text>
  if (typeof v === 'string') return <Text span ff="var(--font-mono)" fz={12} style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{v}</Text>
  return <Text span ff="var(--font-mono)" fz={12} fw={600}>{String(v)}</Text>
}

function Node({ name, v, depth, open, toggle, path, match }: {
  name: string; v: J; depth: number; open: Set<string>; toggle: (p: string) => void; path: string; match: string
}) {
  const container = v !== null && typeof v === 'object'
  const isOpen = open.has(path)
  const entries: [string, J][] = container ? (Array.isArray(v) ? v.map((x, i) => [`${i}`, x]) : Object.entries(v)) : []
  const label = Array.isArray(v) ? null : itemLabel(v)
  return (
    <Box pl={depth ? 14 : 0} style={depth ? { borderLeft: '1px solid var(--color-line)' } : undefined}>
      <Group gap={4} wrap="nowrap" align="flex-start" py={1}>
        {container ? (
          <UnstyledButton onClick={() => toggle(path)} aria-label={`${isOpen ? 'Close' : 'Open'} ${name}`} aria-expanded={isOpen}
            style={{ display: 'flex', alignItems: 'center', gap: 4, minWidth: 0 }}>
            {isOpen ? <IconChevronDown size={12} /> : <IconChevronRight size={12} />}
            <Text span ff="var(--font-mono)" fz={12} fw={600} c={match && name.toLowerCase().includes(match) ? 'var(--mantine-color-amber-text)' : undefined}>{name}</Text>
            {label && <Text span fz={12} truncate>{label}</Text>}
            {!isOpen && !label && <Text span fz={FS.micro} c="dimmed" truncate>{Array.isArray(v) ? `${v.length} items` : summary(v, 60)}</Text>}
          </UnstyledButton>
        ) : (
          <>
            <Text span ff="var(--font-mono)" fz={12} fw={600} style={{ flexShrink: 0 }}>{name}:</Text>
            <Scalar v={v} />
          </>
        )}
      </Group>
      {container && isOpen && entries.map(([k, x]) => (
        <Node key={k} name={k} v={x} depth={depth + 1} open={open} toggle={toggle} path={`${path}/${k}`} match={match} />
      ))}
    </Box>
  )
}

/** Every path to a container, for Open all; and those holding a match. */
function paths(v: J, path: string, out: string[], match: string, hits: Set<string>): boolean {
  if (v === null || typeof v !== 'object') return Boolean(match) && String(v).toLowerCase().includes(match)
  out.push(path)
  let hit = false
  const entries: [string, J][] = Array.isArray(v) ? v.map((x, i) => [`${i}`, x]) : Object.entries(v)
  for (const [k, x] of entries) {
    if (paths(x, `${path}/${k}`, out, match, hits) || (match && k.toLowerCase().includes(match))) hit = true
  }
  if (hit) hits.add(path)
  return hit
}

function Tree({ value }: { value: J }) {
  const [open, setOpen] = useState<Set<string>>(() => new Set(['$']))
  const [find, setFind] = useState('')
  const match = find.trim().toLowerCase()
  const all = useMemo(() => { const out: string[] = []; const hits = new Set<string>(); paths(value, '$', out, match, hits); return { out, hits } }, [value, match])
  const shown = match ? all.hits : open
  const toggle = (p: string) => setOpen((s) => { const n = new Set(match ? all.hits : s); if (n.has(p)) n.delete(p); else n.add(p); if (match) setFind(''); return n })
  return (
    <Box>
      <Group gap={6} mb={6} wrap="nowrap">
        <TextInput size="xs" placeholder="Find a key or value" leftSection={<IconSearch size={12} />} value={find} onChange={(e) => setFind(e.currentTarget.value)}
          aria-label="Find in the JSON" style={{ flex: 1 }} />
        <Button size="compact-xs" variant="subtle" onClick={() => setOpen(new Set(all.out))}>Open all</Button>
        <Button size="compact-xs" variant="subtle" onClick={() => setOpen(new Set(['$']))}>Close all</Button>
      </Group>
      {match && all.hits.size === 0 && <Text size={FS.micro} c="dimmed">Nothing matches.</Text>}
      <Node name={Array.isArray(value) ? `${value.length} items` : 'value'} v={value} depth={0} open={shown} toggle={toggle} path="$" match={match} />
    </Box>
  )
}

function Grid({ rows }: { rows: { [k: string]: J }[] }) {
  const keys = useMemo(() => {
    const seen = new Map<string, number>()
    for (const r of rows) for (const k of Object.keys(r)) seen.set(k, (seen.get(k) ?? 0) + 1)
    return [...seen.keys()]
  }, [rows])
  const [open, setOpen] = useState<string | null>(null)
  return (
    <ScrollArea type="auto">
      <Table striped withColumnBorders fz={12} verticalSpacing={4} aria-label="As a table">
        <Table.Thead>
          <Table.Tr><Table.Th>#</Table.Th>{keys.map((k) => <Table.Th key={k} ff="var(--font-mono)">{k}</Table.Th>)}</Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {rows.map((r, i) => (
            <Table.Tr key={i}>
              <Table.Td c="dimmed">{i + 1}</Table.Td>
              {keys.map((k) => {
                const v = r[k]
                const id = `${i}/${k}`
                if (v === undefined) return <Table.Td key={k} />
                if (v !== null && typeof v === 'object') {
                  const list = Array.isArray(v) && v.every((x) => typeof x !== 'object' || x === null)
                  return (
                    <Table.Td key={k} style={{ verticalAlign: 'top', minWidth: 160 }}>
                      {list ? (
                        <Box component="ul" m={0} pl={14} style={{ fontFamily: 'var(--font-mono)' }}>
                          {(open === id ? v : v.slice(0, 3)).map((x, j) => <li key={j} style={{ wordBreak: 'break-word' }}>{String(x)}</li>)}
                          {v.length > 3 && (
                            <UnstyledButton fz={FS.micro} c="var(--mantine-color-azure-text)" onClick={() => setOpen(open === id ? null : id)}>
                              {open === id ? 'Show less' : `${v.length - 3} more`}
                            </UnstyledButton>
                          )}
                        </Box>
                      ) : <Text span fz={12}>{summary(v, 60)}</Text>}
                    </Table.Td>
                  )
                }
                return <Table.Td key={k} style={{ verticalAlign: 'top', wordBreak: 'break-word', minWidth: 120 }}><Scalar v={v} /></Table.Td>
              })}
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </ScrollArea>
  )
}

type View = 'tree' | 'table' | 'text'

function Body({ value, text, view }: { value: J; text: string; view: View }) {
  if (view === 'table' && tabular(value)) return <Grid rows={value} />
  if (view === 'text') return <Box component="pre" m={0} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{text}</Box>
  return <Tree value={value} />
}

/** A JSON value in the row panel: tree, table or text, and a bigger view. */
export function JsonView({ raw, column }: { raw: string; column: string }) {
  const parsed = useMemo(() => { try { return { v: JSON.parse(raw) as J } } catch { return null } }, [raw])
  const text = useMemo(() => (parsed ? JSON.stringify(parsed.v, null, 2) : raw), [parsed, raw])
  const canTable = parsed ? tabular(parsed.v) : false
  // The panel is narrow: a tree reads best there, and a list of objects
  // opens as a table in the bigger view.
  const [view, setView] = useState<View>('tree')
  const [big, setBig] = useState(false)
  const [bigView, setBigView] = useState<View>(canTable ? 'table' : 'tree')
  if (!parsed) return <Box component="pre" m={0} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{raw}</Box>
  const views = [{ value: 'tree', label: 'Tree' }, ...(canTable ? [{ value: 'table', label: 'Table' }] : []), { value: 'text', label: 'Text' }]
  const bar = (v: View, set: (v: View) => void) => (
    <Group gap={6} justify="space-between" wrap="nowrap" style={{ minWidth: 0 }}>
      <SegmentedControl size="xs" value={v} onChange={(x) => set(x as View)} data={views} aria-label={`Show ${column} as`} style={{ flexShrink: 0 }} />
      <Text size={FS.micro} c="dimmed" truncate style={{ minWidth: 0 }}>{summary(parsed.v, 50)}</Text>
    </Group>
  )
  return (
    // The panel scrolls as a table, which widens to its content: contain
    // keeps a long line from widening the panel.
    <Box style={{ minWidth: 0, maxWidth: '100%', overflow: 'hidden', contain: 'inline-size' }}>
      <Group gap={6} wrap="nowrap" mb={6}>
        <Box style={{ flex: 1, minWidth: 0 }}>{bar(view, setView)}</Box>
        <Tooltip label="Open bigger" withinPortal>
          <ActionIcon size="sm" variant="subtle" onClick={() => setBig(true)} aria-label={`Open ${column} bigger`}><IconArrowsMaximize size={13} /></ActionIcon>
        </Tooltip>
      </Group>
      <Box style={{ maxHeight: 420, overflow: 'auto' }}><Body value={parsed.v} text={text} view={view} /></Box>
      <Modal opened={big} onClose={() => setBig(false)} size="90%" title={<Text fw={650} ff="var(--font-mono)">{column}</Text>}>
        {bar(bigView, setBigView)}
        <Box mt="sm"><Body value={parsed.v} text={text} view={bigView} /></Box>
      </Modal>
    </Box>
  )
}
