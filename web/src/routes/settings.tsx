import { useMemo, useState, type ComponentType } from 'react'
import {
  Alert, Badge, Box, Button, Card, Code, Group, List, Grid, Modal, NavLink, Select,
  Skeleton, Stack, Switch, Text, ThemeIcon,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconDeviceDesktop, IconDeviceFloppy, IconInfoCircle, IconLock, IconNetwork,
  IconShieldLock, IconTerminal2, type IconProps,
} from '@tabler/icons-react'
import { PageBody, PageHeader, SectionCard } from '~/components/page'
import { Mono } from '~/components/primitives'
import { meQuery, policyQuery, useSavePolicy } from '~/lib/queries'
import { notifyError, notifyOk } from '~/lib/notify'
import { isConfigured } from '~/lib/live'
import { FS, SP } from '~/theme'
import type { GatewayPolicy, PolicyKey } from '~/types/domain'
import { guard } from '~/lib/guard'
import {
  CONTROL_SECTIONS, ControlSection, useControlSettings, type ControlSectionId,
  type ControlSettingsState,
} from '~/components/ControlSettings'

export const Route = createFileRoute('/settings')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  component: Settings,
  // Which section is open, so a reload or a shared link lands on it.
  validateSearch: (s: Record<string, unknown>): { section?: string } => ({
    ...(typeof s.section === 'string' && s.section ? { section: s.section } : {}),
  }),
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

const ACCESS_POLICY: FieldSpec[] = [
  {
    key: 'allowNativeClients',
    label: 'Allow native clients (ssh, scp, sftp, mstsc)',
    description:
      'Off, every session and every file transfer opens here in the console, through the server\'s agent: the gateway refuses ssh, scp, sftp and mstsc, and an agent refuses a login that reached sshd without Argus, reporting the attempt. On lets them in again.',
    riskOn: true,
  },
]

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

const ALL_FIELDS = [...ACCESS_POLICY, ...SSH_POLICY, ...RECORDING_POLICY]

/** Roles the control plane lets change gateway policy. */
const CAN_EDIT_POLICY = new Set(['owner', 'admin'])

type PolicySectionId = 'access' | 'channels' | 'recording' | 'hosts'
type SectionId = PolicySectionId | ControlSectionId

interface NavItem {
  id: SectionId
  label: string
  icon: ComponentType<IconProps>
  changed: boolean
}

const POLICY_SECTIONS: { id: PolicySectionId; label: string; icon: ComponentType<IconProps> }[] = [
  { id: 'access', label: 'How people connect', icon: IconNetwork },
  { id: 'channels', label: 'SSH channels', icon: IconTerminal2 },
  { id: 'recording', label: 'Recording', icon: IconLock },
  { id: 'hosts', label: 'Hosts and trust', icon: IconShieldLock },
]

const POLICY_FIELDS: Record<PolicySectionId, FieldSpec[]> = {
  access: ACCESS_POLICY,
  channels: SSH_POLICY,
  recording: RECORDING_POLICY,
  hosts: [],
}

