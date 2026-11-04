import type { ReactNode } from 'react'
import { Box, Group, Skeleton, Stack, Text, Tooltip, UnstyledButton } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconChartBar, IconServer2, IconShieldCheck } from '@tabler/icons-react'
import { reachOf, shortName, type Reach } from '~/lib/reach'
import { EmptyState, SectionCard } from '~/components/page'
import { now } from '~/components/primitives'
import { AnchorLink } from '~/components/links'
import { assetsQuery, sessionsQuery } from '~/lib/queries'
import { FS } from '~/theme'
import type { RecordingFidelity, Session } from '~/types/domain'
import './command.css'

/**
 * Home's command-centre pieces for someone who sees the whole fleet.
 *
 * Every figure here is computed from a list the console already fetched
 * (sessions, servers) or from the stats the control plane reports. Nothing is
 * a trend the console was not given: the one sparkline is sessions by hour,
 * counted from the session list itself, and the meters are fractions of two
 * reported numbers.
 */

/**
 * The top of Home: who, when, the actions, and for the whole fleet a dark
 * band with its five numbers. `plain` is the same header without the band,
 * for everyone else and for the moment before we know who this is -- one
 * element either way, so the greeting is not torn down and redrawn when the
 * answer arrives.
 */
export function CommandBand({ title, subtitle, actions, children, plain = false }: {
  title: string; subtitle: string; actions?: ReactNode; children?: ReactNode; plain?: boolean
}) {
  return (
    <Box className={plain ? 'argus-band-plain' : 'argus-band'} px={{ base: 'md', sm: 'xl' }} pt={{ base: 'md', sm: 'xl' }}
      pb={plain ? 'xs' : { base: 'md', sm: 'lg' }}>
      <Group justify="space-between" align="flex-start" gap="md" className="argus-band-head">
        <Box style={{ minWidth: 0 }}>
          <Text component="h1" fw={700} fz={{ base: 22, sm: 26 }} lh={1.2} style={{ margin: 0, letterSpacing: '-0.01em' }}>
            {title}
          </Text>
          <Text size="sm" mt={4} className="argus-band-sub" c={plain ? 'dimmed' : undefined}>{subtitle}</Text>
        </Box>
        {actions && <Group gap="xs" wrap="nowrap">{actions}</Group>}
      </Group>
      {children && <Box className="argus-band-stats">{children}</Box>}
    </Box>
  )
}

/**
 * One number on the band. `meter` is a fraction drawn as a bar; `spark` is a
 * row of counts drawn as columns, the last one lit. Either is optional.
 */
export function BandStat({ label, value, sub, warn, meter, spark, onClick }: {
  label: string
  /** `undefined` while loading: a skeleton, never a zero. */
  value: string | number | undefined
  sub?: string
  warn?: boolean
  meter?: number
  spark?: number[]
  onClick?: () => void
}) {
  const body = (
    <>
      <Text size={FS.meta} className="argus-band-k">{label}</Text>
      {value === undefined ? (
        <Skeleton h={30} w={90} mt={6} radius="sm" className="argus-band-skel" />
      ) : (
        <Text fz={{ base: 22, sm: 28 }} fw={650} lh={1.15} mt={4}>{value}</Text>
      )}
      {sub && <Text size={FS.meta} mt={2} className={warn ? 'argus-band-warn' : 'argus-band-sub'}>{sub}</Text>}
      {meter !== undefined && (
        <Box className="argus-band-meter" mt="sm" aria-hidden>
          <Box style={{ width: `${Math.round(Math.max(0, Math.min(1, meter)) * 100)}%` }} data-warn={warn || undefined} />
        </Box>
      )}
      {spark && spark.length > 0 && <Spark values={spark} />}
    </>
  )
  return onClick ? (
    <UnstyledButton className="argus-band-stat" data-link onClick={onClick}>{body}</UnstyledButton>
  ) : (
    <Box className="argus-band-stat">{body}</Box>
  )
}

