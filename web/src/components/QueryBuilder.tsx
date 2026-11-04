import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  ActionIcon, Alert, Box, Button, Checkbox, Code, Divider, Group, Menu, Modal, MultiSelect, NumberInput, ScrollArea, Select, Stack, Switch, Text,
} from '@mantine/core'
import { IconPlus, IconX } from '@tabler/icons-react'
import { FS } from '~/theme'
import type { Collect } from '~/components/DatabaseAdmin'
import {
  COLUMNS_SQL, FKS_SQL, TABLES_SQL, buildSQL, isJSON, jsonKeysSQL, nextAlias, paramFor, parseFks, refLabel, tableName,
  type Aggregate, type BColumn, type BQuery, type BRef, type BTable, type FilterOp, type ForeignKeyRef,
} from '~/lib/queryBuilder'

/*
The visual query builder. Pick a table, join others along their foreign
keys, tick the columns (or keys inside a JSON column) to show, add filters,
summarize, sort; the SELECT it writes opens in a new tab with a field for
each filter's value. It only reads the catalogue to offer the choices.
*/

interface TableInfo { columns: BColumn[]; fks: ForeignKeyRef[]; keys: Record<string, string[]> }

const OPS: { value: FilterOp; label: string }[] = [
  { value: '=', label: 'is' }, { value: '<>', label: 'is not' }, { value: '<', label: 'less than' }, { value: '<=', label: 'at most' },
  { value: '>', label: 'more than' }, { value: '>=', label: 'at least' }, { value: 'contains', label: 'contains' },
  { value: 'starts with', label: 'starts with' }, { value: 'is null', label: 'is empty' }, { value: 'is not null', label: 'is not empty' },
]
const AGGS: { value: Aggregate; label: string }[] = [
  { value: 'count', label: 'Count' }, { value: 'count distinct', label: 'Count distinct' }, { value: 'sum', label: 'Sum' },
  { value: 'avg', label: 'Average' }, { value: 'min', label: 'Lowest' }, { value: 'max', label: 'Highest' },
]

const tkey = (t: BTable) => `${t.schema}.${t.name}`
const enc = (r: BRef) => `${r.alias}|${r.column}|${r.key ?? ''}`
const dec = (s: string): BRef => { const [alias, column, key] = s.split('|'); return { alias: alias!, column: column!, key: key || undefined } }