function Settings() {
  const { data: saved } = useQuery(policyQuery())
  const { data: me } = useQuery(meQuery())
  const savePolicy = useSavePolicy()
  const [confirmOpen, confirm] = useDisclosure(false)
  const { section } = Route.useSearch()
  const navigate = useNavigate()

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
  // The control plane's own configuration: what used to be edited in
  // control.yaml and control.env by hand. Admins and owners only, and only
  // against a real control plane -- there is no fixture of a configuration
  // file worth pretending to edit.
  const showControl = canEdit && isConfigured()
  const ctl = useControlSettings(showControl)

  const changes = useMemo(() => {
    if (!saved || !draft) return []
    return ALL_FIELDS.filter((f) => draft[f.key] !== saved[f.key])
  }, [saved, draft])

  const loosening = changes.filter((f) => f.riskOn === Boolean(draft?.[f.key]))

  const policyItems: NavItem[] = POLICY_SECTIONS.map((p) => ({
    ...p,
    changed: changes.some((f) => POLICY_FIELDS[p.id].includes(f)),
  }))
  const controlItems: NavItem[] = showControl
    ? CONTROL_SECTIONS.map((c) => ({ id: c.id, label: c.label, icon: c.icon, changed: ctl.changedSections.includes(c) }))
    : []
  const items = [...policyItems, ...controlItems]
  const active: SectionId = items.find((i) => i.id === section)?.id ?? 'access'
  const open = (id: string | null) => {
    if (id) void navigate({ to: '/settings', search: { section: id }, replace: true })
  }

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

  const policyDisabled = !canEdit || savePolicy.isPending

  return (
    <Box>
      <PageHeader
        title="Settings"
        description="Session policy, and everything the control plane is configured with. Defaults are the safe choice."
        actions={
          changes.length > 0 ? (
            <>
              <Button variant="subtle" color="slate" onClick={discard}>
                Discard
              </Button>
              <Button
                color={loosening.length > 0 ? 'amber' : 'azure'}
                loading={savePolicy.isPending}
                onClick={confirm.open}
              >
                Review {changes.length} change{changes.length === 1 ? '' : 's'}
              </Button>
            </>
          ) : null
        }
      />

      <PageBody>
        {!canEdit && me && (
          <Alert color="slate" icon={<IconInfoCircle size={16} />} title="Read-only" p="xs">
            <Text size={FS.body} lh={1.5}>
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
            icon={<IconAlertTriangle size={16} />}
            title="Not connected to a control plane"
            p="xs"
          >
            <Text size={FS.body} lh={1.5}>
              Changes are held in this browser session only and reach no gateway. Point{' '}
              <Mono>VITE_CONTROL_URL</Mono> at a running control plane to make this page
              authoritative.
            </Text>
          </Alert>
        )}

        {/* One section at a time, picked from the list, so no section is
            further down a long page than the screen. */}
        <Group align="flex-start" gap="lg" wrap="nowrap">
          <Box component="nav" aria-label="Settings sections" w={210} visibleFrom="md" style={{ flexShrink: 0 }}>
            <SectionNav title="Session policy" items={policyItems} active={active} onOpen={open} />
            {controlItems.length > 0 && (
              <SectionNav title="Control plane" items={controlItems} active={active} onOpen={open} />
            )}
          </Box>

          <Stack gap="sm" style={{ flex: 1, minWidth: 0 }} maw={960}>
            <Select
              hiddenFrom="md"
              aria-label="Settings section"
              allowDeselect={false}
              value={active}
              onChange={open}
              data={[
                { group: 'Session policy', items: policyItems.map(navOption) },
                ...(controlItems.length > 0
                  ? [{ group: 'Control plane', items: controlItems.map(navOption) }]
                  : []),
              ]}
            />

            {active === 'access' && (
              <>
                <PolicyCard
                  icon={IconShieldLock}
                  title="How people connect"
                  note={
                    <>
                      Console-only is the default. A server&apos;s agent applies a change at its
                      next check-in, about a minute; each server&apos;s page says whether its agent
                      has confirmed it. A server with no agent cannot refuse a direct login, so
                      close sshd to everything but the agent there, or install one.
                    </>
                  }
                  fields={ACCESS_POLICY}
                  policy={current}
                  saved={saved ?? null}
                  disabled={!canEdit || savePolicy.isPending}
                  onChange={set}
                />
                {current?.allowNativeClients ? (
                  <SectionCard
                    title="Gateway endpoint"
                    icon={IconNetwork}
                    iconColor="sky"
                    description="Native clients are allowed, so users can also connect with their normal client. The target is encoded in the username."
                  >
                    <Code block fz={FS.digest}>
                      {`ssh ops:db-01@argus.northwind.id
scp report.csv ops:db-01@argus.northwind.id:/tmp/
sftp ops:db-01@argus.northwind.id`}
                    </Code>
                  </SectionCard>
                ) : (
                  <SectionCard
                    title="Console only"
                    icon={IconNetwork}
                    iconColor="sky"
                    description="Sessions open from Connect: pick a server, pick an account, and the terminal, desktop or file browser opens here, through the server's agent."
                  >
                    <Text size={FS.meta} c="dimmed" lh={1.5}>
                      ssh, scp, sftp and mstsc to the gateway are refused with a message pointing
                      here, and a direct login to a server with an agent is refused and written
                      to the audit log.
                    </Text>
                  </SectionCard>
                )}
              </>
            )}

            {active === 'channels' && (
              <PolicyCard
                icon={IconTerminal2}
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
                disabled={policyDisabled}
                onChange={set}
              />
            )}

            {active === 'recording' && (
              <>
                <PolicyCard
                  icon={IconLock}
                  title="Recording & retention"
                  fields={RECORDING_POLICY}
                  policy={current}
                  saved={saved ?? null}
                  disabled={policyDisabled}
                  onChange={set}
                />
                <Alert color="amber" icon={<IconAlertTriangle size={16} />} p="xs">
                  <Text size={FS.body} lh={1.5}>
                    Command detection from PTY output is a heuristic, and <Mono>argus</Mono> labels
                    it as such everywhere it appears. Do not present it to an auditor as proof of
                    what ran — that is what the eBPF tier is for.
                  </Text>
                </Alert>
              </>
            )}

            {active === 'hosts' && <HostsAndTrust />}

            {showControl && controlItems.some((c) => c.id === active) && (
              <>
                <ControlSaveBar ctl={ctl} />
                {ctl.loading ? (
                  <Skeleton h={240} />
                ) : (
                  <ControlSection id={active as ControlSectionId} ctl={ctl} />
                )}
              </>
            )}
          </Stack>
        </Group>
      </PageBody>

      <Modal
        opened={confirmOpen}
        onClose={confirm.close}
        title="Apply gateway policy"
        size="md"
      >
        {loosening.length > 0 && (
          <Alert color="rose" icon={<IconAlertTriangle size={16} />} mb="md">
            <Text size={FS.body} lh={1.5}>
              {loosening.length === 1 ? 'One change loosens' : `${loosening.length} changes loosen`}{' '}
              the gateway. Every session opened after this uses the new policy.
            </Text>
          </Alert>
        )}
        <Text size={FS.meta} c="dimmed" mb="xs">
          The whole policy is written, and the change is recorded in the audit log against
          your account.
        </Text>
        <List size="xs" spacing={SP.snug}>
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
          <Button variant="subtle" color="slate" onClick={confirm.close}>
            Cancel
          </Button>
          <Button
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

const navOption = (i: NavItem) => ({ value: i.id, label: i.changed ? `${i.label} (unsaved)` : i.label })

function SectionNav({ title, items, active, onOpen }: {
  title: string
  items: NavItem[]
  active: SectionId
  onOpen: (id: SectionId) => void
}) {
  return (
    <Box mb="md">
      <Text size={FS.micro} c="dimmed" fw={600} tt="uppercase" px="sm" mb={SP.tight}>
        {title}
      </Text>
      {items.map((i) => (
        <NavLink
          key={i.id}
          component="button"
          type="button"
          label={i.label}
          active={i.id === active}
          variant="light"
          leftSection={<i.icon size={15} />}
          rightSection={i.changed ? <Badge size="xs" color="amber" variant="outline">unsaved</Badge> : null}
          onClick={() => onOpen(i.id)}
          aria-current={i.id === active ? 'page' : undefined}
          style={{ borderRadius: 'var(--mantine-radius-sm)' }}
        />
      ))}
    </Box>
  )
}

/** The control plane's one save, for whichever of its sections changed. */
function ControlSaveBar({ ctl }: { ctl: ControlSettingsState }) {
  if (!ctl.dirty) return null
  return (
    <Alert color="amber" variant="light" p="xs" icon={<IconDeviceFloppy size={16} />}>
      <Group justify="space-between" wrap="wrap" gap="xs">
        <Text size={FS.body}>
          Unsaved: {ctl.changedSections.map((c) => c.label).join(', ')}. One save applies them all.
        </Text>
        <Group gap="xs">
          <Button variant="subtle" color="slate" size="xs" onClick={ctl.discard} disabled={ctl.saving}>
            Discard settings
          </Button>
          <Button size="xs" leftSection={<IconDeviceFloppy size={14} />}
            loading={ctl.saving} onClick={() => void ctl.save()}>
            Save and apply
          </Button>
        </Group>
      </Group>
    </Alert>
  )
}

function HostsAndTrust() {
  return (
    <Grid gap="sm">
      <Grid.Col span={{ base: 12, lg: 6 }}>
        <SectionCard
          title="Certificate authority"
          icon={IconShieldLock}
          iconColor="teal"
          description="Add this to a host to move it off standing credentials. Argus then mints a short-lived certificate per session and there is nothing left in the vault to steal for that host."
        >
          <Code block fz={FS.micro}>
            {`# /etc/ssh/sshd_config
TrustedUserCAKeys /etc/ssh/argus_ca.pub`}
          </Code>
        </SectionCard>
      </Grid.Col>

      <Grid.Col span={{ base: 12, lg: 6 }}>
        <Alert color="sky" icon={<IconInfoCircle size={16} />} title="Threat model, stated plainly">
          <Text size={FS.body} lh={1.55}>
            Argus terminates SSH, so the gateway holds session plaintext in memory. Treat gateway
            nodes as your highest-value asset: no shared tenancy, no third-party agents, hardware
            keys for the vault, and the audit log replicated somewhere the gateway cannot write.
          </Text>
        </Alert>
      </Grid.Col>

      <Grid.Col span={{ base: 12, lg: 6 }}>
        <Card style={{ borderStyle: 'dashed' }}>
          <Group gap={SP.cozy} mb={SP.snug}>
            <ThemeIcon variant="light" color="slate" size={22} radius="sm">
              <IconDeviceDesktop size={13} />
            </ThemeIcon>
            <Text fw={600} size="sm">Windows RDP</Text>
            <Badge size="xs" color="slate" variant="outline">phase 3</Badge>
          </Group>
          <Text size={FS.micro} c="dimmed" lh={1.5}>
            RDP runs as a separate service rather than inside the SSH gateway — different
            protocol, different recording pipeline, different failure modes. Linux SSH gets
            finished first.
          </Text>
        </Card>
      </Grid.Col>
    </Grid>
  )
}

function PolicyCard({
  icon, title, note, fields, policy, saved, disabled, onChange,
}: {
  icon: typeof IconTerminal2
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
    <SectionCard title={title} icon={icon}>
      {note && (
        <Alert color="slate" icon={<IconInfoCircle size={15} />} mb="md">
          <Text size={FS.micro} lh={1.55}>{note}</Text>
        </Alert>
      )}
      <Grid gap="lg">
        {fields.map((f) => (
          <Grid.Col key={f.key} span={{ base: 12, lg: fields.length > 2 ? 6 : 12 }}>
            <Toggle
              spec={f}
              checked={policy?.[f.key] ?? false}
              loading={!policy}
              modified={Boolean(saved && policy && saved[f.key] !== policy[f.key])}
              disabled={disabled}
              onChange={(v) => onChange(f.key, v)}
            />
          </Grid.Col>
        ))}
      </Grid>
    </SectionCard>
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
        <Group gap={SP.snug}>
          <Text size="xs" fw={500}>{spec.label}</Text>
          {risky && <Badge size="xs" color="rose">raises risk</Badge>}
          {modified && <Badge size="xs" color="amber" variant="outline">unsaved</Badge>}
        </Group>
        <Text size={FS.micro} c="dimmed" mt={SP.hair} lh={1.5}>{spec.description}</Text>
      </Box>
      {loading ? (
        <Skeleton height={20} width={36} radius="xl" style={{ flexShrink: 0 }} />
      ) : (
        <Switch
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
