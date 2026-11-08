import { memo, useEffect, useState, type ReactNode } from 'react'
import { Badge, Box, Group, Skeleton, Text, Tooltip, UnstyledButton } from '@mantine/core'
import { useNavigate } from '@tanstack/react-router'
import {
  IconActivityHeartbeat, IconChartBar, IconClipboardCheck, IconServer2,
} from '@tabler/icons-react'
import { AnchorLink, ButtonLink } from '~/components/links'
import { EmptyState, SectionCard } from '~/components/page'
import { Mono, duration, now, relTime } from '~/components/primitives'
import { useHome, useHomeStatus } from '~/lib/homeLive'
import { shortName } from '~/lib/reach'
import { FS, INK, SP } from '~/theme'
import type { AuditEvent, HomeSnapshot } from '~/types/domain'
import './command.css'

/**
 * Home's live dashboard for whoever sees the whole fleet.
 *
 * Every figure is the control plane's (see internal/control/home_snapshot.go):
 * counted from the store, or read from each agent's last usage report while
 * it is younger than three minutes. The lines are the last hour, one point a
 * minute; the bars are the last 24 hours by hour. A tile turns amber or rose
 * only when someone needs to act, so a quiet day reads as quiet.
 */

/* ── The pill: how fresh the numbers are ─────────────────────────────────── */

export function LivePill() {
  const { status, heardAt } = useHomeStatus()
  const [, tick] = useState(0)
  // The "from 09:14" wording needs no ticking; only "polling" shows an age.
  useEffect(() => {
    if (status !== 'polling') return
    const t = setInterval(() => tick((n) => n + 1), 5_000)
    return () => clearInterval(t)
  }, [status])
  const at = heardAt ? new Date(heardAt).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' }) : ''
  const [word, tip, tone] = ({
    fixture: ['Sample data', 'No control plane is configured: these are the fixture\'s numbers, not a fleet\'s.', 'pending'],
    connecting: ['Connecting…', 'Opening the live stream to argus-control.', 'idle'],
    live: ['Live', 'Changes arrive as they happen; argus-control checks every 5 seconds.', 'live'],
    paused: ['Paused while hidden', 'Nothing is sent while this tab is in the background. It catches up when you come back.', 'idle'],
    polling: [at ? `Updated ${relTime(new Date(heardAt).toISOString())}` : 'Updating', 'The live stream is not available; the page reads the dashboard every 15 seconds instead.', 'pending'],
    off: ['Not available', 'Your role no longer sees the whole fleet. Reload the page.', 'idle'],
  } as const)[status]
  return (
    <Tooltip label={tip} multiline w={260} withArrow>
      <Box className="argus-livepill" data-tone={tone} data-testid="home-live" aria-live="polite">
        <span className="argus-livepill-dot" aria-hidden />
        <Text span size={FS.meta} fw={550}>{word}</Text>
      </Box>
    </Tooltip>
  )
}

/* ── Six numbers ─────────────────────────────────────────────────────────── */

type Tone = 'calm' | 'pending' | 'denied'

function Tile({ label, value, small, sub, tone = 'calm', viz, to, testid }: {
  label: string
  value: ReactNode | undefined
  small?: string
  sub?: ReactNode
  tone?: Tone
  viz?: ReactNode
  to?: () => void
  testid?: string
}) {
  const body = (
    <>
      <Text size={FS.meta} c="dimmed" truncate>{label}</Text>
      {value === undefined ? <Skeleton h={30} w={80} mt={6} radius="sm" /> : (
        <Group gap={5} align="baseline" mt={4} wrap="nowrap">
          {/* Keyed by the value, so a change fades in once. */}
          <Text key={String(value)} className="argus-num" fz={{ base: 22, sm: 26 }} fw={650} lh={1.15}>{value}</Text>
          {small && <Text size={FS.meta} c="dimmed" fw={500}>{small}</Text>}
        </Group>
      )}
      <Text size={FS.meta} c="dimmed" mt={2} truncate>{sub ?? ' '}</Text>
      <Box mt={8} h={30}>{viz}</Box>
    </>
  )
  return to ? (
    <UnstyledButton className="argus-tile" data-tone={tone} data-link onClick={to} data-testid={testid}>{body}</UnstyledButton>
  ) : (
    <Box className="argus-tile" data-tone={tone} data-testid={testid}>{body}</Box>
  )
}

