import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  ActionIcon, Badge, Box, Button, CloseButton, Drawer, Group, Loader, Menu, ScrollArea, Stack, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import {
  IconArrowBackUp, IconArrowDown, IconArrowUp, IconChartBar, IconChevronDown, IconChevronLeft, IconChevronRight, IconCopy, IconEyeOff,
  IconCopyPlus, IconFilter, IconLink, IconPencil, IconPlus, IconSortAscending, IconSortDescending, IconTrash, IconWand, IconX,
} from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { itemLabel, summary, type J } from '~/lib/jsonSummary'
import { notifyOk, notifyWarn } from '~/lib/notify'
import {
  addToQuery, columnStats, copyAs, emptyView, isJSONType, isNumeric, pretty, singleTable, viewRows,
  type Cell, type Col, type CopyFormat, type Filter, type ViewState,
} from '~/lib/resultView'
import type { Collected } from '~/components/DatabaseAdmin'
import {
  deleteStmt, duplicateValues, editable, editableGroups, editorKind, groupUpdateStmt, insertStmt, jsonKeyHints, updateStmt,
  type EditGroup, type Stmt, type TableColumn,
} from '~/lib/rowEdit'
import { rowStatements } from '~/lib/sqlGen'
import { CellInput, ChangesBar, ReviewDialog, RowForm, useEditTargets, type EditSupport, type FormField } from '~/components/RowEdits'
import { ArrayEditor, JsonEditor } from '~/components/JsonEditor'

// The JSON viewer loads when a row with JSON is first opened.
const JsonView = lazy(() => import('~/components/JsonView').then((m) => ({ default: m.JsonView })))
// Summarize and chart loads when it is first opened.
const ResultSummary = lazy(() => import('~/components/ResultSummary').then((m) => ({ default: m.ResultSummary })))

/*
The query tab's results: a windowed grid of the rows the statement sent,
with a menu on each column (sort, filter, hide, copy, quick figures), Copy
as, and a row panel with every field in full. A value that is a foreign key
opens the row it points at, read with the person's role.

Sorting and filtering work on the loaded rows only, and say so; "Add to the
query" writes the same thing as SQL around the statement, in a new tab, for
the database to do over every row.

When every column came from one table and its primary key is among them,
the rows can be changed in place (see rowEdit): double-click a cell, or open
a row and Edit it; Add a row opens an empty one. Nothing is sent until Save,
and Review SQL shows exactly what Save sends.
*/

export interface GridResult {
  id: number
  sql: string
  cols: Col[]
  rows: Cell[][]
  /** Cells the agent cut at its size limit, as `row:col`. */
  cut?: Set<string>
  done?: { tag?: string; truncated?: boolean; ms?: number }
  error?: { error?: string; code?: string; hint?: string; guard?: unknown }
}

/** A single-column foreign key from a result column to another table. */
export interface ForeignKey { table: string; column: string }

type GridCollect = (sql: string, maxRows?: number, params?: (string | null)[]) => Promise<Collected>

const ROW_H = 28

const th: React.CSSProperties = {
  textAlign: 'left', padding: 0, borderRight: '1px solid var(--color-line)',
  borderBottom: '1px solid var(--color-line)', whiteSpace: 'nowrap', fontWeight: 600,
}
const td: React.CSSProperties = {
  padding: '0 12px', borderRight: '1px solid var(--color-line)', borderBottom: '1px solid var(--color-line)',
  whiteSpace: 'nowrap', maxWidth: 480, overflow: 'hidden', textOverflow: 'ellipsis',
}

const FK_SQL = `SELECT c.conrelid::int8::text, c.conkey[1]::text, c.confrelid::regclass::text, fa.attname::text
FROM pg_constraint c
JOIN pg_attribute fa ON fa.attrelid = c.confrelid AND fa.attnum = c.confkey[1]
WHERE c.contype = 'f' AND cardinality(c.conkey) = 1 AND c.conrelid = ANY($1::oid[])`

/** The foreign keys of a result's columns, by column index. */
function useForeignKeys(cols: Col[], collect: GridCollect | undefined, key: number): Record<number, ForeignKey> {
  const [fks, setFks] = useState<Record<number, ForeignKey>>({})
  const tables = useMemo(() => [...new Set(cols.map((c) => c.table).filter((t): t is number => Boolean(t)))].sort(), [cols])
  const sig = tables.join(',')
  useEffect(() => {
    setFks({})
    if (!collect || tables.length === 0) return
    let live = true
    void collect(FK_SQL, 1000, [`{${sig}}`]).then((r) => {
      if (!live || r.error) return
      const out: Record<number, ForeignKey> = {}
      cols.forEach((c, i) => {
        const hit = r.rows.find((x) => Number(x[0]) === c.table && Number(x[1]) === c.attnum)
        if (hit && hit[2] && hit[3]) out[i] = { table: hit[2], column: hit[3] }
      })
      setFks(out)
    })
    return () => { live = false }
    // A new result, or new source tables, asks again.
  }, [key, sig]) // eslint-disable-line react-hooks/exhaustive-deps
  return fks
}

/** A short, readable preview of a JSON value for one grid cell. */
function jsonPreview(v: string): ReactNode {
  let j: J
  try { j = JSON.parse(v) as J } catch { return v }
  if (Array.isArray(j)) {
    const names = j.map(itemLabel).filter((x): x is string => x !== null)
    return (
      <span>
        <Text span size={FS.micro} c="dimmed" mr={4}>{j.length} {j.length === 1 ? 'item' : 'items'}</Text>
        {names.length ? names.slice(0, 4).join(', ') + (j.length > 4 ? ', …' : '') : summary(j, 120)}
      </span>
    )
  }
  return summary(j, 160)
}

function filterLabel(c: Col, f: Filter): string {
  switch (f.kind) {
    case 'null': return `${c.name} is NULL`
    case 'notnull': return `${c.name} has a value`
    case 'equals': return `${c.name} = ${f.text}`
    case 'contains': return `${c.name} contains “${f.text}”`
  }
}

async function copyText(text: string, what: string) {
  try {
    await navigator.clipboard.writeText(text)
    notifyOk('Copied', what)
  } catch {
    notifyWarn('Could not copy', 'This browser did not allow the clipboard.')
  }
}

