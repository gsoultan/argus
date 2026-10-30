import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Checkbox, Group, Modal, MultiSelect, Radio,
  SegmentedControl, Stack, Text, TextInput, Textarea, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { notifications } from '@mantine/notifications'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconCheck, IconClipboardCheck, IconClockHour4, IconPlus, IconTerminal2, IconX,
} from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { Mono, RequestStateBadge, absTime, relTime } from '~/components/primitives'
import { FS, SP } from '~/theme'
import { notifyError, notifyWarn } from '~/lib/notify'
import {
  assetsQuery, directLoginsQuery, meQuery, requestsQuery, useCreateRequest, useDecideDirectLogin,
  useDecideRequest,
} from '~/lib/queries'
import { canSee } from '~/lib/roles'
import type { AccessRequest, DirectLogin } from '~/types/domain'

/**
 * Everything waiting on a person, in one place: logins straight to a server
 * that its rule holds, and access people asked for. It is a menu item of its
 * own because for the first kind someone is sitting at a terminal.
 *
 * Open to everyone. Someone who cannot decide sees their own requests here and
 * asks for access from it; the menu only lists it for those who decide.
 */
export const Route = createFileRoute('/approvals')({
  component: Approvals,
  // `ask` opens the request form for one server, from a server someone could
  // not open; `as` is the account they wanted.
  validateSearch: (s: Record<string, unknown>): { ask?: string; as?: string } => ({
    ...(typeof s.ask === 'string' && s.ask ? { ask: s.ask } : {}),
    ...(typeof s.as === 'string' && s.as ? { as: s.as } : {}),
  }),
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

function Approvals() {
  const [tab, setTab] = useState<'pending' | 'all'>('pending')
  const { data: requests } = useQuery(requestsQuery(tab === 'pending' ? 'pending' : undefined))
  const search = Route.useSearch()
  const [newOpen, newModal] = useDisclosure(Boolean(search.ask))
  const { data: me } = useQuery(meQuery())
  // Whoever can decide requests is sent the whole queue; everyone else is sent
  // their own (the control plane narrows it), so the page talks to each.
  const decides = canSee(me?.role, 'approvers')

  return (
    <Box>
      <PageHeader
        title={decides ? 'Approvals' : 'My requests'}
        description={
          decides
            ? 'People waiting on you. Logins straight to a server come first, because someone is at a terminal.'
            : 'Access you asked for. An approver decides, and it ends by itself when the time is up.'
        }
        actions={
          <Button variant={decides ? 'default' : 'filled'} leftSection={<IconPlus size={14} />} onClick={newModal.open}>
            Ask for access
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

        {decides && tab === 'pending' && (
          <DirectLoginQueue
            canDecide={me?.role === 'approver' || me?.role === 'admin' || me?.role === 'owner'}
          />
        )}

        {decides && (
          <Group gap={SP.cozy}>
            <IconClipboardCheck size={16} />
            <Text fw={600} size="sm">Access asked for</Text>
          </Group>
        )}
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
                    ? 'Press Ask for access to ask for a server or an account you do not have. An approver will decide.'
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
                    Ask for access
                  </Button>
                }
              />
            </Card>
          )}
        </Stack>
      </PageBody>

      <NewRequestModal opened={newOpen} onClose={newModal.close} host={search.ask} account={search.as} />
    </Box>
  )
}

/**
 * Direct SSH logins waiting for a decision. Someone is sitting at a terminal
 * for each of these, so they come first and the list polls.
 */
function DirectLoginQueue({ canDecide }: { canDecide: boolean }) {
  const { data } = useQuery(directLoginsQuery())
  const now = Date.now()
  const waiting = (data ?? []).filter(
    (d) => d.state === 'pending' && new Date(d.expiresAt).getTime() > now,
  )
  if (waiting.length === 0) return null
  return (
    <Stack gap="sm">
      <Group gap={SP.cozy}>
        <IconTerminal2 size={16} />
        <Text fw={600} size="sm">Logins straight to a server</Text>
        <Badge size="xs" color="amber">{waiting.length}</Badge>
      </Group>
      {waiting.map((d) => <DirectLoginCard key={d.id} login={d} canDecide={canDecide} />)}
    </Stack>
  )
}

