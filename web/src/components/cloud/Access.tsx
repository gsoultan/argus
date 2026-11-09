import {
  Alert, Badge, Button, Card, Code, CopyButton, Group, Modal, SegmentedControl, Select, Stack, Table, Text, Textarea,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconCheck, IconCopy, IconExternalLink, IconKey, IconLock, IconPlayerStop, IconUserCheck, IconX } from '@tabler/icons-react'
import { useState } from 'react'
import { DataTable, EmptyState } from '~/components/page'
import { Field, relTime } from '~/components/primitives'
import type { CloudAccount } from '~/lib/cloud'
import {
  accessNeedsApproval, cloudAccessQuery, consoleRoleMissing, keysBlock, minutesText, useAskCloudAccess, useCloudAccessVerb, useCloudSession,
  type CloudAccessGrant, type CloudKeys, type ConsoleLevel, type ConsoleLink,
} from '~/lib/cloudAccess'
import { notifyError, notifyOk } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import { FS, INK, SP } from '~/theme'
import { EnvBadge } from './common'

/*
 * Just-in-time access to an account's AWS console and command line. Nobody
 * holds it standing: a person asks for a level for a while with a reason,
 * someone else agrees where the environment asks for it, and it ends on its
 * own. While it lasts, its holder opens sessions as themselves; each one is
 * in the audit log, and AWS's own log names them.
 */

const LEVEL: Record<ConsoleLevel, { label: string; text: string; color: string }> = {
  read: { label: 'Read', color: 'sky', text: 'See everything and change nothing (AWS ReadOnlyAccess). It can read data too, such as S3 objects and logs.' },
  operate: { label: 'Operate', color: 'amber', text: 'Change anything except users, roles and policies (AWS PowerUserAccess). Changes made this way skip Argus\'s own approvals, so ask for it only to do what Argus cannot.' },
}

const STATE: Record<CloudAccessGrant['state'], { label: string; color: string }> = {
  awaiting_approval: { label: 'Waiting', color: 'amber' }, active: { label: 'In force', color: 'teal' }, expired: { label: 'Ran out', color: 'slate' },
  declined: { label: 'Declined', color: 'rose' }, cancelled: { label: 'Withdrawn', color: 'slate' }, ended: { label: 'Ended early', color: 'slate' },
}

const mayAsk = (role: string | undefined, level: ConsoleLevel) =>
  level === 'operate' ? role === 'admin' || role === 'owner' : role === 'admin' || role === 'owner' || role === 'auditor'
const decides = (role?: string) => role === 'admin' || role === 'owner' || role === 'approver'

export function AccessPanel({ accounts }: { accounts: CloudAccount[] }) {
  const { data: me } = useQuery(meQuery())
  const q = useQuery(cloudAccessQuery())
  const grants = q.data?.grants ?? []
  const live = grants.filter((g) => g.state === 'active')
  const waiting = grants.filter((g) => g.state === 'awaiting_approval')
  const past = grants.filter((g) => g.state !== 'active' && g.state !== 'awaiting_approval')
  const manageable = accounts.filter((a) => a.access === 'manage')
  return (
    <Stack gap="lg">
      {mayAsk(me?.role, 'read') && (
        manageable.length > 0
          ? <AskForm accounts={manageable} minutes={q.data?.minutes ?? [30, 60, 120, 240, 480, 720]} role={me?.role} />
          : <Alert color="sky" variant="light">Console access goes through an account's manage role. Allow changes on an account first, from the Overview.</Alert>
      )}
      {live.length > 0 && (
        <Stack gap="sm">
          <Text fw={600} size="sm">In force</Text>
          {live.map((g) => <GrantCard key={g.id} g={g} me={me?.email} role={me?.role} />)}
        </Stack>
      )}
      {waiting.length > 0 && (
        <Stack gap="sm">
          <Text fw={600} size="sm">Waiting for a decision</Text>
          {waiting.map((g) => <GrantCard key={g.id} g={g} me={me?.email} role={me?.role} />)}
        </Stack>
      )}
      <Stack gap="sm">
        <Text fw={600} size="sm">Earlier</Text>
        <DataTable minWidth={760} loading={q.isPending} isEmpty={past.length === 0}
          empty={<EmptyState icon={IconLock} title="No console access yet" description="Each grant shows here once it ends: who had it, who agreed, and how many sessions were opened." />}
          columns={['Who', 'Account', 'Level', 'Asked', 'Outcome', 'Sessions']}>
          {past.map((g) => (
            <Table.Tr key={g.id}>
              <Table.Td><Text size={FS.meta}>{g.requestedBy}</Text><Text size={FS.micro} c="dimmed" lineClamp={1} maw={260}>{g.reason}</Text></Table.Td>
              <Table.Td><Text size={FS.meta}>{g.accountName}</Text></Table.Td>
              <Table.Td><Badge variant="light" color={LEVEL[g.level].color} radius="sm">{LEVEL[g.level].label}</Badge></Table.Td>
              <Table.Td><Text size={FS.meta} c="dimmed">{relTime(g.requestedAt)} · {minutesText(g.minutes)}</Text></Table.Td>
              <Table.Td>
                <Badge variant="light" color={STATE[g.state].color} radius="sm">{STATE[g.state].label}</Badge>
                {(g.decidedBy || g.endedBy) && <Text size={FS.micro} c="dimmed">{g.state === 'ended' ? `by ${g.endedBy}` : g.decidedBy ? `by ${g.decidedBy}` : ''}</Text>}
              </Table.Td>
              <Table.Td><Text size={FS.meta}>{g.sessions}</Text></Table.Td>
            </Table.Tr>
          ))}
        </DataTable>
      </Stack>
    </Stack>
  )
}

