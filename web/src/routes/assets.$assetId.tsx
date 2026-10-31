import { Suspense, lazy, useState } from 'react'
import { Alert, Badge, Box, Button, Card, Grid, Group, Modal, Select, Stack, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconHistory, IconPencil, IconPlugConnected, IconRouteOff,
  IconServer2, IconTrash, IconUsers,
} from '@tabler/icons-react'
import { useDisclosure } from '@mantine/hooks'
import { WhoCanConnect } from '~/components/WhoCanConnect'
import { ReachBadge, reachOf } from '~/components/myservers'
import { EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { run } from '~/lib/notify'
import {
  ArtefactPending, Digest, Field, FidelityBadge,
  HostKeyBadge, Mono, SessionDuration, SessionStateBadge, absTime, relTime, rowNav,
} from '~/components/primitives'
import { FS, SP } from '~/theme'
import {
  assetQuery, meQuery, policyQuery, sessionsQuery, useArchiveAsset, usePinHostKey,
  useSetAssetDirectSsh,
} from '~/lib/queries'
import { DIRECT_SSH_CHOICES, directSshLabel, directSshLoosens } from '~/lib/directSsh'
import type { Asset, DirectSsh } from '~/types/domain'
import { guard } from '~/lib/guard'
import { isElevatedAccount } from '~/lib/roles'

/** Opened only by an administrator, so their chunks are fetched only for one. */
const AssetForm = lazy(() =>
  import('~/components/AssetForm').then((m) => ({ default: m.AssetForm })),
)

export const Route = createFileRoute('/assets/$assetId')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  component: AssetDetail,
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(assetQuery(params.assetId)),
})