/** The last hour as a line. Nulls are minutes nobody reported; they are skipped. */
const Line = memo(function Line({ values, max, color = 'var(--color-brand)', label }: {
  values: (number | null)[] | undefined; max?: number; color?: string; label: string
}) {
  const pts = (values ?? []).map((v, i) => [i, v] as const).filter((p): p is readonly [number, number] => p[1] !== null)
  if (pts.length < 2) return <Box className="argus-viz-empty" aria-hidden />
  const hi = max ?? Math.max(1, ...pts.map((p) => p[1]))
  const n = Math.max(59, (values?.length ?? 1) - 1)
  const off = 59 - ((values?.length ?? 1) - 1) // a short line ends at "now", on the right
  const xy = pts.map(([i, v]) => `${(((i + off) / n) * 100).toFixed(2)},${(28 - (v / hi) * 26).toFixed(2)}`)
  const last = xy[xy.length - 1]!.split(',')
  return (
    <svg viewBox="0 0 100 30" preserveAspectRatio="none" width="100%" height="30" role="img" aria-label={label}>
      <path d={`M${xy.join(' L')} L${last[0]},30 L${xy[0]!.split(',')[0]},30 Z`} fill={color} opacity={0.08} />
      <path d={`M${xy.join(' L')}`} fill="none" stroke={color} strokeWidth={1.6} vectorEffect="non-scaling-stroke" />
    </svg>
  )
})

/** The last 24 hours as bars, the hour so far lit. */
const Bars = memo(function Bars({ values, color = 'var(--color-brand)', label }: {
  values: number[] | undefined; color?: string; label: string
}) {
  const v = values ?? []
  if (v.length === 0) return <Box className="argus-viz-empty" aria-hidden />
  const hi = Math.max(1, ...v)
  const w = 100 / v.length
  return (
    <svg viewBox="0 0 100 30" preserveAspectRatio="none" width="100%" height="30" role="img" aria-label={label}>
      {v.map((n, i) => {
        const h = Math.max(1.5, (n / hi) * 29)
        return <rect key={i} x={i * w + w * 0.15} y={30 - h} width={w * 0.7} height={h} rx={0.6}
          fill={color} opacity={i === v.length - 1 ? 1 : 0.35} />
      })}
    </svg>
  )
})

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

