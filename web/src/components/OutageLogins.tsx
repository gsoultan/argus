import { useEffect, useState } from 'react'
import { Alert, Badge, Button, Group, SegmentedControl, Skeleton, Stack, Switch, TagsInput, Text } from '@mantine/core'
import { IconAlertTriangle, IconPlugConnectedX } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { run } from '~/lib/notify'
import {
  OUTAGE_CHOICES, outageAfterLabel, outageGroupsError, useAssetOutageLogins, useOutageLogins,
  useSaveOutageLogins, useSetAssetOutageLoginsOff,
} from '~/lib/outageLogins'
import { FS, SP } from '~/theme'

/**
 * Settings › Logins: who is let in over plain SSH while a server cannot reach
 * Argus. Saved on its own, apart from the session policy.
 */
export function OutageLoginsCard({ canEdit }: { canEdit: boolean }) {
  const { data: saved } = useOutageLogins()
  const save = useSaveOutageLogins()
  const [after, setAfter] = useState<string | null>(null)
  const [groups, setGroups] = useState<string[] | null>(null)
  useEffect(() => { setAfter(null); setGroups(null) }, [saved])

  if (!saved) {
    return <SectionCard title="While Argus cannot be reached" icon={IconPlugConnectedX}><Skeleton h={160} /></SectionCard>
  }
  const a = after ?? String(saved.afterMinutes)
  const g = groups ?? saved.groups
  const dirty = a !== String(saved.afterMinutes) || g.join(',') !== saved.groups.join(',')
  const error = outageGroupsError(g)
  const on = a !== '0'

  const commit = () => void run(
    () => save.mutateAsync({ afterMinutes: Number(a), groups: g }),
    {
      success: ['Saved',
        on
          ? `Servers that refuse direct SSH let ${g.join(', ')} in after ${outageAfterLabel(Number(a))} without Argus. Agents pick it up within a minute.`
          : 'Servers refuse everyone while Argus cannot be reached.'],
      failure: 'Not saved',
    },
  )

  return (
    <SectionCard
      title="While Argus cannot be reached"
      icon={IconPlugConnectedX}
      iconColor="amber"
      description="When a server has not heard from Argus for a while, let named admins in over plain SSH so they can fix things. They are still recorded on the server, and the recording and an alert reach Argus when it is back. Off by default."
      badge={dirty ? <Badge size="xs" color="amber" variant="outline">unsaved</Badge> : undefined}
    >
      <Stack gap="sm">
        <Group gap="md" wrap="wrap" align="center">
          <Text size={FS.meta} c="dimmed" w={110}>Let in after</Text>
          <SegmentedControl size="xs" value={a} onChange={setAfter} disabled={!canEdit}
            data={OUTAGE_CHOICES.map((c) => ({ value: c.value, label: c.label }))}
            aria-label="Let in after" />
        </Group>
        <Group gap="md" wrap="nowrap" align="flex-start">
          <Text size={FS.meta} c="dimmed" w={110} pt={6}>Who</Text>
          <TagsInput size="xs" style={{ flex: 1 }} value={g} onChange={setGroups} disabled={!canEdit}
            splitChars={[',', ' ']} aria-label="Groups let in" placeholder="Add a group"
            description="Unix groups on the server. Root is let in only if one of these is root."
            error={dirty ? error : null} />
        </Group>
        <Group gap="md" wrap="nowrap" align="flex-start">
          <Text size={FS.meta} c="dimmed" w={110}>Recording</Text>
          <Text size={FS.meta}>Always. A login that cannot be recorded is refused, as now.</Text>
        </Group>
        <Group gap="md" wrap="nowrap" align="flex-start">
          <Text size={FS.meta} c="dimmed" w={110}>Applies to</Text>
          <Text size={FS.meta}>
            Servers set to refuse or ask for approval <Text span c="dimmed" size={FS.meta}>· a server&rsquo;s Access tab can turn it off</Text>
          </Text>
        </Group>
        {on && (
          <Alert color="amber" variant="light" p="xs" icon={<IconAlertTriangle size={16} />}>
            <Text size={FS.meta} lh={1.5}>
              Someone who can cut a server off from Argus, and holds a password or key for one of
              these groups, gets in after this wait without anyone approving it. They are still recorded.
            </Text>
          </Alert>
        )}
        <Text size={FS.micro} c="dimmed" lh={1.5}>
          Agents older than this setting ignore it and keep refusing. Everyone else is still refused,
          and told which groups are let in.
        </Text>
        {canEdit && dirty && (
          <Group justify="flex-end" gap={SP.snug}>
            <Button size="xs" variant="subtle" color="slate" onClick={() => { setAfter(null); setGroups(null) }}>Discard</Button>
            <Button size="xs" color={on ? 'rose' : 'teal'} loading={save.isPending} disabled={error !== null} onClick={commit}>
              Save
            </Button>
          </Group>
        )}
      </Stack>
    </SectionCard>
  )
}

/** One server's Access tab: follow the setting in Settings, or turn it off here. */
export function ServerOutageLogins({ assetId, hostname, canEdit }: {
  assetId: string; hostname: string; canEdit: boolean
}) {
  const { data } = useAssetOutageLogins(assetId)
  const set = useSetAssetOutageLoginsOff(assetId)
  if (!data) return null
  const on = data.afterMinutes > 0
  return (
    <Stack gap={SP.tight} pt="sm" mt={SP.tight} style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
      <Text size="sm" fw={600}>While Argus cannot be reached</Text>
      <Switch
        size="xs"
        label="Let admins in while this server cannot reach Argus"
        checked={!data.off}
        disabled={!canEdit || set.isPending}
        onChange={(e) => {
          const off = !e.currentTarget.checked
          void run(() => set.mutateAsync(off), {
            success: ['Changed', off
              ? `${hostname} refuses everyone while it cannot reach Argus.`
              : `${hostname} follows the setting in Settings again.`],
            failure: 'Not changed',
          })
        }}
      />
      <Text size={FS.meta} c="dimmed" lh={1.5}>
        {!on
          ? 'Off in Settings › Logins, so nobody is let in this way.'
          : data.off
            ? 'Off for this server. Settings › Logins lets ' + data.groups.join(', ') + ' in elsewhere.'
            : `Members of ${data.groups.join(', ')} are let in, recorded, after ${outageAfterLabel(data.afterMinutes)} without Argus.`}
      </Text>
    </Stack>
  )
}
