import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Grid, Group, Modal, Stack, Table, Text,
  Textarea, Tooltip,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconEyeOff, IconPlus, IconShieldCheck, IconShieldOff,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { Mono, relTime } from '~/components/primitives'
import { coverageQuery, discoveredQuery, useEnrolHost, useIgnoreHost } from '~/lib/queries'
import { isConfigured } from '~/lib/live'
import type { DiscoveredHost } from '~/types/domain'

export const Route = createFileRoute('/coverage')({ component: CoverageView })

/**
 * Answers "does Argus actually see everything?".
 *
 * Deliberately shows both gaps side by side. A managed host with no agent is
 * one where a direct connection to port 22 leaves no trace; an agent reporting
 * from outside the inventory is a privileged host Argus is not managing at all.
 * Neither implies the other, and a page showing only the first would look
 * complete while missing whole machines.
 */
function CoverageView() {
  const { data: cov } = useQuery(coverageQuery())
  const { data: unreviewed } = useQuery(discoveredQuery('unreviewed'))
  const { data: ignored } = useQuery(discoveredQuery('ignored'))

  const [dismissing, setDismissing] = useState<DiscoveredHost | null>(null)
  const [note, setNote] = useState('')
  const enrol = useEnrolHost()
  const ignore = useIgnoreHost()

  if (!isConfigured()) {
    return (
      <Box>
        <PageHeader
          title="Coverage"
          description="Which privileged hosts Argus can actually see."
        />
        <Box p="lg">
          <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />}>
            <Text size="xs">
              Coverage is reported by agents through the control plane, so it has nothing to
              show against the local fixture. Point <Mono>VITE_CONTROL_URL</Mono> at a running
              control plane to see it.
            </Text>
          </Alert>
        </Box>
      </Box>
    )
  }

  const onEnrol = async (h: DiscoveredHost) => {
    try {
      await enrol.mutateAsync(h.hostname)
    } catch (err) {
      notifications.show({
        color: 'rose',
        title: 'Not enrolled',
        message: err instanceof Error ? err.message : 'the control plane refused',
      })
      return
    }
    notifications.show({
      color: 'teal',
      title: 'Host enrolled',
      // Saying this plainly avoids the assumption that enrolling granted access.
      message: `${h.hostname} is now a managed asset with no principals. Add the accounts people may assume.`,
    })
  }

  const onIgnore = async () => {
    if (!dismissing) return
    try {
      await ignore.mutateAsync({ hostname: dismissing.hostname, note })
    } catch (err) {
      notifications.show({
        color: 'rose',
        title: 'Not dismissed',
        message: err instanceof Error ? err.message : 'the control plane refused',
      })
      return
    }
    setDismissing(null)
    setNote('')
  }

  return (
    <Box>
      <PageHeader
        title="Coverage"
        description="Which privileged hosts Argus can actually see — in both directions."
      />

      <Box p="lg">
        <Grid gap="sm" mb="md">
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Linux assets with an agent"
              value={`${cov?.assetsWithAgent ?? 0} / ${cov?.sshAssets ?? 0}`}
              tone={cov && cov.assetsUnmonitored > 0 ? 'warn' : 'ok'}
              hint="An asset with no agent records nothing when someone connects to port 22 directly."
            />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Hosts outside the inventory"
              value={String(cov?.unreviewedHosts ?? 0)}
              tone={cov && cov.unreviewedHosts > 0 ? 'warn' : 'ok'}
              hint="Agents reporting from machines Argus is not managing."
            />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Agents gone quiet"
              value={String(cov?.assetsAgentStale ?? 0)}
              tone={cov && cov.assetsAgentStale > 0 ? 'warn' : 'ok'}
              hint="An agent can be killed by root on the host. The silence is what gives it away."
            />
          </Grid.Col>
        </Grid>

        {cov && cov.rdpAwaitingAgent > 0 && (
          <Alert
            color="slate"
            variant="light"
            icon={<IconShieldCheck size={16} />}
            mb="md"
            title={`${cov.rdpAwaitingAgent} Remote Desktop ${cov.rdpAwaitingAgent === 1 ? 'host has' : 'hosts have'} no agent available`}
          >
            <Text size="xs">
              Sessions brokered through Argus are recorded. A connection made straight to
              port 3389 is not, and no Windows agent exists yet to close that — so this is a
              limit of the product rather than something to fix here. It is listed
              separately from the gaps you can act on.
            </Text>
          </Alert>
        )}

        {cov && cov.assetsUnmonitored > 0 && (
          <Alert
            color="rose"
            variant="light"
            icon={<IconShieldOff size={16} />}
            mb="md"
            title={`${cov.assetsUnmonitored} managed ${cov.assetsUnmonitored === 1 ? 'host has' : 'hosts have'} no healthy agent`}
          >
            <Text size="xs">
              A session opened straight to sshd on these hosts is not recorded at all. Brokered
              sessions still are — this is the gap the agent exists to close.
            </Text>
          </Alert>
        )}

        <Card padding={0} mb="md">
          <Box p="md" pb="xs">
            <Group gap="xs">
              <IconShieldCheck size={16} />
              <Text fw={600} size="sm">
                Discovered hosts
              </Text>
              <Badge size="sm" variant="light" color={unreviewed?.length ? 'amber' : 'slate'}>
                {unreviewed?.length ?? 0} unreviewed
              </Badge>
            </Group>
            <Text size="xs" c="dimmed" mt={4}>
              Agents reporting from machines the inventory has no entry for. Enrolling creates
              the asset; it grants nobody a login.
            </Text>
          </Box>

          {!unreviewed?.length ? (
            <Box p="md" pt={0}>
              <Text size="xs" c="dimmed">
                Every host running an agent is in the inventory.
              </Text>
            </Box>
          ) : (
            <Table highlightOnHover verticalSpacing="xs" fz="xs">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Host</Table.Th>
                  <Table.Th>OS</Table.Th>
                  <Table.Th>Addresses</Table.Th>
                  <Table.Th>SSH ports</Table.Th>
                  <Table.Th>Accounts</Table.Th>
                  <Table.Th>Seen</Table.Th>
                  <Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {unreviewed.map((h) => (
                  <Table.Tr key={h.hostname}>
                    <Table.Td>
                      <Mono>{h.fqdn || h.hostname}</Mono>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" c="dimmed">
                        {h.os || '—'}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Mono>{h.addresses.join(', ') || '—'}</Mono>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4}>
                        {h.sshPorts.map((p) => (
                          <Badge
                            key={p}
                            size="xs"
                            variant="light"
                            color={p === 22 ? 'slate' : 'amber'}
                          >
                            {p}
                          </Badge>
                        ))}
                        {h.sshPorts.length > 1 && (
                          <Tooltip label="sshd listens on more than one port; each is a separate way in">
                            <IconAlertTriangle size={13} />
                          </Tooltip>
                        )}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" c="dimmed">
                        {h.accounts.slice(0, 4).join(', ')}
                        {h.accounts.length > 4 && ` +${h.accounts.length - 4}`}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" c="dimmed">
                        {relTime(h.lastSeenAt)}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} justify="flex-end" wrap="nowrap">
                        <Button
                          size="compact-xs"
                          variant="light"
                          leftSection={<IconPlus size={12} />}
                          loading={enrol.isPending && enrol.variables === h.hostname}
                          onClick={() => void onEnrol(h)}
                        >
                          Enrol
                        </Button>
                        <Button
                          size="compact-xs"
                          variant="subtle"
                          color="slate"
                          leftSection={<IconEyeOff size={12} />}
                          onClick={() => {
                            setDismissing(h)
                            setNote('')
                          }}
                        >
                          Dismiss
                        </Button>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
        </Card>

        {!!ignored?.length && (
          <Card padding="md">
            <Text fw={600} size="sm" mb={4}>
              Deliberately unmanaged
            </Text>
            <Text size="xs" c="dimmed" mb="sm">
              Kept rather than deleted. A dismissal that left no trace would be
              indistinguishable from a host nobody ever looked at.
            </Text>
            <Stack gap={6}>
              {ignored.map((h) => (
                <Group key={h.hostname} gap="xs" wrap="nowrap">
                  <Mono>{h.fqdn || h.hostname}</Mono>
                  <Text size="xs" c="dimmed">
                    — {h.reviewNote} ({h.reviewedBy}, {relTime(h.reviewedAt ?? h.lastSeenAt)})
                  </Text>
                </Group>
              ))}
            </Stack>
          </Card>
        )}
      </Box>

      <Modal
        opened={!!dismissing}
        onClose={() => setDismissing(null)}
        title={`Leave ${dismissing?.hostname ?? ''} unmanaged`}
        size="md"
      >
        <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} mb="md">
          <Text size="xs">
            Privileged sessions on this host will not be brokered or recorded by Argus. The
            decision is written to the audit log against your account.
          </Text>
        </Alert>
        <Textarea
          label="Why is this host deliberately unmanaged?"
          placeholder="e.g. Ephemeral CI runner, destroyed after each build."
          minRows={3}
          autosize
          value={note}
          onChange={(e) => setNote(e.currentTarget.value)}
        />
        <Group justify="flex-end" mt="md">
          <Button variant="subtle" color="slate" size="xs" onClick={() => setDismissing(null)}>
            Cancel
          </Button>
          <Button
            size="xs"
            color="amber"
            loading={ignore.isPending}
            disabled={note.trim().length < 8}
            onClick={() => void onIgnore()}
          >
            Dismiss host
          </Button>
        </Group>
      </Modal>
    </Box>
  )
}

function Stat({
  label,
  value,
  tone,
  hint,
}: {
  label: string
  value: string
  tone: 'ok' | 'warn'
  hint: string
}) {
  return (
    <Card padding="md">
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text fz={28} fw={600} c={tone === 'warn' ? 'amber.4' : undefined} lh={1.2} my={4}>
        {value}
      </Text>
      <Text size="xs" c="dimmed">
        {hint}
      </Text>
    </Card>
  )
}
