import {
  Alert, Badge, Box, Button, Card, Code, Grid, Group, Stack, Table, Text, ThemeIcon,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconArrowLeft, IconCertificate, IconRefresh, IconShieldCheck,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { ButtonLink } from '~/components/links'
import {
  CredentialBadge, Digest, FidelityBadge, HealthDot, HostKeyBadge, Mono,
  SessionStateBadge, absTime, duration, relTime,
} from '~/components/primitives'
import {
  assetQuery, sessionsQuery, usePinHostKey, useRotateCredential,
} from '~/lib/queries'

export const Route = createFileRoute('/assets/$assetId')({
  component: AssetDetail,
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(assetQuery(params.assetId)),
})

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Box>
      <Text size="10px" c="dimmed" fw={600} style={{ letterSpacing: '0.05em' }}>
        {label.toUpperCase()}
      </Text>
      <Box mt={3}>{children}</Box>
    </Box>
  )
}

function AssetDetail() {
  const navigate = useNavigate()
  const { assetId } = Route.useParams()
  const { data: asset } = useQuery(assetQuery(assetId))
  const { data: allSessions } = useQuery(sessionsQuery())
  const pin = usePinHostKey()
  const rotate = useRotateCredential()

  if (!asset) {
    return <Box p="lg"><Text size="sm" c="dimmed">Asset not found.</Text></Box>
  }

  const history = (allSessions ?? []).filter((s) => s.assetId === asset.id).slice(0, 12)
  const isCa = asset.credentialMode === 'ca-certificate'

  const onPin = async () => {
    await pin.mutateAsync(asset.id)
    notifications.show({
      color: 'teal',
      title: 'Host key pinned',
      message: 'Argus will now refuse any connection where the presented key differs.',
    })
  }

  const onRotate = async () => {
    try {
      await rotate.mutateAsync(asset.id)
      notifications.show({
        color: 'teal',
        title: 'Credential rotated',
        message: 'A new key was generated, installed and the old one revoked.',
      })
    } catch (err) {
      notifications.show({
        color: 'rose',
        title: 'Rotation not applicable',
        message: err instanceof Error ? err.message : 'Unknown error',
      })
    }
  }

  return (
    <Box>
      <PageHeader
        title={asset.hostname}
        description={`${asset.os} · ${asset.address}:${asset.port}`}
        actions={
          <>
            <ButtonLink
              size="xs"
              variant="subtle"
              color="slate"
              to="/assets"
              leftSection={<IconArrowLeft size={14} />}
            >
              Back
            </ButtonLink>
            {!isCa && (
              <Button
                size="xs"
                variant="default"
                leftSection={<IconRefresh size={14} />}
                loading={rotate.isPending}
                onClick={onRotate}
              >
                Rotate credential
              </Button>
            )}
          </>
        }
      />

      <Box p="lg">
        {asset.hostKeyState === 'changed' && (
          <Alert
            color="rose"
            variant="light"
            icon={<IconAlertTriangle size={17} />}
            mb="md"
            title="Host key does not match the pin"
          >
            <Text size="xs" mb="sm">
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
            variant="light"
            icon={<IconAlertTriangle size={17} />}
            mb="md"
            title="No host key pinned"
          >
            <Text size="xs" mb="sm">
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
              <Card padding="md">
                <Group justify="space-between" mb="sm">
                  <Text fw={600} size="sm">Identity</Text>
                  <Group gap={6}>
                    <HealthDot health={asset.health} />
                    <Text size="xs" c="dimmed" tt="capitalize">{asset.health}</Text>
                  </Group>
                </Group>
                <Stack gap={11}>
                  <Field label="Host key">
                    <Group gap={7}>
                      <HostKeyBadge state={asset.hostKeyState} />
                      {asset.hostKeyPinnedAt && (
                        <Text size="10px" c="dimmed">
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
                    <Group gap={4}>
                      {asset.tags.map((t) => (
                        <Badge key={t} size="xs" variant="outline" color="slate">{t}</Badge>
                      ))}
                    </Group>
                  </Field>
                </Stack>
              </Card>

              <Card padding="md">
                <Group gap={7} mb={7}>
                  <ThemeIcon
                    variant="light"
                    color={isCa ? 'teal' : 'slate'}
                    size={22}
                    radius="sm"
                  >
                    {isCa ? <IconCertificate size={13} /> : <IconShieldCheck size={13} />}
                  </ThemeIcon>
                  <Text fw={600} size="sm">Authentication to target</Text>
                </Group>
                <Stack gap={11}>
                  <Field label="Mode"><CredentialBadge mode={asset.credentialMode} /></Field>

                  {isCa ? (
                    <>
                      <Text size="10px" c="dimmed" lh={1.45}>
                        Argus mints a short-lived certificate for each session. No reusable
                        credential exists on the gateway or the host, so there is nothing to
                        rotate and nothing to steal from the vault.
                      </Text>
                      <Field label="Required on host">
                        <Code block fz={10}>
                          TrustedUserCAKeys /etc/ssh/argus_ca.pub
                        </Code>
                      </Field>
                    </>
                  ) : (
                    <>
                      <Text size="10px" c="dimmed" lh={1.45}>
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
                    <Group gap={5}>
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
              </Card>

              <Card padding="md">
                <Text fw={600} size="sm" mb={7}>Connect</Text>
                <Text size="10px" c="dimmed" mb={7} lh={1.45}>
                  No client install and no wrapper script — the target is encoded in the
                  username, so ordinary <Mono>ssh</Mono>, <Mono>scp</Mono>, <Mono>sftp</Mono> and
                  Ansible all work unchanged.
                </Text>
                <Code block fz={11}>
                  {`ssh ${asset.principals[0]}:${asset.hostname.split('.')[0]}@argus.northwind.id`}
                </Code>
              </Card>
            </Stack>
          </Grid.Col>

          <Grid.Col span={{ base: 12, md: 7 }}>
            <Card padding={0}>
              <Group justify="space-between" p="md" pb="sm">
                <Text fw={600} size="sm">Recent sessions</Text>
                <Badge size="xs" variant="light" color="slate">{history.length}</Badge>
              </Group>
              <Table verticalSpacing={7} horizontalSpacing="md" highlightOnHover>
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
                  onClick={() => navigate({ to: '/sessions/$sessionId', params: { sessionId: s.id } })}
                  className="cursor-pointer"
                >
                      <Table.Td><SessionStateBadge state={s.state} /></Table.Td>
                      <Table.Td>
                        <Text size="xs">{s.userEmail.split('@')[0]}</Text>
                      </Table.Td>
                      <Table.Td><Mono c="teal.4">{s.principal}</Mono></Table.Td>
                      <Table.Td>
                        <Text size="xs" c="dimmed" title={absTime(s.startedAt)}>
                          {relTime(s.startedAt)}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="xs" c="dimmed">{duration(s.startedAt, s.endedAt)}</Text>
                      </Table.Td>
                      <Table.Td><FidelityBadge fidelity={s.fidelity} /></Table.Td>
                    </Table.Tr>
                  ))}
                  {history.length === 0 && (
                    <Table.Tr>
                      <Table.Td colSpan={6}>
                        <Text size="xs" c="dimmed" ta="center" py="lg">
                          Nobody has connected to this host yet.
                        </Text>
                      </Table.Td>
                    </Table.Tr>
                  )}
                </Table.Tbody>
              </Table>
            </Card>
          </Grid.Col>
        </Grid>
      </Box>
    </Box>
  )
}
