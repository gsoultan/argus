import { useState } from 'react'
import { Alert, Badge, Button, Card, Group, Loader, Modal, Stack, Text, TextInput, ThemeIcon } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconCheck, IconCircleDashed, IconRefresh } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Field, relTime } from '~/components/primitives'
import { isConfigured, type AgentUpdateStatus, type AgentUpdateView } from '~/lib/live'
import { run } from '~/lib/notify'
import { agentUpdateQuery, useCancelAgentUpdate, useRequestAgentUpdate } from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

const RELEASE = /^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$/

/**
 * The agent on one server: which version it runs, and updating it from here.
 *
 * Updating used to mean running a package upgrade in a terminal on the
 * server, which restarted the agent ten seconds later and took every session
 * on the server with it -- including the terminal the upgrade was typed in.
 * From here the agent waits for the server to be free, restarts in a couple
 * of seconds with new logins waiting rather than refused, and goes back to
 * the old version on its own if the new one does not reconnect. The card says
 * which of those steps it is on, from what the agent itself reports.
 */
export function AgentUpdateCard({ asset, canEdit, bare = false }: {
  asset: Asset
  canEdit: boolean
  /** A plain card under a heading the page already shows, rather than a titled one. */
  bare?: boolean
}) {
  const { data: v } = useQuery(agentUpdateQuery(asset.id))
  const [asking, ask] = useDisclosure(false)
  const cancel = useCancelAgentUpdate(asset.id)
  const request = useRequestAgentUpdate(asset.id)
  if (bare && isConfigured() && v && !v.enrolled) {
    return (
      <Card padding="md">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          No agent is enrolled on this server. Install one from Servers, Add a server, and it
          appears here.
        </Text>
      </Card>
    )
  }
  if (!isConfigured() || !v || !v.enrolled) return null

  const st = v.status
  const live = v.request !== null
  const current = v.latest !== '' && v.agentVersion === v.latest
  const pickedUp = live && st?.id === v.request?.id
  const offer = canEdit && !live && st?.supported === true && !current

  const body = (
    <>
      <Stack gap="sm">
        <Field label="Version">
          <Group gap={SP.snug}>
            <Text size="xs" ff="monospace">
              {v.agentVersion || 'not reported yet'}
              {v.agentLastSeenAt && (
                <Text span size={FS.micro} c="dimmed" ff="inherit"> · checked in {relTime(v.agentLastSeenAt)}</Text>
              )}
            </Text>
            {bare && versionBadge(v, current)}
          </Group>
        </Field>

        {st === null && (
          <Text size={FS.body} c="dimmed" lh={1.5}>
            This agent is too old to update itself from the console. Upgrade the argus-agent package on
            the server once, by hand; from then on it updates from here.
          </Text>
        )}
        {st && !st.supported && (
          <Text size={FS.body} c="dimmed" lh={1.5}>Updating from the console is not available here: {st.reason}.</Text>
        )}

        {live && <UpdateSteps view={v} status={pickedUp ? st : null} />}
        {!live && st?.state && st.id && stillTrue(st, v.agentVersion) && <LastUpdate status={st} />}

        {canEdit && live && (
          <Group gap={SP.cozy}>
            {pickedUp && st?.state === 'waiting' && !v.request?.now && (
              <Button size="compact-xs" color="rose" variant="light" loading={request.isPending}
                onClick={() => void run(() => request.mutateAsync({ version: v.request?.version, now: true }), {
                  failure: 'Update not changed',
                  success: ['Updating now', `The agent ends the ${st.sessions ?? 0} open session(s) and restarts on ${v.request?.version}.`],
                })}>
                Update now, ending {st.sessions ?? 0} session(s)
              </Button>
            )}
            {(!pickedUp || st?.state === 'downloading' || st?.state === 'waiting') && (
              <Button size="compact-xs" variant="default" loading={cancel.isPending}
                onClick={() => void run(() => cancel.mutateAsync(), {
                  failure: 'Update not taken back',
                  success: ['Update taken back', 'The agent stays on the version it runs.'],
                })}>
                Take back
              </Button>
            )}
          </Group>
        )}

        {offer && (
          <Group>
            <Button size="compact-sm" leftSection={<IconRefresh size={14} />} onClick={ask.open}>
              {v.latest ? `Update to ${v.latest}` : 'Update agent'}
            </Button>
          </Group>
        )}
      </Stack>
      {asking && <AskUpdate view={v} onClose={ask.close} assetId={asset.id} />}
    </>
  )

  if (bare) return <Card padding="md">{body}</Card>
  return (
    <SectionCard
      title="Agent"
      icon={IconRefresh}
      badge={versionBadge(v, current)}
      description="The Argus agent on this server, which opens and records every session here."
    >
      {body}
    </SectionCard>
  )
}

function versionBadge(v: AgentUpdateView, current: boolean) {
  if (!v.agentVersion) return null
  if (current) return <Badge size="xs" variant="light" color="teal">up to date</Badge>
  if (v.latest) return <Badge size="xs" variant="light" color="amber">{v.latest} available</Badge>
  return null
}

const STEPS: { key: string; label: string }[] = [
  { key: 'downloading', label: 'Download the new version and check it against the release' },
  { key: 'waiting', label: 'Wait until nobody is connected to this server' },
  { key: 'restarting', label: 'Restart on the new version; logins in the meantime wait a moment instead of being refused' },
  { key: 'done', label: 'Check the new version is back and reconnected, or go back to the old one' },
]