export function HomeTiles() {
  const navigate = useNavigate()
  const servers = useHome((s) => s.servers)
  const live = useHome((s) => s.live)
  const sessions = useHome((s) => s.sessions)
  const waiting = useHome((s) => s.waiting)
  const cpu = useHome((s) => s.processor)
  const signIns = useHome((s) => s.signIns)
  const trends = useHome((s) => s.trends)

  const notAnswering = servers ? servers.down + servers.quiet : 0
  const total24 = sessions?.hours.map((h) => h[0] + h[1] + h[2])
  const change = sessions && sessions.weekAgo > 0 ? Math.round(((sessions.last24h - sessions.weekAgo) / sessions.weekAgo) * 100) : null
  const busiest = cpu?.busiest[0]
  const cpuTone: Tone = cpu?.average != null && cpu.average >= 85 ? 'denied' : cpu?.average != null && cpu.average >= 60 ? 'pending' : 'calm'
  const oldestMin = waiting?.oldestAt ? Math.max(0, Math.round((now() - Date.parse(waiting.oldestAt)) / 60_000)) : null

  return (
    <Box className="argus-tiles" data-testid="home-tiles">
      <Tile
        testid="tile-servers"
        label="Servers online"
        value={servers ? servers.answering : undefined}
        small={servers ? `of ${servers.total}` : undefined}
        tone={servers && servers.down > 0 ? 'denied' : notAnswering > 0 ? 'pending' : 'calm'}
        sub={servers ? (notAnswering === 0 ? 'Every server answers'
          : <Text span inherit c={servers.down > 0 ? INK.denied : INK.pending} fw={600}>{plural(notAnswering, 'not answering', 'not answering')}</Text>) : undefined}
        viz={<Line values={trends?.answering} max={servers?.total} color={servers && servers.down > 0 ? 'var(--color-denied)' : 'var(--color-verified)'} label="Servers answering, last hour" />}
        to={() => void navigate({ to: '/assets' })}
      />
      <Tile
        testid="tile-live"
        label="Live sessions"
        value={live?.count}
        sub={live ? (live.count === 0 ? 'Nobody is on a server' : [live.root > 0 && `${live.root} as root`, live.database > 0 && plural(live.database, 'database')].filter(Boolean).join(' · ') || 'None as root') : undefined}
        viz={<Line values={trends?.live} label="Live sessions, last hour" />}
        to={() => void navigate({ to: '/activity' })}
      />
      <Tile
        testid="tile-sessions"
        label="Sessions, last 24 h"
        value={sessions?.last24h}
        sub={change === null ? (sessions ? 'Each bar is one hour' : undefined)
          : <><Text span inherit fw={600} c={change >= 0 ? INK.verified : undefined}>{change >= 0 ? '+' : ''}{change}%</Text> on a week ago</>}
        viz={<Bars values={total24} label="Sessions by hour, last 24 hours" />}
      />
      <Tile
        testid="tile-waiting"
        label="Waiting on approval"
        value={waiting?.count}
        tone={waiting && waiting.count > 0 && (oldestMin ?? 0) >= 15 ? 'pending' : 'calm'}
        sub={waiting ? (waiting.count === 0 ? 'Nothing waiting' : oldestMin !== null ? `oldest ${oldestMin < 1 ? 'just now' : `${oldestMin} min`}` : '') : undefined}
        viz={<Line values={trends?.waiting} color="var(--color-pending)" label="Waiting on approval, last hour" />}
        to={() => void navigate({ to: '/approvals' })}
      />
      <Tile
        testid="tile-cpu"
        label="Fleet processor"
        value={cpu ? (cpu.average === null ? '—' : `${Math.round(cpu.average)}%`) : undefined}
        small={cpu && cpu.average !== null ? 'avg' : undefined}
        tone={cpuTone}
        sub={cpu ? (busiest ? `busiest ${shortName(busiest.name)} · ${Math.round(busiest.busy)}%` : 'No server reported in 3 min') : undefined}
        viz={<Line values={trends?.processor} max={100} color="var(--mantine-color-slate-5)" label="Fleet processor, last hour" />}
      />
      <Tile
        testid="tile-signins"
        label="Sign-ins, last 24 h"
        value={signIns?.last24h}
        tone={signIns && signIns.failed15m >= 5 ? 'denied' : 'calm'}
        sub={signIns ? (signIns.failed24h === 0 ? 'None failed'
          : <Text span inherit fw={600} c={signIns.failed15m >= 5 ? INK.denied : INK.pending}>{signIns.failed24h} failed</Text>) : undefined}
        viz={<Bars values={signIns?.hours} color="var(--mantine-color-slate-5)" label="Sign-ins by hour, last 24 hours" />}
        to={() => void navigate({ to: '/audit' })}
      />
    </Box>
  )
}

/* ── Sessions, last 24 hours ─────────────────────────────────────────────── */

const KINDS = [
  { word: 'Terminal', color: 'var(--mantine-color-azure-5)' },
  { word: 'Desktop', color: 'var(--mantine-color-violet-5)' },
  { word: 'Database', color: 'var(--mantine-color-cyan-6)' },
] as const

const HOUR = 3_600_000

