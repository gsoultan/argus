import { useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Code, Grid, Group, List, Modal, Skeleton, Stack,
  Switch, Text, ThemeIcon,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconDeviceDesktop, IconInfoCircle, IconLock, IconNetwork,
  IconShieldLock, IconTerminal2,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { Mono } from '~/components/primitives'
import { meQuery, policyQuery, useSavePolicy } from '~/lib/queries'
import { notifyError, notifyOk } from '~/lib/notify'
import { isConfigured } from '~/lib/live'
import { FS } from '~/theme'
import type { GatewayPolicy, PolicyKey } from '~/types/domain'

export const Route = createFileRoute('/settings')({
  component: Settings,
  loader: ({ context }) => context.queryClient.ensureQueryData(policyQuery()),
})

interface FieldSpec {
  key: PolicyKey
  label: string
  description: string
  /**
   * True when turning the field ON loosens the gateway.
   *
   * Drives both the badge and the confirmation step, so "which way is
   * dangerous" is stated once instead of being implied by each caller.
   */
  riskOn: boolean
}

const SSH_POLICY: FieldSpec[] = [
  {
    key: 'allowLocalForward',
    label: 'Allow local port forwarding (-L)',
    description:
      'Unrestricted forwarding turns the gateway into an open tunnel into the private network — the exact thing a bastion exists to prevent. Leave off and allowlist per role.',
    riskOn: true,
  },
  {
    key: 'allowRemoteForward',
    label: 'Allow remote port forwarding (-R)',
    description:
      'Lets a target open a listener back through the gateway. Rarely needed, and a clean egress path for an attacker who already has the host.',
    riskOn: true,
  },
  {
    key: 'allowAgentForward',
    label: 'Allow SSH agent forwarding',
    description:
      "Anyone with root on the gateway can sign challenges with the user's keys for the life of the session. Credential injection exists precisely so this is unnecessary.",
    riskOn: true,
  },
  {
    key: 'allowX11Forward',
    label: 'Allow X11 forwarding',
    description: 'Broad attack surface, almost never used for server administration.',
    riskOn: true,
  },
  {
    key: 'proxySftpSubsystem',
    label: 'Proxy SFTP as a subsystem',
    description:
      'Decodes the SFTP protocol rather than teeing raw bytes, so every file open, read, write and delete becomes an audit event with a path and a size.',
    riskOn: false,
  },
]

const RECORDING_POLICY: FieldSpec[] = [
  {
    key: 'failClosedOnRecordingLoss',
    label: 'Fail closed when recording is unavailable',
    description:
      "If the recorder cannot write, refuse the session rather than allowing an unrecorded one. This is what an auditor means by 'all privileged sessions are recorded'.",
    riskOn: false,
  },
  {
    key: 'requireEbpfForRoot',
    label: 'Require eBPF agent for root sessions',
    description:
      'PTY capture alone can be defeated by base64 or by running a script. For root, insist on kernel-observed execve evidence.',
    riskOn: false,
  },
]

const ALL_FIELDS = [...SSH_POLICY, ...RECORDING_POLICY]

/** Roles the control plane lets change gateway policy. */
const CAN_EDIT_POLICY = new Set(['owner', 'admin'])

function Settings() {
  const { data: saved } = useQuery(policyQuery())
  const { data: me } = useQuery(meQuery())
  const savePolicy = useSavePolicy()
  const [confirmOpen, confirm] = useDisclosure(false)

  /**
   * Edits live here until they are saved.
   *
   * The switches used to be uncontrolled, with a `defaultChecked` and no
   * handler: they moved, nothing was sent anywhere, and navigating away
   * reverted them silently. For a page whose toggles decide whether port
   * forwarding is permitted and whether an unrecorded session may proceed, a
   * control that looks set and is not is the worst thing on the screen.
   */
  const [draft, setDraft] = useState<GatewayPolicy | null>(null)
  const current = draft ?? saved ?? null

  const canEdit = me ? CAN_EDIT_POLICY.has(me.role) : false

  const changes = useMemo(() => {
    if (!saved || !draft) return []
    return ALL_FIELDS.filter((f) => draft[f.key] !== saved[f.key])
  }, [saved, draft])

  const loosening = changes.filter((f) => f.riskOn === Boolean(draft?.[f.key]))

  const set = (key: PolicyKey, value: boolean) => {
    if (!saved) return
    setDraft({ ...(draft ?? saved), [key]: value })
  }

  const discard = () => setDraft(null)

  const commit = async () => {
    if (!draft) return
    try {
      await savePolicy.mutateAsync(draft)
    } catch (err) {
      // Draft kept, dialog closed: the operator can see what they asked for and
      // retry, instead of losing the edit to an error toast.
      confirm.close()
      notifyError('Policy not saved', err)
      return
    }
    confirm.close()
    setDraft(null)
    notifyOk(
      'Gateway policy saved',
      `${changes.length} change${changes.length === 1 ? '' : 's'} applied. New sessions use it immediately; sessions already open keep the policy they started under.`,
    )
  }

  return (
    <Box>
      <PageHeader
        title="Settings"
        description="Gateway policy. Defaults are the safe choice — each toggle says what loosening it costs you."
        actions={
          changes.length > 0 ? (
            <>
              <Button size="xs" variant="subtle" color="slate" onClick={discard}>
                Discard
              </Button>
              <Button
                size="xs"
                color={loosening.length > 0 ? 'amber' : 'teal'}
                loading={savePolicy.isPending}
                onClick={confirm.open}
              >
                Review {changes.length} change{changes.length === 1 ? '' : 's'}
              </Button>
            </>
          ) : null
        }
      />

      <Box p="lg">
        {!canEdit && me && (
          <Alert
            color="slate"
            variant="light"
            icon={<IconInfoCircle size={16} />}
            mb="md"
            title="Read-only"
          >
            <Text size="xs">
              Gateway policy is changed by an owner or admin. Your role is{' '}
              <Mono>{me.role}</Mono>, so these are shown as configured rather than as
              controls you can move.
            </Text>
          </Alert>
        )}

        {/* Keyed on whether a control plane is configured, because that is
            exactly the condition under which savePolicy writes to the fixture
            instead of the control plane. It used to be keyed on isLive(),
            which any single refused request turned false -- so an admin could
            be told their changes reached no gateway while the save was
            reaching the gateway, and open a forwarding channel believing it
            was a local sandbox. */}
        {!isConfigured() && (
          <Alert
            color="amber"
            variant="light"
            icon={<IconAlertTriangle size={16} />}
            mb="md"
            title="Not connected to a control plane"
          >
            <Text size="xs">
              Changes are held in this browser session only and reach no gateway. Point{' '}
              <Mono>VITE_CONTROL_URL</Mono> at a running control plane to make this page
              authoritative.
            </Text>
          </Alert>
        )}

        <Grid gap="sm">
          <Grid.Col span={{ base: 12, lg: 7 }}>
            <Stack gap="sm">
              <PolicyCard
                icon={<IconTerminal2 size={13} />}
                title="SSH channel policy"
                note={
                  <>
                    A forwarded connection's <strong>contents are not recorded</strong>. The
                    tunnel carries someone else's protocol and can be arbitrarily large, so
                    Argus records the fact of it instead — who, from where, to which host and
                    port, when it opened and closed, and how many bytes moved each way. Those
                    land in the audit chain, not just the session recording.
                  </>
                }
                fields={SSH_POLICY}
                policy={current}
                saved={saved ?? null}
                disabled={!canEdit || savePolicy.isPending}
                onChange={set}
              />
              <PolicyCard
                icon={<IconLock size={13} />}
                title="Recording & retention"
                fields={RECORDING_POLICY}
                policy={current}
                saved={saved ?? null}
                disabled={!canEdit || savePolicy.isPending}
                onChange={set}
              />
            </Stack>
          </Grid.Col>

          <Grid.Col span={{ base: 12, lg: 5 }}>
            <Stack gap="sm">
              <Card padding="md">
                <Group gap={8} mb="sm">
                  <ThemeIcon variant="light" color="sky" size={22} radius="sm">
                    <IconNetwork size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Gateway endpoint</Text>
                </Group>
                <Text size={FS.micro} c="dimmed" mb="xs" lh={1.45}>
                  Users connect with their normal client. The target is encoded in the username,
                  so there is nothing to install and existing tooling keeps working.
                </Text>
                <Code block fz={FS.digest}>
                  {`ssh ops:db-01@argus.northwind.id
scp report.csv ops:db-01@argus.northwind.id:/tmp/
sftp ops:db-01@argus.northwind.id`}
                </Code>
              </Card>

              <Card padding="md">
                <Group gap={8} mb="sm">
                  <ThemeIcon variant="light" color="teal" size={22} radius="sm">
                    <IconShieldLock size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Certificate authority</Text>
                </Group>
                <Text size={FS.micro} c="dimmed" mb="xs" lh={1.45}>
                  Add this to a host to move it off standing credentials. Argus then mints a
                  short-lived certificate per session and there is nothing left in the vault to
                  steal for that host.
                </Text>
                <Code block fz={FS.micro}>
                  {`# /etc/ssh/sshd_config
TrustedUserCAKeys /etc/ssh/argus_ca.pub`}
                </Code>
              </Card>

              <Alert color="sky" variant="light" icon={<IconInfoCircle size={16} />}>
                <Text size="xs" fw={600} mb={4}>Threat model, stated plainly</Text>
                <Text size="xs" lh={1.5}>
                  Argus terminates SSH, so the gateway holds session plaintext in memory. That is
                  the price of working against hosts with no agent installed. Treat gateway nodes
                  as your highest-value asset: no shared tenancy, no third-party agents, hardware
                  keys for the vault, and the audit log replicated somewhere the gateway cannot
                  write.
                </Text>
              </Alert>

              <Card padding="md" style={{ borderStyle: 'dashed' }}>
                <Group gap={8} mb={6}>
                  <ThemeIcon variant="light" color="slate" size={22} radius="sm">
                    <IconDeviceDesktop size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Windows RDP</Text>
                  <Badge size="xs" color="slate" variant="outline">phase 3</Badge>
                </Group>
                <Text size={FS.micro} c="dimmed" lh={1.45}>
                  RDP runs as a separate service rather than inside the SSH gateway — different
                  protocol, different recording pipeline, different failure modes. Linux SSH gets
                  finished first.
                </Text>
              </Card>

              <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />}>
                <Text size="xs">
                  Command detection from PTY output is a heuristic, and <Mono>argus</Mono> labels
                  it as such everywhere it appears. Do not present it to an auditor as proof of
                  what ran — that is what the eBPF tier is for.
                </Text>
              </Alert>
            </Stack>
          </Grid.Col>
        </Grid>
      </Box>

      <Modal
        opened={confirmOpen}
        onClose={confirm.close}
        title="Apply gateway policy"
        size="md"
      >
        {loosening.length > 0 && (
          <Alert color="rose" variant="light" icon={<IconAlertTriangle size={16} />} mb="md">
            <Text size="xs">
              {loosening.length === 1 ? 'One change loosens' : `${loosening.length} changes loosen`}{' '}
              the gateway. Every session opened after this uses the new policy.
            </Text>
          </Alert>
        )}
        <Text size="xs" c="dimmed" mb="xs">
          The whole policy is written, and the change is recorded in the audit log against
          your account.
        </Text>
        <List size="xs" spacing={6}>
          {changes.map((f) => {
            const on = Boolean(draft?.[f.key])
            return (
              <List.Item
                key={f.key}
                icon={
                  <ThemeIcon
                    size={16}
                    radius="sm"
                    variant="light"
                    color={f.riskOn === on ? 'rose' : 'teal'}
                  >
                    {on ? <IconLock size={10} /> : <IconAlertTriangle size={10} />}
                  </ThemeIcon>
                }
              >
                <Text size="xs" span>{f.label} — </Text>
                <Text size="xs" span fw={600} c={f.riskOn === on ? 'rose.4' : 'teal.4'}>
                  {on ? 'enabled' : 'disabled'}
                </Text>
              </List.Item>
            )
          })}
        </List>
        <Group justify="flex-end" mt="md">
          <Button size="xs" variant="subtle" color="slate" onClick={confirm.close}>
            Cancel
          </Button>
          <Button
            size="xs"
            color={loosening.length > 0 ? 'rose' : 'teal'}
            loading={savePolicy.isPending}
            onClick={commit}
          >
            Apply policy
          </Button>
        </Group>
      </Modal>
    </Box>
  )
}

