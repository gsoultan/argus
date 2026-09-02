import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Checkbox, Group, Modal, MultiSelect, Radio,
  SegmentedControl, Stack, Text, Textarea, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { notifications } from '@mantine/notifications'
import { useForm } from '@tanstack/react-form'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconCheck, IconClockHour4, IconPlus, IconX,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { Mono, RequestStateBadge, absTime, relTime } from '~/components/primitives'
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

const validateAssetIds = ({ value }: { value: string[] }) =>
  value.length === 0 ? 'Select at least one host.' : undefined

const validateJustification = ({ value }: { value: string }) =>
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
    if (decision === 'denied' && note.trim().length < 5) {
      setExpanded(true)
      notifications.show({
        color: 'amber',
        title: 'Reason required',
        message: 'A denial must say why, so the requester can act on it.',
      })
      return
    }
    await decide.mutateAsync({ id: r.id, decision, note })
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
          <Group gap={7} mb={3}>
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
          <Group gap={5} wrap="wrap">
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
          <Text size="10px" c="dimmed">{relTime(r.createdAt)}</Text>
          {r.expiresAt && r.state === 'approved' && (
            <Group gap={4} justify="flex-end" mt={3}>
              <IconClockHour4 size={11} className="text-amber-400" />
              <Text size="10px" c="amber.4">expires {relTime(r.expiresAt)}</Text>
            </Group>
          )}
        </Box>
      </Group>

      {!pending && (
        <Box pt="xs" style={{ borderTop: '1px solid var(--color-line)' }}>
          <Text size="10px" c="dimmed">
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

function NewRequestModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const { data: assets } = useQuery(assetsQuery({}))
  const create = useCreateRequest()

  const form = useForm({
    defaultValues: {
      assetHostnames: [] as string[],
      principal: 'ops',
      durationMinutes: 60,
      justification: '',
      breakGlass: false,
    },
    onSubmit: async ({ value }) => {
      await create.mutateAsync(value)
      notifications.show({
        color: 'teal',
        title: 'Request submitted',
        message: 'Approvers have been notified. You will get access the moment it is granted.',
      })
      form.reset()
      onClose()
    },
  })

  const assetOptions =
    assets?.map((a) => ({
      value: a.hostname,
      label: `${a.hostname.split('.')[0]} — ${a.address}`,
    })) ?? []

  return (
    <Modal opened={opened} onClose={onClose} title="Request privileged access" size="lg">
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void form.handleSubmit()
        }}
      >
        <Stack gap="md">
          <form.Field
            name="assetHostnames"
            validators={{ onMount: validateAssetIds, onChange: validateAssetIds }}
          >
            {(field) => (
              <MultiSelect
                label="Target hosts"
                description="Request only what the task needs — broad scope gets denied."
                placeholder="Search hosts"
                searchable
                clearable
                maxDropdownHeight={240}
                data={assetOptions}
                value={field.state.value}
                onChange={field.handleChange}
                onBlur={field.handleBlur}
                error={field.state.meta.errors[0]}
              />
            )}
          </form.Field>

          <form.Field name="principal">
            {(field) => (
              <Radio.Group
                label="Connect as"
                value={field.state.value}
                onChange={field.handleChange}
              >
                <Group gap="lg" mt={6}>
                  <Radio value="deploy" label="deploy" size="xs" />
                  <Radio value="ops" label="ops" size="xs" />
                  <Radio
                    value="root"
                    size="xs"
                    label={
                      <Group gap={5}>
                        <Text size="sm">root</Text>
                        <Badge size="xs" color="rose">elevated</Badge>
                      </Group>
                    }
                  />
                </Group>
              </Radio.Group>
            )}
          </form.Field>

          <form.Field name="durationMinutes">
            {(field) => (
              <Box>
                <Text size="sm" fw={500} mb={6}>Window</Text>
                <SegmentedControl
                  fullWidth
                  size="xs"
                  value={String(field.state.value)}
                  onChange={(v) => field.handleChange(Number(v))}
                  data={[
                    { label: '30 min', value: '30' },
                    { label: '1 hour', value: '60' },
                    { label: '2 hours', value: '120' },
                    { label: '4 hours', value: '240' },
                  ]}
                />
                <Text size="10px" c="dimmed" mt={5}>
                  Policy caps operator grants at {MAX_DURATION_MINUTES / 60} hours. Access
                  revokes itself when the window closes — no cleanup task to forget.
                </Text>
              </Box>
            )}
          </form.Field>

          <form.Field
            name="justification"
            validators={{ onMount: validateJustification, onChange: validateJustification }}
          >
            {(field) => (
              <Textarea
                label="Justification"
                description="Reference the incident or change ticket. This is what the approver sees and what the audit log keeps."
                placeholder="INC-4471 — settlement worker stuck in retry loop, need to inspect queue depth on the primary."
                minRows={3}
                autosize
                value={field.state.value}
                onChange={(e) => field.handleChange(e.currentTarget.value)}
                onBlur={field.handleBlur}
                error={field.state.meta.isTouched ? field.state.meta.errors[0] : undefined}
              />
            )}
          </form.Field>

          <form.Field name="breakGlass">
            {(field) => (
              <Box>
                <Checkbox
                  size="xs"
                  color="rose"
                  label="Break-glass — production is down and no approver is reachable"
                  checked={field.state.value}
                  onChange={(e) => field.handleChange(e.currentTarget.checked)}
                />
                {field.state.value && (
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
            )}
          </form.Field>

          <Group justify="flex-end" mt="xs">
            <Button variant="subtle" color="slate" size="xs" onClick={onClose}>
              Cancel
            </Button>
            <form.Subscribe selector={(s) => [s.canSubmit, s.isSubmitting] as const}>
              {([canSubmit, isSubmitting]) => (
                <Button type="submit" size="xs" disabled={!canSubmit} loading={isSubmitting}>
                  Submit request
                </Button>
              )}
            </form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
