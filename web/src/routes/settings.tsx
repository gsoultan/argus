import {
  Alert, Badge, Box, Card, Code, Grid, Group, Stack, Switch, Text, ThemeIcon,
} from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconDeviceDesktop, IconInfoCircle, IconLock, IconNetwork,
  IconShieldLock, IconTerminal2,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { Mono } from '~/components/primitives'

export const Route = createFileRoute('/settings')({ component: Settings })

function Toggle({
  label, description, defaultChecked, danger,
}: {
  label: string
  description: string
  defaultChecked?: boolean
  danger?: boolean
}) {
  return (
    <Group justify="space-between" wrap="nowrap" align="flex-start" gap="lg">
      <Box>
        <Group gap={6}>
          <Text size="xs" fw={500}>{label}</Text>
          {danger && <Badge size="xs" color="rose">raises risk</Badge>}
        </Group>
        <Text size="10px" c="dimmed" mt={2} lh={1.45}>{description}</Text>
      </Box>
      <Switch
        size="sm"
        color={danger ? 'rose' : 'teal'}
        defaultChecked={defaultChecked}
        style={{ flexShrink: 0 }}
      />
    </Group>
  )
}

function Settings() {
  return (
    <Box>
      <PageHeader
        title="Settings"
        description="Gateway policy. Defaults are the safe choice — each toggle says what loosening it costs you."
      />

      <Box p="lg">
        <Grid gap="sm">
          <Grid.Col span={{ base: 12, lg: 7 }}>
            <Stack gap="sm">
              <Card padding="md">
                <Group gap={7} mb="md">
                  <ThemeIcon variant="light" color="teal" size={22} radius="sm">
                    <IconTerminal2 size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">SSH channel policy</Text>
                </Group>
                <Stack gap="lg">
                  <Toggle
                    label="Allow local port forwarding (-L)"
                    description="Unrestricted forwarding turns the gateway into an open tunnel into the private network — the exact thing a bastion exists to prevent. Leave off and allowlist per role."
                    danger
                  />
                  <Toggle
                    label="Allow remote port forwarding (-R)"
                    description="Lets a target open a listener back through the gateway. Rarely needed, and a clean egress path for an attacker who already has the host."
                    danger
                  />
                  <Toggle
                    label="Allow SSH agent forwarding"
                    description="Anyone with root on the gateway can sign challenges with the user's keys for the life of the session. Credential injection exists precisely so this is unnecessary."
                    danger
                  />
                  <Toggle
                    label="Allow X11 forwarding"
                    description="Broad attack surface, almost never used for server administration."
                    danger
                  />
                  <Toggle
                    label="Proxy SFTP as a subsystem"
                    description="Decodes the SFTP protocol rather than teeing raw bytes, so every file open, read, write and delete becomes an audit event with a path and a size."
                    defaultChecked
                  />
                </Stack>
              </Card>

              <Card padding="md">
                <Group gap={7} mb="md">
                  <ThemeIcon variant="light" color="teal" size={22} radius="sm">
                    <IconLock size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Recording &amp; retention</Text>
                </Group>
                <Stack gap="lg">
                  <Toggle
                    label="Fail closed when recording is unavailable"
                    description="If the recorder cannot write, refuse the session rather than allowing an unrecorded one. This is what an auditor means by 'all privileged sessions are recorded'."
                    defaultChecked
                  />
                  <Toggle
                    label="Require eBPF agent for root sessions"
                    description="PTY capture alone can be defeated by base64 or by running a script. For root, insist on kernel-observed execve evidence."
                    defaultChecked
                  />
                  <Toggle
                    label="Encrypt recordings at rest with a separate key"
                    description="Keeps the recording key distinct from the credential vault key, so compromising one does not yield the other."
                    defaultChecked
                  />
                </Stack>
              </Card>
            </Stack>
          </Grid.Col>

          <Grid.Col span={{ base: 12, lg: 5 }}>
            <Stack gap="sm">
              <Card padding="md">
                <Group gap={7} mb="sm">
                  <ThemeIcon variant="light" color="sky" size={22} radius="sm">
                    <IconNetwork size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Gateway endpoint</Text>
                </Group>
                <Text size="10px" c="dimmed" mb="xs" lh={1.45}>
                  Users connect with their normal client. The target is encoded in the username,
                  so there is nothing to install and existing tooling keeps working.
                </Text>
                <Code block fz={11}>
                  {`ssh ops:db-01@argus.northwind.id
scp report.csv ops:db-01@argus.northwind.id:/tmp/
sftp ops:db-01@argus.northwind.id`}
                </Code>
              </Card>

              <Card padding="md">
                <Group gap={7} mb="sm">
                  <ThemeIcon variant="light" color="teal" size={22} radius="sm">
                    <IconShieldLock size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Certificate authority</Text>
                </Group>
                <Text size="10px" c="dimmed" mb="xs" lh={1.45}>
                  Add this to a host to move it off standing credentials. Argus then mints a
                  short-lived certificate per session and there is nothing left in the vault to
                  steal for that host.
                </Text>
                <Code block fz={10}>
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
                <Group gap={7} mb={6}>
                  <ThemeIcon variant="light" color="slate" size={22} radius="sm">
                    <IconDeviceDesktop size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Windows RDP</Text>
                  <Badge size="xs" color="slate" variant="outline">phase 3</Badge>
                </Group>
                <Text size="10px" c="dimmed" lh={1.45}>
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
    </Box>
  )
}
