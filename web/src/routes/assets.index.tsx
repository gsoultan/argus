import { lazy, Suspense } from 'react'
import { Alert, Box, Group, Stack, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconClipboardCheck, IconHourglassHigh, IconPlus, IconRefresh } from '@tabler/icons-react'
import { PageBody, PageHeader } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { ServerCards } from '~/components/myservers'
import { relTime } from '~/components/primitives'
import { agentFleetQuery, assetsQuery, discoveredQuery, meQuery } from '~/lib/queries'
import { isConfigured } from '~/lib/live'
import { kindOfHost } from '~/lib/reach'
import { canSee, seesWholeFleet } from '~/lib/roles'
import { FS, SP } from '~/theme'

/**
 * Assets: the first page for everyone, and one list of servers, workstations
 * and devices.
 *
 * Every server you can open, each with Connect. An admin also adds servers
 * here and reaches each one's page (who can connect, its rules) from its card.
 * Servers whose agent is installed but that nobody finished adding are at the
 * top, because the person who installed it is usually waiting to use it.
 *
 * The old Manage table (host keys, credential modes, bypass posture) is gone
 * from the list; what an admin needs from it is on each server's own page.
 */
// Loaded only once there are agents to show, so the first visit to a
// console with no enrolled servers (and every non-admin) skips its code.
const AgentFleetCard = lazy(() =>
  import('~/components/AgentFleet').then((m) => ({ default: m.AgentFleetCard })),
)

const PackageFleetCard = lazy(() =>
  import('~/components/PackageFleet').then((m) => ({ default: m.PackageFleetCard })),
)

export const Route = createFileRoute('/assets/')({
  // No role guard: this is the one page every role is sent to. The control
  // plane has already narrowed the list to what each person may reach.
  component: Servers,
  loader: ({ context }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      context.queryClient.ensureQueryData(assetsQuery({})),
    ]),
})

function Servers() {
  const { data: me } = useQuery(meQuery())
  const { data: assets } = useQuery(assetsQuery({}))
  const whole = seesWholeFleet(me?.role)
  const admin = canSee(me?.role, 'admin')

  return (
    <Box>
      <PageHeader
        title="Assets"
        description={
          !whole
            ? 'The servers, workstations and devices you were given. Open one in this browser, or reach a server from your own terminal.'
            : 'Every server, workstation and device in Argus. Open one, or Manage it to choose who can connect.'
        }
        actions={
          admin ? (
            <>
              <ButtonLink to="/assets/updates" size="sm" variant="default" leftSection={<IconRefresh size={16} />}>
                Updates
              </ButtonLink>
              <ButtonLink to="/assets/add" size="sm" leftSection={<IconPlus size={16} />}>
                Add asset
              </ButtonLink>
            </>
          ) : whole ? (
            <ButtonLink to="/assets/updates" size="sm" variant="default" leftSection={<IconRefresh size={16} />}>
              Updates
            </ButtonLink>
          ) : (
            (assets?.length ?? 0) > 0 && (
              <ButtonLink to="/approvals" variant="default" leftSection={<IconClipboardCheck size={16} />}>
                Ask for access
              </ButtonLink>
            )
          )
        }
      />
      <PageBody>
        {admin && <WaitingServers />}
        {whole && <Agents canEdit={admin} />}
        {whole && isConfigured() && (
          <Suspense fallback={null}>
            <PackageFleetCard canEdit={admin} />
          </Suspense>
        )}
        <ServerCards />
      </PageBody>
    </Box>
  )
}

/** Every server's agent, once the fleet has any. */
function Agents({ canEdit }: { canEdit: boolean }) {
  const { data } = useQuery(agentFleetQuery())
  if (!Array.isArray(data?.servers) || data.servers.length === 0) return null
  return (
    <Suspense fallback={null}>
      <AgentFleetCard canEdit={canEdit} />
    </Suspense>
  )
}

/**
 * Agents that checked in and that nobody finished adding, usually because the
 * Add server page was closed before its last step. One press finishes it.
 */
function WaitingServers() {
  const { data: waiting } = useQuery({ ...discoveredQuery('unreviewed'), enabled: isConfigured() })
  if (!waiting || waiting.length === 0) return null
  return (
    <Alert color="amber" icon={<IconHourglassHigh size={18} />} title={
      waiting.length === 1 ? '1 server is installed and waiting' : `${waiting.length} servers are installed and waiting`
    }>
      <Text size={FS.body} mb="sm">
        Their agent, or the Argus app, is running. Say who can use them to finish adding them.
      </Text>
      <Stack gap={SP.snug}>
        {waiting.map((h) => (
          <Group key={h.hostname} justify="space-between" wrap="nowrap" gap="sm">
            <Box style={{ minWidth: 0 }}>
              <Text size="sm" fw={600} ff="monospace" truncate>{h.hostname}</Text>
              <Text size={FS.meta} c="dimmed">Checked in {relTime(h.lastSeenAt)}</Text>
            </Box>
            <ButtonLink
              size="compact-sm"
              to="/assets/add"
              search={{ host: h.hostname, ...(kindOfHost(h) ? { kind: kindOfHost(h) } : {}) }}
            >
              Finish adding
            </ButtonLink>
          </Group>
        ))}
      </Stack>
    </Alert>
  )
}