/** The update's steps, with the one the agent last reported. */
function UpdateSteps({ view, status }: { view: AgentUpdateView; status: AgentUpdateStatus | null }) {
  const req = view.request
  if (!req) return null
  const at = status ? STEPS.findIndex((s) => s.key === status.state) : -1
  return (
    <Stack gap={SP.snug}>
      <Text size={FS.body} fw={600}>
        Updating to {req.version}{req.now ? ', now' : ''}
        <Text span size={FS.micro} c="dimmed" fw={400}> · asked by {req.requestedBy} {relTime(req.requestedAt)}</Text>
      </Text>
      {!status && (
        <Text size={FS.body} c="dimmed" lh={1.5}>
          Asked. The agent picks this up at its next check-in, within 30 seconds.
        </Text>
      )}
      {STEPS.map((s, i) => {
        const done = at > i
        const now = at === i
        return (
          <Group key={s.key} gap={SP.cozy} wrap="nowrap" align="flex-start">
            <ThemeIcon size={18} radius="xl" variant={done ? 'filled' : 'light'} color={done ? 'teal' : now ? 'azure' : 'slate'}>
              {done ? <IconCheck size={12} /> : now ? <Loader size={10} color="azure" /> : <IconCircleDashed size={12} />}
            </ThemeIcon>
            <Text size={FS.body} c={done || now ? undefined : 'dimmed'} lh={1.4}>
              {s.label}
              {now && s.key === 'waiting' && status?.sessions
                ? ` (${status.sessions} open)` : ''}
            </Text>
          </Group>
        )
      })}
      {status?.message && <Text size={FS.micro} c="dimmed">The agent says: {status.message}.</Text>}
    </Stack>
  )
}

/**
 * Whether the last outcome still describes the server: an update that went
 * back to 1.0 says nothing true about an agent someone has since upgraded by
 * hand.
 */
function stillTrue(st: AgentUpdateStatus, running: string) {
  return st.state === 'done' ? st.target === running : st.from === running
}

/** How the last update went, as the agent reported it. */
function LastUpdate({ status }: { status: AgentUpdateStatus }) {
  const tone = {
    done: ['teal', 'Updated'],
    rolled_back: ['amber', 'Update went back'],
    failed: ['rose', 'Update failed'],
    cancelled: ['slate', 'Update taken back'],
  }[status.state as 'done' | 'rolled_back' | 'failed' | 'cancelled']
  if (!tone) return null
  return (
    <Alert color={tone[0]} title={tone[1]} p="xs">
      <Text size={FS.body} lh={1.5}>
        {status.message ? `${status.message.charAt(0).toUpperCase()}${status.message.slice(1)}.` : ''}
        {status.at && <Text span size={FS.micro} c="dimmed"> {relTime(status.at)}</Text>}
      </Text>
    </Alert>
  )
}

function AskUpdate({ view, assetId, onClose }: { view: AgentUpdateView; assetId: string; onClose: () => void }) {
  const request = useRequestAgentUpdate(assetId)
  const [version, setVersion] = useState(view.latest)
  const ok = RELEASE.test(version)
  const open = view.activeSessions
  const go = (now: boolean) =>
    run(() => request.mutateAsync({ version, now }), {
      failure: 'Update not started',
      success: now
        ? ['Updating now', `The agent restarts on ${version} as soon as it has downloaded it.`]
        : ['Update asked', `The agent restarts on ${version} once nobody is connected to ${view.hostname}.`],
    }).then((done) => done && onClose())

  return (
    <Modal opened onClose={onClose} title={`Update the agent on ${view.hostname}`} size="lg">
      <Stack gap="sm">
        <Text size={FS.body} lh={1.5}>
          The agent downloads {ok ? version : 'the new version'} and checks it, then waits until nobody is
          connected to this server before it restarts. The restart takes a few seconds, and anyone who logs
          in during it waits a moment instead of being refused. If the new version does not come back and
          reconnect to Argus within 90 seconds, the agent goes back to {view.agentVersion || 'the version it runs'} on its own.
        </Text>
        {!view.latest && (
          <TextInput label="Version" description="This control plane was built from source, so name the release to install."
            placeholder="1.2.3" value={version} onChange={(e) => setVersion(e.currentTarget.value.trim())}
            error={version && !ok ? 'A release looks like 1.2.3' : undefined} />
        )}
        {open > 0 && (
          <Alert color="amber" p="xs">
            <Text size={FS.body} lh={1.5}>
              {open} session(s) are open on this server now. Waiting lets them finish. Updating now closes
              sessions opened from the console; SSH logins stay connected, and their recording carries on
              under the new agent.
            </Text>
          </Alert>
        )}
        <Group justify="flex-end" gap={SP.cozy}>
          <Button variant="default" onClick={onClose}>Cancel</Button>
          {open > 0 && (
            <Button color="rose" variant="light" disabled={!ok} loading={request.isPending} onClick={() => void go(true)}>
              Update now, ending {open}
            </Button>
          )}
          <Button disabled={!ok} loading={request.isPending} onClick={() => void go(false)}>
            {open > 0 ? 'Update when nobody is connected' : 'Update'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