export function QueryBuilder({ collect, start, narrow, onClose, onOpen }: {
  collect: Collect
  start?: BTable
  narrow: boolean
  onClose: () => void
  onOpen: (sql: string, name: string) => void
}) {
  const [tables, setTables] = useState<BTable[] | null>(null)
  const [info, setInfo] = useState<Record<string, TableInfo>>({})
  const [error, setError] = useState<string | null>(null)
  const [q, setQ] = useState<BQuery | null>(null)

  useEffect(() => {
    void collect(TABLES_SQL, 2000).then((r) => {
      if (r.error) setError(r.error.error ?? 'Postgres refused it')
      else setTables(r.rows.map((x) => ({ schema: x[0] ?? '', name: x[1] ?? '' })))
    })
  }, [collect])

  const load = useCallback(async (t: BTable) => {
    if (info[tkey(t)]) return
    const [c, f] = await Promise.all([collect(COLUMNS_SQL, 2000, [tableName(t)]), collect(FKS_SQL, 500, [tableName(t)])])
    if (c.error) { setError(c.error.error ?? 'Postgres refused it'); return }
    setInfo((m) => ({ ...m, [tkey(t)]: { columns: c.rows.map((x) => ({ name: x[0] ?? '', type: x[1] ?? '' })), fks: f.error ? [] : parseFks(f.rows), keys: {} } }))
  }, [collect, info])

  const pickBase = useCallback((t: BTable) => {
    setQ({ base: t, joins: [], columns: [], measures: [], filters: [], sort: [], limit: 100 })
    void load(t)
  }, [load])
  useEffect(() => { if (start) pickBase(start) }, [start]) // eslint-disable-line react-hooks/exhaustive-deps

  // Each alias in the query and its table.
  const aliases = useMemo(() => (q ? [{ alias: 't0', table: q.base }, ...q.joins.map((j) => ({ alias: j.alias, table: j.forward ? j.fk.to : j.fk.from }))] : []), [q])

  const loadKeys = async (t: BTable, column: string) => {
    const r = await collect(jsonKeysSQL(t, column), 200)
    if (r.error) { setError(r.error.error ?? 'Postgres refused it'); return }
    setInfo((m) => ({ ...m, [tkey(t)]: { ...m[tkey(t)]!, keys: { ...m[tkey(t)]!.keys, [column]: r.rows.map((x) => x[0] ?? '').filter(Boolean) } } }))
  }

  // Every ref the query could use: each column of each table, and loaded JSON keys.
  const refOptions = useMemo(() => aliases.flatMap(({ alias, table }) => {
    const ti = info[tkey(table)]
    if (!ti) return []
    const pre = aliases.length > 1 ? `${alias} ` : ''
    return ti.columns.flatMap((c) => [
      { value: enc({ alias, column: c.name }), label: `${pre}${c.name}` },
      ...(ti.keys[c.name] ?? []).map((k) => ({ value: enc({ alias, column: c.name, key: k }), label: `${pre}${c.name} → ${k}` })),
    ])
  }), [aliases, info])

  const set = (f: (q: BQuery) => BQuery) => setQ((x) => (x ? f(x) : x))
  const sql = q ? buildSQL(q) : ''
  const outputs = q ? [...q.columns.map(refLabel), ...q.measures.map((m) => `${m.fn}${m.ref ? ` of ${refLabel(m.ref)}` : ''}`)] : []

  return (
    <Modal opened onClose={onClose} size="xl" fullScreen={narrow} centered title={<Text fw={650}>Build a query</Text>}>
      <Stack gap="sm">
        {error && <Alert color="rose" variant="light" withCloseButton onClose={() => setError(null)}>{error}</Alert>}
        <Select label="From" placeholder={tables ? 'Pick a table or view' : 'Reading the tables…'} searchable comboboxProps={{ withinPortal: true }}
          data={(tables ?? []).map((t) => ({ value: tkey(t), label: tkey(t) }))} value={q ? tkey(q.base) : null}
          onChange={(v) => { const t = tables?.find((x) => tkey(x) === v); if (t) pickBase(t) }} />
        {q && (
          <>
            {/* Joins along foreign keys, from any table already in the query. */}
            {q.joins.map((j, i) => (
              <Group key={j.alias} gap="xs" wrap="nowrap">
                <Text size={FS.meta} style={{ flex: 1 }}>
                  <b>{j.kind === 'left' ? 'With, if any,' : 'Joined to'}</b> {tkey(j.forward ? j.fk.to : j.fk.from)} as {j.alias}, on {j.on}.{(j.forward ? j.fk.fromCols : j.fk.toCols).join(', ')}
                </Text>
                <Switch size="xs" label="Keep rows without a match" checked={j.kind === 'left'}
                  onChange={(e) => { const kind = e.currentTarget.checked ? 'left' as const : 'inner' as const; set((x) => ({ ...x, joins: x.joins.map((y, k) => (k === i ? { ...y, kind } : y)) })) }} />
                {i === q.joins.length - 1 && (
                  <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Remove the join to ${j.alias}`}
                    onClick={() => set((x) => ({ ...x, joins: x.joins.slice(0, -1), columns: x.columns.filter((c) => c.alias !== j.alias), filters: x.filters.filter((f) => f.ref.alias !== j.alias), measures: x.measures.filter((m) => m.ref?.alias !== j.alias) }))}>
                    <IconX size={13} />
                  </ActionIcon>
                )}
              </Group>
            ))}
            <Menu withinPortal position="bottom-start">
              <Menu.Target><Button size="xs" variant="subtle" leftSection={<IconPlus size={13} />} style={{ alignSelf: 'flex-start' }}>Join a related table</Button></Menu.Target>
              <Menu.Dropdown>
                {aliases.flatMap(({ alias, table }) => (info[tkey(table)]?.fks ?? []).map((fk) => {
                  const forward = tkey(fk.from) === tkey(table)
                  const other = forward ? fk.to : fk.from
                  return (
                    <Menu.Item key={`${alias}-${fk.name}-${forward}`} onClick={() => {
                      set((x) => ({ ...x, joins: [...x.joins, { on: alias, fk, forward, kind: forward ? 'inner' : 'left', alias: nextAlias(x) }] }))
                      void load(other)
                    }}>
                      {tkey(other)} <Text span size={FS.micro} c="dimmed">({forward ? `${alias}.${fk.fromCols.join(', ')} points to it` : `it points to ${alias}`})</Text>
                    </Menu.Item>
                  )
                }))}
                {aliases.every(({ table }) => !(info[tkey(table)]?.fks.length)) && <Menu.Item disabled>No foreign keys to follow</Menu.Item>}
              </Menu.Dropdown>
            </Menu>

            <Divider label="Show" labelPosition="left" />
            {aliases.map(({ alias, table }) => {
              const ti = info[tkey(table)]
              if (!ti) return <Text key={alias} size={FS.micro} c="dimmed">Reading {tkey(table)}…</Text>
              const jsonCols = ti.columns.filter((c) => isJSON(c.type))
              return (
                <Box key={alias}>
                  <MultiSelect size="xs" label={aliases.length > 1 ? `${tkey(table)} (${alias})` : 'Columns'} placeholder="Every column" searchable clearable
                    comboboxProps={{ withinPortal: true }}
                    data={refOptions.filter((o) => o.value.startsWith(`${alias}|`))}
                    value={q.columns.filter((c) => c.alias === alias).map(enc)}
                    onChange={(vs) => set((x) => ({ ...x, columns: [...x.columns.filter((c) => c.alias !== alias), ...vs.map(dec)] }))} />
                  {jsonCols.length > 0 && (
                    <Group gap={4} mt={4}>
                      <Text size={FS.micro} c="dimmed">JSON keys:</Text>
                      {jsonCols.map((c) => (
                        <Button key={c.name} size="compact-xs" variant="light" disabled={Boolean(ti.keys[c.name])} onClick={() => void loadKeys(table, c.name)}>
                          {ti.keys[c.name] ? `${c.name}: ${ti.keys[c.name]!.length} keys` : `Find keys in ${c.name}`}
                        </Button>
                      ))}
                    </Group>
                  )}
                </Box>
              )
            })}

            <Divider label="Only rows where" labelPosition="left" />
            {q.filters.map((f, i) => (
              <Group key={i} gap="xs" wrap={narrow ? 'wrap' : 'nowrap'}>
                <Select size="xs" data={refOptions} value={enc(f.ref)} searchable w={narrow ? '100%' : 220} comboboxProps={{ withinPortal: true }} aria-label={`Filter ${i + 1} column`}
                  onChange={(v) => v && set((x) => ({ ...x, filters: x.filters.map((y, k) => (k === i ? { ...y, ref: dec(v), param: paramFor({ ...x, filters: x.filters.filter((_, z) => z !== i) }, dec(v)) } : y)) }))} />
                <Select size="xs" data={OPS} value={f.op} w={130} allowDeselect={false} comboboxProps={{ withinPortal: true }} aria-label={`Filter ${i + 1} test`}
                  onChange={(v) => v && set((x) => ({ ...x, filters: x.filters.map((y, k) => (k === i ? { ...y, op: v as FilterOp } : y)) }))} />
                {f.op !== 'is null' && f.op !== 'is not null' && <Text size={FS.micro} c="dimmed" ff="monospace">:{f.param}</Text>}
                <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Remove filter ${i + 1}`} onClick={() => set((x) => ({ ...x, filters: x.filters.filter((_, k) => k !== i) }))}><IconX size={13} /></ActionIcon>
              </Group>
            ))}
            <Button size="xs" variant="subtle" leftSection={<IconPlus size={13} />} style={{ alignSelf: 'flex-start' }} disabled={refOptions.length === 0}
              onClick={() => set((x) => { const ref = dec(refOptions[0]!.value); return { ...x, filters: [...x.filters, { ref, op: '=', param: paramFor(x, ref) }] } })}>
              Add a filter
            </Button>
            {q.filters.some((f) => f.op !== 'is null' && f.op !== 'is not null') && (
              <Text size={FS.micro} c="dimmed">Each value is a field under the editor once the query opens, sent as a parameter.</Text>
            )}

            <Divider label="Summarize" labelPosition="left" />
            {q.measures.map((m, i) => (
              <Group key={i} gap="xs" wrap="nowrap">
                <Select size="xs" data={AGGS} value={m.fn} w={150} allowDeselect={false} comboboxProps={{ withinPortal: true }} aria-label={`Summary ${i + 1}`}
                  onChange={(v) => v && set((x) => ({ ...x, measures: x.measures.map((y, k) => (k === i ? { ...y, fn: v as Aggregate } : y)) }))} />
                <Select size="xs" data={refOptions} value={m.ref ? enc(m.ref) : null} placeholder={m.fn === 'count' ? 'rows' : 'of…'} clearable={m.fn === 'count'} searchable
                  w={narrow ? 140 : 220} comboboxProps={{ withinPortal: true }} aria-label={`Summary ${i + 1} of`}
                  onChange={(v) => set((x) => ({ ...x, measures: x.measures.map((y, k) => (k === i ? { ...y, ref: v ? dec(v) : undefined } : y)) }))} />
                <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Remove summary ${i + 1}`} onClick={() => set((x) => ({ ...x, measures: x.measures.filter((_, k) => k !== i), sort: [] }))}><IconX size={13} /></ActionIcon>
              </Group>
            ))}
            <Button size="xs" variant="subtle" leftSection={<IconPlus size={13} />} style={{ alignSelf: 'flex-start' }}
              onClick={() => set((x) => ({ ...x, measures: [...x.measures, { fn: 'count' }], sort: [] }))}>
              Add a count or total
            </Button>
            {q.measures.length > 0 && <Text size={FS.micro} c="dimmed">With a summary, the rows are grouped by the columns shown.</Text>}

            <Group gap="xs" align="flex-end">
              <Select size="xs" label="Sort by" data={outputs.map((o, i) => ({ value: `${i}`, label: o }))} value={q.sort[0] ? `${q.sort[0].by}` : null}
                clearable w={200} comboboxProps={{ withinPortal: true }} placeholder={outputs.length ? 'None' : 'Pick columns first'}
                onChange={(v) => set((x) => ({ ...x, sort: v === null ? [] : [{ by: Number(v), desc: x.sort[0]?.desc ?? false }] }))} />
              <Checkbox size="xs" label="Largest first" checked={q.sort[0]?.desc ?? false} disabled={!q.sort[0]} mb={6}
                onChange={(e) => { const desc = e.currentTarget.checked; set((x) => ({ ...x, sort: x.sort.map((s) => ({ ...s, desc })) })) }} />
              <NumberInput size="xs" label="At most" value={q.limit} min={0} max={100000} w={110} onChange={(v) => set((x) => ({ ...x, limit: Number(v) || 0 }))} />
            </Group>

            <ScrollArea.Autosize mah={200} type="auto">
              <Code block style={{ whiteSpace: 'pre-wrap', fontSize: 12 }} aria-label="The query">{sql}</Code>
            </ScrollArea.Autosize>
          </>
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!q} onClick={() => q && onOpen(sql, `query ${q.base.name}`)}>Open in a new tab</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
