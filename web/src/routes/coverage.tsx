import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Grid, Group, Modal, Stack, Table, Text, Textarea, Tooltip,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconArrowRight, IconCircleCheck, IconEyeOff, IconPlus,
  IconShieldCheck, IconShieldOff,
} from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { Mono, Stat, relTime } from '~/components/primitives'
import { FS, SP } from '~/theme'
import { coverageQuery, discoveredQuery, useEnrolHost, useIgnoreHost } from '~/lib/queries'
import { isConfigured } from '~/lib/live'
import type { DiscoveredHost } from '~/types/domain'

/**
 * No loader, deliberately — see the note on `/connect`.
 *
 * What made this page mislead was never the missing loader but `?? 0`: a screen
 * whose whole job is "does Argus see everything?" answered "no gaps anywhere"
 * for as long as the request was in flight. `Stat` renders a skeleton for a
 * count it does not have, which fixes that without blanking the console.
 */
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
  const navigate = useNavigate()
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
        <PageBody>
          <Alert color="amber" icon={<IconAlertTriangle size={16} />}>
            <Text size={FS.body} lh={1.5}>
              Coverage is reported by agents through the control plane, so it has nothing to
              show against the local fixture. Point <Mono>VITE_CONTROL_URL</Mono> at a running
              control plane to see it.
            </Text>
          </Alert>
        </PageBody>
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

      <PageBody>
        <Grid gap="sm">
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Linux assets with an agent"
              value={cov ? `${cov.assetsWithAgent} / ${cov.sshAssets}` : undefined}
              tone={cov && cov.assetsUnmonitored > 0 ? 'warn' : 'ok'}
              sub="An asset with no agent records nothing when someone connects to port 22 directly."
            />
          </Grid.Col>
          {/* Not a link, on purpose. A host outside the inventory is the one
              thing the Assets table cannot show — it lists what Argus manages —
              and it is the reason this page exists separately from it. The
              hosts themselves are in the table below. */}
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Hosts outside the inventory"
              value={cov ? String(cov.unreviewedHosts) : undefined}
              tone={cov && cov.unreviewedHosts > 0 ? 'warn' : 'ok'}
              sub="Agents reporting from machines Argus is not managing. Listed below — the inventory cannot show these."
            />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Agents gone quiet"
              value={cov ? String(cov.assetsAgentStale) : undefined}
              tone={cov && cov.assetsAgentStale > 0 ? 'warn' : 'ok'}
              sub="An agent can be killed by root on the host. The silence is what gives it away."
              onClick={() => navigate({ to: '/assets', search: { agent: 'stale' } })}
            />
          </Grid.Col>
        </Grid>

        {cov && cov.rdpAwaitingAgent > 0 && (
          <Alert
            color="slate"
            icon={<IconShieldCheck size={16} />}
            title={`${cov.rdpAwaitingAgent} Remote Desktop ${cov.rdpAwaitingAgent === 1 ? 'host has' : 'hosts have'} no agent available`}
          >
            <Text size={FS.body} lh={1.5}>
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
            icon={<IconShieldOff size={16} />}
            title={`${cov.assetsUnmonitored} ${cov.assetsUnmonitored === 1 ? 'host' : 'hosts'} would not record a bypass`}
          >
            <Text size={FS.body} mb="xs" lh={1.5}>
              A session opened straight to sshd on these hosts is not recorded at all. Brokered
              sessions still are — this is the gap the agent exists to close.
            </Text>
            <ButtonLink
              size="compact-xs"
              variant="subtle"
              color="rose"
              to="/assets"
              search={{ bypass: 'open' }}
              rightSection={<IconArrowRight size={12} />}
            >
              Show these hosts
            </ButtonLink>
          </Alert>
        )}

        <SectionCard
          title="Discovered hosts"
          icon={IconShieldCheck}
          iconColor={unreviewed?.length ? 'amber' : 'slate'}
          description="Agents reporting from machines the inventory has no entry for. Enrolling creates the asset; it grants nobody a login."
          badge={
            unreviewed && (
              <Badge size="sm" variant="light" color={unreviewed.length ? 'amber' : 'slate'}>
                {unreviewed.length} unreviewed
              </Badge>
            )
          }
          flush
        >
          {!unreviewed?.length ? (
            <EmptyState
              compact
              icon={IconCircleCheck}
              title="Every host running an agent is in the inventory."
              description="Nothing is reporting from a machine Argus does not manage."
            />
          ) : (
            <Table.ScrollContainer minWidth={860} type="native">
              <Table fz="xs">
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
                        <Text size="xs" c="dimmed">{h.os || '—'}</Text>
                      </Table.Td>
                      <Table.Td>
                        <Mono>{h.addresses.join(', ') || '—'}</Mono>
                      </Table.Td>
                      <Table.Td>
                        {h.sshPorts.length === 0 && <Text size="xs" c="dimmed">—</Text>}
                        <Group gap={SP.tight}>
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
                          {h.accounts.length === 0
                            ? '—'
                            : h.accounts.slice(0, 4).join(', ')}
                          {h.accounts.length > 4 && ` +${h.accounts.length - 4}`}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="xs" c="dimmed">{relTime(h.lastSeenAt)}</Text>
                      </Table.Td>
                      <Table.Td>
                        <Group gap={SP.tight} justify="flex-end" wrap="nowrap">
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
            </Table.ScrollContainer>
          )}
        </SectionCard>

        {!!ignored?.length && (
          <SectionCard
            title="Deliberately unmanaged"
            icon={IconEyeOff}
            iconColor="slate"
            description="Kept rather than deleted. A dismissal that left no trace would be indistinguishable from a host nobody ever looked at."
          >
            <Stack gap={SP.snug}>
              {ignored.map((h) => (
                <Group key={h.hostname} gap="xs" wrap="nowrap">
                  <Mono>{h.fqdn || h.hostname}</Mono>
                  <Text size="xs" c="dimmed">
                    — {h.reviewNote} ({h.reviewedBy}, {relTime(h.reviewedAt ?? h.lastSeenAt)})
                  </Text>
                </Group>
              ))}
            </Stack>
          </SectionCard>
        )}
      </PageBody>

      <Modal
        opened={!!dismissing}
        onClose={() => setDismissing(null)}
        title={`Leave ${dismissing?.hostname ?? ''} unmanaged`}
        size="md"
      >
        <Alert color="amber" icon={<IconAlertTriangle size={16} />} mb="md">
          <Text size={FS.body} lh={1.5}>
            Privileged sessions on this host will not be brokered or recorded by Argus. The
            decision is written to the audit log against your account.
          </Text>
        </Alert>
        <Textarea
          size="sm"
          label="Why is this host deliberately unmanaged?"
          placeholder="e.g. Ephemeral CI runner, destroyed after each build."
          minRows={3}
          autosize
          value={note}
          onChange={(e) => setNote(e.currentTarget.value)}
        />
        <Group justify="flex-end" mt="md">
          <Button variant="subtle" color="slate" onClick={() => setDismissing(null)}>
            Cancel
          </Button>
          <Button
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
