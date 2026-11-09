import { Badge, Box, Button, Card, Flex, Group, Stack, Text, ThemeIcon, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconCamera, IconCirclePlus, IconDeviceFloppy, IconHistory, IconLink, IconLinkOff, IconLock, IconLockOpen, IconPhoto, IconPlayerPlay,
  IconPlayerStop, IconPlugConnected, IconPlugConnectedX, IconRefresh, IconResize, IconShield, IconShieldPlus, IconTag, IconTrash, IconWorld,
} from '@tabler/icons-react'
import { EmptyState } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import { CHANGE_STATE, useDecideCloudChange, type CloudAction, type CloudChange } from '~/lib/cloud'
import { notifyError, notifyOk } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import { FS } from '~/theme'
import { EnvBadge } from './common'

const ICON: Record<CloudAction, typeof IconTrash> = {
  launch: IconCirclePlus, start: IconPlayerPlay, stop: IconPlayerStop, reboot: IconRefresh, resize: IconResize,
  tags: IconTag, protect: IconLock, unprotect: IconLockOpen, remove: IconTrash,
  'volume-create': IconDeviceFloppy, 'volume-grow': IconResize, 'volume-attach': IconLink, 'volume-detach': IconLinkOff,
  'volume-delete': IconTrash, 'snapshot-create': IconCamera, 'snapshot-delete': IconTrash, 'image-create': IconPhoto,
  'image-delete': IconTrash, 'address-allocate': IconWorld, 'address-associate': IconLink, 'address-disassociate': IconLinkOff,
  'address-release': IconTrash, 'firewall-create': IconShieldPlus, 'firewall-rules': IconShield, 'firewall-delete': IconTrash,
  'instance-firewalls': IconShield, 'target-register': IconPlugConnected, 'target-deregister': IconPlugConnectedX,
}
const ICON_COLOR: Partial<Record<CloudAction, string>> = {
  launch: 'teal', remove: 'rose', stop: 'amber', 'volume-create': 'teal', 'address-allocate': 'teal',
  'volume-delete': 'rose', 'snapshot-delete': 'rose', 'image-delete': 'rose', 'address-release': 'rose',
  'firewall-create': 'teal', 'firewall-delete': 'rose', 'target-deregister': 'amber',
}

/**
 * Cloud changes as cards: what, where, who asked and why, and what came of
 * it. Someone who may decide gets Approve and Decline on others' waiting
 * changes; the person who asked gets Withdraw on their own.
 */
export function ChangesList({ changes, loading, empty }: { changes?: CloudChange[]; loading?: boolean; empty: { title: string; text: string } }) {
  const { data: me } = useQuery(meQuery())
  const decides = me?.role === 'admin' || me?.role === 'owner' || me?.role === 'approver'
  if (loading) return <Text c="dimmed" size={FS.meta}>Loading…</Text>
  if (!changes?.length) {
    return <Card><EmptyState icon={IconHistory} title={empty.title} description={empty.text} /></Card>
  }
  return (
    <Stack gap="sm">
      {changes.map((c) => (
        <ChangeCard key={c.id} c={c} mine={c.requestedBy.toLowerCase() === me?.email.toLowerCase()} decides={decides} />
      ))}
    </Stack>
  )
}

function ChangeCard({ c, mine, decides }: { c: CloudChange; mine: boolean; decides: boolean }) {
  const decide = useDecideCloudChange()
  const Icon = ICON[c.action]
  const st = CHANGE_STATE[c.state]
  const waiting = c.state === 'awaiting_approval'
  const act = async (verb: 'approve' | 'decline' | 'cancel') => {
    try {
      const out = await decide.mutateAsync({ id: c.id, verb })
      if (verb === 'approve') {
        if (out.state === 'failed') notifyError('Approved, but AWS refused it', new Error(out.error || 'No reason was given.'))
        else if (out.state === 'awaiting_approval') notifyOk('Sent back', 'AWS changed since this was asked for. Look at it again before approving.')
        else notifyOk('Approved', `${out.summary}. It ran as ${out.requestedBy}.`)
      } else notifyOk(verb === 'decline' ? 'Declined' : 'Withdrawn', out.summary)
    } catch (err) {
      notifyError('Could not record the decision', err)
    }
  }
  const snaps = Object.entries(c.result?.snapshots ?? {})
  return (
    <Card padding="md">
      <Flex direction={{ base: 'column', sm: 'row' }} align={{ base: 'stretch', sm: 'flex-start' }} gap="sm">
        <Group wrap="nowrap" align="flex-start" gap="sm" style={{ flex: 1, minWidth: 0 }}>
          <ThemeIcon variant="light" color={ICON_COLOR[c.action] ?? 'azure'} size={34} radius="md"><Icon size={18} /></ThemeIcon>
          <Box style={{ flex: 1, minWidth: 0 }}>
            <Group gap="xs" wrap="wrap">
              <Text fw={600} size={FS.body}>{c.summary}</Text>
              <Badge variant="light" color={st.color} radius="sm">{st.label}</Badge>
              <EnvBadge env={c.environment} />
            </Group>
            <Text size={FS.meta} c="dimmed" mt={2}>
              {c.accountName} · {c.region}{c.resourceId ? <> · <Text span ff="monospace" size={FS.meta}>{c.resourceId}</Text></> : null}
            </Text>
            <Text size={FS.meta} mt={6}>
              <Tooltip label={absTime(c.requestedAt)}><span>Asked by <b>{c.requestedBy}</b> {relTime(c.requestedAt)}</span></Tooltip>
              {c.decidedBy && c.state !== 'cancelled' && <> · {c.state === 'declined' ? 'declined' : 'approved'} by <b>{c.decidedBy}</b></>}
            </Text>
            {c.reason && <Text size={FS.meta} c="dimmed" fs="italic" mt={2}>“{c.reason}”</Text>}
            {c.action === 'remove' && c.plan?.plan && (
              <Text size={FS.meta} mt={6}>
                {(() => { const n = c.plan.plan.volumes.filter((v) => v.deleted).length; return n === 1 ? '1 disk is deleted with it' : `${n} disks are deleted with it` })()}
                {c.spec.finalSnapshots ? ', after a final snapshot.' : ', with no final snapshot.'}
              </Text>
            )}
            {c.state === 'failed' && <Text size={FS.meta} c="rose" mt={6}>{c.error}</Text>}
            {snaps.length > 0 && <Text size={FS.micro} c="dimmed" mt={4} ff="monospace">Snapshots: {snaps.map(([, s]) => s).join(', ')}</Text>}
            {c.requestId && <Text size={FS.micro} c="dimmed" mt={2}>AWS request <Text span ff="monospace" size={FS.micro}>{c.requestId}</Text></Text>}
          </Box>
        </Group>
        {waiting && (
          <Group gap="xs" wrap="nowrap" justify="flex-end">
            {mine && <Button size="xs" variant="default" loading={decide.isPending} onClick={() => void act('cancel')}>Withdraw</Button>}
            {!mine && decides && (
              <>
                <Button size="xs" variant="default" loading={decide.isPending} onClick={() => void act('decline')}>Decline</Button>
                <Button size="xs" color={c.action === 'remove' ? 'rose' : undefined} loading={decide.isPending} onClick={() => void act('approve')}>Approve</Button>
              </>
            )}
            {mine && decides && <Tooltip label="Someone else approves a change you asked for"><Text size={FS.micro} c="dimmed">Yours</Text></Tooltip>}
          </Group>
        )}
      </Flex>
    </Card>
  )
}
