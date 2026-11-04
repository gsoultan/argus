import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  ActionIcon, Badge, Box, Button, CloseButton, Drawer, Group, Loader, Menu, ScrollArea, Stack, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import {
  IconArrowBackUp, IconArrowDown, IconArrowUp, IconChartBar, IconChevronDown, IconChevronLeft, IconChevronRight, IconCopy, IconEyeOff,
  IconFilter, IconLink, IconPencil, IconPlus, IconSortAscending, IconSortDescending, IconTrash, IconWand, IconX,
} from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { itemLabel, summary, type J } from '~/lib/jsonSummary'
import { notifyOk, notifyWarn } from '~/lib/notify'
import {
  addToQuery, columnStats, copyAs, emptyView, isJSONType, isNumeric, pretty, singleTable, viewRows,
  type Cell, type Col, type CopyFormat, type Filter, type ViewState,
} from '~/lib/resultView'
import type { Collected } from '~/components/DatabaseAdmin'
import { editable, editorKind, insertStmt, updateStmt, type Stmt, type TableColumn } from '~/lib/rowEdit'
import { rowStatements } from '~/lib/sqlGen'
import { CellInput, ChangesBar, ReviewDialog, RowForm, useEditTarget, type EditSupport, type FormField } from '~/components/RowEdits'
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

/** A row's changed cells, by result column. */
type Changes = Map<number, Map<number, Cell>>

