import { useMemo, useState } from 'react'
import { Alert, Box, Button, Group, ScrollArea, SegmentedControl, Select, Stack, Table, Text } from '@mantine/core'
import { IconPlayerPlay } from '@tabler/icons-react'
import { FS } from '~/theme'
import type { Cell, Col } from '~/lib/resultView'
import {
  isDateCol, isJSONCol, isNumericCol, jsonKeys, summarize, summaryQuery, type Bucket, type Measure, type SummaryRow, type SummarySpec,
} from '~/lib/summarize'

/*
Summarize and chart a result: group the loaded rows by a column, a key in a
JSON column, or a date's day, month or year; count them or add up a number;
see it as bars, a line, or a table. "Summarize every row" writes the same
GROUP BY for the database, over the whole statement rather than the rows
loaded.
*/

const fmt = (n: number) => (Number.isInteger(n) ? n.toLocaleString() : n.toLocaleString(undefined, { maximumFractionDigits: 2 }))

/** Horizontal bars: a label per group, longest first. One series, so no legend. */
function Bars({ data, title }: { data: SummaryRow[]; title: string }) {
  const [hover, setHover] = useState<number | null>(null)
  const max = Math.max(1, ...data.map((d) => d.value))
  const row = 22, gap = 2, labelW = 140, valueW = 70, w = 640
  const plotW = w - labelW - valueW
  const h = data.length * (row + gap)
  return (
    <Box style={{ position: 'relative' }}>
      <svg role="img" aria-label={title} viewBox={`0 0 ${w} ${h}`} width="100%" style={{ maxWidth: w, display: 'block', overflow: 'visible' }}>
        {data.map((d, i) => {
          const y = i * (row + gap)
          const bw = Math.max(d.value > 0 ? 2 : 0, (Math.max(0, d.value) / max) * plotW)
          return (
            <g key={d.label} onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(null)}>
              <rect x={0} y={y} width={w} height={row} fill="transparent" />
              <text x={labelW - 8} y={y + row / 2} dy="0.35em" textAnchor="end" fontSize={11} fill="var(--mantine-color-text)">
                {d.label.length > 22 ? `${d.label.slice(0, 21)}…` : d.label}
              </text>
              <path d={`M${labelW},${y + 3} h${Math.max(0, bw - 4)} a4,4 0 0 1 4,4 v${row - 14} a4,4 0 0 1 -4,4 h${-Math.max(0, bw - 4)} z`}
                fill="var(--color-brand)" opacity={hover === null || hover === i ? 1 : 0.55} />
              <text x={labelW + bw + 6} y={y + row / 2} dy="0.35em" fontSize={11} fill="var(--mantine-color-dimmed)">{fmt(d.value)}</text>
            </g>
          )
        })}
      </svg>
      {hover !== null && data[hover] && (
        <Box role="tooltip" px={8} py={4} style={{ position: 'absolute', top: hover * (row + gap) - 30, left: labelW, background: 'var(--mantine-color-body)', border: '1px solid var(--color-line)', borderRadius: 6, pointerEvents: 'none', fontSize: 12, whiteSpace: 'nowrap' }}>
          <b>{data[hover].label}</b>: {fmt(data[hover].value)} · {data[hover].rows.toLocaleString()} rows
        </Box>
      )}
    </Box>
  )
}