export function SessionsChart() {
  const sessions = useHome((s) => s.sessions)
  const at = useHome((s) => s.at)
  const end = at ? Date.parse(at) : now()
  const hours = sessions?.hours ?? []
  const hi = Math.max(1, ...hours.map((h) => h[0] + h[1] + h[2]))
  const label = (i: number) => new Date(end - (23 - i) * HOUR).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })
  return (
    <SectionCard
      title="Sessions, last 24 hours"
      icon={IconChartBar}
      badge={sessions ? (
        <Group gap="md" className="argus-legend" visibleFrom="sm">
          {KINDS.map((k, i) => (
            <Group key={k.word} gap={6} wrap="nowrap">
              <Box w={9} h={9} style={{ borderRadius: 2, background: k.color }} />
              <Text size={FS.meta} c="dimmed">{k.word} {sessions.kinds[i]}</Text>
            </Group>
          ))}
        </Group>
      ) : undefined}
      action={<AnchorLink to="/activity" size={FS.meta}>All sessions ›</AnchorLink>}
    >
      {!sessions ? <Skeleton h={150} /> : sessions.last24h === 0 ? (
        <EmptyState compact title="Nobody connected in the last 24 hours" />
      ) : (
        <>
          <Box className="argus-hourchart" role="img" aria-label={`Sessions by hour: ${sessions.last24h} in the last 24 hours`} data-testid="sessions-chart">
            {hours.map((h, i) => {
              const n = h[0] + h[1] + h[2]
              return (
                <Tooltip key={i} label={`${label(i)} · ${plural(n, 'session')}${n ? ` (${KINDS.map((k, j) => h[j] ? `${h[j]} ${k.word.toLowerCase()}` : '').filter(Boolean).join(', ')})` : ''}`}>
                  <Box className="argus-hourcol" data-now={i === hours.length - 1 || undefined}>
                    <Box className="argus-hourstack" style={{ height: `${(n / hi) * 100}%` }}>
                      {h.map((v, j) => v > 0 && <i key={j} style={{ flexGrow: v, background: KINDS[j]!.color }} />)}
                    </Box>
                  </Box>
                </Tooltip>
              )
            })}
          </Box>
          <Group justify="space-between" mt={6}>
            {[0, 6, 12, 18, 23].map((i) => (
              <Text key={i} size={FS.micro} c="dimmed">{i === 23 ? 'Now' : label(i)}</Text>
            ))}
          </Group>
        </>
      )}
    </SectionCard>
  )
}

/* ── Live now ────────────────────────────────────────────────────────────── */

/** Re-renders every second while the tab is visible, for durations that count up. */
function useSecond() {
  const [, tick] = useState(0)
  useEffect(() => {
    let t: ReturnType<typeof setInterval> | null = null
    const go = () => {
      if (t) clearInterval(t)
      t = document.visibilityState === 'visible' ? setInterval(() => tick((n) => n + 1), 1_000) : null
    }
    go()
    document.addEventListener('visibilitychange', go)
    return () => {
      if (t) clearInterval(t)
      document.removeEventListener('visibilitychange', go)
    }
  }, [])
}

const KIND_WORD: Record<string, string> = { terminal: 'Terminal', desktop: 'Desktop', database: 'Database', files: 'Files' }