function Spark({ values }: { values: number[] }) {
  const max = Math.max(1, ...values)
  return (
    <Box className="argus-band-spark" mt="sm" aria-hidden>
      {values.map((v, i) => (
        <i key={i} data-now={i === values.length - 1 || undefined} style={{ height: `${Math.max(6, (v / max) * 100)}%` }} />
      ))}
    </Box>
  )
}

const HOUR = 3_600_000

/** Sessions started in each of the last `hours` hours, oldest first. */
export function perHour(sessions: Session[] | undefined, hours = 24): { all: number[]; direct: number[] } {
  const all = new Array<number>(hours).fill(0)
  const direct = new Array<number>(hours).fill(0)
  const end = now()
  for (const s of sessions ?? []) {
    const ago = Math.floor((end - Date.parse(s.startedAt)) / HOUR)
    if (ago < 0 || ago >= hours) continue
    const i = hours - 1 - ago
    all[i] = (all[i] ?? 0) + 1
    if (s.origin === 'direct') direct[i] = (direct[i] ?? 0) + 1
  }
  return { all, direct }
}

/**
 * Sessions by hour over the last day, with the ones that went straight to a
 * server drawn in the denied colour on top of their hour's column.
 */
export function SessionsByHour() {
  const { data, isPending } = useQuery(sessionsQuery())
  const { all, direct } = perHour(data)
  const max = Math.max(1, ...all)
  const total = all.reduce((a, b) => a + b, 0)
  const end = now()
  const hourLabel = (i: number) =>
    new Date(end - (23 - i) * HOUR).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })
  return (
    <SectionCard
      title="Sessions by hour"
      icon={IconChartBar}
      description={isPending ? undefined : `${total} started in the last 24 hours${direct.some((d) => d > 0) ? ', straight-to-server ones in red' : ''}`}
      action={<AnchorLink to="/activity" size={FS.meta}>Activity ›</AnchorLink>}
    >
      {isPending ? <Skeleton h={160} /> : total === 0 ? (
        <EmptyState compact title="Nobody connected in the last 24 hours" />
      ) : (
        <>
          <Box className="argus-hourchart" role="img" aria-label={`Sessions by hour, ${total} in the last 24 hours`}>
            {all.map((v, i) => (
              <Tooltip key={i} label={`${hourLabel(i)} · ${v} session${v === 1 ? '' : 's'}${direct[i] ? `, ${direct[i]} straight to a server` : ''}`}>
                <Box className="argus-hourcol" data-now={i === all.length - 1 || undefined}>
                  <i style={{ height: `${(v / max) * 100}%` }}>
                    {direct[i] ? <b style={{ height: `${((direct[i] ?? 0) / Math.max(1, v)) * 100}%` }} /> : null}
                  </i>
                </Box>
              </Tooltip>
            ))}
          </Box>
          <Group justify="space-between" mt={6}>
            {[0, 6, 12, 18, 23].map((i) => (
              <Text key={i} size={FS.micro} c="dimmed">{i === 23 ? 'Now' : hourLabel(i)}</Text>
            ))}
          </Group>
        </>
      )}
    </SectionCard>
  )
}

const REACH_ORDER: Reach[] = ['blocked', 'offline', 'slow', 'online']
const REACH_WORD: Record<Reach, string> = { online: 'Online', slow: 'Slow', offline: 'Offline', blocked: 'Blocked' }