/** A line over ordered groups (dates, numbers), with a crosshair on hover. */
function Line({ data, title }: { data: SummaryRow[]; title: string }) {
  const [hover, setHover] = useState<number | null>(null)
  const w = 640, h = 220, left = 56, bottom = 24, top = 10, right = 12
  const max = Math.max(...data.map((d) => d.value), 0)
  const min = Math.min(...data.map((d) => d.value), 0)
  const x = (i: number) => left + (data.length === 1 ? (w - left - right) / 2 : (i / (data.length - 1)) * (w - left - right))
  const y = (v: number) => top + (1 - (v - min) / Math.max(1e-9, max - min)) * (h - top - bottom)
  const path = data.map((d, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(d.value).toFixed(1)}`).join(' ')
  const ticks = [min, (min + max) / 2, max]
  const every = Math.max(1, Math.ceil(data.length / 6))
  return (
    <Box style={{ position: 'relative' }}>
      <svg role="img" aria-label={title} viewBox={`0 0 ${w} ${h}`} width="100%" style={{ maxWidth: w, display: 'block' }}
        onMouseLeave={() => setHover(null)}
        onMouseMove={(e) => {
          const r = (e.currentTarget as SVGSVGElement).getBoundingClientRect()
          const px = ((e.clientX - r.left) / r.width) * w
          let best = 0
          for (let i = 1; i < data.length; i++) if (Math.abs(x(i) - px) < Math.abs(x(best) - px)) best = i
          setHover(best)
        }}>
        {ticks.map((t, i) => (
          <g key={i}>
            <line x1={left} x2={w - right} y1={y(t)} y2={y(t)} stroke="var(--color-line)" strokeWidth={1} />
            <text x={left - 6} y={y(t)} dy="0.35em" textAnchor="end" fontSize={10} fill="var(--mantine-color-dimmed)">{fmt(t)}</text>
          </g>
        ))}
        {data.map((d, i) => (i % every === 0 || i === data.length - 1) && (
          <text key={d.label} x={x(i)} y={h - 6} textAnchor="middle" fontSize={10} fill="var(--mantine-color-dimmed)">{d.label}</text>
        ))}
        <path d={path} fill="none" stroke="var(--color-brand)" strokeWidth={2} strokeLinejoin="round" />
        {hover !== null && data[hover] && (
          <>
            <line x1={x(hover)} x2={x(hover)} y1={top} y2={h - bottom} stroke="var(--mantine-color-dimmed)" strokeWidth={1} strokeDasharray="3 3" />
            <circle cx={x(hover)} cy={y(data[hover].value)} r={4.5} fill="var(--color-brand)" stroke="var(--mantine-color-body)" strokeWidth={2} />
          </>
        )}
      </svg>
      {hover !== null && data[hover] && (
        <Box role="tooltip" px={8} py={4} style={{ position: 'absolute', top: 0, left: `${(x(hover) / w) * 100}%`, transform: 'translateX(-50%)', background: 'var(--mantine-color-body)', border: '1px solid var(--color-line)', borderRadius: 6, pointerEvents: 'none', fontSize: 12, whiteSpace: 'nowrap' }}>
          <b>{data[hover].label}</b>: {fmt(data[hover].value)}
        </Box>
      )}
    </Box>
  )
}

export function ResultSummary({ cols, rows, sql, truncated, onRunSQL }: {
  cols: Col[]
  rows: Cell[][]
  sql: string
  /** The rows loaded are not all the statement returns. */
  truncated: boolean
  onRunSQL?: (sql: string) => void
}) {
  // What to group by: each column, and each key found in a JSON column.
  const groupOptions = useMemo(() => {
    const out: { value: string; label: string }[] = []
    cols.forEach((c, i) => {
      out.push({ value: `${i}`, label: c.name })
      if (isJSONCol(c)) for (const k of jsonKeys(rows, i).slice(0, 50)) out.push({ value: `${i}:${k}`, label: `${c.name} → ${k}` })
    })
    return out
  }, [cols, rows])
  const numeric = cols.map((c, i) => ({ c, i })).filter((x) => isNumericCol(x.c))
  const firstText = cols.findIndex((c) => !isNumericCol(c))
  const [by, setBy] = useState<string>(`${firstText >= 0 ? firstText : 0}`)
  const [measure, setMeasure] = useState<Measure>('count')
  const [of, setOf] = useState<string | null>(numeric[0] ? `${numeric[0].i}` : null)
  const [bucket, setBucket] = useState<Bucket>('value')
  const [view, setView] = useState<'chart' | 'table'>('chart')

  const [byCol, key] = (() => { const [a, ...k] = by.split(':'); return [Number(a), k.join(':') || undefined] as const })()
  const col = cols[byCol]
  const dateBy = Boolean(col && isDateCol(col) && !key)
  const spec: SummarySpec = { by: byCol, key, bucket: dateBy ? bucket : 'value', measure, of: measure === 'count' || of === null ? undefined : Number(of) }
  const data = useMemo(() => (col ? summarize(cols, rows, spec) : []), [cols, rows, col, spec.by, spec.key, spec.bucket, spec.measure, spec.of]) // eslint-disable-line react-hooks/exhaustive-deps
  const ordered = Boolean(col && !key && (isDateCol(col) || isNumericCol(col)))
  const valueName = measure === 'count' ? 'Rows' : `${measure} of ${cols[Number(of)]?.name ?? ''}`
  const title = `${valueName} by ${key ? `${col?.name} → ${key}` : col?.name}${dateBy && bucket !== 'value' ? `, per ${bucket}` : ''}`

  if (cols.length === 0 || rows.length === 0) return <Text size={FS.meta} c="dimmed" p="md">Nothing to summarize.</Text>
  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Stack gap="sm" p="md">
        <Group gap="xs" wrap="wrap" align="flex-end">
          <Select size="xs" label="Group by" data={groupOptions} value={by} onChange={(v) => v && setBy(v)} searchable allowDeselect={false} w={200} comboboxProps={{ withinPortal: true }} />
          {dateBy && (
            <Select size="xs" label="Per" value={bucket} onChange={(v) => v && setBucket(v as Bucket)} allowDeselect={false} w={110}
              data={[{ value: 'value', label: 'Each value' }, { value: 'day', label: 'Day' }, { value: 'month', label: 'Month' }, { value: 'year', label: 'Year' }]} />
          )}
          <Select size="xs" label="Show" value={measure} onChange={(v) => v && setMeasure(v as Measure)} allowDeselect={false} w={130}
            data={[{ value: 'count', label: 'Count of rows' }, ...(numeric.length ? [
              { value: 'sum', label: 'Sum' }, { value: 'avg', label: 'Average' }, { value: 'min', label: 'Lowest' }, { value: 'max', label: 'Highest' }] : [])]} />
          {measure !== 'count' && (
            <Select size="xs" label="Of" value={of} onChange={setOf} allowDeselect={false} w={160} data={numeric.map((x) => ({ value: `${x.i}`, label: x.c.name }))} />
          )}
          <SegmentedControl size="xs" value={view} onChange={(v) => setView(v as 'chart' | 'table')} data={[{ value: 'chart', label: 'Chart' }, { value: 'table', label: 'Table' }]} />
        </Group>
        {truncated && (
          <Alert color="amber" variant="light" py={6}>
            This summarizes the {rows.length.toLocaleString()} rows loaded, not every row the statement returns.
          </Alert>
        )}
        <Text size={FS.body} fw={600}>{title}</Text>
        {view === 'chart'
          ? ordered && data.length > 1 ? <Line data={data} title={title} /> : <Bars data={data} title={title} />
          : (
            <Table striped fz={FS.meta} verticalSpacing={4} maw={560} aria-label="Summary">
              <Table.Thead><Table.Tr><Table.Th>{key ?? col?.name}</Table.Th><Table.Th ta="right">{valueName}</Table.Th><Table.Th ta="right">Rows</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {data.map((d) => <Table.Tr key={d.label}><Table.Td ff="var(--font-mono)">{d.label}</Table.Td><Table.Td ta="right">{fmt(d.value)}</Table.Td><Table.Td ta="right">{d.rows.toLocaleString()}</Table.Td></Table.Tr>)}
              </Table.Tbody>
            </Table>
          )}
        {onRunSQL && (
          <Group>
            <Button size="xs" variant="light" leftSection={<IconPlayerPlay size={13} />} onClick={() => onRunSQL(summaryQuery(sql, cols, spec))}>
              Summarize every row in the database
            </Button>
            <Text size={FS.micro} c="dimmed">Opens the same GROUP BY in a new tab, over the whole statement.</Text>
          </Group>
        )}
      </Stack>
    </ScrollArea>
  )
}
