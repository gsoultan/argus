import { Alert, Badge, Box, Button, Card, Code, Grid, Group, Stack, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconCertificate, IconHistory, IconPlugConnected, IconRefresh,
  IconServer2, IconShieldCheck,
} from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { run } from '~/lib/notify'
import {
  CredentialBadge, Digest, Field, FidelityBadge, HealthDot, HostKeyBadge, Mono,
  SessionDuration, SessionStateBadge, absTime, relTime, rowNav,
} from '~/components/primitives'
import { FS, SP } from '~/theme'
import {
  assetQuery, sessionsQuery, usePinHostKey, useRotateCredential,
} from '~/lib/queries'

export const Route = createFileRoute('/assets/$assetId')({
  component: AssetDetail,
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(assetQuery(params.assetId)),
})

function AssetDetail() {
  const navigate = useNavigate()
  const { assetId } = Route.useParams()
  const { data: asset } = useQuery(assetQuery(assetId))
  const { data: allSessions } = useQuery(sessionsQuery())
  const pin = usePinHostKey()
  const rotate = useRotateCredential()

  if (!asset) {
    return (
      <Box>
        <PageHeader crumbs={[{ label: 'Assets', to: '/assets' }]} title="Asset not found" />
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
  const isCa = asset.credentialMode === 'ca-certificate'

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

  const onRotate = () =>
    run(() => rotate.mutateAsync(asset.id), {
      failure: 'Credential not rotated',
      success: [
        'Credential rotated',
        'A new key was generated, installed and the old one revoked.',
      ],
    })

  return (
    <Box>
      <PageHeader
        crumbs={[
          { label: 'Assets', to: '/assets' },
          { label: asset.hostname.split('.')[0] ?? asset.hostname },
        ]}
        title={asset.hostname}
        status={<HealthDot health={asset.health} />}
        description={`${asset.os} · ${asset.address}:${asset.port}`}
        actions={
          !isCa ? (
            <Button
              variant="default"
              leftSection={<IconRefresh size={14} />}
              loading={rotate.isPending}
              onClick={onRotate}
            >
              Rotate credential
            </Button>
          ) : null
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
              <SectionCard
                title="Identity"
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

              <SectionCard
                title="Authentication to target"
                icon={isCa ? IconCertificate : IconShieldCheck}
                iconColor={isCa ? 'teal' : 'slate'}
              >
                <Stack gap="sm">
                  <Field label="Mode"><CredentialBadge mode={asset.credentialMode} /></Field>

                  {isCa ? (
                    <>
                      <Text size={FS.micro} c="dimmed" lh={1.5}>
                        Argus mints a short-lived certificate for each session. No reusable
                        credential exists on the gateway or the host, so there is nothing to
                        rotate and nothing to steal from the vault.
                      </Text>
                      <Field label="Required on host">
                        <Code block fz={FS.micro}>
                          TrustedUserCAKeys /etc/ssh/argus_ca.pub
                        </Code>
                      </Field>
                    </>
                  ) : (
                    <>
                      <Text size={FS.micro} c="dimmed" lh={1.5}>
                        A vaulted secret is injected by the gateway — the user never sees it. It
                        is still a standing credential. Moving this host to certificate auth
                        removes that exposure entirely.
                      </Text>
                      <Group grow>
                        <Field label="Last rotated">
                          <Text size="xs">{relTime(asset.credentialRotatedAt)}</Text>
                        </Field>
                        <Field label="Interval">
                          <Text size="xs">{asset.rotationIntervalDays} days</Text>
                        </Field>
                      </Group>
                    </>
                  )}

                  <Field label="Available principals">
                    <Group gap={SP.snug}>
                      {asset.principals.map((p) => (
                        <Badge
                          key={p}
                          size="xs"
                          color={p === 'root' ? 'rose' : 'slate'}
                          variant={p === 'root' ? 'light' : 'outline'}
                        >
                          {p}
                        </Badge>
                      ))}
                    </Group>
                  </Field>
                </Stack>
              </SectionCard>

              <SectionCard
                title="Connect"
                icon={IconPlugConnected}
                description="No client install and no wrapper script — the target is encoded in the username, so ordinary ssh, scp, sftp and Ansible all work unchanged."
              >
                <Code block fz={FS.digest}>
                  {`ssh ${asset.principals[0]}:${asset.hostname.split('.')[0]}@argus.northwind.id`}
                </Code>
              </SectionCard>
            </Stack>
          </Grid.Col>

          <Grid.Col span={{ base: 12, md: 7 }}>
            <SectionCard
              title="Recent sessions"
              icon={IconHistory}
              flush
              badge={<Badge size="xs" variant="light" color="slate">{history.length}</Badge>}
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
                        <Table.Td><SessionStateBadge state={s.state} /></Table.Td>
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
                        <Table.Td><FidelityBadge fidelity={s.fidelity} /></Table.Td>
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
    </Box>
  )
}