function AssetDetail() {
  const navigate = useNavigate()
  const { assetId } = Route.useParams()
  const { data: asset } = useQuery(assetQuery(assetId))
  const { data: allSessions } = useQuery(sessionsQuery())
  const { data: me } = useQuery(meQuery())
  const pin = usePinHostKey()

  const canEdit = me?.role === 'admin' || me?.role === 'owner'
  const [editing, setEditing] = useState(false)

  if (!asset) {
    return (
      <Box>
        <PageHeader crumbs={[{ label: 'Servers', to: '/assets' }]} title="Asset not found" />
        <PageBody>
          <Card>
            <EmptyState
              icon={IconServer2}
              title="No such asset."
              description="It may have been removed from the inventory, or the link is stale."
            />
          </Card>
        </PageBody>
      </Box>
    )
  }

  const history = (allSessions ?? []).filter((s) => s.assetId === asset.id).slice(0, 12)

  // Guarded. Pinning is the control an admin reaches for when they suspect
  // interception, so a refusal that looked like success would be the worst
  // possible moment to leave someone guessing.
  const onPin = () =>
    run(() => pin.mutateAsync(asset.id), {
      failure: 'Host key not pinned',
      success: [
        'Host key pinned',
        'Argus will now refuse any connection where the presented key differs.',
      ],
    })


  return (
    <Box>
      <PageHeader
        crumbs={[
          { label: 'Servers', to: '/assets' },
          { label: asset.hostname.split('.')[0] ?? asset.hostname },
        ]}
        title={asset.hostname}
        status={<ReachBadge reach={reachOf(asset)} />}
        description={`${asset.os || (asset.protocol === 'rdp' ? 'Windows' : 'Linux')} · ${asset.address}`}
        actions={
          <Group gap={SP.cozy}>
            {canEdit && (
              <Button
                variant="default"
                leftSection={<IconPencil size={16} />}
                onClick={() => setEditing(true)}
              >
                Edit
              </Button>
            )}
            <ButtonLink
              size="md"
              to="/connect"
              search={{ host: asset.hostname }}
              leftSection={<IconPlugConnected size={18} />}
              disabled={asset.principals.length === 0}
            >
              Connect
            </ButtonLink>
          </Group>
        }
      />

      <PageBody>
        {asset.hostKeyState === 'changed' && (
          <Alert
            color="rose"
            icon={<IconAlertTriangle size={17} />}
            title="Host key does not match the pin"
          >
            <Text size={FS.body} mb="sm" lh={1.5}>
              Argus is refusing connections to this host. Either it was rebuilt or reprovisioned,
              or something is intercepting the connection. Verify the new fingerprint out of band
              — console access, your provisioning system, or a colleague physically at the host —
              before re-pinning. Do not re-pin just to clear the alert.
            </Text>
            <Button size="compact-xs" color="rose" loading={pin.isPending} onClick={onPin}>
              I verified out of band — re-pin
            </Button>
          </Alert>
        )}

        {asset.hostKeyState === 'unpinned' && (
          <Alert
            color="amber"
            icon={<IconAlertTriangle size={17} />}
            title="No host key pinned"
          >
            <Text size={FS.body} mb="sm" lh={1.5}>
              Argus terminates SSH for this host, which means it is responsible for verifying the
              target's identity. Until a key is pinned, nothing has been verified — the first
              connection is trust-on-first-use.
            </Text>
            <Button size="compact-xs" color="amber" loading={pin.isPending} onClick={onPin}>
              Pin currently presented key
            </Button>
          </Alert>
        )}

        <Grid gap="sm">
          <Grid.Col span={{ base: 12, md: 5 }}>
            <Stack gap="sm">
              <SectionCard title="Who can connect" icon={IconUsers}>
                <Stack gap="sm">
                  <WhoCanConnect asset={asset} canEdit={canEdit} />
                  <Field label="Login accounts on this server">
                    <Group gap={SP.snug}>
                      {asset.principals.length === 0 && (
                        <Text size="xs" c="dimmed">
                          None yet.{canEdit && ' Press Edit and add the account you log in with.'}
                        </Text>
                      )}
                      {asset.principals.map((p) => (
                        <Badge
                          key={p}
                          size="sm"
                          color={isElevatedAccount(p) ? 'rose' : 'slate'}
                          variant={isElevatedAccount(p) ? 'light' : 'outline'}
                          tt="none"
                          ff="monospace"
                        >
                          {p}
                        </Badge>
                      ))}
                    </Group>
                  </Field>
                </Stack>
              </SectionCard>

              <DirectSshCard asset={asset} canEdit={canEdit} />

              <SectionCard
                title="Server details"
                icon={IconServer2}
                badge={
                  <Badge size="xs" variant="light" color="slate" tt="capitalize">
                    {asset.health}
                  </Badge>
                }
              >
                <Stack gap="sm">
                  <Field label="Host key">
                    <Group gap={SP.cozy}>
                      <HostKeyBadge state={asset.hostKeyState} />
                      {asset.hostKeyPinnedAt && (
                        <Text size={FS.micro} c="dimmed">
                          pinned {relTime(asset.hostKeyPinnedAt)}
                        </Text>
                      )}
                    </Group>
                  </Field>
                  <Field label="Fingerprint">
                    {asset.hostKeyFingerprint
                      ? <Digest value={asset.hostKeyFingerprint} chars={40} />
                      : <Text size="xs" c="amber.4">not pinned</Text>}
                  </Field>
                  <Field label="Last reachability check">
                    <Text size="xs" c="dimmed">{relTime(asset.lastCheckedAt)}</Text>
                  </Field>
                  <Field label="Tags">
                    <Group gap={SP.tight}>
                      {asset.tags.map((t) => (
                        <Badge key={t} size="xs" variant="outline" color="slate">{t}</Badge>
                      ))}
                    </Group>
                  </Field>
                </Stack>
              </SectionCard>

              {canEdit && <RemoveServer asset={asset} />}
            </Stack>
          </Grid.Col>

          <Grid.Col span={{ base: 12, md: 7 }}>
            <SectionCard
              title="Recent sessions"
              icon={IconHistory}
              flush
              badge={<Badge size="xs" variant="light" color="slate">{history.length}</Badge>}
              // This card shows the latest twelve; the Sessions page has the
              // rest, already narrowed to this server.
              action={
                <ButtonLink
                  size="compact-xs"
                  variant="subtle"
                  to="/sessions"
                  search={{ host: asset.hostname }}
                >
                  All sessions on this server
                </ButtonLink>
              }
            >
              <Table.ScrollContainer minWidth={640} type="native">
                <Table>
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>State</Table.Th>
                      <Table.Th>User</Table.Th>
                      <Table.Th>As</Table.Th>
                      <Table.Th>Started</Table.Th>
                      <Table.Th>Duration</Table.Th>
                      <Table.Th>Recording</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {history.map((s) => (
                      <Table.Tr
                        key={s.id}
                        {...rowNav(() =>
                          navigate({ to: '/sessions/$sessionId', params: { sessionId: s.id } }),
                        )}
                      >
                        <Table.Td>
                          <SessionStateBadge
                            state={s.state}
                            silent={s.silent}
                            lastReportedAt={s.lastReportedAt}
                            endInferred={s.endInferred}
                          />
                        </Table.Td>
                        <Table.Td>
                          <Text size="xs">{s.userEmail.split('@')[0]}</Text>
                        </Table.Td>
                        <Table.Td><Mono c="azure.3">{s.principal}</Mono></Table.Td>
                        <Table.Td>
                          <Text size="xs" c="dimmed" title={absTime(s.startedAt)}>
                            {relTime(s.startedAt)}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <SessionDuration session={s} />
                        </Table.Td>
                        <Table.Td>
                          <Group gap={SP.snug} wrap="nowrap">
                            <FidelityBadge fidelity={s.fidelity} />
                            <ArtefactPending session={s} />
                          </Group>
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
              {history.length === 0 && (
                <EmptyState
                  compact
                  icon={IconHistory}
                  title="Nobody has connected to this host yet."
                  description="Brokered sessions and anything the agent reports will appear here."
                />
              )}
            </SectionCard>
          </Grid.Col>
        </Grid>
      </PageBody>

      <Suspense fallback={null}>
        {editing && canEdit && (
          <AssetForm opened={editing} onClose={() => setEditing(false)} asset={asset} />
        )}
      </Suspense>
    </Box>
  )
}

const INHERIT = 'inherit'

/**
 * Direct SSH logins on this server: its own choice, or the one in Settings,
 * and whether its agent has said it is applying it.
 */
function DirectSshCard({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data: policy } = useQuery(policyQuery())
  const setMode = useSetAssetDirectSsh()
  const own = asset.directSsh ?? null
  const global = policy?.directSsh ?? null
  const effective: DirectSsh | null = own ?? global
  const applied = asset.agentDirectSsh ?? null

  const choose = (v: string | null) => {
    if (!v) return
    const next = v === INHERIT ? null : (v as DirectSsh)
    if (next === own) return
    const nextEffective = next ?? global
    const loosens = effective && nextEffective ? directSshLoosens(effective, nextEffective) : false
    void run(
      () => setMode.mutateAsync({ id: asset.id, mode: next }),
      {
        success: ['Direct SSH logins changed',
          `${asset.hostname}: ${next ? directSshLabel(next) : `the setting in Settings (${global ? directSshLabel(global) : 'unknown'})`}. ` +
          (loosens ? 'This lets more logins in, and is in the audit log as a loosening. ' : '') +
          'The agent applies it at its next check-in, within a minute.'],
        failure: 'Direct SSH logins not changed',
      },
    )
  }

  return (
    <SectionCard
      title="Logins that don't go through Argus"
      icon={IconRouteOff}
      description="Someone with a key or password can still reach the server with ssh, scp or sftp. Pick what happens then."
    >
      <Stack gap="sm">
        <Select
          label="When someone logs in directly"
          size="xs"
          allowDeselect={false}
          disabled={!canEdit || setMode.isPending}
          value={own ?? INHERIT}
          onChange={choose}
          data={[
            { value: INHERIT, label: `Use Settings (${global ? directSshLabel(global) : '…'})` },
            ...DIRECT_SSH_CHOICES.map((c) => ({ value: c.value, label: c.label })),
          ]}
          description={effective
            ? DIRECT_SSH_CHOICES.find((c) => c.value === effective)?.description
            : undefined}
        />
        <Text size={FS.meta} c="dimmed" lh={1.5}>
          {applied === null
            ? "This server's agent has not confirmed a setting. An agent older than this setting refuses direct logins under Refuse and Ask for approval, and accepts and records them under Allow; upgrade it to hold logins for approval."
            : applied === effective
              ? `This server's agent confirmed at its last check-in that it is applying ${directSshLabel(applied)}.`
              : `This server's agent is still applying ${directSshLabel(applied)}; it picks up ${effective ? directSshLabel(effective) : 'the change'} at its next check-in.`}
        </Text>
      </Stack>
    </SectionCard>
  )
}

/**
 * Takes a server out of Argus. Its recordings and its activity stay; nobody
 * can connect to it from here any more, and its agent uninstalls itself so
 * plain SSH works again. Left running, the agent kept refusing direct logins
 * to a server the console could no longer open.
 */
function RemoveServer({ asset }: { asset: Asset }) {
  const navigate = useNavigate()
  const archive = useArchiveAsset()
  const [open, modal] = useDisclosure(false)
  const connected = asset.tunnelConnected === true
  return (
    <Card>
      <Group justify="space-between" wrap="nowrap" gap="md">
        <Box>
          <Text size="sm" fw={600}>Remove server</Text>
          <Text size={FS.meta} c="dimmed">
            Takes it out of Argus and uninstalls the agent from it. Its recordings and activity
            are kept.
          </Text>
        </Box>
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