function PolicyCard({
  icon, title, note, fields, policy, saved, disabled, onChange,
}: {
  icon: React.ReactNode
  title: string
  /** Stated once for the card rather than repeated on every toggle. */
  note?: React.ReactNode
  fields: FieldSpec[]
  policy: GatewayPolicy | null
  saved: GatewayPolicy | null
  disabled: boolean
  onChange: (key: PolicyKey, value: boolean) => void
}) {
  return (
    <Card padding="md">
      <Group gap={8} mb="md">
        <ThemeIcon variant="light" color="teal" size={22} radius="sm">
          {icon}
        </ThemeIcon>
        <Text fw={600} size="sm">{title}</Text>
      </Group>
      {note && (
        <Alert color="slate" variant="light" icon={<IconInfoCircle size={15} />} mb="md">
          <Text size={FS.micro} lh={1.5}>{note}</Text>
        </Alert>
      )}
      <Stack gap="lg">
        {fields.map((f) => (
          <Toggle
            key={f.key}
            spec={f}
            checked={policy?.[f.key] ?? false}
            loading={!policy}
            modified={Boolean(saved && policy && saved[f.key] !== policy[f.key])}
            disabled={disabled}
            onChange={(v) => onChange(f.key, v)}
          />
        ))}
      </Stack>
    </Card>
  )
}

