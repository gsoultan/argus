import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  ActionIcon, Badge, Box, Button, CloseButton, Drawer, Group, Loader, Menu, ScrollArea, Stack, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import {
  IconArrowBackUp, IconArrowDown, IconArrowUp, IconChevronDown, IconChevronLeft, IconChevronRight, IconCopy, IconEyeOff,
  IconFilter, IconLink, IconSortAscending, IconSortDescending, IconWand, IconX,
} from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { notifyOk, notifyWarn } from '~/lib/notify'
import {
  addToQuery, columnStats, copyAs, emptyView, isJSONType, isNumeric, pretty, singleTable, viewRows,
  type Cell, type Col, type CopyFormat, type Filter, type ViewState,
} from '~/lib/resultView'
import type { Collected } from '~/components/DatabaseAdmin'

/*
The query tab's results: a windowed grid of the rows the statement sent,
with a menu on each column (sort, filter, hide, copy, quick figures), Copy
as, and a row panel with every field in full. A value that is a foreign key
opens the row it points at, read with the person's role.

Sorting and filtering work on the loaded rows only, and say so; "Add to the
query" writes the same thing as SQL around the statement, in a new tab, for
the database to do over every row.
*/

export interface GridResult {
  id: number
  sql: string
  cols: Col[]
  rows: Cell[][]
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

/** A short preview of a JSON value for one grid cell. */
function jsonPreview(v: string): ReactNode {
  let j: unknown
  try { j = JSON.parse(v) } catch { return v }
  if (Array.isArray(j)) {
    return (
      <Group gap={4} wrap="nowrap" component="span">
        <Text span size={FS.micro} c="dimmed">[{j.length}]</Text>
        {j.slice(0, 4).map((x, i) => (
          <Badge key={i} size="xs" variant="light" color="gray" radius="sm" tt="none" style={{ fontFamily: 'var(--font-mono)', fontWeight: 500 }}>
            {typeof x === 'object' && x !== null ? (Array.isArray(x) ? `[${x.length}]` : '{…}') : String(x).slice(0, 24)}
          </Badge>
        ))}
        {j.length > 4 && <Text span size={FS.micro} c="dimmed">…</Text>}
      </Group>
    )
  }
  if (j && typeof j === 'object') {
    const keys = Object.keys(j)
    return <span>{'{'}{keys.slice(0, 3).join(', ')}{keys.length > 3 ? ', …' : ''}{'}'}</span>
  }
  return v
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
  | { kind: 'row'; at: number }
  | { kind: 'linked'; title: string; cols: Col[]; row: Cell[] | null; loading: boolean; error?: string; back: Panel }

function Fields({ cols, row, fks, onFollow }: { cols: Col[]; row: Cell[]; fks: Record<number, ForeignKey>; onFollow?: (i: number) => void }) {
  return (
    <Stack gap="sm">
      {cols.map((c, i) => {
        const v = row[i] ?? null
        const p = pretty(c, v)
        const fk = fks[i]
        return (
          <Box key={i}>
            <Group gap={6} wrap="nowrap">
              <Text size={FS.meta} fw={600} ff="var(--font-mono)">{c.name}</Text>
              <Text size={FS.micro} c="dimmed">{c.type}</Text>
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
            <Box component="pre" m={0} mt={2} p={6}
              style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'pre-wrap', wordBreak: 'break-word', borderRadius: 6,
                background: 'var(--color-raised)', color: v === null ? 'var(--mantine-color-dimmed)' : undefined, fontStyle: v === null ? 'italic' : undefined,
                maxHeight: p.json ? 320 : undefined, overflow: 'auto' }}>
              {p.text}
            </Box>
          </Box>
        )
      })}
    </Stack>
  )
}

export function ResultsGrid({ result, narrow, collect, onAddToQuery }: {
  result: GridResult | null
  narrow: boolean
  collect?: GridCollect
  /** Opens the statement with the grid's sort and filters as SQL. */
  onAddToQuery?: (sql: string, params: Record<string, string>) => void
}) {
  const viewport = useRef<HTMLDivElement | null>(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [height, setHeight] = useState(400)
  const [view, setViewState] = useState<ViewState>(emptyView)
  const [panel, setPanel] = useState<Panel | null>(null)
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

  // Windowed rows: only what is visible plus a margin is in the DOM.
  const total = order.length
  const first = Math.max(0, Math.floor(scrollTop / ROW_H) - 10)
  const last = Math.min(total, Math.ceil((scrollTop + height) / ROW_H) + 10)
  const slice = order.slice(first, last)

  const panelRow = panel?.kind === 'row' ? result.rows[order[panel.at] ?? -1] : undefined

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
        {!result.error && result.rows.length > 0 && (
          <Menu position="bottom-end" withinPortal>
            <Menu.Target>
              <Button size="compact-xs" variant="subtle" ml="auto" rightSection={<IconChevronDown size={12} />} leftSection={<IconCopy size={12} />}>
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
            </Menu.Dropdown>
          </Menu>
        )}
      </Group>
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
        <div
          ref={viewport}
          onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
          style={{ flex: 1, overflow: 'auto', fontFamily: 'var(--font-mono)', fontSize: 12, position: 'relative' }}
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
                      const cell = row[j] ?? null
                      const c = cols[j]!
                      const fk = fks[j]
                      return (
                        <td key={j} style={{ ...td, color: cell === null ? 'var(--mantine-color-dimmed)' : undefined, fontStyle: cell === null ? 'italic' : undefined,
                          textAlign: isNumeric(c) ? 'right' : undefined }} title={cell ?? 'NULL'}>
                          {cell === null ? 'null'
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
      )}

      <Drawer opened={panel !== null} onClose={() => setPanel(null)} position={narrow ? 'bottom' : 'right'} size={narrow ? '85%' : 460}
        withinPortal title={
          panel?.kind === 'linked'
            ? <Group gap={6} wrap="nowrap">
                <ActionIcon variant="subtle" size="sm" aria-label="Back" onClick={() => setPanel(panel.back)}><IconArrowBackUp size={14} /></ActionIcon>
                <Text size={FS.body} fw={600} truncate>{panel.title}</Text>
              </Group>
            : panel?.kind === 'row'
              ? <Group gap={6} wrap="nowrap">
                  <Text size={FS.body} fw={600}>Row {(order[panel.at] ?? 0) + 1}</Text>
                  <Text size={FS.micro} c="dimmed">{panel.at + 1} of {order.length.toLocaleString()}</Text>
                  <ActionIcon variant="subtle" size="sm" aria-label="Previous row" disabled={panel.at === 0} onClick={() => setPanel({ kind: 'row', at: panel.at - 1 })}>
                    <IconChevronLeft size={14} />
                  </ActionIcon>
                  <ActionIcon variant="subtle" size="sm" aria-label="Next row" disabled={panel.at >= order.length - 1} onClick={() => setPanel({ kind: 'row', at: panel.at + 1 })}>
                    <IconChevronRight size={14} />
                  </ActionIcon>
                </Group>
              : null
        }
        scrollAreaComponent={ScrollArea.Autosize}>
        {panel?.kind === 'row' && panelRow && (
          <>
            <Group justify="flex-end" mb="sm">
              <Button size="compact-xs" variant="default" leftSection={<IconCopy size={12} />}
                onClick={() => void copyText(copyAs('json', cols, [panelRow]).replace(/^\[\n|\n\]$/g, ''), 'the row as JSON')}>
                Copy as JSON
              </Button>
            </Group>
            <Fields cols={cols} row={panelRow} fks={fks} onFollow={(i) => follow(panelRow, i, fks, panel)} />
          </>
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
