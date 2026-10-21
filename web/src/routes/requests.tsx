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
  IconAlertTriangle, IconCheck, IconClipboardCheck, IconClockHour4, IconPlus, IconX,
} from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { Mono, RequestStateBadge, absTime, relTime } from '~/components/primitives'
import { FS, SP } from '~/theme'
import { notifyError, notifyWarn } from '~/lib/notify'
import {
  assetsQuery, meQuery, requestsQuery, useCreateRequest, useDecideRequest,
} from '~/lib/queries'
import { canSee } from '~/lib/roles'
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
  const { data: me } = useQuery(meQuery())
  // Whoever can decide requests is sent the whole queue; everyone else is sent
  // their own (the control plane narrows it), so the page talks to each.
  const decides = canSee(me?.role, 'approvers')

  return (
    <Box>
      <PageHeader
        title={decides ? 'Access requests' : 'My access requests'}
        description={
          decides
            ? 'Ask for temporary access to a server, or decide on someone else\u2019s request. Someone other than you approves it, and access ends by itself when the time is up.'
            : 'Ask for temporary access to a server. An approver decides, and the access ends by itself when the time is up. Approved servers show on My servers under Temporary access.'
        }
        actions={
          <Button leftSection={<IconPlus size={14} />} onClick={newModal.open}>
            Request access
          </Button>
        }
      />

      <PageBody>
        {/* Queue/History narrows the list below rather than acting on anything,
            so it sits with the list. It used to share the header's action slot
            with the button that files a new request. */}
        <Toolbar
          right={
            requests && requests.length > 0 ? (
              <Text size={FS.micro} c="dimmed">
                {requests.length} {tab === 'pending' ? 'waiting' : 'total'}
              </Text>
            ) : undefined
          }
        >
          <SegmentedControl
            value={tab}
            onChange={(v) => setTab(v as 'pending' | 'all')}
            data={[
              { label: decides ? 'Queue' : 'Waiting', value: 'pending' },
              { label: 'History', value: 'all' },
            ]}
          />
        </Toolbar>

        <Stack gap="sm">
          {requests?.map((r) => (
            <RequestCard
              key={r.id}
              request={r}
              // The control plane refuses a decision from anyone else, and
              // refuses anyone deciding their own, so neither gets the buttons.
              canDecide={decides && r.requesterEmail.toLowerCase() !== me?.email.toLowerCase()}
            />
          ))}
          {requests?.length === 0 && (
            <Card>
              <EmptyState
                icon={IconClipboardCheck}
                title={
                  !decides
                    ? tab === 'pending' ? 'None of your requests are waiting.' : 'You have not asked for access yet.'
                    : tab === 'pending' ? 'Nothing waiting for a decision.' : 'No requests yet.'
                }
                description={
                  !decides
                    ? 'Press Request access to ask for a server or an account you do not have. An approver will decide.'
                    : tab === 'pending'
                      ? 'Every request has been decided. Approved grants expire on their own when their window closes.'
                      : 'Nobody has asked for privileged access. Requests filed here are what the audit log records a decision against.'
                }
                action={
                  <Button
                    variant="light"
                    leftSection={<IconPlus size={14} />}
                    onClick={newModal.open}
                  >
                    Request access
                  </Button>
                }
              />
            </Card>
          )}
        </Stack>
      </PageBody>

      <NewRequestModal opened={newOpen} onClose={newModal.close} />
    </Box>
  )
}

function RequestCard({ request: r, canDecide }: { request: AccessRequest; canDecide: boolean }) {
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
      // A stable handle for the list, the way .argus-stat marks a dashboard
      // tile. The Overview's "Pending approvals" tile lands here and the two
      // numbers have to be checkable against each other.
      className="argus-request"
      style={r.breakGlass && pending ? { borderColor: 'var(--color-denied)' } : undefined}
    >
      <Group justify="space-between" align="flex-start" wrap="nowrap" mb="sm" gap="md">
        <Box style={{ minWidth: 0 }}>
          <Group gap={SP.cozy} mb={SP.tight}>
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
          <Text size={FS.body} c="dimmed" mb="xs" lh={1.5}>{r.justification}</Text>
          <Group gap={SP.snug} wrap="wrap">
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
            <Group gap={SP.tight} justify="flex-end" mt={SP.tight}>
              <IconClockHour4 size={11} style={{ color: 'var(--color-pending)' }} />
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

      {pending && !canDecide && (
        <Box pt="xs" style={{ borderTop: '1px solid var(--color-line)' }}>
          <Text size={FS.micro} c="dimmed">Waiting for an approver.</Text>
        </Box>
      )}

      {pending && canDecide && (
        <Box pt="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
          {expanded && (
            <Textarea
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
    <Modal opened={opened} onClose={close} title="Request access" size="lg">
      <form onSubmit={onSubmit}>
        <Stack gap="md">
          <MultiSelect
            size="sm"
            label="Servers"
            description="Pick only the servers the task needs. Asking for many at once usually gets denied."
            placeholder="Type a server name"
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
            label="Log in as"
            value={value.principal}
            onChange={(v) => set('principal', v)}
          >
            <Group gap="lg" mt={SP.snug}>
              <Radio value="deploy" label="deploy" />
              <Radio value="ops" label="ops" />
              <Radio
                value="root"
                label={
                  <Group gap={SP.snug}>
                    <Text size="sm">root</Text>
                    <Badge size="xs" color="rose">elevated</Badge>
                  </Group>
                }
              />
            </Group>
          </Radio.Group>

          <Box>
            <Text size="sm" fw={500} mb={SP.snug}>Window</Text>
            <SegmentedControl
              fullWidth
              value={String(value.durationMinutes)}
              onChange={(v) => set('durationMinutes', Number(v))}
              data={[
                { label: '30 min', value: '30' },
                { label: '1 hour', value: '60' },
                { label: '2 hours', value: '120' },
                { label: '4 hours', value: '240' },
              ]}
            />
            <Text size={FS.micro} c="dimmed" mt={SP.snug} lh={1.5}>
              Policy caps operator grants at {MAX_DURATION_MINUTES / 60} hours. Access
              revokes itself when the window closes — no cleanup task to forget.
            </Text>
          </Box>

          <Textarea
            size="sm"
            label="Why do you need it?"
            description="Mention the incident or change ticket if there is one. The approver reads this, and it is kept in the audit log."
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
              color="rose"
              label="Break-glass — production is down and no approver is reachable"
              checked={value.breakGlass}
              onChange={(e) => set('breakGlass', e.currentTarget.checked)}
            />
            {value.breakGlass && (
              <Alert color="rose" mt="xs" icon={<IconAlertTriangle size={15} />}>
                <Text size={FS.body} lh={1.5}>
                  Break-glass grants access immediately and notifies every admin plus the
                  security channel. The credential is revoked and regenerated when the window
                  closes, and the session is reviewed. Use it when the alternative is a longer
                  outage — not to skip the queue.
                </Text>
              </Alert>
            )}
          </Box>

          <Group justify="flex-end" mt={SP.tight}>
            <Button variant="subtle" color="slate" onClick={close}>
              Cancel
            </Button>
            <Button type="submit" disabled={!canSubmit} loading={create.isPending}>
              Submit request
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
