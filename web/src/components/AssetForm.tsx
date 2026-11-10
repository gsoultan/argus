import { useEffect, useState } from 'react'
import {
  Box, Button, Group, Modal, NumberInput, SegmentedControl, Stack, Switch, TagsInput, Text, TextInput,
} from '@mantine/core'
import { useCreateAsset, useUpdateAsset } from '~/lib/queries'
import { run } from '~/lib/notify'
import { SP } from '~/theme'
import { ASSET_TYPE_BLURB } from '~/lib/assetType'
import { DEVICE_PRINCIPALS, type Asset, type AssetInput, type AssetKind, type AssetProtocol, type ControlMode, type DesktopMode } from '~/types/domain'

/**
 * Adding a host to the inventory, and editing one.
 *
 * Every control here is controlled: a field that appears to hold a value and
 * does not is the specific failure this product cannot have, and the Settings
 * page shipped it once already.
 *
 * What is deliberately *not* on this form is anything observed rather than
 * decided — host-key state, agent liveness, bypass posture. An administrator
 * declaring a host verified would be the opposite of verifying it.
 */

const empty: AssetInput = {
  hostname: '',
  address: '',
  port: 22,
  protocol: 'ssh',
  os: '',
  tags: [],
  principals: [],
  credentialMode: 'injected-key',
  credentialRef: '',
  domain: '',
  desktopMode: 'rdp',
}

/**
 * What the hostname field says. Renaming a server in Argus does not rename
 * the machine: its agent keeps the name it was enrolled under, and the
 * console reaches it by that name, so the rename is safe to make and the
 * form says so instead of leaving the administrator to wonder.
 */
export function hostnameNote(asset: Asset | undefined, hostname: string): string {
  const base = 'The name Argus knows this host by, and what a certificate would name.'
  if (!asset) return base
  const next = hostname.trim().toLowerCase()
  const agent = asset.agentHostname || asset.hostname
  if (!next || next === asset.hostname) {
    return asset.agentHostname
      ? `${base} Its agent goes by ${asset.agentHostname}.`
      : base
  }
  if (asset.agentState === 'absent' && !asset.tunnelConnected && !asset.agentHostname) {
    return `Renaming it keeps who can connect and its approvals. ${asset.hostname} stays on its past sessions.`
  }
  return next === agent.toLowerCase()
    ? `Renaming it back to its agent's own name. Who can connect, approvals and sessions carry over.`
    : `Renaming it keeps its agent, who can connect, approvals and databases. The agent still goes by ${agent}; ${asset.hostname} stays on its past sessions.`
}

function fromAsset(a: Asset): AssetInput {
  return {
    hostname: a.hostname,
    address: a.address,
    port: a.port,
    protocol: a.protocol ?? 'ssh',
    os: a.os,
    tags: a.tags,
    principals: a.principals,
    credentialMode: a.credentialMode,
    credentialRef: a.credentialRef ?? '',
    domain: a.domain ?? '',
    desktopMode: a.desktopMode ?? 'rdp',
    kind: a.kind ?? 'server',
    controlMode: a.controlMode,
    controlNeedsApproval: a.controlNeedsApproval,
    connectAsk: a.connectAsk ?? null,
    rotationIntervalDays: a.rotationIntervalDays,
  }
}

