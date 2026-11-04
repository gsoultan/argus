import { Text } from '@mantine/core'
import { DayBars, Facet, FacetOption, InsightCard, RankBars, perDay, topCounts } from '~/components/Rails'
import { now } from '~/components/primitives'
import { FS } from '~/theme'

/** What the rails read from an entry: the chain's links carry these too. */
type Entry = { at: string; action: string; severity: string; actorEmail: string }

const SEVERITIES = ['info', 'notice', 'warning', 'critical'] as const

/** "session.start" is a session thing; the part before the dot names the area. */
export function areaOf(action: string): string {
  return action.split('.')[0] ?? action
}

const AREA_WORD: Record<string, string> = {
  session: 'Sessions', request: 'Access requests', auth: 'Sign-in', hostkey: 'Host keys', agent: 'Agents',
  policy: 'Policy', file: 'Files', files: 'Files', credential: 'Credentials', asset: 'Servers',
  unmanaged_key: 'Unmanaged keys',
}

/** The audit log's filter rail. Counts are over everything the page holds. */
export function AuditFacets({ events, severity, onSeverity, area, onArea, actor, onActor }: {
  events: Entry[]
  severity: string | null
  onSeverity: (s: string | null) => void
  area: string | null
  onArea: (a: string | null) => void
  actor: string | null
  onActor: (a: string | null) => void
}) {
  const areas = topCounts(events, (e) => areaOf(e.action), 10)
  const actors = topCounts(events, (e) => e.actorEmail || 'argus', 8)
  return (
    <>
      <Text fw={600} size="sm">Filters</Text>
      <Facet title="Severity">
        <FacetOption label="Any" count={events.length} active={!severity} onClick={() => onSeverity(null)} />
        {SEVERITIES.map((s) => (
          <FacetOption key={s} label={s[0]!.toUpperCase() + s.slice(1)} tone={s === 'critical' ? 'warn' : undefined}
            count={events.filter((e) => e.severity === s).length} active={severity === s}
            onClick={() => onSeverity(severity === s ? null : s)} />
        ))}
      </Facet>
      <Facet title="Area">
        {areas.map((a) => (
          <FacetOption key={a.label} label={AREA_WORD[a.label] ?? a.label} count={a.n} active={area === a.label}
            onClick={() => onArea(area === a.label ? null : a.label)} />
        ))}
      </Facet>
      <Facet title="Who">
        {actors.map((a) => (
          <FacetOption key={a.label} label={a.label.split('@')[0]} count={a.n} active={actor === a.label}
            onClick={() => onActor(actor === a.label ? null : a.label)} />
        ))}
      </Facet>
    </>
  )
}

/** The audit log's summary rail, over the rows the table shows. */
export function AuditInsight({ events, onActor }: { events: Entry[]; onActor: (a: string) => void }) {
  const days = perDay(events.map((e) => e.at), now())
  const actors = topCounts(events, (e) => e.actorEmail || 'argus')
  const actions = topCounts(events, (e) => e.action)
  const critical = events.filter((e) => e.severity === 'critical').length
  return (
    <>
      <Text fw={600} size="sm">Summary <Text span size={FS.micro} c="dimmed" fw={400}>of the {events.length} entries shown</Text></Text>
      <InsightCard title="Entries per day" aside="Last 7 days">
        <DayBars values={days.values} labels={days.labels} />
      </InsightCard>
      <InsightCard title="Most active" aside="entries">
        <RankBars rows={actors.map((a) => ({ label: a.label.split('@')[0] ?? a.label, n: a.n, onClick: () => onActor(a.label) }))} />
      </InsightCard>
      <InsightCard title="Most frequent" aside="entries">
        <RankBars mono rows={actions} />
      </InsightCard>
      {critical > 0 && (
        <InsightCard title="Worth a look" tone="warn">
          <Text size={FS.meta} lh={1.55}>{critical} critical {critical === 1 ? 'entry' : 'entries'} in this view.</Text>
        </InsightCard>
      )}
    </>
  )
}
