import { Alert, Badge, Box, Button, Card, Group, Modal, Stack, Text } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useNavigate } from '@tanstack/react-router'
import { IconAlertTriangle, IconPencil, IconShieldCheck, IconTrash } from '@tabler/icons-react'
import { AgentUpdateCard } from '~/components/AgentUpdate'
import { Digest, HostKeyBadge, Mono, absTime, relTime } from '~/components/primitives'
import { usePin } from '~/components/server/hostKey'
import { SettingsSection, SettingsSections } from '~/components/server/layout'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import { useArchiveAsset } from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * The controls a server needs rarely: its details, its host key, its agent,
 * and taking it out of Argus.
 */
export function SettingsTab({ asset, canEdit, onEdit }: {
  asset: Asset
  canEdit: boolean
  onEdit: () => void
}) {
  return (
    <SettingsSections>
      <SettingsSection title="Details" description="Name, address, protocol and tags.">
        <Card padding="md">
          <Stack gap="sm">
            <Detail label="Name"><Text size={FS.meta} style={{ overflowWrap: 'anywhere' }}>{asset.hostname}</Text></Detail>
            <Detail label="Address"><Mono>{asset.address}{asset.port ? `:${asset.port}` : ''}</Mono></Detail>
            <Detail label="Protocol"><Text size={FS.meta}>{asset.protocol === 'rdp' ? 'RDP' : 'SSH'}</Text></Detail>
            <Detail label="Tags">
              {asset.tags.length === 0 ? <Text size={FS.meta} c="dimmed">None</Text> : (
                <Group gap={SP.tight}>
                  {asset.tags.map((t) => <Badge key={t} size="xs" variant="outline" color="slate">{t}</Badge>)}
                </Group>
              )}
            </Detail>
            <Detail label="Last check">
              <Text size={FS.meta} c="dimmed" title={absTime(asset.lastCheckedAt)}>{relTime(asset.lastCheckedAt)}</Text>
            </Detail>
            {canEdit && (
              <Box>
                <Button variant="default" size="xs" leftSection={<IconPencil size={14} />} onClick={onEdit}>
                  Edit details
                </Button>
              </Box>
            )}
          </Stack>
        </Card>
      </SettingsSection>

      <SettingsSection
        title="Host key"
        description="Argus checks the server's identity against this key on every connection."
      >
        <HostKey asset={asset} />
      </SettingsSection>

      {isConfigured() && (
        <SettingsSection title="Agent" description="The Argus agent on this server, which opens and records every session here.">
          <AgentUpdateCard asset={asset} canEdit={canEdit} bare />
        </SettingsSection>
      )}

      {canEdit && (
        <SettingsSection title="Remove server" description="Its recordings and activity are kept." danger>
          <RemoveServer asset={asset} />
        </SettingsSection>
      )}
    </SettingsSections>
  )
}

function Detail({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <Text size={FS.meta} c="dimmed" w={90} style={{ flexShrink: 0 }}>{label}</Text>
      <Box style={{ minWidth: 0 }}>{children}</Box>
    </Group>
  )
}

/**
 * The pinned key, or why there is none. A changed key is explained in the
 * alert above the tabs, which is shown on every tab.
 */
function HostKey({ asset }: { asset: Asset }) {
  const { onPin, pending } = usePin(asset)
  return (
    <Card padding="md">
      <Stack gap="sm">
        <Group gap={SP.cozy}>
          <HostKeyBadge state={asset.hostKeyState} />
          {asset.hostKeyPinnedAt && (
            <Text size={FS.micro} c="dimmed" title={absTime(asset.hostKeyPinnedAt)}>
              pinned {relTime(asset.hostKeyPinnedAt)}
            </Text>
          )}
        </Group>
        {asset.hostKeyFingerprint
          ? <Digest value={asset.hostKeyFingerprint} chars={40} />
          : <Text size={FS.meta} c="dimmed">No fingerprint pinned.</Text>}
        {asset.hostKeyState === 'unpinned' && (
          <>
            <Text size={FS.body} c="dimmed" lh={1.5}>
              Argus terminates SSH for this host, which means it is responsible for verifying the
              target&rsquo;s identity. Until a key is pinned, nothing has been verified: the first
              connection is trust-on-first-use.
            </Text>
            <Box>
              <Button size="xs" color="amber" leftSection={<IconShieldCheck size={14} />} loading={pending} onClick={() => void onPin()}>
                Pin currently presented key
              </Button>
            </Box>
          </>
        )}
      </Stack>
    </Card>
  )
}

/**
 * Takes a server out of Argus. Its recordings and its activity stay; nobody
 * can connect to it from here any more, and its agent uninstalls itself so
 * plain SSH works again. Left running, the agent kept refusing direct logins
 * to a server the console could no longer open.
 */
export function RemoveServer({ asset }: { asset: Asset }) {
  const navigate = useNavigate()
  const archive = useArchiveAsset()
  const [open, modal] = useDisclosure(false)
  const connected = asset.tunnelConnected === true
  return (
    <Card padding="md" style={{ borderColor: 'var(--mantine-color-rose-3)' }}>
      <Group justify="space-between" wrap="wrap" gap="md">
        <Text size={FS.body} lh={1.5} style={{ flex: '1 1 240px' }}>
          Takes it out of Argus and uninstalls the agent from it. Adding it back means installing
          the agent again.
        </Text>
        <Button variant="light" color="rose" leftSection={<IconTrash size={16} />} onClick={modal.open}>
          Remove
        </Button>
      </Group>
      <Modal opened={open} onClose={modal.close} title={`Remove ${asset.hostname} from Argus?`}>
        <Stack gap="sm" mb="md">
          <Text size="sm">
            Everyone loses access to it from Argus, right away. Its recordings and activity are
            kept.
          </Text>
          <Text size="sm">
            {connected
              ? 'The agent then uninstalls itself from the server: it takes itself out of SSH, so people can log in to the server directly again, and switches itself off.'
              : 'The agent is not connected right now, so it uninstalls itself when it next connects. To do it now, run "sudo argus-agent uninstall" on the server.'}
          </Text>
          <Alert color="amber" icon={<IconAlertTriangle size={16} />} title="Adding it back means installing the agent again">
            <Text size={FS.meta} lh={1.5}>
              Once removed, this server cannot simply be switched back on. To add it to Argus
              again, install and enroll the agent on it again from Servers, Add a server.
            </Text>
          </Alert>
        </Stack>
        <Group justify="flex-end">
          <Button variant="default" onClick={modal.close}>Keep it</Button>
          <Button
            color="rose"
            loading={archive.isPending}
            onClick={() =>
              void run(() => archive.mutateAsync(asset.id), {
                failure: 'Server not removed',
                success: [
                  'Server removed',
                  connected
                    ? `${asset.hostname} is out of Argus, and its agent is uninstalling itself.`
                    : `${asset.hostname} is out of Argus. Its agent uninstalls itself when it next connects.`,
                ],
              }).then((ok) => { if (ok) void navigate({ to: '/assets' }) })
            }
          >
            Remove and uninstall agent
          </Button>
        </Group>
      </Modal>
    </Card>
  )
}