/** Where a JSON or array editor is open: a loaded row, or a new one. */
type Open = { row: number; attnum: number } | { added: number; attnum: number }

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
  const target = useEditTarget(cols, canEdit ? collect : undefined, canEdit ? rid : 0)
  const ed = useMemo(() => editable(cols, target), [cols, target])

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

  // Editing. `live` is the editable table, when there is one.
  const live = canEdit && ed.ok ? ed : null
  const tcol = (j: number): TableColumn | undefined => live?.target.columns.find((c) => c.attnum === cols[j]!.attnum)
  const colOf = (attnum: number) => cols.findIndex((c) => c.attnum === attnum)
  const cut = edit?.cut
  const lockReason = (ri: number, j: number): string | undefined => {
    if (!live) return 'read only'
    if (live.pkCols.includes(j)) return 'primary key'
    if (tcol(j)?.generated) return 'generated'
    if (cut?.has(`${ri}:${j}`)) return 'too long to edit here'
    if (live.pkCols.some((p) => cut?.has(`${ri}:${p}`))) return 'its key is too long'
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
  const addRow = () => {
    if (!live) return
    // A column Postgres cannot fill in starts as a value to give.
    const m = new Map<number, Cell>()
    for (const c of live.target.columns) if (c.notNull && !c.hasDefault && !c.generated) m.set(c.attnum, '')
    setAdded((a) => [...a, m])
    setPanel({ kind: 'new', index: added.length })
  }
  const startEdit = (ri: number, j: number) => {
    if (!live || lockReason(ri, j)) return
    const c = tcol(j)
    const kind = editorKind(c?.type ?? cols[j]!.type, c?.enumValues)
    if (kind === 'json' || kind === 'array') setOpen({ row: ri, attnum: cols[j]!.attnum! })
    else setEditing({ row: ri, col: j })
  }
  const pending = (): { list: Stmt[]; rows: (number | null)[] } => {
    const list: Stmt[] = []
    const rows: (number | null)[] = []
    if (!live) return { list, rows }
    for (const ri of [...changes.keys()].sort((a, b) => a - b)) {
      list.push(updateStmt(live.target, cols, live.pkCols, result.rows[ri]!, changes.get(ri)!))
      rows.push(ri)
    }
    for (const a of added) { list.push(insertStmt(live.target, a)); rows.push(null) }
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
    const next = result.rows.slice()
    r.returned.forEach((c, k) => {
      const row = asResultRow(c)
      const ri = rows[k]
      if (!row) return
      if (ri === null || ri === undefined) next.push(row)
      else next[ri] = row
    })
    setChanges(new Map())
    setAdded([])
    setReviewing(false)
    setPanel(null)
    onRows?.(next)
    notifyOk('Saved', `${list.length} ${list.length === 1 ? 'change' : 'changes'} to ${live.target.table}`)
  }
  const changeCount = changes.size + added.length
  const rowFields = (ri: number): FormField[] => cols.flatMap((_, j) => {
    const col = tcol(j)
    if (!col) return []
    return [{ col, value: valueAt(ri, j), original: result.rows[ri]?.[j] ?? null, locked: lockReason(ri, j) }]
  })
  const newFields = (k: number): FormField[] => (live?.target.columns ?? []).map((col) => {
    const m = added[k]
    return { col, value: m?.get(col.attnum) ?? null, unset: !m?.has(col.attnum), locked: col.generated ? 'generated' : undefined }
  })
  const openValue: Cell = open
    ? 'row' in open ? valueAt(open.row, colOf(open.attnum)) : (added[open.added]?.get(open.attnum) ?? null)
    : null
  const openCol = open ? live?.target.columns.find((c) => c.attnum === open.attnum) : undefined
  const applyOpen = (v: Cell) => {
    if (!open) return
    if ('row' in open) setCell(open.row, colOf(open.attnum), v)
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
      {canEdit && cols.length > 0 && (live || ed.ok === false && ed.reason !== 'Reading the table…') && (
        <Group gap="sm" px="md" py={4} wrap="nowrap" aria-label="Editing" style={{ borderBottom: '1px solid var(--color-line)' }}>
          {live
            ? <>
                <IconPencil size={12} color="var(--mantine-color-azure-text)" />
                <Text size={FS.micro} style={{ flex: 1 }} truncate>
                  <Text span fw={600} size={FS.micro}>Editable</Text> · {live.target.table} ·{' '}
                  {narrow ? 'tap a row number, then Edit' : 'double-click a cell to change it'}
                  {edit?.inTransaction ? ' · saves go into your open transaction' : ''}
                </Text>
                <Button size="compact-xs" variant="light" leftSection={<IconPlus size={12} />} onClick={addRow}>Add a row</Button>
              </>
            : <Text size={FS.micro} c="dimmed" truncate>Read only here: {ed.ok ? '' : ed.reason}</Text>}
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
                        <UnstyledButton px={12} py={6} w="100%" aria-label={`Column ${c.name}`}
                          style={{ display: 'flex', alignItems: 'center', gap: 6, fontWeight: 600, fontSize: 12, fontFamily: 'var(--font-mono)' }}>
                          <span>{c.name}</span>
                          {!narrow && <span style={{ fontWeight: 400, color: 'var(--mantine-color-dimmed)' }}>{c.type}</span>}
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
                  <tr key={ri} style={{ height: ROW_H }}>
                    <td style={{ ...td, padding: 0, background: 'var(--color-void)' }}>
                      <UnstyledButton onClick={() => setPanel({ kind: 'row', at })} aria-label={`Open row ${ri + 1}`}
                        style={{ display: 'block', width: '100%', padding: '0 12px', textAlign: 'right', color: 'var(--mantine-color-dimmed)', fontSize: 12, fontFamily: 'inherit' }}>
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
                        <td key={j} style={{ ...td, color: cell === null ? 'var(--mantine-color-dimmed)' : undefined, fontStyle: cell === null ? 'italic' : undefined,
                          textAlign: isNumeric(c) ? 'right' : undefined, background: changed ? 'var(--mantine-color-amber-light)' : undefined,
                          cursor: live && !lockReason(ri, j) ? 'cell' : undefined }}
                          title={changed ? `${cell ?? 'NULL'}\nwas ${row[j] ?? 'NULL'}` : (cell ?? 'NULL')}
                          onDoubleClick={live ? () => startEdit(ri, j) : undefined}>
                          {result.cut?.has(`${ri}:${j}`) && <Badge size="xs" variant="light" color="amber" mr={4} title={CUT_NOTE}>cut</Badge>}
                          {cell === null ? 'null'
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
        <ChangesBar changedRows={changes.size} newRows={added.length} busy={Boolean(edit?.busy)} saving={saving} error={saveError} narrow={narrow}
          onDiscard={() => { setChanges(new Map()); setAdded([]); setSaveError(null); if (panel?.kind === 'new') setPanel(null) }}
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
            ? <Text size={FS.body} fw={600} truncate>New row in {live?.target.table}</Text>
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
            {live && panel.editing
              ? <RowForm fields={rowFields(panelIndex)}
                  onChange={(a, v) => setCell(panelIndex, colOf(a), v)}
                  onOpen={(a) => setOpen({ row: panelIndex, attnum: a })} />
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