/** Every server as one square, coloured by whether it answered. Problems first. */
export function FleetMap() {
  const { data, isPending } = useQuery(assetsQuery({}))
  const navigate = useNavigate()
  const assets = [...(data ?? [])].sort((a, b) =>
    REACH_ORDER.indexOf(reachOf(a)) - REACH_ORDER.indexOf(reachOf(b)) || a.hostname.localeCompare(b.hostname))
  const counts = REACH_ORDER.map((r) => [r, assets.filter((a) => reachOf(a) === r).length] as const)
  return (
    <SectionCard
      title="Fleet"
      icon={IconServer2}
      description="Every server, problems first. Open one to see why."
      action={<AnchorLink to="/assets" size={FS.meta}>Servers ›</AnchorLink>}
    >
      {isPending ? <Skeleton h={120} /> : assets.length === 0 ? (
        <EmptyState compact title="No servers yet" />
      ) : (
        <Stack gap="sm">
          <Box className="argus-fleetmap" data-testid="fleet-map">
            {assets.map((a) => (
              <Tooltip key={a.id} label={`${shortName(a.hostname)} · ${REACH_WORD[reachOf(a)]}`}>
                <UnstyledButton
                  className="argus-fleetcell"
                  data-reach={reachOf(a)}
                  aria-label={`${a.hostname}, ${REACH_WORD[reachOf(a)]}`}
                  onClick={() => void navigate({ to: '/assets/$assetId', params: { assetId: a.id } })}
                />
              </Tooltip>
            ))}
          </Box>
          <Group gap="md">
            {counts.filter(([, n]) => n > 0).map(([r, n]) => (
              <Group key={r} gap={6} wrap="nowrap">
                <Box className="argus-fleetcell" data-reach={r} w={10} h={10} />
                <Text size={FS.meta} c="dimmed">{REACH_WORD[r]} {n}</Text>
              </Group>
            ))}
          </Group>
        </Stack>
      )}
    </SectionCard>
  )
}

const FIDELITY: { key: RecordingFidelity; word: string; color: string }[] = [
  { key: 'ebpf', word: 'Commands and screen', color: 'var(--color-verified)' },
  { key: 'pty', word: 'Screen only', color: 'var(--color-pending)' },
  { key: 'rdp', word: 'Desktop', color: 'var(--color-live)' },
  { key: 'statement', word: 'Statements', color: 'var(--mantine-color-violet-5)' },
  { key: 'none', word: 'Not recorded', color: 'var(--color-denied)' },
]

/**
 * How the last day's sessions were recorded, as a ring. The share with
 * kernel-observed commands is the headline, because it is the tier that
 * survives an auditor; screen-only is honest but weaker.
 */
export function RecordingRing() {
  const { data, isPending } = useQuery(sessionsQuery())
  const end = now()
  const day = (data ?? []).filter((s) => end - Date.parse(s.startedAt) < 24 * HOUR)
  const total = day.length
  const parts = FIDELITY.map((f) => ({ ...f, n: day.filter((s) => s.fidelity === f.key).length })).filter((p) => p.n > 0)
  let at = 0
  const stops = parts.map((p) => {
    const from = at
    at += (p.n / Math.max(1, total)) * 100
    return `${p.color} ${from}% ${at}%`
  })
  const strong = total ? Math.round(((parts.find((p) => p.key === 'ebpf')?.n ?? 0) / total) * 100) : 0
  return (
    <SectionCard title="How sessions were recorded" icon={IconShieldCheck} iconColor="teal" description="Sessions in the last 24 hours">
      {isPending ? <Skeleton h={120} /> : total === 0 ? (
        <EmptyState compact title="No sessions in the last 24 hours" />
      ) : (
        <Group gap="lg" wrap="nowrap" align="center">
          <Box className="argus-ring" style={{ background: `conic-gradient(${stops.join(', ')})` }}
            role="img" aria-label={`${strong}% recorded with commands`}>
            <Box className="argus-ring-hole">
              <Text fz={22} fw={650} lh={1}>{strong}%</Text>
              <Text size={FS.micro} c="dimmed">with commands</Text>
            </Box>
          </Box>
          <Stack gap={6}>
            {parts.map((p) => (
              <Group key={p.key} gap={8} wrap="nowrap">
                <Box w={9} h={9} style={{ borderRadius: 3, background: p.color, flexShrink: 0 }} />
                <Text size={FS.meta}>{p.word}</Text>
                <Text size={FS.meta} c="dimmed">{p.n}</Text>
              </Group>
            ))}
          </Stack>
        </Group>
      )}
    </SectionCard>
  )
}