function ColumnMenu({ col, index, rows, view, setView, children }: {
  col: Col; index: number; rows: Cell[][]; view: ViewState; setView: (f: (v: ViewState) => ViewState) => void; children: ReactNode
}) {
  const [opened, setOpened] = useState(false)
  const current = view.filters[index]
  const [text, setText] = useState(current && (current.kind === 'contains' || current.kind === 'equals') ? current.text : '')
  const stats = useMemo(() => (opened ? columnStats(col, rows, index) : null), [opened, col, rows, index])
  const setFilter = (f: Filter | null) => setView((v) => {
    const filters = { ...v.filters }
    if (f) filters[index] = f
    else delete filters[index]
    return { ...v, filters }
  })
  const sort = (desc: boolean | null) => setView((v) => ({ ...v, sort: desc === null ? null : { col: index, desc } }))
  const n = (x: number) => x.toLocaleString(undefined, { maximumFractionDigits: 4 })
  return (
    <Menu opened={opened} onChange={setOpened} position="bottom-start" withinPortal width={260} closeOnItemClick>
      <Menu.Target>{children}</Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>{col.name} · {col.type}</Menu.Label>
        <Menu.Item leftSection={<IconSortAscending size={14} />} onClick={() => sort(false)}>Sort ascending</Menu.Item>
        <Menu.Item leftSection={<IconSortDescending size={14} />} onClick={() => sort(true)}>Sort descending</Menu.Item>
        {view.sort?.col === index && <Menu.Item leftSection={<IconX size={14} />} onClick={() => sort(null)}>Clear the sort</Menu.Item>}
        <Menu.Divider />
        <Box px="sm" py={4}>
          <TextInput size="xs" placeholder="Contains…" aria-label={`Filter ${col.name}`} leftSection={<IconFilter size={12} />}
            value={text} onChange={(e) => setText(e.currentTarget.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') { setFilter(text ? { kind: 'contains', text } : null); setOpened(false) }
              e.stopPropagation()
            }} />
        </Box>
        <Menu.Item onClick={() => setFilter({ kind: 'null' })}>Only NULL</Menu.Item>
        <Menu.Item onClick={() => setFilter({ kind: 'notnull' })}>Only with a value</Menu.Item>
        {current && <Menu.Item leftSection={<IconX size={14} />} onClick={() => { setText(''); setFilter(null) }}>Clear the filter</Menu.Item>}
        <Menu.Divider />
        <Menu.Item leftSection={<IconEyeOff size={14} />} onClick={() => setView((v) => ({ ...v, hidden: [...v.hidden, index] }))}>Hide the column</Menu.Item>
        <Menu.Item leftSection={<IconCopy size={14} />}
          onClick={() => void copyText(rows.map((r) => r[index] ?? '').join('\n'), `${rows.length.toLocaleString()} values of ${col.name}`)}>
          Copy the values
        </Menu.Item>
        {stats && (
          <>
            <Menu.Divider />
            <Box px="sm" py={4} aria-label={`Figures for ${col.name}`}>
              <Text size={FS.micro} c="dimmed">Over the {stats.count.toLocaleString()} loaded rows</Text>
              <Text size={FS.micro}>{n(stats.distinct)} distinct · {n(stats.nulls)} NULL</Text>
              {stats.min !== undefined && <Text size={FS.micro} ff="var(--font-mono)" truncate>min {stats.min} · max {stats.max}</Text>}
              {stats.sum !== undefined && <Text size={FS.micro} ff="var(--font-mono)">sum {n(stats.sum)} · avg {n(stats.avg!)}</Text>}
            </Box>
          </>
        )}
      </Menu.Dropdown>
    </Menu>
  )
}

type Panel =
  | { kind: 'row'; at: number; editing?: boolean }
  | { kind: 'new'; index: number }
  | { kind: 'linked'; title: string; cols: Col[]; row: Cell[] | null; loading: boolean; error?: string; back: Panel }

/** What the console says about a value the agent cut at its size limit. */
const CUT_NOTE = 'The console loaded only the first part of this value (a value over 16 MB, or past 64 MB in one result). Read the rest in pieces with substring() in SQL.'

function Fields({ cols, row, fks, onFollow, cut }: { cols: Col[]; row: Cell[]; fks: Record<number, ForeignKey>; onFollow?: (i: number) => void; cut?: (i: number) => boolean }) {
  return (
    <Stack gap="sm">
      {cols.map((c, i) => {
        const v = row[i] ?? null
        const p = pretty(c, v)
        const fk = fks[i]
        return (
          <Box key={i} style={{ minWidth: 0 }}>
            <Group gap={6} wrap="nowrap">
              <Text size={FS.meta} fw={600} ff="var(--font-mono)">{c.name}</Text>
              <Text size={FS.micro} c="dimmed">{c.type}</Text>
              {cut?.(i) && <Badge size="xs" variant="light" color="amber" title={CUT_NOTE}>cut</Badge>}
              {fk && v !== null && onFollow && (
                <Button size="compact-xs" variant="subtle" leftSection={<IconLink size={12} />} onClick={() => onFollow(i)} ml="auto">
                  {fk.table}
                </Button>
              )}
              <Tooltip label="Copy" withinPortal>
                <ActionIcon size="xs" variant="subtle" ml={fk && v !== null ? 0 : 'auto'} aria-label={`Copy ${c.name}`}
                  onClick={() => void copyText(v ?? '', c.name)} disabled={v === null}>
                  <IconCopy size={12} />
                </ActionIcon>
              </Tooltip>
            </Group>
            {p.json && v !== null && !cut?.(i) ? (
              <Box mt={2} p={6} style={{ borderRadius: 6, background: 'var(--color-raised)' }}>
                <Suspense fallback={<Text size={FS.micro} c="dimmed">{summary(JSON.parse(v) as J, 120)}</Text>}>
                  <JsonView raw={v} column={c.name} />
                </Suspense>
              </Box>
            ) : <Box component="pre" m={0} mt={2} p={6}
              style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'pre-wrap', wordBreak: 'break-word', borderRadius: 6,
                background: 'var(--color-raised)', color: v === null ? 'var(--mantine-color-dimmed)' : undefined, fontStyle: v === null ? 'italic' : undefined,
                maxHeight: p.json ? 320 : undefined, overflow: 'auto' }}>
              {p.text}
            </Box>}
            {cut?.(i) && <Text size={FS.micro} c={INK.pending} mt={2}>{CUT_NOTE}</Text>}
          </Box>
        )
      })}
    </Stack>
  )
}