function DirectLoginCard({ login: d, canDecide }: { login: DirectLogin; canDecide: boolean }) {
  const decide = useDecideDirectLogin()
  const [note, setNote] = useState('')

  const onDecide = async (approve: boolean) => {
    try {
      await decide.mutateAsync({ id: d.id, approve, note })
    } catch (err) {
      notifyError(approve ? 'Login not approved' : 'Login not denied', err)
      return
    }
    notifications.show({
      color: approve ? 'teal' : 'rose',
      title: approve ? 'Login approved' : 'Login denied',
      message: approve
        ? `${d.principal} on ${d.hostname} is let in now, and the session is recorded.`
        : `${d.principal} on ${d.hostname} is refused.`,
    })
  }

  return (
    <Card className="argus-direct-login" style={{ borderColor: 'var(--mantine-color-amber-6)' }}>
      <Group justify="space-between" align="flex-start" wrap="wrap" gap="md">
        <Box style={{ minWidth: 0 }}>
          <Text fw={600} size="sm">
            <Mono>{d.principal}</Mono> on <Mono>{d.hostname}</Mono>
          </Text>
          <Text size={FS.meta} c="dimmed" mt={SP.hair}>
            From {d.clientAddr || 'an unknown address'}
            {d.command ? <>, to run <Mono>{d.command}</Mono></> : ', for an interactive shell'}
            {' · '}asked <Tooltip label={absTime(d.createdAt)}><span>{relTime(d.createdAt)}</span></Tooltip>
            {' · '}refused in {secondsLeft(d.expiresAt)} if nobody answers
          </Text>
          <Text size={FS.micro} c="dimmed" mt={SP.tight} lh={1.5}>
            The server already accepted this account&rsquo;s key or password. Argus cannot tell
            which person is using it, so approve only a login you expect.
          </Text>
        </Box>
        {canDecide && <Stack gap={SP.snug} style={{ flexShrink: 0 }}>
          <TextInput
            size="xs"
            aria-label="Note for the audit log"
            placeholder="Note for the audit log (optional)"
            maxLength={1000}
            value={note}
            onChange={(e) => setNote(e.currentTarget.value)}
            w={260}
          />
          <Group gap="xs" justify="flex-end">
            <Button size="xs" variant="light" color="rose" leftSection={<IconX size={14} />}
              loading={decide.isPending && decide.variables?.approve === false}
              disabled={decide.isPending} onClick={() => void onDecide(false)}>
              Deny
            </Button>
            <Button size="xs" color="teal" leftSection={<IconCheck size={14} />}
              loading={decide.isPending && decide.variables?.approve === true}
              disabled={decide.isPending} onClick={() => void onDecide(true)}>
              Approve
            </Button>
          </Group>
        </Stack>}
      </Group>
    </Card>
  )
}

function secondsLeft(iso: string): string {
  const s = Math.max(0, Math.round((Date.parse(iso) - Date.now()) / 1000))
  return s >= 60 ? `${Math.floor(s / 60)}m ${s % 60}s` : `${s}s`
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
function NewRequestModal({ opened, onClose, host, account }: {
  opened: boolean
  onClose: () => void
  host?: string
  account?: string
}) {
  const { data: assets } = useQuery(assetsQuery({}))
  const create = useCreateRequest()

  const [value, setValue] = useState<RequestDraft>(() => ({
    ...EMPTY_REQUEST,
    ...(host ? { assetHostnames: [host] } : {}),
    ...(account ? { principal: account } : {}),
  }))
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
  // A server you cannot open is not in your list, so the one you came from is
  // added by name. The control plane decides whether it exists.
  if (host && !assetOptions.some((o) => o.value === host)) assetOptions.unshift({ value: host, label: host })

  return (
    <Modal opened={opened} onClose={close} title="Ask for access" size="lg">
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
              {!['deploy', 'ops', 'root'].includes(value.principal) && (
                <Radio value={value.principal} label={value.principal} />
              )}
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
