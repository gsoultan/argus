import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Group, NumberInput, Skeleton, Stack, Table, Text, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconCheck, IconDatabaseExport, IconLoader2, IconX,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { ErrorState } from '~/components/ErrorState'
import { Mono, absTime, bytes, relTime } from '~/components/primitives'
import {
  controlBackups, controlBackupsKey, useRunControlBackup, useSetControlBackupKeep, type ControlBackupsView,
} from '~/lib/controlBackups'
import { notifyOk, run } from '~/lib/notify'
import { FS, SP } from '~/theme'

/**
 * Settings › Control plane › Backups: argus-control's own backups, the ones
 * argus-backup.service takes on its timer and before every update.
 *
 * Until this page, how many were kept was a line in /etc/argus/backup.env on
 * the server and the only way to see them was a shell. Here: the last run and
 * how it went, a backup now, how many to keep, and every backup on disk.
 */
export function ControlBackupsSection() {
  const { data, error, isPending, refetch } = useQuery({
    queryKey: controlBackupsKey,
    queryFn: controlBackups,
    // Quick while one is asked for or running, so it shows finishing.
    refetchInterval: (q) => (q.state.data?.running || q.state.data?.requested ? 3_000 : 30_000),
    retry: false,
  })
  if (isPending) return <Skeleton h={420} />
  if (!data) {
    return (
      <ErrorState title="argus-control did not say how its backups are" error={error}
        reset={() => void refetch()} />
    )
  }
  return <ControlBackupsPanel v={data} />
}

export function ControlBackupsPanel({ v }: { v: ControlBackupsView }) {
  return (
    <Stack gap="sm" data-testid="control-backups">
      <Latest v={v} />
      <Keep v={v} />
      <Runs v={v} />
    </Stack>
  )
}

function Latest({ v }: { v: ControlBackupsView }) {
  const now = useRunControlBackup()
  const busy = v.running || Boolean(v.requested && !v.late)
  const l = v.last
  return (
    <SectionCard title="Backups of this control plane"
      description={<>The audit chain, the database and the recordings, into <Mono>{v.dir}</Mono>. One is also taken
        before every update.</>}
      action={
        <Button size="xs" leftSection={<IconDatabaseExport size={14} />} data-testid="backup-now"
          loading={now.isPending} disabled={!v.canRunNow || busy}
          onClick={() => void run(async () => {
            await now.mutateAsync()
            notifyOk('Backup asked for', 'It starts in a few seconds and shows here when it finishes.')
          }, { failure: 'Could not start a backup' })}>
          Back up now
        </Button>
      }>
      <Stack gap="sm">
        {!v.installed && (
          <Alert color="amber" icon={<IconAlertTriangle size={16} />} p="xs" data-testid="backup-not-installed">
            <Text size={FS.body} lh={1.5}>{v.reason}</Text>
          </Alert>
        )}
        {v.installed && !v.canRunNow && v.runNowReason && (
          <Text size={FS.meta} c="dimmed">{v.runNowReason}</Text>
        )}
        {v.running && l ? (
          <Line tone="busy" title="Backing up now" testId="backup-state">Started {relTime(l.startedAt)}.</Line>
        ) : v.requested ? (
          v.late ? (
            <Line tone="bad" title="The backup has not started" testId="backup-state">
              Asked for {relTime(v.requested.at)} by {v.requested.by}. The watcher that starts it,
              argus-backup-now.path, does not seem to be running: <Mono>systemctl enable --now argus-backup-now.path</Mono>
            </Line>
          ) : (
            <Line tone="busy" title="Starting a backup" testId="backup-state">Asked for by {v.requested.by}.</Line>
          )
        ) : l?.finishedAt ? (
          l.ok ? (
            <Line tone="ok" title={`Last backup ${relTime(l.finishedAt)}`} testId="backup-state">
              {absTime(l.finishedAt)}{l.pruned ? `, and ${l.pruned} older ${l.pruned === 1 ? 'one' : 'ones'} removed` : ''}.
            </Line>
          ) : (
            <Line tone="bad" title={`The last backup failed ${relTime(l.finishedAt)}`} testId="backup-state">
              {l.error}
            </Line>
          )
        ) : (
          <Line tone="warn" title="No backup has run yet" testId="backup-state">
            Press Back up now, or turn on the nightly one below.
          </Line>
        )}
        <Group gap="xs">
          <Badge variant="light" color={v.timerOn ? 'teal' : 'amber'} data-testid="backup-timer">
            {v.timerOn ? 'Nightly backup on' : 'Nightly backup off'}
          </Badge>
          {!v.timerOn && v.installed && (
            <Text size={FS.meta} c="dimmed">Turn it on, on the server: <Mono>sudo systemctl enable --now argus-backup.timer</Mono></Text>
          )}
        </Group>
      </Stack>
    </SectionCard>
  )
}

