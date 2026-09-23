import { useEffect, useState } from 'react'
import {
  Alert, Button, Group, Modal, NumberInput, SegmentedControl, Select, Stack,
  TagsInput, Text, TextInput,
} from '@mantine/core'
import { IconInfoCircle } from '@tabler/icons-react'
import { useCreateAsset, useUpdateAsset } from '~/lib/queries'
import { run } from '~/lib/notify'
import { FS, SP } from '~/theme'
import type { Asset, AssetInput, AssetProtocol, CredentialMode } from '~/types/domain'

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

const SSH_MODES: { value: CredentialMode; label: string }[] = [
  { value: 'injected-key', label: 'Injected key — the gateway holds it' },
  { value: 'ca-certificate', label: 'Certificate — no standing credential' },
]
const RDP_MODES: { value: CredentialMode; label: string }[] = [
  { value: 'injected-password', label: 'Injected password — the gateway holds it' },
]

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
    }))

  const isRDP = form.protocol === 'rdp'
  const needsCredential = form.credentialMode !== 'ca-certificate'
  const pending = create.isPending || update.isPending
  const submit = async () => {
    const ok = await run(
      () =>
        asset
          ? update.mutateAsync({ id: asset.id, input: form })
          : create.mutateAsync(form),
      {
        failure: asset ? 'Not saved' : 'Not added',
        success: asset
          ? ['Asset saved', `${form.hostname} is updated. Gateways pick it up within a minute.`]
          : [
              'Asset added',
              `${form.hostname} is in the inventory. Nobody is assigned to it yet — assign it before anyone can connect.`,
            ],
      },
    )
    if (ok) onClose()
  }

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={asset ? `Edit ${asset.hostname}` : 'Add an asset'}
      size="lg"
    >
      <Stack gap="sm">
        <SegmentedControl
          fullWidth
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
            description="The name Argus knows this host by, and what a certificate would name."
            placeholder="pay-01.payments.example"
            value={form.hostname}
            onChange={(e) => set('hostname', e.currentTarget.value)}
          />
          <TextInput
            label="Address"
            description="Where to dial it. Defaults to the hostname."
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
          label="Principals"
          description="Accounts Argus may broker a session as. This list is the authority, not the target's own /etc/passwd — and assigning someone can never exceed it."
          placeholder="ops, deploy"
          value={form.principals}
          onChange={(v) => set('principals', v)}
        />

        <Select
          label="Credential"
          description="How Argus authenticates to the target."
          allowDeselect={false}
          value={form.credentialMode ?? 'injected-key'}
          onChange={(v) => set('credentialMode', (v as CredentialMode) ?? 'injected-key')}
          data={isRDP ? RDP_MODES : SSH_MODES}
        />

        {needsCredential && (
          <TextInput
            required
            label={isRDP ? 'Vault directory name' : 'Vault entry name'}
            description={
              isRDP
                ? 'A directory in the gateway’s vault holding one file per principal, each containing that account’s password.'
                : 'A file in the gateway’s vault holding the private key to inject.'
            }
            placeholder="pay-01"
            value={form.credentialRef ?? ''}
            onChange={(e) => set('credentialRef', e.currentTarget.value)}
          />
        )}

        {isRDP && (
          <TextInput
            label="Windows domain"
            description="Empty means the account is local to the host — which is not the same as typing the host's own name."
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

        {/* Says where the secret is, because this is the step people get wrong:
            the console never holds credential material, so naming one here does
            not put it anywhere. */}
        <Alert
          variant="light"
          color="azure"
          icon={<IconInfoCircle size={16} />}
          title="The credential stays on the gateway"
        >
          <Text size={FS.meta}>
            Argus stores the name, never the secret. Put the key or password in the
            gateway&rsquo;s <code>vault_dir</code> under exactly this name, readable only by
            the service account. Nothing you type here can point outside that directory.
          </Text>
        </Alert>

        <Group justify="flex-end" gap={SP.cozy} mt="xs">
          <Button variant="default" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button
            onClick={() => void submit()}
            loading={pending}
            disabled={!form.hostname.trim() || form.principals.length === 0}
          >
            {asset ? 'Save' : 'Add asset'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
