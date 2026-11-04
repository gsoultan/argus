import type { ReactNode } from 'react'
import {
  Alert, Badge, Box, Card, Group, SimpleGrid, Skeleton, Stack, Table, Text, ThemeIcon, UnstyledButton,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconCheck, IconCpu, IconDatabase, IconExclamationMark, IconListCheck,
  IconBug, IconX,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Mono, absTime, relTime } from '~/components/primitives'
import { Meter, bytes } from '~/components/server/SystemCard'
import { ErrorState } from '~/components/ErrorState'
import { controlStatus, type ControlStatus as Status, type StatusProblem } from '~/lib/live'
import { FS, SP } from '~/theme'

/**
 * How argus-control itself is doing: the machine it runs on, its database,
 * where recordings go, its certificate, and the requests that failed.
 *
 * Platform owners only, and only against a real control plane: there is no
 * fixture of a server's health worth showing, and a made-up "Healthy" is the
 * one thing this page must never say. Every number comes from the control
 * plane's own report (`GET /api/v1/platform/status`); this page only decides
 * how to show it. Which things are problems is decided there too, so the pill,
 * the tiles and the checks cannot disagree with each other.
 */
export { statusKey } from '~/lib/queries'
import { statusKey } from '~/lib/queries'

export function ControlStatusSection({ onOpen }: { onOpen: (section: string) => void }) {
  const { data, error, isPending, refetch } = useQuery({
    queryKey: statusKey,
    queryFn: controlStatus,
    refetchInterval: 15_000,
  })

  if (isPending) return <Skeleton h={420} />
  if (!data) {
    return (
      <ErrorState
        title="argus-control did not report its status"
        error={error}
        reset={() => void refetch()}
      />
    )
  }
  return <StatusView st={data} stale={Boolean(error)} onOpen={onOpen} />
}

export function StatusView({ st, stale, onOpen }: { st: Status; stale?: boolean; onOpen: (section: string) => void }) {
  const byArea = (area: StatusProblem['area']) => st.problems.filter((p) => p.area === area)
  const critical = st.problems.filter((p) => p.severity === 'critical')

  return (
    <Stack gap="sm" data-testid="control-status">
      <Header st={st} stale={stale} />

      {critical.map((p) => (
        <Alert key={p.title} color="rose" icon={<IconAlertTriangle size={16} />} title={p.title} p="xs">
          {p.detail && <Text size={FS.body} lh={1.5}>{p.detail}</Text>}
        </Alert>
      ))}

      <SimpleGrid cols={{ base: 2, md: 4 }} spacing="sm">
        <Tile
          label="Servers connected"
          value={st.fleet ? <>{st.fleet.agentsConnected} <Small>of {st.fleet.agentsEnrolled}</Small></> : '—'}
          sub={st.fleet
            ? st.fleet.agentsEnrolled === 0
              ? 'No agent is enrolled yet'
              : st.fleet.agentsEnrolled > st.fleet.agentsConnected
                ? `${st.fleet.agentsEnrolled - st.fleet.agentsConnected} not connected`
                : 'Every enrolled agent'
            : 'Needs the database'}
          tone={st.fleet && st.fleet.agentsEnrolled > st.fleet.agentsConnected ? 'amber' : undefined}
        />
        <Tile
          label="Live sessions"
          value={st.fleet ? String(st.fleet.sessionsActive) : '—'}
          sub={st.fleet
            ? st.fleet.sessionsSilent > 0
              ? `${st.fleet.sessionsSilent} gone silent`
              : `${st.fleet.sessionsToday} in the last 24 h`
            : 'Needs the database'}
          tone={st.fleet && st.fleet.sessionsSilent > 0 ? 'amber' : undefined}
        />
        <Tile
          label="Database"
          value={st.database.ok ? <>{ms(st.database.latencyMs)} <Small>ms</Small></> : 'Down'}
          sub={`${st.database.inUse} of ${st.database.max} connections in use`}
          tone={!st.database.ok ? 'rose' : byArea('database').length > 0 ? 'amber' : undefined}
        />
        <Tile
          label="Errors, last hour"
          value={String(st.errors.lastHour)}
          sub={`${st.errors.sinceStart} since it started`}
          tone={byArea('errors').length > 0 ? 'rose' : undefined}
        />
      </SimpleGrid>

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="sm">
        <Machine st={st} />
        <Checks st={st} byArea={byArea} onOpen={onOpen} />
      </SimpleGrid>

      <Storage st={st} />
      <RecentErrors st={st} />
    </Stack>
  )
}

