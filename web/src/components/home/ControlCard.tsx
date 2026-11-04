import { Badge, Group, Skeleton, Stack, Text, ThemeIcon } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconCheck, IconExclamationMark, IconServer2, IconX } from '@tabler/icons-react'
import { AnchorLink } from '~/components/links'
import { SectionCard } from '~/components/page'
import { Mono, relTime } from '~/components/primitives'
import { Meter } from '~/components/server/SystemCard'
import { bytes } from '~/components/primitives'
import { isStatus } from '~/components/home/parts'
import { controlStatusQuery } from '~/lib/queries'
import type { ControlStatus, StatusProblem } from '~/lib/live'
import { FS, SP } from '~/theme'

/**
 * Home's summary of argus-control's own health, for platform managers. Its
 * own chunk: nobody else draws it, and it brings the meters with it.
 */
export default function ControlCard() {
  const { data, error } = useQuery(controlStatusQuery())
  // A control plane older than the status endpoint answers something else;
  // that is "did not report", not a page that fails to draw.
  const st = isStatus(data) ? data : undefined
  const n = st?.problems.length ?? 0
  return (
    <SectionCard
      title="argus-control"
      icon={IconServer2}
      iconColor="slate"
      badge={st ? (
        n === 0
          ? <Badge color="teal" variant="light">Healthy</Badge>
          : <Badge color={st.problems.some((p) => p.severity === 'critical') ? 'rose' : 'amber'} variant="light">
              {n} problem{n === 1 ? '' : 's'}
            </Badge>
      ) : undefined}
      action={<AnchorLink to="/settings" search={{ section: 'status' }} size={FS.meta}>Status ›</AnchorLink>}
    >
      {!st ? (
        error || data !== undefined
          ? <Text size={FS.meta} c="dimmed">argus-control did not report its status.</Text>
          : <Skeleton h={160} />
      ) : (
        <ControlSummary st={st} />
      )}
    </SectionCard>
  )
}

function ControlSummary({ st }: { st: ControlStatus }) {
  const has = (a: StatusProblem['area']) => st.problems.filter((p) => p.area === a)
  const worst = (ps: StatusProblem[]) => ps.some((p) => p.severity === 'critical') ? 'bad' : ps.length ? 'warn' : 'ok'
  const m = st.machine
  const memUsed = m.memTotal && m.memAvailable !== undefined ? m.memTotal - m.memAvailable : undefined
  const disk = m.disks[0]
  const c = st.certificate
  return (
    <Stack gap="sm" data-testid="control-summary">
      <Text size={FS.meta} c="dimmed">
        Version <Mono>{st.version || 'unknown'}</Mono> · up {uptime(st.uptimeSeconds)}
      </Text>
      <Stack gap={SP.snug}>
        <CheckLine state={st.database.ok ? worst(has('database')) : 'bad'} name="Database"
          detail={st.database.ok ? `${ms(st.database.latencyMs)} ms, ${st.database.inUse} of ${st.database.max} connections` : 'not answering'} />
        <CheckLine state={st.recordings.where === '' ? 'warn' : worst(has('recordings'))} name="Recording storage"
          detail={st.recordings.where === '' ? 'not configured'
            : st.recordings.lastUpload ? `last upload ${relTime(st.recordings.lastUpload)}` : 'nothing uploaded since it started'} />
        {c && (
          <CheckLine state={c.daysLeft < 0 ? 'bad' : worst(has('certificate'))} name="Certificate"
            detail={c.daysLeft < 0 ? 'expired' : `expires in ${c.daysLeft} days`} />
        )}
        <CheckLine state={worst(has('errors'))} name="Errors"
          detail={st.errors.lastHour === 0 ? 'none in the last hour' : `${st.errors.lastHour} in the last hour`} />
      </Stack>
      {m.cpuBusy !== undefined && (
        <Meter label="Processor" value={<><b>{Math.round(m.cpuBusy)}%</b></>} pct={m.cpuBusy} />
      )}
      {m.memTotal && memUsed !== undefined ? (
        <Meter label="Memory" value={<><b>{bytes(memUsed)}</b> of {bytes(m.memTotal)}</>} pct={(memUsed / m.memTotal) * 100}
          tone={has('memory').length ? 'amber' : undefined} />
      ) : null}
      {disk && (
        <Meter
          label={<Mono>{disk.path}</Mono>}
          value={<><b>{bytes(disk.total - disk.avail)}</b> of {bytes(disk.total)}</>}
          pct={disk.total ? (1 - disk.avail / disk.total) * 100 : 0}
          tone={(() => { const ps = has('disk'); return ps.some((p) => p.severity === 'critical') ? 'rose' : ps.length ? 'amber' : undefined })()}
        />
      )}
    </Stack>
  )
}

function CheckLine({ state, name, detail }: { state: 'ok' | 'warn' | 'bad'; name: string; detail: string }) {
  const s = { ok: { c: 'teal', I: IconCheck }, warn: { c: 'amber', I: IconExclamationMark }, bad: { c: 'rose', I: IconX } }[state]
  return (
    <Group gap={SP.cozy} wrap="nowrap" align="flex-start" data-check={name} data-state={state}>
      <ThemeIcon size={16} radius="xl" color={s.c} mt={1}><s.I size={10} stroke={3} /></ThemeIcon>
      <Text size={FS.meta} lh={1.45}>{name} <Text span inherit c="dimmed">· {detail}</Text></Text>
    </Group>
  )
}


function ms(v: number): string {
  return v < 10 ? v.toFixed(1) : String(Math.round(v))
}

function uptime(secs: number): string {
  const d = Math.floor(secs / 86400)
  if (d > 0) return `${d} day${d === 1 ? '' : 's'}`
  const h = Math.floor(secs / 3600)
  return h > 0 ? `${h} h` : `${Math.floor(secs / 60)} min`
}
