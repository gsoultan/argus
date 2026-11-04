import { Text } from '@mantine/core'
import { DayBars, Facet, FacetOption, InsightCard, RankBars, perDay, topCounts } from '~/components/Rails'
import { now } from '~/components/primitives'
import { FS } from '~/theme'
import type { RecordingFidelity, Session, SessionProtocol } from '~/types/domain'

type Filter = 'all' | 'active' | 'silent' | 'direct' | 'flagged'

const FIDELITY_WORD: Record<RecordingFidelity, string> = {
  ebpf: 'eBPF (commands)',
  pty: 'PTY only',
  rdp: 'Desktop',
  statement: 'Every statement',
  none: 'Not recorded',
}
const PROTOCOL_WORD: Record<SessionProtocol, string> = { ssh: 'SSH', sftp: 'Files (SFTP)', rdp: 'Desktop (RDP)', postgres: 'Postgres' }

/**
 * Activity's filter rail. State, window and server are the same filters the
 * toolbar offers (and write to the address); recording and kind are extra.
 * Every count is over `sessions`, the set after the window and server.
 */
export function SessionFacets({
  sessions, filter, onFilter, since, onSince, host, onHost, fidelity, onFidelity, protocol, onProtocol,
}: {
  sessions: Session[]
  filter: Filter
  onFilter: (f: Filter) => void
  since: '24h' | undefined
  onSince: (v: '24h' | undefined) => void
  host: string | undefined
  onHost: (h: string | undefined) => void
  fidelity: RecordingFidelity | null
  onFidelity: (f: RecordingFidelity | null) => void
  protocol: SessionProtocol | null
  onProtocol: (p: SessionProtocol | null) => void
}) {
  const live = sessions.filter((s) => s.state === 'active' && !s.silent).length
  const silent = sessions.filter((s) => s.state === 'active' && s.silent).length
  const direct = sessions.filter((s) => s.origin === 'direct').length
  const flagged = sessions.filter((s) => s.riskFlags.length > 0).length
  const servers = topCounts(sessions, (s) => s.assetHostname, 8)
  const fidelities = topCounts(sessions, (s) => s.fidelity, 5) as { label: RecordingFidelity; n: number }[]
  const protocols = topCounts(sessions, (s) => s.protocol, 4) as { label: SessionProtocol; n: number }[]
  const any = filter !== 'all' || since || host || fidelity || protocol

  return (
    <>
      <Text fw={600} size="sm">
        Filters
        {any && (
          <Text span size={FS.meta} c="azure" ml="sm" style={{ cursor: 'pointer' }} role="button" tabIndex={0}
            onClick={() => { onFilter('all'); onSince(undefined); onHost(undefined); onFidelity(null); onProtocol(null) }}>
            Clear all
          </Text>
        )}
      </Text>
      <Facet title="State">
        <FacetOption label="Everything" count={sessions.length} active={filter === 'all'} onClick={() => onFilter('all')} />
        <FacetOption label="Live" count={live} active={filter === 'active'} onClick={() => onFilter('active')} />
        <FacetOption label="Unknown" count={silent} tone="warn" active={filter === 'silent'} onClick={() => onFilter('silent')} />
        <FacetOption label="Bypassed Argus" count={direct} tone="warn" active={filter === 'direct'} onClick={() => onFilter('direct')} />
        <FacetOption label="Flagged" count={flagged} tone="warn" active={filter === 'flagged'} onClick={() => onFilter('flagged')} />
      </Facet>
      <Facet title="When">
        <FacetOption label="All time" active={!since} onClick={() => onSince(undefined)} />
        <FacetOption label="Last 24 hours" active={since === '24h'} onClick={() => onSince('24h')} />
      </Facet>
      <Facet title="Recording">
        {fidelities.map((f) => (
          <FacetOption key={f.label} label={FIDELITY_WORD[f.label] ?? f.label} count={f.n} active={fidelity === f.label}
            onClick={() => onFidelity(fidelity === f.label ? null : f.label)} />
        ))}
      </Facet>
      <Facet title="Kind">
        {protocols.map((p) => (
          <FacetOption key={p.label} label={PROTOCOL_WORD[p.label] ?? p.label} count={p.n} active={protocol === p.label}
            onClick={() => onProtocol(protocol === p.label ? null : p.label)} />
        ))}
      </Facet>
      <Facet title="Server">
        {host && !servers.some((s) => s.label === host) && (
          <FacetOption label={host} active onClick={() => onHost(undefined)} />
        )}
        {servers.map((s) => (
          <FacetOption key={s.label} label={s.label.split('.')[0]} count={s.n} active={host === s.label}
            onClick={() => onHost(host === s.label ? undefined : s.label)} />
        ))}
      </Facet>
    </>
  )
}

/** Activity's summary rail: the rows the table holds, counted. */
export function SessionInsight({ sessions, onHost, onPerson }: {
  sessions: Session[]
  onHost: (h: string) => void
  onPerson: (p: string) => void
}) {
  const days = perDay(sessions.map((s) => s.startedAt), now())
  const people = topCounts(sessions, (s) => s.userEmail.split('@')[0] ?? s.userEmail)
  const hosts = topCounts(sessions, (s) => s.assetHostname)
  const direct = sessions.filter((s) => s.origin === 'direct').length
  const root = sessions.filter((s) => s.principal === 'root').length
  const silent = sessions.filter((s) => s.state === 'active' && s.silent).length
  return (
    <>
      <Text fw={600} size="sm">Summary <Text span size={FS.micro} c="dimmed" fw={400}>of the {sessions.length} sessions shown</Text></Text>
      <InsightCard title="Sessions per day" aside="Last 7 days">
        <DayBars values={days.values} labels={days.labels} />
      </InsightCard>
      <InsightCard title="Top people" aside="sessions">
        <RankBars rows={people.map((p) => ({ ...p, onClick: () => onPerson(p.label) }))} />
      </InsightCard>
      <InsightCard title="Top servers" aside="sessions">
        <RankBars mono rows={hosts.map((h) => ({ label: h.label.split('.')[0] ?? h.label, n: h.n, onClick: () => onHost(h.label) }))} />
      </InsightCard>
      {(direct > 0 || root > 0 || silent > 0) && (
        <InsightCard title="Worth a look" tone="warn">
          <Text size={FS.meta} lh={1.55}>
            {[
              direct > 0 && `${direct} went straight to a server, skipping Argus`,
              root > 0 && `${root} logged in as root`,
              silent > 0 && `${silent} active but no longer reporting`,
            ].filter(Boolean).join('. ')}.
          </Text>
        </InsightCard>
      )}
    </>
  )
}