function Header({ st, stale }: { st: Status; stale?: boolean }) {
  const n = st.problems.length
  const worst = st.problems.some((p) => p.severity === 'critical') ? 'rose' : 'amber'
  const mem = st.machine.memTotal ? ` · ${bytes(st.machine.memTotal)}` : ''
  return (
    <Card padding="md">
      <Group justify="space-between" align="flex-start" gap="sm">
        <Box style={{ minWidth: 0 }}>
          <Group gap={SP.cozy}>
            <Text fw={650} size="lg">argus-control</Text>
            {n === 0
              ? <Badge color="teal" variant="light" data-testid="status-pill">Healthy</Badge>
              : <Badge color={worst} variant="light" data-testid="status-pill">
                  {n} problem{n === 1 ? '' : 's'}
                </Badge>}
          </Group>
          <Text size={FS.meta} c="dimmed" mt={SP.tight}>
            {st.hostname && <><Mono>{st.hostname}</Mono> · </>}
            Version <Mono>{st.version || 'unknown'}</Mono> · Up {uptime(st.uptimeSeconds)}, since{' '}
            {absTime(st.startedAt)}
            {st.machine.os && <> · {st.machine.os}, {st.machine.cores} cores{mem}</>}
          </Text>
        </Box>
        <Text size={FS.meta} c={stale ? 'amber.7' : 'dimmed'}>
          {stale ? `Not answering; last report ${relTime(st.checkedAt)}` : `Checked ${relTime(st.checkedAt)}`}
        </Text>
      </Group>
    </Card>
  )
}

function Tile({ label, value, sub, tone }: {
  label: string
  value: ReactNode
  sub: string
  tone?: 'amber' | 'rose'
}) {
  return (
    <Card padding="md" data-tone={tone}>
      <Text size={FS.meta} c="dimmed">{label}</Text>
      <Text fw={650} size="xl" mt={SP.tight} c={tone ? `${tone}.7` : undefined}>{value}</Text>
      <Text size={FS.meta} c={tone ? `${tone}.7` : 'dimmed'} mt={SP.hair}>{sub}</Text>
    </Card>
  )
}

function Small({ children }: { children: ReactNode }) {
  return <Text span size={FS.body} c="dimmed" fw={500}>{children}</Text>
}

function Machine({ st }: { st: Status }) {
  const m = st.machine
  const memUsed = m.memTotal && m.memAvailable !== undefined ? m.memTotal - m.memAvailable : undefined
  const reported = m.memTotal || m.load1 !== undefined || m.disks.length > 0
  return (
    <SectionCard title="This machine" icon={IconCpu} iconColor="slate">
      <Stack gap="md">
        {!reported && (
          <Text size={FS.meta} c="dimmed">
            The processor, memory and disks are read from /proc, which this operating system
            ({m.os || 'unknown'}) does not have.
          </Text>
        )}
        {(m.cpuBusy !== undefined || m.load1 !== undefined) && (
          <Meter
            label="Processor"
            value={m.cpuBusy !== undefined
              ? <><b>{Math.round(m.cpuBusy)}%</b> busy</>
              : <>load <b>{m.load1!.toFixed(2)}</b></>}
            pct={m.cpuBusy ?? (m.cores ? ((m.load1 ?? 0) / m.cores) * 100 : 0)}
            tone={toneFor((m.cpuBusy ?? 0) / 100, 0.8, 0.95)}
            sub={m.load1 !== undefined ? `Load ${m.load1.toFixed(2)} on ${m.cores} cores` : undefined}
          />
        )}
        {m.memTotal && memUsed !== undefined ? (
          <Meter
            label="Memory"
            value={<><b>{bytes(memUsed)}</b> of {bytes(m.memTotal)}</>}
            pct={(memUsed / m.memTotal) * 100}
            tone={toneFor(memUsed / m.memTotal, 0.9, 0.97)}
          />
        ) : null}
        {m.disks.map((d) => {
          const used = d.total > 0 ? 1 - d.avail / d.total : 0
          const tone = toneFor(used, 0.8, 0.9)
          return (
            <Meter
              key={d.path}
              testId={`status-disk-${d.path}`}
              label={<Mono>{d.path}</Mono>}
              value={tone
                ? <><Text span inherit c={`${tone}.6`} fw={600}>{Math.round(used * 100)}%</Text> · {bytes(d.avail)} free</>
                : <><b>{bytes(d.total - d.avail)}</b> of {bytes(d.total)}</>}
              pct={used * 100}
              tone={tone}
              sub={tone ? d.use : `${bytes(d.avail)} free · ${d.use}`}
            />
          )
        })}
        <Box>
          <Text size={FS.micro} c="dimmed" fw={600} tt="uppercase" style={{ letterSpacing: '0.05em' }}>
            argus-control
          </Text>
          <Rows rows={[
            ['Memory in use', bytes(st.process.memoryBytes)],
            ['Goroutines', st.process.goroutines.toLocaleString('en-US')],
            ['Agents on this replica', String(st.process.tunnels)],
            ['Built with', st.process.goVersion],
          ]} />
        </Box>
      </Stack>
    </SectionCard>
  )
}

