import { useState } from 'react'
import { Alert, Badge, Box, Button, Checkbox, Group, Modal, Radio, ScrollArea, SegmentedControl, Stack, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { AnchorLink } from '~/components/links'
import { IconPackage, IconPower } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { MODE_LABEL, RunState } from '~/components/server/UpdatesTab'
import { isConfigured, type PackageFleetRow, type PackageReboot } from '~/lib/live'
import { run } from '~/lib/notify'
import { packageFleetQuery, useStartPackageRuns } from '~/lib/packageQueries'
import { FS, SP } from '~/theme'

type Filter = 'waiting' | 'security' | 'reboot' | 'all'

/**
 * Operating system updates across the fleet, on the Servers page: which
 * Linux servers have updates waiting, which of them fix security issues,
 * which want a reboot, and updating several together. Several are updated
 * one at a time, in the order picked, and a server that fails stops the
 * ones after it, so a bad update reaches one server rather than all.
 */
export function PackageFleetCard({ canEdit }: { canEdit: boolean }) {
  const { data: rows } = useQuery(packageFleetQuery())
  const [filter, setFilter] = useState<Filter>('waiting')
  const [picked, setPicked] = useState<string[]>([])
  const [asking, setAsking] = useState(false)
  if (!isConfigured() || !Array.isArray(rows) || rows.length === 0) return null

  const reported = rows.filter((r) => r.reported && r.supported)
  const waiting = reported.filter((r) => r.updates > 0)
  const security = reported.filter((r) => r.security > 0)
  const reboot = reported.filter((r) => r.rebootRequired)
  const shown = filter === 'waiting' ? waiting : filter === 'security' ? security : filter === 'reboot' ? reboot : rows
  const pickable = (r: PackageFleetRow) => canEdit && r.reported && r.supported && r.run === null
  const toggle = (id: string, on: boolean) =>
    setPicked((p) => (on ? [...p.filter((x) => x !== id), id] : p.filter((x) => x !== id)))
  const chosen = picked.map((id) => rows.find((r) => r.assetId === id)).filter((r): r is PackageFleetRow => Boolean(r && pickable(r)))

  return (
    <SectionCard
      title="Operating system updates"
      icon={IconPackage}
      flush
      badge={security.length > 0
        ? <Badge size="xs" variant="light" color="rose">{security.length} with security updates</Badge>
        : waiting.length === 0 ? <Badge size="xs" variant="light" color="teal">up to date</Badge> : undefined}
      description={`${reported.length} Linux server${reported.length === 1 ? '' : 's'} report their packages · ${waiting.length} have updates waiting, ${security.length} of them security.`}
    >
      <Group px="md" py="xs" justify="space-between" wrap="wrap" gap="xs">
        <SegmentedControl size="xs" value={filter} onChange={(f) => setFilter(f as Filter)} data={[
          { value: 'waiting', label: `Updates waiting ${waiting.length}` },
          { value: 'security', label: `Security ${security.length}` },
          { value: 'reboot', label: `Reboot needed ${reboot.length}` },
          { value: 'all', label: `All ${rows.length}` },
        ]} />
        {canEdit && (
          <Group gap={SP.cozy}>
            {chosen.length > 0 && (
              <Text size={FS.meta} c="dimmed">
                {chosen.length} selected · {chosen.reduce((n, r) => n + r.updates, 0)} updates
              </Text>
            )}
            <Button size="compact-sm" disabled={chosen.length === 0} onClick={() => setAsking(true)}
              color={filter === 'reboot' ? 'amber' : undefined}
              leftSection={filter === 'reboot' ? <IconPower size={14} /> : undefined}>
              {filter === 'reboot' ? 'Reboot' : 'Update'} {chosen.length || ''} server{chosen.length === 1 ? '' : 's'}…
            </Button>
          </Group>
        )}
      </Group>
      {shown.length === 0 ? (
        <Box px="md" pb="md">
          <Text size={FS.body} c="dimmed">
            {filter === 'waiting' ? 'Every server that reports its packages is up to date.' : 'None.'}
          </Text>
        </Box>
      ) : (
        <ScrollArea type="auto">
          <Table verticalSpacing={6} fz="sm" miw={640}>
            <Table.Thead>
              <Table.Tr>
                {canEdit && <Table.Th w={36} />}
                <Table.Th>Server</Table.Th>
                <Table.Th>Updates</Table.Th>
                <Table.Th>Automatic updates</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {shown.map((r) => (
                <Table.Tr key={r.assetId}>
                  {canEdit && (
                    <Table.Td>
                      <Checkbox size="xs" aria-label={`Update ${r.hostname}`} disabled={!pickable(r)}
                        checked={picked.includes(r.assetId) && pickable(r)}
                        onChange={(e) => toggle(r.assetId, e.currentTarget.checked)} />
                    </Table.Td>
                  )}
                  <Table.Td>
                    <AnchorLink to="/assets/$assetId" params={{ assetId: r.assetId }} search={{ tab: 'updates' }}
                      size="sm" fw={600}>
                      {r.hostname}
                    </AnchorLink>
                    <Text size={FS.micro} c="dimmed">{r.os || 'Linux'}{r.tags.length > 0 ? ` · ${r.tags.join(', ')}` : ''}</Text>
                  </Table.Td>
                  <Table.Td><FleetUpdates row={r} /></Table.Td>
                  <Table.Td><Text size={FS.meta} c="dimmed">{MODE_LABEL[r.mode]}</Text></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </ScrollArea>
      )}
      {asking && <AskFleet rows={chosen} reboot={filter === 'reboot'} onClose={() => setAsking(false)}
        onDone={() => { setAsking(false); setPicked([]) }} />}
    </SectionCard>
  )
}

function FleetUpdates({ row: r }: { row: PackageFleetRow }) {
  if (!r.reported) return <Text size={FS.meta} c="dimmed">agent too old to report</Text>
  if (!r.supported) return <Text size={FS.meta} c="dimmed">{r.reason || 'cannot update from here'}</Text>
  return (
    <Group gap={SP.tight} wrap="wrap">
      {r.run && <RunState state={r.run.state} />}
      {r.checkedAt === null ? (
        <Text size={FS.meta} c="dimmed">not looked yet</Text>
      ) : r.updates === 0 ? (
        <Text size={FS.meta} c="dimmed">up to date · {relTime(r.checkedAt)}</Text>
      ) : (
        <Text size="sm"><b>{r.updates}</b> <Text span size={FS.meta} c="dimmed">waiting</Text></Text>
      )}
      {r.security > 0 && <Badge size="xs" variant="light" color="rose">{r.security} security</Badge>}
      {r.rebootRequired && <Badge size="xs" variant="light" color="amber">Reboot needed</Badge>}
      {r.error && <Badge size="xs" variant="light" color="slate" title={r.error}>last look failed</Badge>}
    </Group>
  )
}

function AskFleet({ rows, reboot: rebootOnly, onClose, onDone }: {
  rows: PackageFleetRow[]
  /** Picked from the servers that need a reboot: reboot them, install nothing. */
  reboot: boolean
  onClose: () => void
  onDone: () => void
}) {
  const start = useStartPackageRuns()
  const tooOld = rows.filter((r) => !r.canReboot)
  const [mode, setMode] = useState<'security' | 'all' | 'reboot'>(
    rebootOnly && tooOld.length === 0 ? 'reboot' : rows.some((r) => r.security > 0) ? 'security' : 'all')
  const [reboot, setReboot] = useState<PackageReboot>('never')
  const verb = mode === 'reboot' ? 'Reboot' : 'Update'
  return (
    <Modal opened onClose={onClose} title={`${verb} ${rows.length} server${rows.length === 1 ? '' : 's'}`} size="md">
      <Stack gap="sm">
        <Text size={FS.body} lh={1.5}>
          {mode === 'reboot'
            ? 'One at a time, in this order: each reboots once the one before it is back. If one fails to come back, the servers after it are not rebooted.'
            : 'One at a time, in this order. If one fails or is interrupted, the servers after it are not started, so a bad update reaches one server rather than all of them.'}
        </Text>
        <Text size={FS.meta} ff="monospace" c="dimmed">{rows.map((r, i) => `${i + 1}. ${r.hostname}`).join('\n')}</Text>
        <Radio.Group label="Do" value={mode} onChange={(m) => setMode(m as 'security' | 'all' | 'reboot')}>
          <Stack gap={SP.cozy} mt={SP.snug}>
            <Radio value="security" label="Install security updates only" />
            <Radio value="all" label="Install all updates" description="Held packages are left out either way." />
            <Radio value="reboot" label="Reboot only; install nothing" disabled={tooOld.length > 0}
              description={tooOld.length > 0
                ? `Not for ${tooOld.map((r) => r.hostname).join(', ')}: the agent is too old to reboot from here.`
                : 'For servers waiting for a reboot after their updates.'} />
          </Stack>
        </Radio.Group>
        {mode !== 'reboot' && (
          <Radio.Group label="If an update needs a reboot" value={reboot} onChange={(x) => setReboot(x as PackageReboot)}>
            <Stack gap={SP.cozy} mt={SP.snug}>
              <Radio value="never" label="Do not reboot; tell me it is needed" />
              <Radio value="if_needed" label="Reboot each server at the end of its run" />
            </Stack>
          </Radio.Group>
        )}
        <Alert color="slate" p="xs">
          <Text size={FS.meta}>Servers whose rule needs a second admin wait for approval in their turn.</Text>
        </Alert>
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={start.isPending} color={mode === 'reboot' ? 'rose' : undefined} onClick={() => void run(
            () => start.mutateAsync({ assetIds: rows.map((r) => r.assetId), mode, reboot: mode === 'reboot' ? 'if_needed' : reboot }),
            mode === 'reboot'
              ? { failure: 'Reboots not started', success: ['Reboots asked for', `${rows[0]?.hostname ?? 'The first server'} reboots within 30 seconds; the others follow one by one.`] }
              : { failure: 'Updates not started', success: ['Updates asked for', `${rows[0]?.hostname ?? 'The first server'} starts within 30 seconds; the others follow one by one.`] },
          ).then((ok) => ok && onDone())}>
            {verb} {rows.length} server{rows.length === 1 ? '' : 's'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
