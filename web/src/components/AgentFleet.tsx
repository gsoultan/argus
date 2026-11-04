import { useState } from 'react'
import { Anchor, Badge, Box, Button, Group, Modal, Stack, Switch, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { IconRefresh } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { isConfigured, versionBefore, type AgentFleetRow, type AgentFleetView } from '~/lib/live'
import { run } from '~/lib/notify'
import { agentFleetQuery, useSetAgentAutoUpdate, useUpdateAgentFleet } from '~/lib/queries'
import { FS, SP, INK } from '~/theme'

/**
 * Every server's agent, on the Servers page.
 *
 * The console looks for the newest Argus release with agent packages and
 * offers it (or its own version, when that is newer). This card says which
 * servers run an older agent, updates them all with one press, and has a
 * switch to let the console do that by itself whenever a new version is out.
 *
 * Each agent still updates the careful way: once nobody is connected to its
 * server, holding logins through the restart, and going back on its own if
 * the new version does not come up. So Update all is safe to press with
 * people working; busy servers update later, each when it is free.
 */
export function AgentFleetCard({ canEdit }: { canEdit: boolean }) {
  const { data: v } = useQuery(agentFleetQuery())
  const [confirming, setConfirming] = useState(false)
  const update = useUpdateAgentFleet()
  const auto = useSetAgentAutoUpdate()
  if (!isConfigured() || !v || !Array.isArray(v.servers) || v.servers.length === 0) return null

  const needsLook = v.servers.filter((s) => s.state !== 'current')
  const current = v.servers.length - needsLook.length

  return (
    <SectionCard
      title="Agents"
      icon={IconRefresh}
      badge={summaryBadge(v)}
      description={<OfferLine view={v} />}
    >
      <Stack gap="sm">
        {needsLook.length === 0 ? (
          <Text size={FS.body} c="dimmed">
            {v.latest
              ? `Every server's agent runs ${v.latest} or newer.`
              : 'There is no version to offer: this console was built from source and found no release.'}
          </Text>
        ) : (
          <Stack gap={SP.cozy}>
            {needsLook.map((s) => (
              <AgentRow key={s.hostname} row={s} latest={v.latest} canEdit={canEdit} />
            ))}
            {current > 0 && (
              <Text size={FS.meta} c="dimmed">
                {current === 1 ? '1 other server is' : `${current} other servers are`} up to date.
              </Text>
            )}
          </Stack>
        )}

        {canEdit && (
          <Group justify="space-between" gap="sm" wrap="wrap">
            <Switch
              size="sm"
              checked={v.automatic}
              disabled={auto.isPending || (!v.latest && !v.automatic)}
              label="Update agents automatically when a new version is out"
              description="Each server updates once nobody is connected to it. A version that went back on a server is not tried there again on its own."
              onChange={(e) => {
                const on = e.currentTarget.checked
                void run(() => auto.mutateAsync(on), {
                  failure: 'Automatic updates not changed',
                  success: on
                    ? ['Automatic updates on', 'Agents that are behind update as soon as their server is free.']
                    : ['Automatic updates off', 'Agents update only when someone asks.'],
                })
              }}
              style={{ flex: '1 1 320px' }}
            />
            {v.behind > 0 && (
              <Button size="compact-sm" leftSection={<IconRefresh size={14} />} onClick={() => setConfirming(true)}>
                Update {v.behind === 1 ? '1 server' : `${v.behind} servers`} to {v.latest}
              </Button>
            )}
          </Group>
        )}
      </Stack>
      {confirming && (
        <ConfirmUpdateAll
          view={v}
          pending={update.isPending}
          onClose={() => setConfirming(false)}
          onConfirm={() =>
            void run(
              async () => {
                const r = await update.mutateAsync({})
                if (r.asked.length === 0) throw new Error('No server was behind any more.')
              },
              {
                failure: 'Agents not updated',
                success: ['Update asked', `Each agent updates to ${v.latest} once nobody is connected to its server.`],
              },
            ).then((ok) => ok && setConfirming(false))
          }
        />
      )}
    </SectionCard>
  )
}

function summaryBadge(v: AgentFleetView) {
  if (v.updating > 0) return <Badge size="xs" variant="light" color="azure">{v.updating} updating</Badge>
  if (v.behind > 0) return <Badge size="xs" variant="light" color="amber">{v.behind} behind</Badge>
  if (v.latest && v.servers.every((s) => s.state === 'current'))
    return <Badge size="xs" variant="light" color="teal">all on {v.latest}</Badge>
  return null
}

/** Which version is offered, and where it came from. */
function OfferLine({ view: v }: { view: AgentFleetView }) {
  if (!v.latest) return <>The Argus agent on each server.</>
  const fromRelease = v.release && v.release.version === v.latest
  return (
    <>
      Newest agent: <b>{v.latest}</b>
      {fromRelease ? (
        <>
          {' '}(
          <Anchor href={v.release!.page} target="_blank" rel="noreferrer" fz="inherit">
            release
          </Anchor>
          {v.checkedAt ? `, checked ${relTime(v.checkedAt)}` : ''})
        </>
      ) : (
        ' (this console’s version)'
      )}
      {v.consoleVersion && versionBefore(v.consoleVersion, v.latest) && (
        <> · newer than this console ({v.consoleVersion}); update the control plane too</>
      )}
      {v.checkFailure && <> · could not check for a newer one: {v.checkFailure}</>}
    </>
  )
}

const STATE: Record<AgentFleetRow['state'], [string, string]> = {
  current: ['teal', 'up to date'],
  behind: ['amber', 'update available'],
  updating: ['azure', 'updating'],
  held: ['rose', 'went back'],
  cannot: ['slate', 'update by hand'],
  too_old: ['slate', 'update by hand once'],
  not_reported: ['slate', 'not reported'],
}

function AgentRow({ row: s, latest, canEdit }: { row: AgentFleetRow; latest: string; canEdit: boolean }) {
  const update = useUpdateAgentFleet()
  const [color, label] = STATE[s.state]
  const progress = s.state === 'updating' && s.status?.id === s.request?.id ? s.status?.message : undefined
  const detail =
    s.state === 'updating'
      ? progress ?? `asked by ${s.request?.requestedBy}; the agent picks it up within 30 seconds`
      : s.why
  return (
    <Group justify="space-between" wrap="nowrap" gap="sm" align="flex-start">
      <Box style={{ minWidth: 0 }}>
        <Group gap={SP.snug} wrap="nowrap">
          <Link
            to="/assets/$assetId"
            params={{ assetId: s.assetId }}
            style={{ color: INK.brand, fontSize: 14, fontWeight: 600, fontFamily: 'var(--mantine-font-family-monospace)', textDecoration: 'none', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
          >
            {s.hostname}
          </Link>
          <Text size={FS.meta} ff="monospace" c="dimmed">{s.agentVersion || '?'}</Text>
          <Badge size="xs" variant="light" color={color}>{label}</Badge>
        </Group>
        {detail && <Text size={FS.meta} c="dimmed" lh={1.4}>{detail}</Text>}
      </Box>
      {canEdit && (s.state === 'behind' || s.state === 'held') && latest && (
        <Button
          size="compact-xs"
          variant={s.state === 'held' ? 'default' : 'light'}
          loading={update.isPending}
          onClick={() =>
            void run(() => update.mutateAsync({ hostnames: [s.hostname] }), {
              failure: 'Agent not updated',
              success: ['Update asked', `${s.hostname} updates to ${latest} once nobody is connected to it.`],
            })
          }
        >
          {s.state === 'held' ? 'Try again' : 'Update'}
        </Button>
      )}
    </Group>
  )
}

function ConfirmUpdateAll({ view: v, pending, onClose, onConfirm }: {
  view: AgentFleetView
  pending: boolean
  onClose: () => void
  onConfirm: () => void
}) {
  const busy = v.servers.filter((s) => s.state === 'behind' && s.activeSessions > 0).length
  return (
    <Modal opened onClose={onClose} title={`Update ${v.behind === 1 ? '1 agent' : `${v.behind} agents`} to ${v.latest}`}>
      <Stack gap="sm">
        <Text size={FS.body} lh={1.5}>
          Each agent downloads {v.latest} and checks it against the release, then waits until nobody is
          connected to its server before it restarts. The restart takes a few seconds, and anyone who logs in
          during it waits a moment instead of being refused. If the new version does not come back, that agent
          goes back to the version it runs now.
        </Text>
        {busy > 0 && (
          <Text size={FS.body} c="dimmed" lh={1.5}>
            {busy === 1 ? '1 of them has' : `${busy} of them have`} people connected now, and will update once
            they finish.
          </Text>
        )}
        <Group justify="flex-end" gap={SP.cozy}>
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={pending} onClick={onConfirm}>Update when free</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