export function LiveNow() {
  const list = useHome((s) => s.liveList)
  const count = useHome((s) => s.live.count)
  useSecond()
  return (
    <SectionCard
      title="Live now"
      icon={IconActivityHeartbeat}
      iconColor="sky"
      badge={count ? <Badge color="sky" variant="light">{count}</Badge> : undefined}
      action={<AnchorLink to="/activity" size={FS.meta}>All sessions ›</AnchorLink>}
      flush
    >
      {!list ? (
        <Box px="md" pb="md"><Skeleton h={36} mb="xs" /><Skeleton h={36} /></Box>
      ) : list.length === 0 ? (
        <EmptyState compact title="Nobody is on a server right now" />
      ) : (
        <Box data-testid="live-now">
          {list.map((s) => {
            const root = s.principal === 'root' || s.principal.toLowerCase() === 'administrator'
            return (
              <Group key={s.id} className="argus-liverow" gap="sm" wrap="nowrap" px="md" py={SP.cozy}>
                <Box style={{ flex: 1, minWidth: 0 }}>
                  <Text component="div" size={FS.body} truncate>
                    <Text span inherit fw={550}>{s.user.split('@')[0]}</Text>
                    <Text span inherit c="dimmed"> as </Text>
                    <Mono>{s.principal}</Mono>
                    {root && <Badge ml={SP.snug} size="xs" color="rose" variant="light">{s.principal === 'root' ? 'root' : 'admin'}</Badge>}
                  </Text>
                  <Text size={FS.meta} c="dimmed" truncate>
                    <span className="argus-kind" data-kind={s.kind}>{KIND_WORD[s.kind] ?? s.kind}</span> {s.host}
                  </Text>
                </Box>
                <Text size={FS.meta} c="dimmed" className="argus-dur">{duration(s.startedAt, null)}</Text>
                <ButtonLink to="/sessions/$sessionId" params={{ sessionId: s.id }} variant="default" size="compact-sm" style={{ flexShrink: 0 }}>
                  Watch
                </ButtonLink>
              </Group>
            )
          })}
          {count !== undefined && count > list.length && (
            <Text size={FS.meta} c="dimmed" px="md" py="xs">and {count - list.length} more</Text>
          )}
        </Box>
      )}
    </SectionCard>
  )
}

/* ── Fleet health ────────────────────────────────────────────────────────── */

function Meter({ name, pct, mount, onClick }: { name: string; pct: number; mount?: string; onClick: () => void }) {
  const tone = pct >= 95 ? 'denied' : pct >= 85 ? 'pending' : 'calm'
  return (
    <UnstyledButton className="argus-meter" onClick={onClick} aria-label={`${name}${mount ? ` ${mount}` : ''}, ${Math.round(pct)}%`}>
      <Text size={FS.meta} truncate title={name}>{shortName(name)}{mount && <Text span inherit c="dimmed" ff="monospace" fz={FS.micro}> {mount}</Text>}</Text>
      <Box className="argus-bar"><i data-tone={tone} style={{ width: `${Math.min(100, pct)}%` }} /></Box>
      <Text size={FS.meta} c="dimmed" ta="right" className="argus-dur">{Math.round(pct)}%</Text>
    </UnstyledButton>
  )
}

export function FleetHealth() {
  const navigate = useNavigate()
  const servers = useHome((s) => s.servers)
  const cpu = useHome((s) => s.processor)
  const disks = useHome((s) => s.disks)
  const open = (assetId: string) => () => void navigate({ to: '/assets/$assetId', params: { assetId } })
  const total = Math.max(1, servers?.total ?? 1)
  return (
    <SectionCard
      title="Fleet health"
      icon={IconServer2}
      description="From each agent's last report"
      action={<AnchorLink to="/assets" size={FS.meta}>Servers ›</AnchorLink>}
    >
      {!servers || !cpu || !disks ? <Skeleton h={200} /> : servers.total === 0 ? (
        <EmptyState compact title="No servers yet" />
      ) : (
        <Box className="argus-health" data-testid="fleet-health">
          <Box>
            <Box className="argus-stack" role="img" aria-label={`${servers.answering} answering, ${servers.quiet} quiet, ${servers.down} not answering`}>
              <i data-tone="calm" style={{ width: `${(servers.answering / total) * 100}%` }} />
              <i data-tone="pending" style={{ width: `${(servers.quiet / total) * 100}%` }} />
              <i data-tone="denied" style={{ width: `${(servers.down / total) * 100}%` }} />
            </Box>
            <Group gap="md" mt={8}>
              <Text size={FS.meta} c="dimmed"><Text span inherit fw={600} c="var(--mantine-color-text)">{servers.answering}</Text> answering</Text>
              {servers.quiet > 0 && <Text size={FS.meta} c="dimmed"><Text span inherit fw={600} c="var(--mantine-color-text)">{servers.quiet}</Text> quiet</Text>}
              {servers.down > 0 && <Text size={FS.meta} c="dimmed"><Text span inherit fw={600} c="var(--mantine-color-text)">{servers.down}</Text> not answering</Text>}
            </Group>
          </Box>
          <Box>
            <Text className="argus-sublabel">Busiest right now · processor</Text>
            {cpu.busiest.length === 0 ? <Text size={FS.meta} c="dimmed">No server has reported in the last 3 minutes.</Text>
              : cpu.busiest.map((b) => <Meter key={b.assetId} name={b.name} pct={b.busy} onClick={open(b.assetId)} />)}
          </Box>
          <Box>
            <Text className="argus-sublabel">Disks filling up</Text>
            {disks.length === 0 ? <Text size={FS.meta} c="dimmed">Every disk has more than 10% free.</Text>
              : disks.map((d) => <Meter key={d.assetId + d.mount} name={d.name} mount={d.mount} pct={d.used} onClick={open(d.assetId)} />)}
          </Box>
        </Box>
      )}
    </SectionCard>
  )
}

