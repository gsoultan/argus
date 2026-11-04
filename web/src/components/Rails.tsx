import { Box, Group, Stack, Text, Tooltip, UnstyledButton } from '@mantine/core'
import type { ReactNode } from 'react'
import { FS } from '~/theme'
import './rails.css'

/**
 * A data page with a filter rail on its left and a summary rail on its right.
 *
 * The filters are drawn from 88em, the summary from 112em; below that neither
 * is drawn and the page's own toolbar, which stays, is the way to filter.
 * Both rails describe the same rows the table shows: a count in a rail that
 * disagreed with the table would be worse than no rail.
 */
export function Rails({ facets, insight, children }: { facets?: ReactNode; insight?: ReactNode; children: ReactNode }) {
  return (
    <Box className="argus-rails" data-facets={facets ? true : undefined} data-insight={insight ? true : undefined}>
      {facets && (
        <Box component="aside" className="argus-rails-facets" aria-label="Filters">
          {facets}
        </Box>
      )}
      <Box className="argus-rails-main">{children}</Box>
      {insight && (
        <Box component="aside" className="argus-rails-insight" aria-label="Summary">
          {insight}
        </Box>
      )}
    </Box>
  )
}

export function Facet({ title, children, action }: { title: string; children: ReactNode; action?: ReactNode }) {
  return (
    <Box>
      <Group justify="space-between" mb={4}>
        <Text size={FS.micro} fw={600} c="dimmed" tt="uppercase" style={{ letterSpacing: '0.05em' }}>{title}</Text>
        {action}
      </Group>
      <Stack gap={1}>{children}</Stack>
    </Box>
  )
}

/** One choice in a facet: pressed when it is the filter in force. */
export function FacetOption({ label, count, active, onClick, tone }: {
  label: ReactNode
  count?: number
  active: boolean
  onClick: () => void
  /** Draws the count in the denied colour: a number someone should look at. */
  tone?: 'warn'
}) {
  return (
    <UnstyledButton className="argus-facet" data-active={active || undefined} aria-pressed={active} onClick={onClick}>
      <span className="argus-facet-box" aria-hidden />
      <Text span size={FS.body} truncate style={{ flex: 1, minWidth: 0 }}>{label}</Text>
      {count !== undefined && (
        <Text span size={FS.meta} c={tone === 'warn' && count > 0 ? 'rose' : 'dimmed'} fw={tone === 'warn' && count > 0 ? 600 : undefined}>
          {count}
        </Text>
      )}
    </UnstyledButton>
  )
}

export function InsightCard({ title, aside, children, tone }: {
  title: string; aside?: string; children: ReactNode; tone?: 'warn'
}) {
  return (
    <Box className="argus-insight" data-tone={tone}>
      <Group justify="space-between" mb="xs" wrap="nowrap">
        <Text size={FS.body} fw={600}>{title}</Text>
        {aside && <Text size={FS.micro} c="dimmed">{aside}</Text>}
      </Group>
      {children}
    </Box>
  )
}

/** Ranked rows with a bar each. A row with `onClick` narrows the table to it. */
export function RankBars({ rows, mono }: { rows: { label: string; n: number; onClick?: () => void }[]; mono?: boolean }) {
  const max = Math.max(1, ...rows.map((r) => r.n))
  if (rows.length === 0) return <Text size={FS.meta} c="dimmed">Nothing in this view.</Text>
  return (
    <Stack gap={6}>
      {rows.map((r) => {
        const inner = (
          <>
            <Text span size={FS.meta} ff={mono ? 'monospace' : undefined} truncate className="argus-rank-label">{r.label}</Text>
            <span className="argus-rank-bar"><i style={{ width: `${(r.n / max) * 100}%` }} /></span>
            <Text span size={FS.meta} fw={600} ta="right">{r.n}</Text>
          </>
        )
        return r.onClick ? (
          <UnstyledButton key={r.label} className="argus-rank" onClick={r.onClick} aria-label={`Only ${r.label}`}>{inner}</UnstyledButton>
        ) : (
          <Box key={r.label} className="argus-rank">{inner}</Box>
        )
      })}
    </Stack>
  )
}

/** Counts per day, oldest first, today lit. */
export function DayBars({ values, labels }: { values: number[]; labels: string[] }) {
  const max = Math.max(1, ...values)
  return (
    <>
      <Box className="argus-daybars" role="img" aria-label={`Per day: ${values.join(', ')}`}>
        {values.map((v, i) => (
          <Tooltip key={i} label={`${labels[i]} · ${v}`}>
            <Box className="argus-daybar" data-now={i === values.length - 1 || undefined}>
              <i style={{ height: `${Math.max(3, (v / max) * 100)}%` }} />
            </Box>
          </Tooltip>
        ))}
      </Box>
      <Group justify="space-between" mt={4}>
        <Text size={FS.micro} c="dimmed">{labels[0]}</Text>
        <Text size={FS.micro} c="dimmed">Today</Text>
      </Group>
    </>
  )
}

const DAY = 86_400_000

/** How many of `times` fall on each of the last `days` local days, oldest first. */
export function perDay(times: string[], end: number, days = 7): { values: number[]; labels: string[] } {
  const today = new Date(end)
  today.setHours(0, 0, 0, 0)
  const start = today.getTime() - (days - 1) * DAY
  const values = new Array<number>(days).fill(0)
  for (const t of times) {
    const i = Math.floor((Date.parse(t) - start) / DAY)
    if (i >= 0 && i < days) values[i] = (values[i] ?? 0) + 1
  }
  const labels = values.map((_, i) =>
    new Date(start + i * DAY).toLocaleDateString('en-GB', { weekday: 'short', day: 'numeric' }))
  return { values, labels }
}

/** The `n` most frequent keys, most first. */
export function topCounts<T>(items: T[], key: (t: T) => string, n = 5): { label: string; n: number }[] {
  const m = new Map<string, number>()
  for (const it of items) {
    const k = key(it)
    m.set(k, (m.get(k) ?? 0) + 1)
  }
  return [...m.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, n).map(([label, c]) => ({ label, n: c }))
}
