import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Checkbox, Group, Modal, MultiSelect, Radio,
  SegmentedControl, Stack, Text, Textarea, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { notifications } from '@mantine/notifications'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconCheck, IconClockHour4, IconPlus, IconX,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { Mono, RequestStateBadge, absTime, relTime } from '~/components/primitives'
import { FS } from '~/theme'
import { notifyError, notifyWarn } from '~/lib/notify'
import {
  assetsQuery, requestsQuery, useCreateRequest, useDecideRequest,
} from '~/lib/queries'
import type { AccessRequest } from '~/types/domain'

export const Route = createFileRoute('/requests')({
  component: Requests,
  loader: ({ context }) => context.queryClient.ensureQueryData(requestsQuery()),
})

/** Policy caps. The Go control plane enforces these; the form mirrors them. */
const MAX_DURATION_MINUTES = 240
const MIN_JUSTIFICATION = 20
const MIN_DENIAL_NOTE = 5

interface RequestDraft {
  assetHostnames: string[]
  principal: string
  durationMinutes: number
  justification: string
  breakGlass: boolean
}

const EMPTY_REQUEST: RequestDraft = {
  assetHostnames: [],
  principal: 'ops',
  durationMinutes: 60,
  justification: '',
  breakGlass: false,
}

const validateAssetIds = (value: string[]) =>
  value.length === 0 ? 'Select at least one host.' : undefined

const validateJustification = (value: string) =>
  value.trim().length < MIN_JUSTIFICATION
    ? `Give an approver something to act on — at least ${MIN_JUSTIFICATION} characters.`
    : undefined

function Requests() {
  const [tab, setTab] = useState<'pending' | 'all'>('pending')
  const { data: requests } = useQuery(requestsQuery(tab === 'pending' ? 'pending' : undefined))
  const [newOpen, newModal] = useDisclosure(false)

  return (
    <Box>
      <PageHeader
        title="Access requests"
        description="Time-bounded, justified, and approved by someone other than the requester. Grants expire on their own — nothing to remember to revoke."
        actions={
          <>
            <SegmentedControl
              size="xs"
              value={tab}
              onChange={(v) => setTab(v as 'pending' | 'all')}
              data={[
                { label: 'Queue', value: 'pending' },
                { label: 'History', value: 'all' },
              ]}
            />
            <Button size="xs" leftSection={<IconPlus size={14} />} onClick={newModal.open}>
              Request access
            </Button>
          </>
        }
      />

      <Box p="lg">
        <Stack gap="sm">
          {requests?.map((r) => <RequestCard key={r.id} request={r} />)}
          {requests?.length === 0 && (
            <Card padding="xl">
              <Text size="sm" c="dimmed" ta="center">
                {tab === 'pending' ? 'Nothing waiting for a decision.' : 'No requests yet.'}
              </Text>
            </Card>
          )}
        </Stack>
      </Box>

      <NewRequestModal opened={newOpen} onClose={newModal.close} />
    </Box>
  )
}

