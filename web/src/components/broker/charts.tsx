import { Box, Group, Text } from '@mantine/core'
import { useElementSize } from '@mantine/hooks'
import { FS } from '~/theme'

/*
Two small charts, drawn as SVG so the broker workspace needs no chart
library: a line chart of a few series over the last ten minutes, and a
sparkline for a table cell. Colours are the theme's ramps, so both schemes
read; the third series is dashed so it is told apart without colour.
*/

export interface Series { label: string; values: number[]; color: string; dashed?: boolean }

const niceMax = (v: number) => {
  if (v <= 0) return 1
  const p = Math.pow(10, Math.floor(Math.log10(v)))
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p
  return 10 * p
}

export function LineChart({ series, height = 180, minutes = 10 }: {
  series: Series[]; height?: number; minutes?: number
}) {
  const { ref, width } = useElementSize()
  const W = Math.max(240, Math.round(width) || 800)
  const H = height
  const pl = 44
  const pb = 18
  const iw = W - pl - 6
  const ih = H - pb - 6
  const max = niceMax(Math.max(0, ...series.flatMap((s) => s.values)) * 1.1)
  const empty = series.every((s) => s.values.length < 2)
  return (
    <Box ref={ref}>
      <Group gap="md" mb={6}>
        {series.map((s) => (
          <Group key={s.label} gap={6}>
            <svg width="16" height="4" aria-hidden><line x1="0" y1="2" x2="16" y2="2" stroke={s.color} strokeWidth="3" strokeDasharray={s.dashed ? '4 3' : undefined} /></svg>
            <Text size={FS.micro} c="dimmed">{s.label}</Text>
          </Group>
        ))}
      </Group>
      <svg viewBox={`0 0 ${W} ${H}`} width="100%" height={H} role="img"
        aria-label={`${series.map((s) => s.label).join(', ')} over the last ${minutes} minutes`}>
        {[0, 1, 2, 3, 4].map((g) => {
          const y = 6 + ih - (ih * g) / 4
          return (
            <g key={g}>
              <line x1={pl} x2={W - 6} y1={y} y2={y} stroke="var(--color-line)" vectorEffect="non-scaling-stroke" />
              <text x={pl - 6} y={y + 4} fontSize="10" textAnchor="end" fill="var(--mantine-color-dimmed)">{Math.round((max * g) / 4).toLocaleString()}</text>
            </g>
          )
        })}
        {(W < 500 ? [0, 1] : [0, 0.5, 1]).map((f) => (
          <text key={f} x={pl + iw * f} y={H - 3} fontSize="10" textAnchor={f === 0 ? 'start' : f === 1 ? 'end' : 'middle'}
            fill="var(--mantine-color-dimmed)">{f === 1 ? 'now' : `-${Math.round(minutes * (1 - f))}m`}</text>
        ))}
        {series.map((s) => {
          if (s.values.length < 2) return null
          const pts = s.values.map((v, i) => `${pl + (iw * i) / (s.values.length - 1)},${6 + ih - (ih * Math.min(v, max)) / max}`).join(' ')
          return <polyline key={s.label} points={pts} fill="none" stroke={s.color} strokeWidth="2" vectorEffect="non-scaling-stroke"
            strokeLinejoin="round" strokeDasharray={s.dashed ? '5 4' : undefined} />
        })}
        {empty && <text x={W / 2} y={H / 2} textAnchor="middle" fontSize="12" fill="var(--mantine-color-dimmed)">Collecting samples…</text>}
      </svg>
    </Box>
  )
}

export function Sparkline({ values, color = 'var(--mantine-color-azure-6)', width = 64, height = 18, label }: {
  values: number[]; color?: string; width?: number; height?: number; label?: string
}) {
  if (values.length < 2) return <Box w={width} h={height} />
  const max = Math.max(...values) || 1
  const pts = values.map((v, i) => `${(width * i) / (values.length - 1)},${height - 1 - ((height - 2) * v) / max}`)
  return (
    <svg width={width} height={height} role={label ? 'img' : undefined} aria-label={label} aria-hidden={label ? undefined : true}>
      <polygon points={`0,${height} ${pts.join(' ')} ${width},${height}`} fill={color} opacity={0.12} />
      <polyline points={pts.join(' ')} fill="none" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />
    </svg>
  )
}

/** A bar with an optional alarm mark, for node memory and disk. */
export function Meter({ value, max, mark, color = 'teal' }: { value: number; max: number; mark?: number; color?: string }) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0
  return (
    <Box pos="relative" h={6} mt={6} style={{ borderRadius: 3, background: 'var(--mantine-color-default-hover)' }}>
      <Box pos="absolute" top={0} bottom={0} left={0} w={`${pct}%`} style={{ borderRadius: 3, background: `var(--mantine-color-${color}-6)` }} />
      {mark !== undefined && max > 0 && (
        <Box pos="absolute" top={-3} bottom={-3} w={2} left={`${Math.min(100, (mark / max) * 100)}%`} style={{ background: 'var(--mantine-color-rose-6)' }} />
      )}
    </Box>
  )
}
