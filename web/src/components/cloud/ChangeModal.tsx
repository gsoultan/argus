import { ActionIcon, Alert, Button, Group, Modal, Stack, Text, TextInput, Textarea } from '@mantine/core'
import { IconPlus, IconUserCheck, IconX } from '@tabler/icons-react'
import { useState, type ReactNode } from 'react'
import { needsApproval, useAskCloudChange, type CloudChange, type CloudChangeInput, type CloudEnvironment, type CloudItem } from '~/lib/cloud'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS, INK } from '~/theme'
import { EnvBadge } from './common'

/*
 * What every cloud change shares, whatever it changes: the dialog that says
 * who must approve it and asks why, sending it, and saying what happened.
 */

/** Tells the person what happened to the change they asked for. */
export function reportChange(c: CloudChange, done: string) {
  if (c.state === 'awaiting_approval') notifyOk('Asked for approval', `${c.summary}. Someone else approves it on Approvals or Cloud › Changes.`)
  else if (c.state === 'failed') notifyError('AWS refused the change', new Error(c.error || 'No reason was given.'))
  else notifyOk(done, c.summary)
}

/** The frame every change shares: what it does, who must approve, and why. */
export function ChangeModal({ title, env, level, reason, setReason, children, onClose, onSubmit, submit, color, busy, disabled }: {
  title: ReactNode
  env: CloudEnvironment
  level: CloudChange['level']
  reason: string
  setReason: (s: string) => void
  children: ReactNode
  onClose: () => void
  onSubmit: () => void
  submit: string
  color?: string
  busy: boolean
  disabled?: boolean
}) {
  const approval = needsApproval(env, level)
  return (
    <Modal opened onClose={onClose} centered size="lg" title={<Text fw={700}>{title}</Text>}>
      <Stack gap="md">
        {children}
        {approval && (
          <Alert color="amber" variant="light" icon={<IconUserCheck size={16} />} p="sm">
            <Group gap={6} mb={2}><EnvBadge env={env} /><Text size={FS.meta} fw={600}>Someone else approves this</Text></Group>
            <Text size={FS.meta}>
              Nothing changes in AWS until another admin, owner or approver approves it. It then runs as you, and the audit log has both of you.
            </Text>
          </Alert>
        )}
        <Textarea label={approval ? 'Why' : 'Why (optional)'} description={approval ? 'The approver sees this.' : 'Kept with the change in the audit log.'}
          autosize minRows={2} value={reason} onChange={(e) => setReason(e.currentTarget.value)} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color={color} loading={busy} disabled={disabled || (approval && reason.trim().length < 5)} onClick={onSubmit}>
            {approval ? 'Ask for approval' : submit}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

export function useSubmit(onClose: () => void) {
  const ask = useAskCloudChange()
  const submit = async (in_: CloudChangeInput, done: string) => {
    try {
      const c = await ask.mutateAsync(in_)
      reportChange(c, done)
      onClose()
    } catch (err) {
      notifyError('Could not make the change', err)
    }
  }
  return { submit, busy: ask.isPending }
}

export function TagsChange({ it, env, onClose }: { it: CloudItem; env: CloudEnvironment; onClose: () => void }) {
  const r = it.resource
  const start = Object.entries(r.tags ?? {}).filter(([k]) => !k.startsWith('aws:')).sort(([a], [b]) => a.localeCompare(b))
  const [rows, setRows] = useState<[string, string][]>(start.length ? start : [['', '']])
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const before = new Map(start)
  const after = new Map(rows.filter(([k]) => k.trim()).map(([k, v]) => [k.trim(), v]))
  const set: Record<string, string> = {}
  for (const [k, v] of after) if (before.get(k) !== v) set[k] = v
  const remove = [...before.keys()].filter((k) => !after.has(k))
  const envKey = (k: string) => ['environment', 'env'].includes(k.toLowerCase())
  const touchesEnv = Object.keys(set).some(envKey) || remove.some(envKey)
  const changed = Object.keys(set).length + remove.length
  const edit = (i: number, j: 0 | 1, v: string) => setRows(rows.map((row, n) => n === i ? (j === 0 ? [v, row[1]] : [row[0], v]) : row))
  return (
    <ChangeModal title={`Tags on ${r.name}`} env={env} level={touchesEnv ? 'change' : 'operate'} reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit={changed ? `Save ${changed} change${changed === 1 ? '' : 's'}` : 'Save'} disabled={changed === 0}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'tags', resourceId: r.id,
        kind: r.kind === 'instance' ? undefined : r.kind, spec: { set, remove }, reason }, 'Tags saved')}>
      <Stack gap={6}>
        {rows.map(([k, v], i) => (
          <Group key={i} gap="xs" wrap="nowrap">
            <TextInput placeholder="Key" value={k} onChange={(e) => edit(i, 0, e.currentTarget.value)} style={{ flex: 2 }} aria-label="Tag key"
              error={k.toLowerCase().startsWith('aws:') ? 'AWS keeps aws: tags' : undefined} />
            <TextInput placeholder="Value" value={v} onChange={(e) => edit(i, 1, e.currentTarget.value)} style={{ flex: 3 }} aria-label="Tag value" />
            <ActionIcon variant="subtle" color="gray" aria-label="Remove tag" onClick={() => setRows(rows.filter((_, n) => n !== i))}><IconX size={14} /></ActionIcon>
          </Group>
        ))}
        <Button variant="subtle" size="xs" w="fit-content" leftSection={<IconPlus size={13} />} onClick={() => setRows([...rows, ['', '']])}>Add a tag</Button>
      </Stack>
      {touchesEnv && (
        <Text size={FS.meta} c={INK.pending}>Changing the Environment tag changes which rules apply here, so it counts as a bigger change.</Text>
      )}
    </ChangeModal>
  )
}