function Checks({ st, byArea, onOpen }: {
  st: Status
  byArea: (a: StatusProblem['area']) => StatusProblem[]
  onOpen: (section: string) => void
}) {
  const db = st.database
  const r = st.recordings
  const c = st.certificate
  const dbProblems = byArea('database')
  const recProblems = byArea('recordings')
  const certProblems = byArea('certificate')

  return (
    <SectionCard title="Checks" icon={IconListCheck} iconColor="teal" flush>
      <Stack gap={0}>
        <Check
          state={!db.ok ? 'bad' : dbProblems.length ? 'warn' : 'ok'}
          name="Database"
          detail={db.ok
            ? `${db.version ? `PostgreSQL ${db.version} ` : ''}answered in ${ms(db.latencyMs)} ms; ${db.inUse} of ${db.max} connections in use`
            : db.error || 'Not answering'}
          action={dbProblems.length || !db.ok ? { label: 'Database settings', go: () => onOpen('database') } : undefined}
        />
        <Check
          state={r.where === '' ? 'warn' : recProblems.length ? 'warn' : 'ok'}
          name="Recording storage"
          detail={recordingLine(st)}
          action={r.where === '' || recProblems.length ? { label: 'Recording settings', go: () => onOpen('storage') } : undefined}
        />
        <Check
          state={!c ? 'none' : c.daysLeft < 0 ? 'bad' : certProblems.length ? 'warn' : 'ok'}
          name="Certificate"
          detail={!c
            ? 'Served without TLS here; a proxy in front is expected to provide it.'
            : c.daysLeft < 0
              ? `${c.subject} expired on ${day(c.notAfter)}`
              : `${c.subject}, valid until ${day(c.notAfter)} (${c.daysLeft} days)${c.managed ? '; renewed by argus-control' : ''}`}
          action={certProblems.length ? { label: 'Certificate', go: () => onOpen('certificate') } : undefined}
        />
        {byArea('memory').concat(byArea('disk').filter((p) => p.severity !== 'critical')).map((p) => (
          <Check key={p.title} state="warn" name={p.title} detail={p.detail || ''} />
        ))}
      </Stack>
    </SectionCard>
  )
}

function recordingLine(st: Status): string {
  const r = st.recordings
  if (r.where === '') return 'Nowhere is configured, so recordings stay on the servers that made them.'
  const where = r.where === 'bucket' ? `Bucket ${r.location || '(unnamed)'}` : `Directory ${r.location}`
  if (r.lastFailure && (!r.lastUpload || r.lastFailure > r.lastUpload)) {
    return `${where}: the last recording could not be stored ${relTime(r.lastFailure)}. ${r.lastUploadError ?? ''}`
  }
  return r.lastUpload
    ? `${where}; last recording stored ${relTime(r.lastUpload)}`
    : `${where}; no recording has arrived since argus-control started`
}

function Check({ state, name, detail, action }: {
  state: 'ok' | 'warn' | 'bad' | 'none'
  name: string
  detail: string
  action?: { label: string; go: () => void }
}) {
  const icon = {
    ok: { color: 'teal', I: IconCheck },
    warn: { color: 'amber', I: IconExclamationMark },
    bad: { color: 'rose', I: IconX },
    none: { color: 'slate', I: IconCheck },
  }[state]
  return (
    <Group
      gap="sm" wrap="nowrap" align="flex-start" px="md" py="sm"
      style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}
      data-testid={`check-${name}`} data-state={state}
    >
      <ThemeIcon size={18} radius="xl" color={icon.color} variant={state === 'none' ? 'light' : 'filled'} mt={1}>
        <icon.I size={11} stroke={3} />
      </ThemeIcon>
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Text size={FS.body} fw={550} lh={1.4}>{name}</Text>
        {detail && <Text size={FS.meta} c="dimmed" lh={1.45} mt={SP.hair} style={{ overflowWrap: 'anywhere' }}>{detail}</Text>}
      </Box>
      {action && (
        <UnstyledButton onClick={action.go} style={{ flexShrink: 0 }}>
          <Text size={FS.meta} c="azure.6" fw={500}>{action.label} ›</Text>
        </UnstyledButton>
      )}
    </Group>
  )
}

const TABLE_NAMES: Record<string, string> = {
  audit_events: 'Audit log',
  session_events: 'Session events',
  sessions: 'Sessions',
  assets: 'Servers',
  db_statements: 'Database statements',
  console_alerts: 'Console alerts',
}