function Toggle({
  spec, checked, loading, modified, disabled, onChange,
}: {
  spec: FieldSpec
  checked: boolean
  loading: boolean
  modified: boolean
  disabled: boolean
  onChange: (v: boolean) => void
}) {
  // Risk is a property of the position, not of the field: leaving SFTP
  // proxying off is as much a loosening as turning agent forwarding on.
  const risky = spec.riskOn === checked
  return (
    <Group justify="space-between" wrap="nowrap" align="flex-start" gap="lg">
      <Box>
        <Group gap={6}>
          <Text size="xs" fw={500}>{spec.label}</Text>
          {risky && <Badge size="xs" color="rose">raises risk</Badge>}
          {modified && <Badge size="xs" color="amber" variant="outline">unsaved</Badge>}
        </Group>
        <Text size={FS.micro} c="dimmed" mt={2} lh={1.45}>{spec.description}</Text>
      </Box>
      {loading ? (
        <Skeleton height={20} width={36} radius="xl" style={{ flexShrink: 0 }} />
      ) : (
        <Switch
          size="sm"
          color={spec.riskOn ? 'rose' : 'teal'}
          checked={checked}
          disabled={disabled}
          onChange={(e) => onChange(e.currentTarget.checked)}
          aria-label={spec.label}
          style={{ flexShrink: 0 }}
        />
      )}
    </Group>
  )
}