function Line({ tone, title, children, testId }: {
  tone: 'ok' | 'warn' | 'bad' | 'busy'; title: string; children?: React.ReactNode; testId?: string
}) {
  const color = { ok: 'teal', warn: 'amber', bad: 'rose', busy: 'azure' }[tone]
  const Icon = { ok: IconCheck, warn: IconAlertTriangle, bad: IconX, busy: IconLoader2 }[tone]
  return (
    <Group gap="sm" align="flex-start" wrap="nowrap" data-testid={testId} data-tone={tone}>
      <ThemeIcon size={22} radius="xl" color={color} variant="light"><Icon size={13} /></ThemeIcon>
      <Box style={{ minWidth: 0 }}>
        <Text size={FS.body} fw={600}>{title}</Text>
        {children && <Text size={FS.meta} c="dimmed" lh={1.5} style={{ wordBreak: 'break-word' }}>{children}</Text>}
      </Box>
    </Group>
  )
}

function Keep({ v }: { v: ControlBackupsView }) {
  const save = useSetControlBackupKeep()
  const [keep, setKeep] = useState<number | string>(v.keep)
  const n = typeof keep === 'number' ? keep : Number.parseInt(keep, 10)
  const valid = Number.isInteger(n) && n >= 1 && n <= 365
  const changed = valid && n !== v.keep
  return (
    <SectionCard title="How many to keep"
      description="Each backup removes the oldest database dumps beyond this. Recordings are copied once and never removed by count.">
      <Group gap="sm" align="flex-end" wrap="wrap">
        <NumberInput label="Database backups" value={keep} onChange={setKeep} min={1} max={365} w={160}
          error={valid ? undefined : 'From 1 to 365'} data-testid="backup-keep" />
        <Button size="sm" disabled={!changed} loading={save.isPending} data-testid="backup-keep-save"
          onClick={() => void run(async () => {
            await save.mutateAsync(n)
            notifyOk('Saved', `The next backup keeps the newest ${n}.`)
          }, { failure: 'Could not save' })}>
          Save
        </Button>
      </Group>
      <Text size={FS.meta} c="dimmed" mt={SP.cozy}>
        {v.keepSetHere
          ? <>Set here{v.keepSetBy ? ` by ${v.keepSetBy}` : ''}. This wins over ARGUS_BACKUP_KEEP in /etc/argus/backup.env.</>
          : <>From the server's settings until saved here.</>}
      </Text>
    </SectionCard>
  )
}

function Runs({ v }: { v: ControlBackupsView }) {
  // The complete ones past the count go at the next backup; say so.
  let complete = 0
  return (
    <SectionCard title="On disk" flush
      description={v.runs.length === 0 ? undefined : `${v.runs.filter((r) => r.complete).length} complete, newest first.`}>
      {v.readFailure ? (
        <Text size={FS.body} c="rose" p="md">{v.readFailure}</Text>
      ) : v.runs.length === 0 ? (
        <Text size={FS.body} c="dimmed" p="md">No backups yet.</Text>
      ) : (
        <Table.ScrollContainer minWidth={320}>
          <Table verticalSpacing={6} fz={FS.meta} data-testid="backup-runs">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Taken</Table.Th>
                <Table.Th visibleFrom="sm">Version</Table.Th>
                <Table.Th ta="right">Database</Table.Th>
                <Table.Th ta="right">Recordings</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {v.runs.map((r) => {
                if (r.complete) complete++
                const goes = r.complete && complete > v.keep
                return (
                  <Table.Tr key={r.name}>
                    <Table.Td>
                      <Text size={FS.meta}>{absTime(r.at)}</Text>
                      <Text size={FS.micro} c="dimmed" ff="monospace">{r.name}</Text>
                      {!r.complete
                        ? <Badge size="sm" variant="light" color="rose" mt={4}>Did not finish</Badge>
                        : goes
                          ? <Badge size="sm" variant="light" color="slate" mt={4}>Removed by the next backup</Badge>
                          : null}
                    </Table.Td>
                    <Table.Td visibleFrom="sm">{r.version || '—'}</Table.Td>
                    <Table.Td ta="right" ff="monospace">{r.complete ? bytes(r.databaseBytes) : '—'}</Table.Td>
                    <Table.Td ta="right" ff="monospace">{r.complete ? r.recordings.toLocaleString() : '—'}</Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </SectionCard>
  )
}