function AskForm({ accounts, minutes, role }: { accounts: CloudAccount[]; minutes: number[]; role?: string }) {
  const [acct, setAcct] = useState<string | null>(accounts.length === 1 ? accounts[0]!.id : null)
  const [level, setLevel] = useState<ConsoleLevel>('read')
  const [len, setLen] = useState<string>('60')
  const [reason, setReason] = useState('')
  const ask = useAskCloudAccess()
  const account = accounts.find((a) => a.id === acct)
  const waits = account ? accessNeedsApproval(account.environment, level) : false
  const missing = consoleRoleMissing(account, level)
  const allowed = mayAsk(role, level)
  const submit = async () => {
    try {
      const g = await ask.mutateAsync({ accountId: acct!, level, minutes: Number(len), reason: reason.trim() })
      if (g.state === 'awaiting_approval') notifyOk('Asked for approval', `${LEVEL[level].label} access to ${g.accountName}. Someone else decides on Approvals or here.`)
      else notifyOk('Access in force', `${LEVEL[level].label} access to ${g.accountName} for ${minutesText(g.minutes)}.`)
      setReason('')
    } catch (err) {
      notifyError('Could not ask for access', err)
    }
  }
  return (
    <Card>
      <Stack gap="md">
        <div>
          <Text fw={600}>Ask for console access</Text>
          <Text size={FS.meta} c="dimmed">The AWS console or command line, as you, for a while. It ends on its own.</Text>
        </div>
        <Group grow align="flex-start">
          <Select label="Account" placeholder="Pick an account" value={acct} onChange={setAcct} allowDeselect={false}
            data={accounts.map((a) => ({ value: a.id, label: `${a.name} · ${a.accountId}` }))} />
          <Select label="For" value={len} onChange={(v) => v && setLen(v)} allowDeselect={false}
            data={minutes.map((m) => ({ value: String(m), label: minutesText(m) }))} />
        </Group>
        <Field label="Level">
          <SegmentedControl mt={4} value={level} onChange={(v) => setLevel(v as ConsoleLevel)} w="fit-content"
            data={[{ value: 'read', label: 'Read' }, { value: 'operate', label: 'Operate' }]} />
          <Text size={FS.meta} c={level === 'operate' ? INK.pending : 'dimmed'} mt={6} lh={1.5}>{LEVEL[level].text}</Text>
        </Field>
        {!allowed && <Text size={FS.meta} c={INK.denied}>Operate access is for admins and owners.</Text>}
        {missing && (
          <Alert color="amber" variant="light" p="sm">
            <Text size={FS.meta}>{account!.name} has no console role for this level yet. Update its role from the Overview; Argus sees the update at its next read.</Text>
          </Alert>
        )}
        <Textarea label="Why" description={waits ? 'The approver sees this, and it is kept in the audit log.' : 'Kept with the access in the audit log.'}
          autosize minRows={2} value={reason} onChange={(e) => setReason(e.currentTarget.value)} maxLength={500} />
        <Group justify="space-between">
          <Group gap={6}>
            {account && <EnvBadge env={account.environment} />}
            <Text size={FS.meta} c="dimmed">
              {account ? (waits ? 'Someone else approves this first. It runs from when they do.' : 'No approval needed here; it starts at once.') : ''}
            </Text>
          </Group>
          <Button leftSection={waits ? <IconUserCheck size={16} /> : <IconKey size={16} />} loading={ask.isPending}
            disabled={!acct || !allowed || missing || reason.trim().length < 5} onClick={() => void submit()}>
            {waits ? 'Ask for approval' : 'Get access'}
          </Button>
        </Group>
      </Stack>
    </Card>
  )
}