function Storage({ st }: { st: Status }) {
  const db = st.database
  const r = st.recordings
  const measured = db.measuredAt ?? r.measuredAt
  return (
    <SectionCard
      title="Storage"
      icon={IconDatabase}
      iconColor="azure"
      action={measured ? <Text size={FS.meta} c="dimmed">Measured {relTime(measured)}</Text> : undefined}
    >
      <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="lg">
        <Box>
          <Label>Database</Label>
          <Text fw={650} size="lg">
            {db.sizeBytes !== undefined ? bytes(db.sizeBytes) : '—'}{' '}
            {db.name && <Small>{db.name}</Small>}
          </Text>
          <Rows rows={db.tables.map((t) => [
            t.name ? (TABLE_NAMES[t.name] ?? t.name) : 'Everything else',
            bytes(t.bytes),
          ])} />
        </Box>
        <Box>
          <Label>Recordings</Label>
          <Text fw={650} size="lg">
            {r.measuredAt ? bytes(r.bytes) : '—'}{' '}
            {r.measuredAt && <Small>{r.count.toLocaleString('en-US')} recording{r.count === 1 ? '' : 's'}</Small>}
          </Text>
          <Rows rows={[
            ['Where', r.where === 'bucket' ? `Bucket ${r.location ?? ''}` : r.where === 'directory' ? r.location ?? '' : 'Not configured'],
            ...(r.measuredAt ? [['Added in the last 7 days', bytes(r.weekBytes)] as [string, string]] : []),
            ...(r.oldest ? [['Oldest', day(r.oldest)] as [string, string]] : []),
          ]} />
          <Text size={FS.micro} c="dimmed" mt={SP.cozy}>
            Sizes as each agent reported its recording.
          </Text>
        </Box>
      </SimpleGrid>
    </SectionCard>
  )
}

function RecentErrors({ st }: { st: Status }) {
  const list = st.errors.recent
  return (
    <SectionCard
      title="Recent errors"
      icon={IconBug}
      iconColor="rose"
      description={`Requests that failed on this server since it started ${absTime(st.startedAt)}. A restart clears the list; the log keeps every one.`}
      flush
    >
      {list.length === 0 ? (
        <Text size={FS.meta} c="dimmed" px="md" pb="md">None.</Text>
      ) : (
        <Table.ScrollContainer minWidth={420}>
          <Table verticalSpacing="xs" data-testid="recent-errors">
            <Table.Tbody>
              {list.map((e) => (
                <Table.Tr key={e.op + e.message}>
                  <Table.Td w={130} style={{ verticalAlign: 'top' }}>
                    <Text size={FS.meta} c="dimmed">{relTime(e.at)}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size={FS.meta} fw={500}>{e.op}</Text>
                    <Text size={FS.meta} c="dimmed" style={{ overflowWrap: 'anywhere' }}>
                      {e.message}
                      {e.count > 1 && <Badge ml={SP.snug} size="xs" color="slate" variant="light">×{e.count}</Badge>}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </SectionCard>
  )
}

function Label({ children }: { children: ReactNode }) {
  return (
    <Text size={FS.micro} c="dimmed" fw={600} tt="uppercase" style={{ letterSpacing: '0.05em' }}>
      {children}
    </Text>
  )
}

function Rows({ rows }: { rows: [string, string][] }) {
  return (
    <Stack gap={SP.tight} mt={SP.cozy}>
      {rows.map(([k, v]) => (
        <Group key={k} justify="space-between" gap="xs" wrap="nowrap">
          <Text size={FS.meta}>{k}</Text>
          <Text size={FS.meta} c="dimmed" ta="right" style={{ overflowWrap: 'anywhere' }}>{v}</Text>
        </Group>
      ))}
    </Stack>
  )
}

/** Milliseconds: a decimal below 10, where a rounded 0 would read as no time at all. */
function ms(v: number): string {
  return v < 10 ? v.toFixed(1) : String(Math.round(v))
}

function toneFor(share: number, warn: number, bad: number): 'amber' | 'rose' | undefined {
  return share >= bad ? 'rose' : share >= warn ? 'amber' : undefined
}

function uptime(secs: number): string {
  const d = Math.floor(secs / 86400)
  const h = Math.floor((secs % 86400) / 3600)
  const m = Math.floor((secs % 3600) / 60)
  if (d > 0) return `${d} day${d === 1 ? '' : 's'}`
  if (h > 0) return `${h} h ${m} min`
  return `${m} min`
}

function day(iso: string): string {
  return new Date(iso).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' })
}

/** Exported for the section list: the count beside "Status". */
export function useStatusProblems(enabled: boolean): StatusProblem[] | undefined {
  const { data } = useQuery({ queryKey: statusKey, queryFn: controlStatus, enabled, refetchInterval: 60_000 })
  return data?.problems
}
