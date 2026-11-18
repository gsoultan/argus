import { Fragment, useState } from 'react'
import {
  Alert, Badge, Button, Group, Modal, Select, Skeleton, Stack, Switch, Table, Text, TextInput, Textarea,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { IconAlertTriangle, IconChecklist, IconInfoCircle, IconLock, IconUsers } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import {
  approvalPolicyQuery, cellChanges, CHOICES, choiceOf, ENV_LABEL, loosens, useDecidePolicyChange,
  useSaveApprovalPolicy, withChoice, type ApprovalChoice, type ApprovalEnv, type ApprovalKindInfo,
  type ApprovalPolicy, type ApprovalPolicyChange,
} from '~/lib/approvalPolicy'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import { FS, SP } from '~/theme'

const COLUMNS: ApprovalEnv[] = ['production', 'staging', 'other']

const CHOICE_COLOR: Record<ApprovalChoice, string> = { second: 'slate', owners: 'amber', admins: 'amber', anyone: 'rose' }

const shortOf = (c: ApprovalChoice) => CHOICES.find((x) => x.value === c)!.short
const labelOf = (c: ApprovalChoice) => CHOICES.find((x) => x.value === c)!.label

/**
 * Settings › Approval policy: who may act directly where Argus would wait for
 * a second person. Everyone reads it; owners change it. Asking for more
 * approval applies at once; letting more people act directly waits for
 * another owner (or the phrase, for the only one).
 */
export function ApprovalPolicySettings({ me }: { me: string }) {
  const { data, isLoading } = useQuery(approvalPolicyQuery())
  const [draft, setDraft] = useState<ApprovalPolicy | null>(null)
  const [reviewing, setReviewing] = useState(false)

  if (!isConfigured()) {
    return (
      <Alert color="slate">
        <Text size={FS.body}>The approval policy is kept on a control plane. Connect this console to Argus to see it.</Text>
      </Alert>
    )
  }
  if (isLoading) return <Skeleton h={320} />
  // A control plane without the policy (older, or not answering) is said so
  // plainly rather than drawn from a reply of the wrong shape.
  if (!data || !Array.isArray(data.kinds) || !data.policy) {
    return (
      <Alert color="slate">
        <Text size={FS.body}>This control plane has no approval policy yet. Every action waits for a second person, as before.</Text>
      </Alert>
    )
  }

  const saved = data.policy
  const p = draft ?? saved
  const changes = draft ? cellChanges(data.kinds, saved, draft) : []
  const switchChanged = draft !== null &&
    (draft.requireReason !== saved.requireReason || draft.notifyProduction !== saved.notifyProduction)
  const dirty = changes.length > 0 || switchChanged
  const looser = draft !== null && loosens(data.kinds, saved, draft)
  const edit = data.canChange && !data.pending
  const loosened = saved.rules.length

  const groups: { name: string; kinds: ApprovalKindInfo[] }[] = []
  for (const k of data.kinds) {
    const g = groups.find((x) => x.name === k.group)
    if (g) g.kinds.push(k)
    else groups.push({ name: k.group, kinds: [k] })
  }

  return (
    <Stack gap="sm">
      <SectionCard
        title="Approval policy"
        icon={IconChecklist}
        description={
          <>
            Argus asks a second person to agree before some actions run. Here your organization decides who may act
            directly instead, by kind of action and by environment. Nobody gets access they do not already have: this
            only decides whether someone who may already do a thing must wait for someone else.
          </>
        }
        badge={
          <Badge color={loosened ? 'amber' : 'slate'} variant="light">
            {loosened ? `${loosened} looser than the default` : 'Argus default'}
          </Badge>
        }
      >
        <Group gap="lg">
          <Text size={FS.body}>
            <b>{data.direct30}</b> action{data.direct30 === 1 ? '' : 's'} went straight through in the last 30 days.
          </Text>
          <Text size={FS.body} component={Link} to="/activity" c="azure">See them in Activity</Text>
        </Group>
        {(() => {
          const mine = Object.entries(data.mine ?? {}).filter(([, envs]) => envs.length > 0)
          if (mine.length === 0) return null
          return (
            <Text size={FS.meta} mt={SP.tight}>
              You act directly on:{' '}
              {mine.map(([kind, envs]) => {
                const k = data.kinds.find((x) => x.kind === kind)
                return `${k?.title ?? kind} (${envs.map((e) => ENV_LABEL[e]).join(', ')})`
              }).join('; ')}.
            </Text>
          )
        })()}
        {saved.updatedBy && (
          <Text size={FS.meta} c="dimmed" mt={SP.tight}>
            Last changed by {saved.updatedBy}{saved.updatedAt ? ` on ${new Date(saved.updatedAt).toLocaleString()}` : ''}.
          </Text>
        )}
      </SectionCard>

      {data.pending && <PendingChange pending={data.pending} me={me} canDecide={data.canChange} />}

      {edit && dirty && (
        <Alert color={looser ? 'amber' : 'azure'} p="xs">
          <Group justify="space-between" gap="sm">
            <Text size={FS.body}>
              {changes.length + (switchChanged ? 1 : 0)} unsaved change{changes.length + (switchChanged ? 1 : 0) === 1 ? '' : 's'}
              {looser ? ', and they let more people act directly.' : '.'}
            </Text>
            <Group gap={SP.cozy}>
              <Button size="xs" variant="subtle" color="slate" onClick={() => setDraft(null)}>Discard</Button>
              <Button size="xs" color={looser ? 'amber' : 'azure'} onClick={() => setReviewing(true)}>Review change</Button>
            </Group>
          </Group>
        </Alert>
      )}

      <SectionCard title="Who may act directly" flush>
        <Table.ScrollContainer minWidth={760}>
          <Table verticalSpacing="sm" horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>When Argus would ask for a second person</Table.Th>
                {COLUMNS.map((c) => <Table.Th key={c} w={200}>{ENV_LABEL[c]}</Table.Th>)}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {groups.map((g) => (
                <Fragment key={g.name}>
                  <Table.Tr bg="var(--mantine-color-default-hover)">
                    <Table.Td colSpan={4}>
                      <Text size={FS.meta} fw={700} c="dimmed" tt="uppercase">{g.name}</Text>
                    </Table.Td>
                  </Table.Tr>
                  {g.kinds.map((k) => (
                    <Table.Tr key={k.kind}>
                      <Table.Td>
                        <Text size={FS.body} fw={600}>{k.title}</Text>
                        <Text size={FS.meta} c="dimmed" lh={1.4}>{k.what}</Text>
                      </Table.Td>
                      {k.envs.includes('all') ? (
                        <Table.Td colSpan={3}>
                          <Cell k={k} env="all" p={p} saved={saved} edit={edit}
                            onChange={(c) => setDraft(withChoice(p, k.kind, 'all', c))} />
                        </Table.Td>
                      ) : COLUMNS.map((env) => (
                        <Table.Td key={env}>
                          {k.envs.includes(env) ? (
                            <Cell k={k} env={env} p={p} saved={saved} edit={edit}
                              onChange={(c) => setDraft(withChoice(p, k.kind, env, c))} />
                          ) : <Text size={FS.meta} c="dimmed">Not asked today</Text>}
                        </Table.Td>
                      ))}
                    </Table.Tr>
                  ))}
                </Fragment>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </SectionCard>

      <SectionCard title="For everyone who acts directly" icon={IconInfoCircle}>
        <Stack gap="sm">
          <Switch
            checked={p.requireReason}
            disabled={!edit}
            onChange={(e) => setDraft({ ...p, requireReason: e.currentTarget.checked })}
            label="Ask for a reason"
            description="Where the action has a reason box (cloud changes, schedules and access, every request), it must be filled in to act directly. It is kept with the action in Activity."
          />
          <Switch
            checked={p.notifyProduction}
            disabled={!edit}
            onChange={(e) => setDraft({ ...p, notifyProduction: e.currentTarget.checked })}
            label="Tell owners each time in Production"
            description="A bell alert to every other owner whenever someone acts directly on something in Production."
          />
        </Stack>
      </SectionCard>

      <SectionCard title="Who can change this policy" icon={IconUsers}>
        <Text size={FS.body} lh={1.5}>
          Owners only (admins, in an organization with no owner). A change that lets more people act directly waits
          for another owner to approve it; one that asks for more approval applies at once. Your organization has{' '}
          {data.keepers} {data.keepers === 1 ? 'person' : 'people'} who can change it. Every change is in Activity,
          with before and after.
          {!data.canChange && ' You can read it, but not change it.'}
        </Text>
      </SectionCard>

      {draft && (
        <ReviewModal
          opened={reviewing}
          onClose={() => setReviewing(false)}
          kinds={data.kinds}
          saved={saved}
          draft={draft}
          looser={looser}
          alone={data.keepers <= 1}
          phrase={data.phrase}
          onDone={() => { setReviewing(false); setDraft(null) }}
        />
      )}
    </Stack>
  )
}

function Cell({ k, env, p, saved, edit, onChange }: {
  k: ApprovalKindInfo
  env: ApprovalEnv
  p: ApprovalPolicy
  saved: ApprovalPolicy
  edit: boolean
  onChange: (c: ApprovalChoice) => void
}) {
  const today = k.today[env] ?? 'never'
  const note = k.notes?.[env]
  if (today === 'never') {
    return <Text size={FS.meta} c="dimmed">Not asked today</Text>
  }
  const locked = k.locked?.[env]
  if (locked !== undefined) {
    return (
      <Stack gap={2}>
        <Badge color="slate" variant="light" leftSection={<IconLock size={11} />}>Second person</Badge>
        <Text size={FS.meta} c="dimmed">Locked for every organization{locked ? `: ${locked}` : ''}</Text>
      </Stack>
    )
  }
  const c = choiceOf(p, k.kind, env)
  const changed = c !== choiceOf(saved, k.kind, env)
  return (
    <Stack gap={2}>
      {edit ? (
        <Select
          size="xs"
          aria-label={`${k.title}, ${ENV_LABEL[env]}`}
          allowDeselect={false}
          value={c}
          data={CHOICES.map((x) => ({ value: x.value, label: x.short }))}
          onChange={(v) => v && onChange(v as ApprovalChoice)}
          styles={changed ? { input: { borderColor: 'var(--mantine-color-amber-5)' } } : undefined}
        />
      ) : (
        <Badge color={CHOICE_COLOR[c]} variant="light">{shortOf(c)}</Badge>
      )}
      {note && <Text size={FS.meta} c="dimmed">{note}</Text>}
    </Stack>
  )
}

function ReviewModal({ opened, onClose, kinds, saved, draft, looser, alone, phrase, onDone }: {
  opened: boolean
  onClose: () => void
  kinds: ApprovalKindInfo[]
  saved: ApprovalPolicy
  draft: ApprovalPolicy
  looser: boolean
  alone: boolean
  phrase: string
  onDone: () => void
}) {
  const save = useSaveApprovalPolicy()
  const [reason, setReason] = useState('')
  const [typed, setTyped] = useState('')
  const changes = cellChanges(kinds, saved, draft)
  const ready = !looser || (reason.trim().length >= 5 && (!alone || typed.trim() === phrase))
  const commit = () => void run(
    async () => {
      const out = await save.mutateAsync({ ...draft, reason: reason.trim(), ...(looser && alone ? { confirmSelf: typed.trim() } : {}) })
      onDone()
      return out
    },
    {
      failure: 'Approval policy not saved',
      success: looser && !alone
        ? ['Sent for approval', 'Another owner approves it from this page. Until then the current policy stays.']
        : ['Approval policy saved', 'It applies from the next action.'],
    },
  )
  return (
    <Modal opened={opened} onClose={onClose} size="lg"
      title={<Text fw={700}>{looser ? 'Let more people act directly?' : 'Save the approval policy'}</Text>}>
      <Stack gap="sm">
        {changes.map((c) => (
          <Stack key={`${c.kind.kind}/${c.env}`} gap={4}>
            <Group gap={SP.cozy} wrap="wrap">
              <Text size={FS.body} fw={600}>{c.kind.title}</Text>
              <Badge color={c.env === 'production' ? 'rose' : 'slate'} variant="light">{ENV_LABEL[c.env]}</Badge>
              <Text size={FS.body} c="dimmed">{labelOf(c.was)} → {labelOf(c.now)}</Text>
            </Group>
            {c.looser && CHOICES.find((x) => x.value === c.now)!.risk && (
              <Alert color={c.env === 'production' ? 'rose' : 'amber'} p="xs" icon={<IconAlertTriangle size={16} />}>
                <Text size={FS.meta} lh={1.5}>{CHOICES.find((x) => x.value === c.now)!.risk} Activity still records who did it and why.</Text>
              </Alert>
            )}
          </Stack>
        ))}
        {draft.requireReason !== saved.requireReason && (
          <Text size={FS.body}>Ask for a reason when acting directly: <b>{draft.requireReason ? 'on' : 'off'}</b></Text>
        )}
        {draft.notifyProduction !== saved.notifyProduction && (
          <Text size={FS.body}>Tell owners each time in Production: <b>{draft.notifyProduction ? 'on' : 'off'}</b></Text>
        )}
        {looser && (
          <>
            <Textarea label="Why" description="Kept with the change in Activity." autosize minRows={2}
              value={reason} onChange={(e) => setReason(e.currentTarget.value)} />
            {alone ? (
              <>
                <Alert color="amber" p="xs">
                  <Text size={FS.body} lh={1.5}>
                    Nobody else can change this policy, so you may make the change yourself. Activity records that
                    nobody else agreed.
                  </Text>
                </Alert>
                <TextInput label={`Type "${phrase}" to confirm`} placeholder={phrase} value={typed}
                  onChange={(e) => setTyped(e.currentTarget.value)} />
              </>
            ) : (
              <Alert color="sky" p="xs">
                <Text size={FS.body} lh={1.5}>
                  Another owner approves this first, as for any request. Until then the current policy stays. They
                  get a bell alert, and see the change on this page.
                </Text>
              </Alert>
            )}
          </>
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Back</Button>
          <Button color={looser ? 'amber' : 'azure'} disabled={!ready} loading={save.isPending} onClick={commit}>
            {looser && !alone ? 'Ask another owner' : 'Save'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function PendingChange({ pending, me, canDecide }: {
  pending: ApprovalPolicyChange
  me: string
  canDecide: boolean
}) {
  const decide = useDecidePolicyChange()
  const [note, setNote] = useState('')
  const mine = pending.requestedBy.toLowerCase() === me.toLowerCase()
  const act = (what: 'approve' | 'deny' | 'cancel') => void run(
    () => decide.mutateAsync({ id: pending.id, what, note: note.trim() || undefined }),
    {
      failure: 'Not done',
      success: what === 'approve'
        ? ['Approved', 'The new approval policy is in effect.']
        : what === 'deny' ? ['Denied', 'The policy stays as it is.'] : ['Taken back', 'The policy stays as it is.'],
    },
  )
  return (
    <SectionCard title={`${pending.requestedBy} asks to loosen the approval policy`} icon={IconAlertTriangle}
      iconColor="amber" description={`Asked ${new Date(pending.requestedAt).toLocaleString()}. Why: ${pending.reason}`}>
      <Stack gap="xs">
        {pending.changes.map((c) => <Text key={c} size={FS.body}>{c}</Text>)}
        {mine ? (
          <Group justify="space-between">
            <Text size={FS.meta} c="dimmed">Another owner decides this. The policy stays as it is until then.</Text>
            <Button size="xs" variant="default" loading={decide.isPending} onClick={() => act('cancel')}>Take it back</Button>
          </Group>
        ) : canDecide ? (
          <Group align="flex-end" gap="sm">
            <TextInput size="xs" label="Note (needed to deny)" value={note} onChange={(e) => setNote(e.currentTarget.value)}
              style={{ flex: 1 }} />
            <Button size="xs" variant="default" disabled={note.trim().length < 5} loading={decide.isPending}
              onClick={() => act('deny')}>Deny</Button>
            <Button size="xs" color="amber" loading={decide.isPending} onClick={() => act('approve')}>Approve</Button>
          </Group>
        ) : (
          <Text size={FS.meta} c="dimmed">An owner decides this.</Text>
        )}
      </Stack>
    </SectionCard>
  )
}