/* ── Happening now ───────────────────────────────────────────────────────── */

export function HappeningNow({ words }: { words: (e: AuditEvent) => ReactNode }) {
  const audit = useHome((s) => s.audit)
  const [seen] = useState(() => new Set<number>())
  // An entry that arrives after the first paint slides in with a blue edge.
  const fresh = new Set<number>()
  if (audit) {
    const first = seen.size === 0
    for (const e of audit) {
      if (!seen.has(e.seq)) {
        if (!first) fresh.add(e.seq)
        seen.add(e.seq)
      }
    }
    if (seen.size > 200) seen.clear()
  }
  return (
    <SectionCard
      title="Happening now"
      icon={IconClipboardCheck}
      iconColor="violet"
      action={<AnchorLink to="/audit" size={FS.meta}>Audit log ›</AnchorLink>}
      flush
    >
      {!audit ? (
        <Box px="md" pb="md"><Skeleton h={20} mb="xs" /><Skeleton h={20} /></Box>
      ) : audit.length === 0 ? (
        <EmptyState compact title="Nothing has been recorded yet" />
      ) : (
        <Box data-testid="recent-audit">
          {audit.map((e) => (
            <Group key={e.seq} className="argus-feedrow" data-fresh={fresh.has(e.seq) || undefined}
              gap="md" wrap="nowrap" align="flex-start" px="md" py={SP.cozy}>
              <Text size={FS.meta} c="dimmed" w={70} style={{ flexShrink: 0 }}>{relTime(e.at)}</Text>
              <Box style={{ minWidth: 0, flex: 1 }}>
                <Text component="div" size={FS.meta} lineClamp={2} style={{ overflowWrap: 'anywhere' }}>
                  <Text span inherit fw={600}>{e.actorEmail || 'argus'}</Text>
                  {e.severity === 'critical' && <Badge ml={SP.snug} size="xs" color="rose" variant="light">critical</Badge>}
                  <Text span inherit c="dimmed"> · {words(e)}</Text>
                </Text>
              </Box>
            </Group>
          ))}
        </Box>
      )}
    </SectionCard>
  )
}

/** For Needs you: what the live dashboard says needs someone. */
export function useLiveNeeds(): {
  waiting: HomeSnapshot['waiting'] | undefined
  unreviewed: number | undefined
  fullDisks: HomeSnapshot['disks']
  failed15m: number
} {
  const waiting = useHome((s) => s.waiting)
  const unreviewed = useHome((s) => s.unreviewedHosts)
  const disks = useHome((s) => s.disks)
  const signIns = useHome((s) => s.signIns)
  return {
    waiting,
    unreviewed,
    fullDisks: (disks ?? []).filter((d) => d.used >= 95),
    failed15m: signIns?.failed15m ?? 0,
  }
}