export function AssetForm({
  opened,
  onClose,
  asset,
}: {
  opened: boolean
  onClose: () => void
  /** Editing when present, adding when not. */
  asset?: Asset
}) {
  const [form, setForm] = useState<AssetInput>(empty)
  const create = useCreateAsset()
  const update = useUpdateAsset()

  // Reset when the dialog opens, so an edit never shows the previous host's
  // values for the instant before the state catches up.
  useEffect(() => {
    if (opened) setForm(asset ? fromAsset(asset) : empty)
  }, [opened, asset])

  const set = <K extends keyof AssetInput>(key: K, value: AssetInput[K]) =>
    setForm((f) => ({ ...f, [key]: value }))

  const setProtocol = (p: AssetProtocol) =>
    setForm((f) => ({
      ...f,
      protocol: p,
      // The defaults follow the protocol rather than persisting from the last
      // choice: 22 on an RDP host fails with a protocol error that says nothing
      // about the real mistake.
      port: f.port === 22 || f.port === 3389 || !f.port ? (p === 'rdp' ? 3389 : 22) : f.port,
      credentialMode: p === 'rdp' ? 'injected-password' : 'injected-key',
      domain: p === 'rdp' ? f.domain : '',
      desktopMode: 'rdp',
    }))

  const isRDP = form.protocol === 'rdp'
  // A device is added through the wizard, by the app enrolling; this form
  // only edits one, and what it edits is the name, how it is controlled and
  // the tags. The control plane fixes the rest.
  const device = form.kind === 'android'
  const pending = create.isPending || update.isPending
  const submit = async () => {
    // A device's principals are fixed; a row from before it was one, or one
    // emptied by hand, gets both back rather than becoming unopenable.
    const input: AssetInput = device && form.principals.length === 0
      ? { ...form, principals: [...DEVICE_PRINCIPALS] }
      : form
    const ok = await run(
      () =>
        asset
          ? update.mutateAsync({ id: asset.id, input })
          : create.mutateAsync(input),
      {
        failure: asset ? 'Not saved' : 'Not added',
        success: asset
          ? ['Saved', `${form.hostname} is updated.`]
          : [
              'Server added',
              `${form.hostname} is added. Pick who can connect on its page.`,
            ],
      },
    )
    if (ok) onClose()
  }

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={asset ? `Edit ${asset.hostname}` : 'Add a server by hand'}
      size="lg"
    >
      {device ? (
        <Stack gap="sm">
          <TextInput
            required
            label="Name"
            description="The name Argus knows this device by."
            value={form.hostname}
            onChange={(e) => set('hostname', e.currentTarget.value)}
          />
          <Box>
            <Text size="sm" fw={500}>Controlled</Text>
            <Text size="xs" c="dimmed" mb={6}>
              {form.controlMode === 'unattended'
                ? 'The app opens the device with nobody there, through its helper. For company devices on a managed network.'
                : 'Whoever holds the device accepts each session on it, and sees that it is live.'}
            </Text>
            <SegmentedControl
              fullWidth
              value={form.controlMode ?? 'attended'}
              onChange={(v) => set('controlMode', v as ControlMode)}
              data={[
                { value: 'attended', label: 'Attended' },
                { value: 'unattended', label: 'Unattended' },
              ]}
            />
          </Box>
          <Switch
            checked={form.controlNeedsApproval === true}
            onChange={(e) => set('controlNeedsApproval', e.currentTarget.checked)}
            label="Controlling it needs an approval first"
            description="Like root on a server: someone else approves it, for a set time. Watching never needs one."
          />
          <TagsInput
            label="Tags"
            placeholder="field, tablets"
            value={form.tags ?? []}
            onChange={(v) => set('tags', v)}
          />
          <Group justify="flex-end" gap={SP.cozy} mt="xs">
            <Button variant="default" onClick={onClose} disabled={pending}>
              Cancel
            </Button>
            <Button
              onClick={() => void submit()}
              loading={pending}
              disabled={!form.hostname.trim()}
            >
              Save
            </Button>
          </Group>
        </Stack>
      ) : (
      <Stack gap="sm">
        <Box>
          <Text size="sm" fw={500}>Type</Text>
          <Text size="xs" c="dimmed" mb={6}>
            {ASSET_TYPE_BLURB[form.kind === 'workstation' ? 'workstation' : 'server']}
          </Text>
          <SegmentedControl
            fullWidth
            aria-label="Type"
            value={form.kind === 'workstation' ? 'workstation' : 'server'}
            onChange={(v) => set('kind', v as AssetKind)}
            data={[
              { value: 'server', label: 'Server' },
              { value: 'workstation', label: 'Workstation (laptop or PC)' },
            ]}
          />
        </Box>
        <Box>
          <Text size="sm" fw={500}>Before connecting</Text>
          <Text size="xs" c="dimmed" mb={6}>
            {form.connectAsk === 'never'
              ? 'Opens without asking or telling anyone, like a server. For lab or kiosk machines nobody uses as their own.'
              : form.connectAsk === 'ask'
              ? 'Whoever is using it is asked first, on their screen. A terminal only tells them.'
              : form.connectAsk === 'tell'
              ? 'Opens at once; whoever is using it gets a notice naming who connected.'
              : `As set for every ${form.kind === 'workstation' ? 'workstation' : 'server'} in Settings, Asset types.`}
          </Text>
          <SegmentedControl
            fullWidth
            aria-label="Before connecting"
            value={form.connectAsk ?? 'type'}
            onChange={(v) => set('connectAsk', v === 'type' ? null : (v as 'never' | 'ask' | 'tell'))}
            data={[
              { value: 'type', label: `Like other ${form.kind === 'workstation' ? 'workstations' : 'servers'}` },
              { value: 'never', label: 'Never ask' },
              { value: 'ask', label: 'Ask first' },
              { value: 'tell', label: 'Tell them' },
            ]}
          />
        </Box>
        <SegmentedControl
          fullWidth
          aria-label="Connects by"
          value={form.protocol ?? 'ssh'}
          onChange={(v) => setProtocol(v as AssetProtocol)}
          data={[
            { value: 'ssh', label: 'SSH' },
            { value: 'rdp', label: 'Remote Desktop' },
          ]}
        />

        <Group grow align="flex-start">
          <TextInput
            required
            label="Hostname"
            description={hostnameNote(asset, form.hostname)}
            placeholder="pay-01.payments.example"
            value={form.hostname}
            onChange={(e) => set('hostname', e.currentTarget.value)}
          />
          <TextInput
            label="Address"
            description="Its IP or DNS name on your network. Defaults to the hostname. Sessions go through its agent, so changing it does not cut them off."
            placeholder="192.0.2.10"
            value={form.address}
            onChange={(e) => set('address', e.currentTarget.value)}
          />
        </Group>

        <Group grow align="flex-start">
          <NumberInput
            label="Port"
            min={1}
            max={65535}
            value={form.port ?? (isRDP ? 3389 : 22)}
            onChange={(v) => set('port', typeof v === 'number' ? v : Number(v) || undefined)}
          />
          <TextInput
            label="Operating system"
            placeholder="Ubuntu 24.04"
            value={form.os ?? ''}
            onChange={(e) => set('os', e.currentTarget.value)}
          />
        </Group>

        <TagsInput
          label="Login accounts"
          description="The accounts on the server people can log in as. Who can use which is set under Who can connect."
          placeholder="ubuntu, deploy"
          value={form.principals}
          onChange={(v) => set('principals', v)}
        />

        {isRDP && (
          <Box>
            <Text size="sm" fw={500}>Opens with</Text>
            <Text size="xs" c="dimmed" mb={6}>
              {form.desktopMode === 'screen'
                ? 'Shows the server\u2019s screen as it is, without Remote Desktop: whoever is signed in there, or the sign-in screen. Anyone in front of the server sees what is done. For servers with Remote Desktop turned off.'
                : 'Signs in through the server\u2019s own Remote Desktop, as the account picked, with its Windows password.'}
            </Text>
            <SegmentedControl
              fullWidth
              value={form.desktopMode ?? 'rdp'}
              onChange={(v) => set('desktopMode', v as DesktopMode)}
              data={[
                { value: 'rdp', label: 'Remote Desktop' },
                { value: 'screen', label: 'Screen sharing (no RDP)' },
              ]}
            />
          </Box>
        )}

        {isRDP && form.desktopMode !== 'screen' && (
          <TextInput
            label="Windows domain"
            description="Empty means the account is local to the server."
            placeholder="corp.example"
            value={form.domain ?? ''}
            onChange={(e) => set('domain', e.currentTarget.value)}
          />
        )}

        <TagsInput
          label="Tags"
          placeholder="pci, payments"
          value={form.tags ?? []}
          onChange={(v) => set('tags', v)}
        />

        {/* No credential fields. Sessions open on the server through its own
            agent, which needs no vaulted key or password; the old Credential
            and vault-name fields described the retired gateway and made the
            first edit of an approved server fail. A value an older install
            still holds is carried through untouched. */}

        <Group justify="flex-end" gap={SP.cozy} mt="xs">
          <Button variant="default" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button
            onClick={() => void submit()}
            loading={pending}
            disabled={!form.hostname.trim() || form.principals.length === 0}
          >
            {asset ? 'Save' : 'Add server'}
          </Button>
        </Group>
      </Stack>
      )}
    </Modal>
  )
}