/** NULL in a cell: a quiet tag, so it never reads as the text "null". */
function NullTag() {
  return (
    <span style={{ fontSize: 10, fontStyle: 'italic', letterSpacing: '.03em', padding: '0 5px', borderRadius: 4,
      color: 'var(--mantine-color-dimmed)', background: 'var(--color-raised)' }}>NULL</span>
  )
}

/** A row's changed cells, by result column. */
type Changes = Map<number, Map<number, Cell>>

/** Where a JSON or array editor is open: a loaded row, or a new one. */
type Open = { row: number; col: number } | { added: number; attnum: number }

/** What each statement of a save stands for, to fold what it returns back in. */
type Saved = { update: number; group: EditGroup } | { insert: true } | { del: number }

export function ResultsGrid({ result, narrow, collect, onAddToQuery, edit, onRows }: {
  result: GridResult | null
  narrow: boolean
  collect?: GridCollect
  /** Opens the statement with the grid's sort and filters as SQL. */
  onAddToQuery?: (sql: string, params: Record<string, string>) => void
  /** Editing rows in place, when the session may write. */
  edit?: EditSupport
  /** The rows after a save, with what Postgres returned in place. */
  onRows?: (rows: Cell[][]) => void
}) {
  const viewport = useRef<HTMLDivElement | null>(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [height, setHeight] = useState(400)
  const [view, setViewState] = useState<ViewState>(emptyView)
  const [panel, setPanel] = useState<Panel | null>(null)
  const [changes, setChanges] = useState<Changes>(() => new Map())
  const [added, setAdded] = useState<Map<number, Cell>[]>([])
  // Rows marked to delete when the changes are saved, by result row.
  const [deleted, setDeleted] = useState<Set<number>>(() => new Set())
  const [editing, setEditing] = useState<{ row: number; col: number } | null>(null)
  const [open, setOpen] = useState<Open | null>(null)
  const [reviewing, setReviewing] = useState(false)
  // The grid, or the result summarized as a chart.
  const [mode, setMode] = useState<'grid' | 'summary'>('grid')
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  useEffect(() => {
    const el = viewport.current
    if (!el) return
    const ro = new ResizeObserver(() => setHeight(el.clientHeight))
    ro.observe(el)
    setHeight(el.clientHeight)
    return () => ro.disconnect()
  }, [result?.error])
  const rid = result?.id ?? 0
  // A new statement starts with a fresh view.
  useEffect(() => {
    setScrollTop(0)
    if (viewport.current) viewport.current.scrollTop = 0
    setViewState(emptyView())
    setPanel(null)
    setChanges(new Map())
    setAdded([])
    setDeleted(new Set())
    setEditing(null)
    setOpen(null)
    setSaveError(null)
    setMode('grid')
  }, [rid])
  const setView = useCallback((f: (v: ViewState) => ViewState) => {
    setViewState(f)
    setScrollTop(0)
    if (viewport.current) viewport.current.scrollTop = 0
  }, [])

  const cols = useMemo(() => result?.cols ?? [], [result?.cols])
  const fks = useForeignKeys(cols, result?.done ? collect : undefined, result?.done ? rid : 0)
  const order = useMemo(() => (result ? viewRows(cols, result.rows, view) : []),
    // Rows grow while the statement streams; their count is the change.
    [result, result?.rows.length, cols, view]) // eslint-disable-line react-hooks/exhaustive-deps
  const shownCols = useMemo(() => cols.map((_, i) => i).filter((i) => !view.hidden.includes(i)), [cols, view.hidden])
  const canEdit = Boolean(edit?.allowed && result?.done && !result.error)
  // A builder query says where its JSON key columns came from.
  const hints = useMemo(() => jsonKeyHints(result?.sql ?? ''), [result?.sql])
  const { targets, named } = useEditTargets(cols, hints, canEdit ? collect : undefined, canEdit ? rid : 0)
  const target = useMemo(() => { const o = cols[0]?.table; return o ? targets.get(o) ?? null : null }, [cols, targets])
  // `ed` is the result as one table, for Copy as statements; `eg` is what
  // can be edited, table by table.
  const ed = useMemo(() => editable(cols, target), [cols, target])
  const eg = useMemo(() => editableGroups(cols, targets, hints, named), [cols, targets, hints, named])

  const follow = useCallback((row: Cell[], i: number, fkMap: Record<number, ForeignKey>, back: Panel) => {
    const fk = fkMap[i]
    const v = row[i]
    if (!fk || v === null || v === undefined || !collect) return
    const title = `${fk.table} where ${fk.column} = ${v}`
    setPanel({ kind: 'linked', title, cols: [], row: null, loading: true, back })
    const col = /^[a-z_][a-z0-9_$]*$/.test(fk.column) ? fk.column : `"${fk.column.replace(/"/g, '""')}"`
    void collect(`SELECT * FROM ${fk.table} WHERE ${col} = $1 LIMIT 1`, 1, [v]).then((r) => {
      setPanel((p) => (p?.kind === 'linked' && p.title === title
        ? { ...p, loading: false, cols: r.cols as Col[], row: r.rows[0] ?? null, error: r.error?.error }
        : p))
    })
  }, [collect])

  if (!result) {
    return (
      <Text size={FS.meta} c="dimmed" p="md">
        Results appear here. Pick a table on the left to start.
      </Text>
    )
  }
  const summary = result.error
    ? null
    : result.done
      ? `${result.done.tag ?? ''}${result.done.truncated ? ' · stopped at the row limit' : ''} · ${result.done.ms ?? 0} ms`
      : `${result.rows.length.toLocaleString()} rows so far…`

  const narrowed = Boolean(view.sort || Object.keys(view.filters).length || view.hidden.length)
  const visibleRows = () => order.map((i) => shownCols.map((c) => result.rows[i]![c] ?? null))
  const visibleCols = () => shownCols.map((c) => cols[c]!)
  const doCopy = (fmt: CopyFormat) => {
    const vc = visibleCols()
    const table = singleTable(result.sql, cols) ?? 'query_result'
    const text = copyAs(fmt, vc, visibleRows(), table)
    void copyText(text, `${order.length.toLocaleString()} rows as ${fmt === 'insert' ? 'INSERT statements' : fmt.toUpperCase()}`)
  }
  // Statements for the rows shown, written against the table they came from,
  // keyed on its primary key: to keep, or to run somewhere else.
  const doStatements = (kind: 'update' | 'delete' | 'upsert') => {
    if (!ed.ok) return
    const text = rowStatements(ed.target, cols, order.map((i) => result.rows[i]!), kind)
    void copyText(text, `${order.length.toLocaleString()} rows as ${kind === 'upsert' ? 'upsert' : kind.toUpperCase()} statements`)
  }

  // Windowed rows: only what is visible plus a margin is in the DOM.
  const total = order.length
  // New rows sit above the loaded ones, so the window starts below them.
  const top = Math.max(0, scrollTop - added.length * ROW_H)
  const first = Math.max(0, Math.floor(top / ROW_H) - 10)
  const last = Math.min(total, Math.ceil((top + height) / ROW_H) + 10)
  const slice = order.slice(first, last)

  const panelIndex = panel?.kind === 'row' ? order[panel.at] : undefined
  const panelRow = panelIndex !== undefined ? result.rows[panelIndex] : undefined

  // Editing. `live` is what can be edited, table by table; `primary` is the
  // table a new row goes into: the first column's, as the builder starts there.
  const live = canEdit && eg.ok ? eg : null
  const primary = live ? (live.byCol[0] ?? live.groups[0]) : undefined
  const joined = (live?.groups.length ?? 0) > 1
  const groupOf = (j: number): EditGroup | undefined => live?.byCol[j]
  const tcol = (j: number): TableColumn | undefined => {
    const g = groupOf(j)
    if (!g) return undefined
    const k = g.keys.get(j)
    // A JSON key is edited as text; its JSON type is kept on save.
    if (k) return { attnum: -(j + 1), name: cols[j]!.name, type: `text in ${k.column.name}`, notNull: false, hasDefault: false, generated: false }
    return g.target.columns.find((c) => c.attnum === cols[j]!.attnum)
  }
  const cut = edit?.cut
  const lockReason = (ri: number, j: number): string | undefined => {
    if (!live) return 'read only'
    const g = groupOf(j)
    if (!g) return live.why[j] ?? 'read only'
    if (deleted.has(ri)) return 'marked to delete'
    if (g.pkCols.includes(j)) return 'primary key'
    if (tcol(j)?.generated) return 'generated'
    if (cut?.has(`${ri}:${j}`)) return 'too long to edit here'
    if (g.pkCols.some((p) => cut?.has(`${ri}:${p}`))) return 'its key is too long'
    return undefined
  }
  const valueAt = (ri: number, j: number): Cell => {
    const m = changes.get(ri)
    return m?.has(j) ? m.get(j)! : (result.rows[ri]?.[j] ?? null)
  }
  const setCell = (ri: number, j: number, v: Cell) => {
    setSaveError(null)
    setChanges((prev) => {
      const n = new Map(prev)
      const m = new Map(n.get(ri) ?? [])
      if (v === (result.rows[ri]?.[j] ?? null)) m.delete(j)
      else m.set(j, v)
      if (m.size) n.set(ri, m)
      else n.delete(ri)
      return n
    })
  }
  const setAddedValue = (k: number, attnum: number, v: Cell | undefined) => {
    setSaveError(null)
    setAdded((a) => a.map((m, i) => {
      if (i !== k) return m
      const n = new Map(m)
      if (v === undefined) n.delete(attnum)
      else n.set(attnum, v)
      return n
    }))
  }
  const newRow = (m: Map<number, Cell>) => {
    setSaveError(null)
    setAdded((a) => [...a, m])
    setPanel({ kind: 'new', index: added.length })
  }
  const addRow = () => {
    if (!primary) return
    // A column Postgres cannot fill in starts as a value to give.
    const m = new Map<number, Cell>()
    for (const c of primary.target.columns) if (c.notNull && !c.hasDefault && !c.generated) m.set(c.attnum, '')
    newRow(m)
  }
  /** A copy of a row, as it is now, as a new row: the key is left to Postgres. */
  const duplicate = (ri: number) => {
    if (!primary) return
    newRow(duplicateValues(primary.target, cols, cols.map((_, j) => valueAt(ri, j))))
  }
  const toggleDelete = (ri: number) => {
    setSaveError(null)
    setDeleted((d) => {
      const n = new Set(d)
      if (n.has(ri)) n.delete(ri)
      else n.add(ri)
      return n
    })
    // A row to delete keeps no other change.
    setChanges((prev) => { if (!prev.has(ri)) return prev; const n = new Map(prev); n.delete(ri); return n })
  }
  const startEdit = (ri: number, j: number) => {
    if (!live || lockReason(ri, j)) return
    const c = tcol(j)
    const kind = editorKind(c?.type ?? cols[j]!.type, c?.enumValues)
    if (kind === 'json' || kind === 'array') setOpen({ row: ri, col: j })
    else setEditing({ row: ri, col: j })
  }
  const pending = (): { list: Stmt[]; rows: Saved[] } => {
    const list: Stmt[] = []
    const rows: Saved[] = []
    if (!live) return { list, rows }
    for (const ri of [...changes.keys()].sort((a, b) => a - b)) {
      if (deleted.has(ri)) continue
      // Each table's part of the row is its own UPDATE, in one transaction.
      const parts = new Map<EditGroup, Map<number, Cell>>()
      for (const [j, v] of changes.get(ri)!) {
        const g = groupOf(j)
        if (!g) continue
        const m = parts.get(g) ?? new Map<number, Cell>()
        m.set(j, v)
        parts.set(g, m)
      }
      for (const g of live.groups) {
        const m = parts.get(g)
        if (!m) continue
        list.push(!joined && g.keys.size === 0
          ? updateStmt(g.target, cols, g.pkCols, result.rows[ri]!, m)
          : groupUpdateStmt(g, cols, result.rows[ri]!, m))
        rows.push({ update: ri, group: g })
      }
    }
    if (primary) for (const a of added) { list.push(insertStmt(primary.target, a)); rows.push({ insert: true }) }
    if (primary) {
      for (const ri of [...deleted].sort((a, b) => a - b)) {
        list.push(deleteStmt(primary.target, cols, primary.pkCols, result.rows[ri]!))
        rows.push({ del: ri })
      }
    }
    return { list, rows }
  }
  /** A returned row in the result's own column order. */
  const asResultRow = (c: Collected): Cell[] | null => {
    const src = c.rows[0]
    if (!src) return null
    return cols.map((col) => {
      const k = c.cols.findIndex((x) => (x.attnum && x.attnum === col.attnum) || x.name === col.name)
      return k < 0 ? null : (src[k] ?? null)
    })
  }
  /** A row with one table's part replaced by what its UPDATE returned. */
  const withPart = (row: Cell[], c: Collected, g: EditGroup, ri: number): Cell[] => {
    const src = c.rows[0]
    if (!src) return row
    const next = row.slice()
    for (const j of g.cols) {
      if (g.keys.has(j)) { const m = changes.get(ri); if (m?.has(j)) next[j] = m.get(j)!; continue }
      const k = c.cols.findIndex((x) => x.attnum === cols[j]!.attnum)
      if (k >= 0) next[j] = src[k] ?? null
    }
    return next
  }
  const save = async () => {
    if (!edit || !live || saving) return
    const { list, rows } = pending()
    if (list.length === 0) return
    setSaving(true)
    setSaveError(null)
    const r = await edit.save(list)
    setSaving(false)
    if (!r.ok) {
      setSaveError(`Nothing was saved. ${list[r.at]?.label ?? 'The save'}: ${r.error}`)
      return
    }
    let next = result.rows.slice()
    const gone = new Set<number>()
    let trashed = false
    r.returned.forEach((c, k) => {
      const s = rows[k]
      if (!s) return
      if ('del' in s) { gone.add(s.del); if (c.trashed) trashed = true; return }
      if ('insert' in s) { const row = asResultRow(c); if (row) next.push(row); return }
      next[s.update] = !joined && s.group.keys.size === 0 ? (asResultRow(c) ?? next[s.update]!) : withPart(next[s.update]!, c, s.group, s.update)
    })
    if (gone.size) next = next.filter((_, i) => !gone.has(i))
    setChanges(new Map())
    setAdded([])
    setDeleted(new Set())
    setReviewing(false)
    setPanel(null)
    onRows?.(next)
    const tables = [...new Set(live.groups.map((g) => g.target.table))].join(' and ')
    notifyOk('Saved', `${list.length} ${list.length === 1 ? 'change' : 'changes'} to ${tables}`
      + (trashed ? '. The deleted rows are kept in the trash for 30 days, where Undo puts them back.' : ''))
  }
  const changeCount = changes.size + added.length + deleted.size
  const rowFields = (ri: number): FormField[] => {
    const out: FormField[] = []
    // Fields of each table together, the tables in the order they appear.
    for (const g of live?.groups ?? []) {
      for (const j of [...g.cols].sort((a, b) => a - b)) {
        const col = tcol(j)
        if (!col) continue
        out.push({ col, id: j, group: joined ? g.target.table : undefined, value: valueAt(ri, j), original: result.rows[ri]?.[j] ?? null, locked: lockReason(ri, j) })
      }
    }
    return out
  }
  const newFields = (k: number): FormField[] => {
    if (!primary) return []
    const m = added[k]
    const shown = new Set(primary.cols.map((j) => cols[j]!.attnum))
    const required = (c: TableColumn) => c.notNull && !c.hasDefault && !c.generated
    // Required columns first, then those the result shows; the rest wait
    // under More columns.
    const rank = (c: TableColumn) => (required(c) ? 0 : shown.has(c.attnum) && !c.generated ? 1 : 2)
    return [...primary.target.columns].sort((a, b) => rank(a) - rank(b)).map((col) => ({
      col, value: m?.get(col.attnum) ?? null, unset: !m?.has(col.attnum), locked: col.generated ? 'generated' : undefined, more: rank(col) === 2,
    }))
  }
  const openValue: Cell = open
    ? 'row' in open ? valueAt(open.row, open.col) : (added[open.added]?.get(open.attnum) ?? null)
    : null
  const openCol = open ? ('row' in open ? tcol(open.col) : primary?.target.columns.find((c) => c.attnum === open.attnum)) : undefined
  const applyOpen = (v: Cell) => {
    if (!open) return
    if ('row' in open) setCell(open.row, open.col, v)
    else setAddedValue(open.added, open.attnum, v)
    setOpen(null)
  }

  return (
    <>
      <Group gap="sm" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }} wrap={narrow ? 'wrap' : 'nowrap'}>
        <Text size={FS.meta} fw={600}>{result.error ? 'Message' : 'Results'}</Text>
        {summary && <Text size={FS.meta} c="dimmed" style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{summary}</Text>}
        {!result.error && narrowed && (
          <Group gap={6} wrap="wrap" aria-label="What the grid is showing">
            <Text size={FS.micro} c={INK.pending}>{total.toLocaleString()} of {result.rows.length.toLocaleString()} loaded rows</Text>
            {view.sort && cols[view.sort.col] && (
              <Badge size="sm" variant="light" tt="none" rightSection={<CloseButton size="xs" aria-label="Clear the sort" onClick={() => setView((v) => ({ ...v, sort: null }))} />}
                leftSection={view.sort.desc ? <IconArrowDown size={10} /> : <IconArrowUp size={10} />}>
                {cols[view.sort.col]!.name}
              </Badge>
            )}
            {Object.entries(view.filters).map(([k, f]) => cols[Number(k)] && (
              <Badge key={k} size="sm" variant="light" tt="none" leftSection={<IconFilter size={10} />}
                rightSection={<CloseButton size="xs" aria-label={`Clear the filter on ${cols[Number(k)]!.name}`}
                  onClick={() => setView((v) => { const filters = { ...v.filters }; delete filters[Number(k)]; return { ...v, filters } })} />}>
                {filterLabel(cols[Number(k)]!, f)}
              </Badge>
            ))}
            {view.hidden.length > 0 && (
              <Button size="compact-xs" variant="subtle" onClick={() => setView((v) => ({ ...v, hidden: [] }))}>
                Show {view.hidden.length} hidden
              </Button>
            )}
            {onAddToQuery && (
              <Tooltip label="Opens a new tab with this as SQL, so the database does it over every row" withinPortal>
                <Button size="compact-xs" variant="light" leftSection={<IconWand size={12} />}
                  onClick={() => { const a = addToQuery(result.sql, cols, view); onAddToQuery(a.sql, a.params) }}>
                  Add to the query
                </Button>
              </Tooltip>
            )}
          </Group>
        )}
        {!result.error && result.rows.length > 0 && result.done && (
          <Button size="compact-xs" variant={mode === 'summary' ? 'light' : 'subtle'} ml="auto" leftSection={<IconChartBar size={12} />}
            aria-pressed={mode === 'summary'} onClick={() => setMode((m) => (m === 'grid' ? 'summary' : 'grid'))}>
            {mode === 'summary' ? 'Back to rows' : 'Summarize'}
          </Button>
        )}
        {!result.error && result.rows.length > 0 && (
          <Menu position="bottom-end" withinPortal>
            <Menu.Target>
              <Button size="compact-xs" variant="subtle" rightSection={<IconChevronDown size={12} />} leftSection={<IconCopy size={12} />}>
                Copy as
              </Button>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Label>{total.toLocaleString()} rows, as shown</Menu.Label>
              <Menu.Item onClick={() => doCopy('csv')}>CSV</Menu.Item>
              <Menu.Item onClick={() => doCopy('tsv')}>TSV (pastes into a spreadsheet)</Menu.Item>
              <Menu.Item onClick={() => doCopy('json')}>JSON</Menu.Item>
              <Menu.Item onClick={() => doCopy('markdown')}>Markdown table</Menu.Item>
              <Menu.Item onClick={() => doCopy('insert')}>INSERT statements</Menu.Item>
              {ed.ok && (
                <>
                  <Menu.Item onClick={() => doStatements('update')}>UPDATE statements, by primary key</Menu.Item>
                  <Menu.Item onClick={() => doStatements('delete')}>DELETE statements, by primary key</Menu.Item>
                  <Menu.Item onClick={() => doStatements('upsert')}>Upsert statements (INSERT … ON CONFLICT)</Menu.Item>
                </>
              )}
            </Menu.Dropdown>
          </Menu>
        )}
      </Group>
      {canEdit && cols.length > 0 && (live || !eg.ok && eg.reason !== 'Reading the table…') && (
        <Group gap="sm" px="md" py={4} wrap="nowrap" aria-label="Editing" style={{ borderBottom: '1px solid var(--color-line)' }}>
          {live
            ? <>
                <IconPencil size={12} color="var(--mantine-color-azure-text)" />
                <Text size={FS.micro} style={{ flex: 1 }} truncate>
                  <Text span fw={600} size={FS.micro}>Editable</Text> · {live.groups.map((g) => g.target.table).join(', ')}
                  {live.why.some(Boolean) ? ' (some columns read only)' : ''} ·{' '}
                  {narrow ? 'tap a row number, then Edit' : 'double-click a cell to change it'}
                  {edit?.inTransaction ? ' · saves go into your open transaction' : ''}
                </Text>
                <Button size="compact-xs" variant="light" leftSection={<IconPlus size={12} />} onClick={addRow}>Add a row</Button>
              </>
            : <Text size={FS.micro} c="dimmed" truncate>Read only here: {eg.ok ? '' : eg.reason}</Text>}
        </Group>
      )}
      {result.error ? (
        <Box p="md">
          <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
            <Text size={FS.body} fw={600}>
              {result.error.guard ? 'Argus stopped this before it ran' : 'Postgres refused this'}: {result.error.error}
              {result.error.code && <Text span size={FS.micro} ff="monospace" ml={6}>({result.error.code})</Text>}
            </Text>
            {result.error.hint && <Text size={FS.meta} mt={4}>{result.error.hint}</Text>}
            {result.error.code === '42501' && (
              <Text size={FS.meta} mt={4}>
                Your access to this database does not allow that. Nothing was changed. Use <b>Ask for write access</b> above if you need it.
              </Text>
            )}
          </Box>
        </Box>
      ) : (
        <>
        {mode === 'summary' && (
          <Suspense fallback={null}>
            <ResultSummary cols={cols} rows={result.rows} sql={result.sql} truncated={Boolean(result.done?.truncated)}
              onRunSQL={onAddToQuery ? (text) => onAddToQuery(text, {}) : undefined} />
          </Suspense>
        )}
        <div
          ref={viewport}
          onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
          style={{ flex: 1, overflow: 'auto', fontFamily: 'var(--font-mono)', fontSize: 12, position: 'relative', display: mode === 'summary' ? 'none' : undefined }}
        >
          <table style={{ borderCollapse: 'collapse', minWidth: '100%' }}>
            <thead style={{ position: 'sticky', top: 0, zIndex: 1, background: 'var(--color-raised)' }}>
              <tr>
                <th style={{ ...th, padding: '6px 12px' }}>#</th>
                {shownCols.map((i) => {
                  const c = cols[i]!
                  const sorted = view.sort?.col === i
                  return (
                    <th key={i} style={th} aria-sort={sorted ? (view.sort!.desc ? 'descending' : 'ascending') : undefined}>
                      <ColumnMenu col={c} index={i} rows={result.rows} view={view} setView={setView}>
                        <UnstyledButton px={12} py={narrow ? 6 : 5} w="100%" aria-label={`Column ${c.name}`}
                          style={{ display: 'flex', alignItems: 'center', gap: 6, fontWeight: 600, fontSize: 12, fontFamily: 'var(--font-mono)',
                            justifyContent: isNumeric(c) ? 'flex-end' : undefined }}>
                          <span style={{ display: 'flex', flexDirection: 'column', lineHeight: 1.25, alignItems: isNumeric(c) ? 'flex-end' : undefined }}>
                            <span>{c.name}</span>
                            {!narrow && <span style={{ fontWeight: 400, fontSize: 10.5, color: 'var(--mantine-color-dimmed)' }}>{c.type}</span>}
                          </span>
                          {fks[i] && <IconLink size={11} color="var(--mantine-color-dimmed)" aria-label={`links to ${fks[i]!.table}`} />}
                          {sorted && (view.sort!.desc ? <IconArrowDown size={11} /> : <IconArrowUp size={11} />)}
                          {view.filters[i] && <IconFilter size={11} color="var(--mantine-color-azure-text)" />}
                          <IconChevronDown size={11} style={{ opacity: 0.5, marginLeft: 'auto' }} />
                        </UnstyledButton>
                      </ColumnMenu>
                    </th>
                  )
                })}
              </tr>
            </thead>
            <tbody>
              {live && added.map((m, k) => (
                <tr key={`new${k}`} style={{ height: ROW_H, background: 'var(--mantine-color-teal-light)' }}>
                  <td style={{ ...td, padding: 0 }}>
                    <UnstyledButton onClick={() => setPanel({ kind: 'new', index: k })} aria-label={`Open new row ${k + 1}`}
                      style={{ display: 'block', width: '100%', padding: '0 12px', textAlign: 'right', color: INK.verified, fontSize: 12, fontFamily: 'inherit', fontWeight: 600 }}>
                      new
                    </UnstyledButton>
                  </td>
                  {shownCols.map((j) => {
                    // A new row goes into the first table; other tables' columns stay empty.
                    if (cols[j]!.table !== primary?.target.oid) return <td key={j} style={td} />
                    const a = cols[j]!.attnum!
                    const has = m.has(a)
                    const v = m.get(a) ?? null
                    return (
                      <td key={j} style={{ ...td, color: !has || v === null ? 'var(--mantine-color-dimmed)' : undefined, fontStyle: !has || v === null ? 'italic' : undefined }}
                        onDoubleClick={() => setPanel({ kind: 'new', index: k })}>
                        {!has ? 'default' : v === null ? 'null' : v}
                      </td>
                    )
                  })}
                </tr>
              ))}
              {first > 0 && <tr><td style={{ height: first * ROW_H, padding: 0 }} colSpan={shownCols.length + 1} /></tr>}
              {slice.map((ri, k) => {
                const row = result.rows[ri]!
                const at = first + k
                return (
                  <tr key={ri} style={{ height: ROW_H, ...(deleted.has(ri) ? { textDecoration: 'line-through', background: 'var(--mantine-color-rose-light)' }
                    : ri === panelIndex ? { background: 'var(--mantine-color-azure-light)' } : null) }}
                    aria-label={deleted.has(ri) ? `Row ${ri + 1}, to delete` : undefined} aria-selected={ri === panelIndex || undefined}>
                    <td style={{ ...td, padding: 0, background: ri === panelIndex ? 'var(--mantine-color-azure-light)' : 'var(--color-void)',
                      boxShadow: ri === panelIndex ? 'inset 3px 0 0 var(--color-brand)' : undefined }}>
                      <UnstyledButton onClick={() => setPanel({ kind: 'row', at })} aria-label={`Open row ${ri + 1}`} title="Every field of this row, in full"
                        style={{ display: 'block', width: '100%', padding: '0 12px', textAlign: 'right', fontSize: 12, fontFamily: 'inherit',
                          color: ri === panelIndex ? 'var(--mantine-color-azure-light-color)' : 'var(--mantine-color-dimmed)', fontWeight: ri === panelIndex ? 600 : undefined }}>
                        {ri + 1}
                      </UnstyledButton>
                    </td>
                    {shownCols.map((j) => {
                      const changed = changes.get(ri)?.has(j) ?? false
                      const cell = changed ? valueAt(ri, j) : (row[j] ?? null)
                      const c = cols[j]!
                      const fk = fks[j]
                      if (editing?.row === ri && editing.col === j) {
                        const tc = tcol(j)
                        return (
                          <td key={j} style={{ ...td, padding: '0 2px', overflow: 'visible' }}>
                            <CellInput kind={editorKind(tc?.type ?? c.type, tc?.enumValues)} col={tc} value={cell}
                              onCommit={(v) => { setCell(ri, j, v); setEditing(null) }} onCancel={() => setEditing(null)} />
                          </td>
                        )
                      }
                      return (
                        <td key={j} style={{ ...td,
                          textAlign: isNumeric(c) ? 'right' : undefined, background: changed ? 'var(--mantine-color-amber-light)' : undefined,
                          cursor: live && !lockReason(ri, j) ? 'cell' : undefined }}
                          title={changed ? `${cell ?? 'NULL'}\nwas ${row[j] ?? 'NULL'}` : (cell ?? 'NULL')}
                          onDoubleClick={live ? () => startEdit(ri, j) : undefined}>
                          {result.cut?.has(`${ri}:${j}`) && <Badge size="xs" variant="light" color="amber" mr={4} title={CUT_NOTE}>cut</Badge>}
                          {cell === null ? <NullTag />
                            : fk && live ? (
                              <Group gap={4} wrap="nowrap" component="span" style={{ display: 'inline-flex' }}>
                                <span>{cell}</span>
                                <ActionIcon size="xs" variant="subtle" onClick={() => follow(row, j, fks, { kind: 'row', at })} aria-label={`Open ${fk.table} row ${cell}`}>
                                  <IconLink size={11} />
                                </ActionIcon>
                              </Group>
                            )
                            : fk ? (
                              <UnstyledButton onClick={() => follow(row, j, fks, { kind: 'row', at })}
                                style={{ fontFamily: 'inherit', fontSize: 'inherit', color: INK.brand, textDecoration: 'underline dotted' }}
                                aria-label={`Open ${fk.table} row ${cell}`}>
                                {cell}
                              </UnstyledButton>
                            )
                              : isJSONType(c) ? jsonPreview(cell)
                                : cell.length > 200 ? cell.slice(0, 200) + '…' : cell}
                        </td>
                      )
                    })}
                  </tr>
                )
              })}
              {last < total && <tr><td style={{ height: (total - last) * ROW_H, padding: 0 }} colSpan={shownCols.length + 1} /></tr>}
            </tbody>
          </table>
          {total === 0 && result.rows.length > 0 && (
            <Text size={FS.meta} c="dimmed" p="md">No loaded row matches the filters.</Text>
          )}
        </div>
        </>
      )}
      {live && changeCount > 0 && (
        <ChangesBar changedRows={changes.size} newRows={added.length} deletedRows={deleted.size} busy={Boolean(edit?.busy)} saving={saving} error={saveError} narrow={narrow}
          onDiscard={() => { setChanges(new Map()); setAdded([]); setDeleted(new Set()); setSaveError(null); if (panel?.kind === 'new') setPanel(null) }}
          onReview={() => setReviewing(true)} onSave={() => void save()} />
      )}
      {live && reviewing && (
        <ReviewDialog opened stmts={pending().list} inTransaction={Boolean(edit?.inTransaction)} busy={Boolean(edit?.busy)} saving={saving}
          onClose={() => setReviewing(false)} onSave={() => void save()} />
      )}
      {open && openCol && (editorKind(openCol.type, openCol.enumValues) === 'array'
        ? <ArrayEditor key={JSON.stringify(open)} opened column={openCol.name} type={openCol.type} value={openValue} onApply={applyOpen} onClose={() => setOpen(null)} />
        : <JsonEditor key={JSON.stringify(open)} opened column={openCol.name} type={openCol.type} value={openValue} onApply={applyOpen} onClose={() => setOpen(null)} />)}

      <Drawer opened={panel !== null} onClose={() => setPanel(null)} position={narrow ? 'bottom' : 'right'} size={narrow ? '85%' : 460}
        withinPortal title={
          panel?.kind === 'new'
            ? <Text size={FS.body} fw={600} truncate>New row in {primary?.target.table}</Text>
            : panel?.kind === 'linked'
            ? <Group gap={6} wrap="nowrap">
                <ActionIcon variant="subtle" size="sm" aria-label="Back" onClick={() => setPanel(panel.back)}><IconArrowBackUp size={14} /></ActionIcon>
                <Text size={FS.body} fw={600} truncate>{panel.title}</Text>
              </Group>
            : panel?.kind === 'row'
              ? <Group gap={6} wrap="nowrap">
                  <Text size={FS.body} fw={600}>Row {(order[panel.at] ?? 0) + 1}</Text>
                  <Text size={FS.micro} c="dimmed">{panel.at + 1} of {order.length.toLocaleString()}</Text>
                  <ActionIcon variant="subtle" size="sm" aria-label="Previous row" disabled={panel.at === 0} onClick={() => setPanel({ kind: 'row', at: panel.at - 1, editing: panel.editing })}>
                    <IconChevronLeft size={14} />
                  </ActionIcon>
                  <ActionIcon variant="subtle" size="sm" aria-label="Next row" disabled={panel.at >= order.length - 1} onClick={() => setPanel({ kind: 'row', at: panel.at + 1, editing: panel.editing })}>
                    <IconChevronRight size={14} />
                  </ActionIcon>
                </Group>
              : null
        }
        scrollAreaComponent={ScrollArea.Autosize}>
        {panel?.kind === 'row' && panelRow && panelIndex !== undefined && (
          <>
            <Group justify="flex-end" mb="sm" gap="xs">
              {live && (
                <Button size="compact-xs" variant={panel.editing ? 'filled' : 'light'} leftSection={<IconPencil size={12} />}
                  onClick={() => setPanel({ ...panel, editing: !panel.editing })} aria-pressed={Boolean(panel.editing)}>
                  {panel.editing ? 'Done editing' : 'Edit'}
                </Button>
              )}
              {live && primary && (
                <>
                  <Button size="compact-xs" variant="default" leftSection={<IconCopyPlus size={12} />} onClick={() => duplicate(panelIndex)}>
                    Duplicate as new row
                  </Button>
                  {deleted.has(panelIndex)
                    ? (
                      <Button size="compact-xs" variant="light" color="rose" leftSection={<IconArrowBackUp size={12} />} onClick={() => toggleDelete(panelIndex)}>
                        Undo delete
                      </Button>
                    )
                    : (
                      <Button size="compact-xs" variant="subtle" color="rose" leftSection={<IconTrash size={12} />} onClick={() => toggleDelete(panelIndex)}
                        disabled={primary.pkCols.some((p) => cut?.has(`${panelIndex}:${p}`))}>
                        {joined ? `Delete from ${primary.target.table}` : 'Delete row'}
                      </Button>
                    )}
                </>
              )}
              <Button size="compact-xs" variant="default" leftSection={<IconCopy size={12} />}
                onClick={() => void copyText(copyAs('json', cols, [panelRow]).replace(/^\[\n|\n\]$/g, ''), 'the row as JSON')}>
                Copy as JSON
              </Button>
              {ed.ok && (
                <Menu position="bottom-end" withinPortal>
                  <Menu.Target>
                    <Button size="compact-xs" variant="default" rightSection={<IconChevronDown size={12} />}>Copy as SQL</Button>
                  </Menu.Target>
                  <Menu.Dropdown>
                    {(['insert', 'update', 'delete', 'upsert'] as const).map((k) => (
                      <Menu.Item key={k} onClick={() => void copyText(rowStatements(ed.target, cols, [panelRow], k), `the row as ${k === 'upsert' ? 'an upsert' : k.toUpperCase()}`)}>
                        {k === 'upsert' ? 'Upsert' : k.toUpperCase()}
                      </Menu.Item>
                    ))}
                  </Menu.Dropdown>
                </Menu>
              )}
            </Group>
            {deleted.has(panelIndex) && (
              <Text size={FS.micro} c={INK.denied} mb="sm">
                This row is deleted when you save. Where the database keeps a trash, the row is kept for 30 days and Undo puts it back.
              </Text>
            )}
            {live && panel.editing
              ? <RowForm fields={rowFields(panelIndex)}
                  onChange={(j, v) => setCell(panelIndex, j, v)}
                  onOpen={(j) => setOpen({ row: panelIndex, col: j })} />
              : <Fields cols={cols} row={panelRow} fks={fks} onFollow={(i) => follow(panelRow, i, fks, panel)} cut={(i) => Boolean(result.cut?.has(`${panelIndex}:${i}`))} />}
          </>
        )}
        {panel?.kind === 'new' && live && added[panel.index] && (
          <Stack gap="md">
            <Text size={FS.micro} c="dimmed">
              A column left to Postgres gets its default, or NULL. Nothing is sent until you Save.
            </Text>
            <RowForm fields={newFields(panel.index)}
              onChange={(a, v) => setAddedValue(panel.index, a, v)}
              onUnset={(a) => setAddedValue(panel.index, a, undefined)}
              onOpen={(a) => setOpen({ added: panel.index, attnum: a })} />
            <Group justify="space-between">
              <Button size="xs" variant="subtle" color="rose" leftSection={<IconTrash size={12} />}
                onClick={() => { setAdded((a) => a.filter((_, i) => i !== panel.index)); setPanel(null) }}>
                Remove this new row
              </Button>
              <Button size="xs" onClick={() => setPanel(null)}>Done</Button>
            </Group>
          </Stack>
        )}
        {panel?.kind === 'linked' && (
          panel.loading ? <Group gap="xs"><Loader size="xs" /><Text size={FS.meta} c="dimmed">Reading the linked row…</Text></Group>
            : panel.error ? <Text size={FS.meta} c={INK.denied}>Postgres refused this: {panel.error}</Text>
              : !panel.row ? <Text size={FS.meta} c="dimmed">No row matches. It may have been deleted, or your role may not see it.</Text>
                : <Fields cols={panel.cols} row={panel.row} fks={{}} />
        )}
      </Drawer>
    </>
  )
}