function RequestCard({ request: r }: { request: AccessRequest }) {
  const decide = useDecideRequest()
  const [note, setNote] = useState('')
  const [expanded, setExpanded] = useState(false)

  const onDecide = async (decision: 'approved' | 'denied') => {
    if (decision === 'denied' && note.trim().length < MIN_DENIAL_NOTE) {
      setExpanded(true)
      notifyWarn('Reason required', 'A denial must say why, so the requester can act on it.')
      return
    }
    // Guarded: an approval that the control plane refused must not look like one
    // that succeeded. Without this the button simply stops spinning and the
    // approver walks away believing they granted access.
    try {
      await decide.mutateAsync({ id: r.id, decision, note })
    } catch (err) {
      notifyError(
        decision === 'approved' ? 'Access not granted' : 'Request not denied',
        err,
      )
      return
    }
    notifications.show({
      color: decision === 'approved' ? 'teal' : 'rose',
      title: decision === 'approved' ? 'Access granted' : 'Request denied',
      message:
        decision === 'approved'
          ? `${r.requesterEmail} can reach ${r.assetHostnames.length} host(s) as ${r.principal} for ${r.durationMinutes / 60}h.`
          : `${r.requesterEmail} was notified.`,
    })
  }

  const pending = r.state === 'pending'

  return (
    <Card
      padding="md"
      style={r.breakGlass && pending ? { borderColor: 'var(--color-denied)' } : undefined}
    >
      <Group justify="space-between" align="flex-start" wrap="nowrap" mb="sm">
        <Box style={{ minWidth: 0 }}>
          <Group gap={8} mb={4}>
            <Text fw={600} size="sm">{r.requesterEmail}</Text>
            <RequestStateBadge state={r.state} />
            {r.breakGlass && (
              <Tooltip label="Emergency path — separate approval chain, credential is revoked and regenerated after use">
                <Badge size="xs" color="rose" leftSection={<IconAlertTriangle size={10} />}>
                  break-glass
                </Badge>
              </Tooltip>
            )}
          </Group>
          <Text size="xs" c="dimmed" mb="xs">{r.justification}</Text>
          <Group gap={6} wrap="wrap">
            <Badge size="xs" variant="outline" color="slate">as {r.principal}</Badge>
            <Badge size="xs" variant="outline" color="slate">{r.durationMinutes / 60}h window</Badge>
            {r.assetHostnames.slice(0, 3).map((h) => (
              <Badge key={h} size="xs" variant="outline" color="slate" className="argus-digest">
                {h.split('.')[0]}
              </Badge>
            ))}
            {r.assetHostnames.length > 3 && (
              <Badge size="xs" variant="outline" color="slate">
                +{r.assetHostnames.length - 3} more
              </Badge>
            )}
          </Group>
        </Box>

        <Box ta="right" style={{ flexShrink: 0 }}>
          <Text size={FS.micro} c="dimmed">{relTime(r.createdAt)}</Text>
          {r.expiresAt && r.state === 'approved' && (
            <Group gap={4} justify="flex-end" mt={4}>
              <IconClockHour4 size={11} className="text-amber-400" />
              <Text size={FS.micro} c="amber.4">expires {relTime(r.expiresAt)}</Text>
            </Group>
          )}
        </Box>
      </Group>

      {!pending && (
        <Box pt="xs" style={{ borderTop: '1px solid var(--color-line)' }}>
          <Text size={FS.micro} c="dimmed">
            {r.state} by <Mono>{r.decidedByEmail}</Mono> · {absTime(r.decidedAt)}
            {r.decisionNote && ` — ${r.decisionNote}`}
          </Text>
        </Box>
      )}

      {pending && (
        <Box pt="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
          {expanded && (
            <Textarea
              size="xs"
              mb="xs"
              placeholder="Decision note — required when denying, optional when approving."
              autosize
              minRows={2}
              value={note}
              onChange={(e) => setNote(e.currentTarget.value)}
            />
          )}
          <Group justify="space-between">
            <Button
              size="compact-xs"
              variant="subtle"
              color="slate"
              onClick={() => setExpanded((v) => !v)}
            >
              {expanded ? 'Hide note' : 'Add note'}
            </Button>
            <Group gap="xs">
              <Button
                size="compact-xs"
                variant="light"
                color="rose"
                leftSection={<IconX size={13} />}
                loading={decide.isPending}
                onClick={() => onDecide('denied')}
              >
                Deny
              </Button>
              <Button
                size="compact-xs"
                color="teal"
                leftSection={<IconCheck size={13} />}
                loading={decide.isPending}
                onClick={() => onDecide('approved')}
              >
                Approve {r.durationMinutes / 60}h
              </Button>
            </Group>
          </Group>
        </Box>
      )}
    </Card>
  )
}

/**
 * Five fields and two rules.
 *
 * This was the only consumer of @tanstack/react-form in the application, and it
 * cost 25 kB gzipped — more than the route it lived on. Plain state does the
 * same job here; the validation is two pure predicates that the submit button
 * and the field errors both read, so there is one definition of "valid" rather
 * than a library's copy and ours.
 */
function NewRequestModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const { data: assets } = useQuery(assetsQuery({}))
  const create = useCreateRequest()

  const [value, setValue] = useState(EMPTY_REQUEST)
  const [touched, setTouched] = useState<Record<string, boolean>>({})

  const set = <K extends keyof RequestDraft>(k: K, v: RequestDraft[K]) =>
    setValue((prev) => ({ ...prev, [k]: v }))

  const errors = {
    assetHostnames: validateAssetIds(value.assetHostnames),
    justification: validateJustification(value.justification),
  }
  const canSubmit = !errors.assetHostnames && !errors.justification

  const close = () => {
    setValue(EMPTY_REQUEST)
    setTouched({})
    onClose()
  }

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    // Marking everything touched first means a submit attempt reveals whichever
    // field is blocking it, rather than leaving a disabled button unexplained.
    setTouched({ assetHostnames: true, justification: true })
    if (!canSubmit) return
    try {
      await create.mutateAsync(value)
    } catch (err) {
      // Kept open on failure. Closing would discard a justification the user
      // just wrote for a request that was never filed.
      notifyError('Request not submitted', err)
      return
    }
    notifications.show({
      color: 'teal',
      title: 'Request submitted',
      message: 'Approvers have been notified. You will get access the moment it is granted.',
    })
    close()
  }

  const assetOptions =
    assets?.map((a) => ({
      value: a.hostname,
      label: `${a.hostname.split('.')[0]} — ${a.address}`,
    })) ?? []

  return (
    <Modal opened={opened} onClose={close} title="Request privileged access" size="lg">
      <form onSubmit={onSubmit}>
        <Stack gap="md">
          <MultiSelect
            label="Target hosts"
            description="Request only what the task needs — broad scope gets denied."
            placeholder="Search hosts"
            searchable
            clearable
            maxDropdownHeight={240}
            data={assetOptions}
            value={value.assetHostnames}
            onChange={(v) => set('assetHostnames', v)}
            onBlur={() => setTouched((t) => ({ ...t, assetHostnames: true }))}
            error={touched.assetHostnames ? errors.assetHostnames : undefined}
          />

          <Radio.Group
            label="Connect as"
            value={value.principal}
            onChange={(v) => set('principal', v)}
          >
            <Group gap="lg" mt={6}>
              <Radio value="deploy" label="deploy" size="xs" />
              <Radio value="ops" label="ops" size="xs" />
              <Radio
                value="root"
                size="xs"
                label={
                  <Group gap={6}>
                    <Text size="sm">root</Text>
                    <Badge size="xs" color="rose">elevated</Badge>
                  </Group>
                }
              />
            </Group>
          </Radio.Group>

          <Box>
            <Text size="sm" fw={500} mb={6}>Window</Text>
            <SegmentedControl
              fullWidth
              size="xs"
              value={String(value.durationMinutes)}
              onChange={(v) => set('durationMinutes', Number(v))}
              data={[
                { label: '30 min', value: '30' },
                { label: '1 hour', value: '60' },
                { label: '2 hours', value: '120' },
                { label: '4 hours', value: '240' },
              ]}
            />
            <Text size={FS.micro} c="dimmed" mt={6}>
              Policy caps operator grants at {MAX_DURATION_MINUTES / 60} hours. Access
              revokes itself when the window closes — no cleanup task to forget.
            </Text>
          </Box>

          <Textarea
            label="Justification"
            description="Reference the incident or change ticket. This is what the approver sees and what the audit log keeps."
            placeholder="INC-4471 — settlement worker stuck in retry loop, need to inspect queue depth on the primary."
            minRows={3}
            autosize
            value={value.justification}
            onChange={(e) => set('justification', e.currentTarget.value)}
            onBlur={() => setTouched((t) => ({ ...t, justification: true }))}
            error={touched.justification ? errors.justification : undefined}
          />

          <Box>
            <Checkbox
              size="xs"
              color="rose"
              label="Break-glass — production is down and no approver is reachable"
              checked={value.breakGlass}
              onChange={(e) => set('breakGlass', e.currentTarget.checked)}
            />
            {value.breakGlass && (
              <Alert color="rose" variant="light" mt="xs" icon={<IconAlertTriangle size={15} />}>
                <Text size="xs">
                  Break-glass grants access immediately and notifies every admin plus the
                  security channel. The credential is revoked and regenerated when the window
                  closes, and the session is reviewed. Use it when the alternative is a longer
                  outage — not to skip the queue.
                </Text>
              </Alert>
            )}
          </Box>

          <Group justify="flex-end" mt="xs">
            <Button variant="subtle" color="slate" size="xs" onClick={close}>
              Cancel
            </Button>
            <Button type="submit" size="xs" disabled={!canSubmit} loading={create.isPending}>
              Submit request
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