/** One grant in force or waiting, with what its holder or a decider can do. */
export function GrantCard({ g, me, role }: { g: CloudAccessGrant; me?: string; role?: string }) {
  const verb = useCloudAccessVerb()
  const session = useCloudSession()
  const [keys, setKeys] = useState<CloudKeys | null>(null)
  const mine = me?.toLowerCase() === g.requestedBy.toLowerCase()
  const act = async (v: 'approve' | 'decline' | 'cancel' | 'end') => {
    try {
      await verb.mutateAsync({ id: g.id, verb: v })
      notifyOk({ approve: 'Approved', decline: 'Declined', cancel: 'Withdrawn', end: 'Access ended' }[v], `${LEVEL[g.level].label} access to ${g.accountName} for ${g.requestedBy}`)
    } catch (err) {
      notifyError('Could not do that', err)
    }
  }
  const openConsole = async () => {
    // Opened now, on the click, so a popup blocker lets it through.
    const w = window.open('about:blank', '_blank')
    try {
      const link = await session.mutateAsync({ id: g.id, keys: false }) as ConsoleLink
      if (w) { w.opener = null; w.location.href = link.url } else window.location.href = link.url
    } catch (err) {
      w?.close()
      notifyError('Could not open the console', err)
    }
  }
  const getKeys = async () => {
    try {
      setKeys(await session.mutateAsync({ id: g.id, keys: true }) as CloudKeys)
    } catch (err) {
      notifyError('Could not get keys', err)
    }
  }
  const live = g.state === 'active'
  return (
    <Card withBorder>
      <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
        <Stack gap={4} style={{ flex: 1, minWidth: 260 }}>
          <Group gap={6}>
            <Badge variant="light" color={LEVEL[g.level].color} radius="sm">{LEVEL[g.level].label}</Badge>
            <Text fw={600} size={FS.body}>{g.accountName}</Text>
            <EnvBadge env={g.environment} />
          </Group>
          <Text size={FS.meta}>{mine ? 'You' : g.requestedBy}, for {minutesText(g.minutes)}: {g.reason}</Text>
          <Text size={FS.micro} c="dimmed">
            {live && g.endsAt ? `Ends ${relTime(g.endsAt)}${g.decidedBy ? `, approved by ${g.decidedBy}` : ''}. ${g.sessions} session${g.sessions === 1 ? '' : 's'} opened.`
              : `Asked ${relTime(g.requestedAt)}.`}
          </Text>
        </Stack>
        <Group gap="xs">
          {live && mine && (
            <>
              <Button size="xs" leftSection={<IconExternalLink size={14} />} loading={session.isPending && !session.variables?.keys} onClick={() => void openConsole()}>Open AWS console</Button>
              <Button size="xs" variant="default" leftSection={<IconKey size={14} />} loading={session.isPending && session.variables?.keys} onClick={() => void getKeys()}>Command-line keys</Button>
            </>
          )}
          {live && (mine || decides(role)) && (
            <Button size="xs" variant="default" color="rose" leftSection={<IconPlayerStop size={14} />} loading={verb.isPending} onClick={() => void act('end')}>End now</Button>
          )}
          {g.state === 'awaiting_approval' && mine && (
            <Button size="xs" variant="default" loading={verb.isPending} onClick={() => void act('cancel')}>Withdraw</Button>
          )}
          {g.state === 'awaiting_approval' && !mine && decides(role) && (
            <>
              <Button size="xs" color="teal" leftSection={<IconCheck size={14} />} loading={verb.isPending} onClick={() => void act('approve')}>Approve</Button>
              <Button size="xs" variant="default" leftSection={<IconX size={14} />} loading={verb.isPending} onClick={() => void act('decline')}>Decline</Button>
            </>
          )}
        </Group>
      </Group>
      {keys && <KeysModal keys={keys} onClose={() => setKeys(null)} />}
    </Card>
  )
}

function KeysModal({ keys, onClose }: { keys: CloudKeys; onClose: () => void }) {
  const block = keysBlock(keys)
  return (
    <Modal opened onClose={onClose} centered size="lg" title={<Text fw={700}>Command-line keys</Text>}>
      <Stack gap="md">
        <Text size={FS.body} lh={1.55}>
          Paste this into a terminal; the AWS CLI and SDKs pick it up. The keys are yours alone, work until {new Date(keys.expires).toLocaleTimeString()},
          and are not kept by Argus: close this and they are gone from here.
        </Text>
        <Code block fz={FS.meta} style={{ overflowWrap: 'anywhere', whiteSpace: 'pre-wrap' }}>{block}</Code>
        <Group justify="space-between">
          <Text size={FS.micro} c="dimmed">Ask for new ones when they run out, while your access lasts.</Text>
          <CopyButton value={block}>{({ copied, copy }) => (
            <Button size="xs" variant="default" leftSection={copied ? <IconCheck size={14} /> : <IconCopy size={14} />} onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>
          )}</CopyButton>
        </Group>
      </Stack>
    </Modal>
  )
}

/** Grants waiting for someone, for the Approvals page. Nothing when there are none. */
export function AccessQueue() {
  const { data: me } = useQuery(meQuery())
  const q = useQuery(cloudAccessQuery({ waiting: true }))
  const waiting = (q.data?.grants ?? []).filter((g) => g.requestedBy.toLowerCase() !== me?.email?.toLowerCase())
  if (waiting.length === 0) return null
  return (
    <Stack gap="sm" mb="lg">
      <Group gap={SP.cozy}>
        <IconKey size={16} />
        <Text fw={600} size="sm">AWS console access</Text>
        <Badge size="sm" variant="light" color="amber">{waiting.length}</Badge>
      </Group>
      {waiting.map((g) => <GrantCard key={g.id} g={g} me={me?.email} role={me?.role} />)}
    </Stack>
  )
}
